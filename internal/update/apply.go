package update

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/listo-delivery/listo-cli/internal/version"
)

// The ways an update stops. Each leaves the installed binary exactly as it was.
var (
	// ErrNoSigningKey means this build still carries the placeholder, so it has
	// nothing to verify a release with and refuses to install one.
	ErrNoSigningKey = errors.New("this build has no release signing key")
	// ErrBadSignature means checksums.txt was not signed by the release key.
	ErrBadSignature = errors.New("the checksums file signature does not match the release key")
	// ErrChecksum means the archive's sha256 is not the one checksums.txt lists.
	ErrChecksum = errors.New("the downloaded archive does not match its checksum")
	// ErrUnsupported means there is no archive for this operating system or
	// architecture that this updater can unpack.
	ErrUnsupported = errors.New("self-update is not available on this platform")
	// ErrNotWritable means the folder holding the binary cannot be written.
	ErrNotWritable = errors.New("the folder holding listo is not writable")
)

//go:embed signing_key.pem
var embeddedKey []byte

// placeholder is what signing_key.pem and install.sh held before the release key
// pair existed. It stays as a guard: the release sync refuses to publish while either
// file has it, and this build refuses to update with it.
const placeholder = "LISTO_CLI_PUBLIC_KEY_PLACEHOLDER"

// SigningKey is the release public key (PEM) this build verifies with.
func SigningKey() []byte { return embeddedKey }

// ParseKey reads an ECDSA public key in PEM. The placeholder answers
// ErrNoSigningKey, never a parse error, so the person is told the real reason.
func ParseKey(raw []byte) (*ecdsa.PublicKey, error) {
	if bytes.Contains(raw, []byte(placeholder)) {
		return nil, ErrNoSigningKey
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("release signing key is not PEM")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("release signing key: %w", err)
	}
	key, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("release signing key is not an ECDSA key")
	}
	return key, nil
}

// VerifyChecksums checks sig (the DER ECDSA signature `openssl dgst -sha256
// -sign` writes) over the bytes of checksums.txt.
func VerifyChecksums(key *ecdsa.PublicKey, checksums, sig []byte) error {
	sum := sha256.Sum256(checksums)
	if !ecdsa.VerifyASN1(key, sum[:], sig) {
		return ErrBadSignature
	}
	return nil
}

// ArchiveName is the release archive for a platform, the name install.sh and
// the npm postinstall build too: listo_<os>_<arch>.tar.gz.
func ArchiveName(goos, goarch string) (string, error) {
	switch goos {
	case "linux", "darwin":
	default:
		return "", ErrUnsupported
	}
	switch goarch {
	case "amd64", "arm64":
	default:
		return "", ErrUnsupported
	}
	return "listo_" + goos + "_" + goarch + ".tar.gz", nil
}

// ChecksumFor finds the sha256 listed for name in a checksums.txt body
// ("<hex>  <file>" per line).
func ChecksumFor(checksums []byte, name string) (string, bool) {
	sc := bufio.NewScanner(bytes.NewReader(checksums))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			return strings.ToLower(fields[0]), true
		}
	}
	return "", false
}

// Applier downloads one release and installs it over the running binary.
type Applier struct {
	HTTP         *http.Client // nil means a client with a five minute timeout
	Base         string       // empty means DownloadBase
	Key          []byte       // empty means the embedded release key
	GOOS, GOARCH string
	Exe          string // the binary to replace
}

const (
	maxSmall   = 1 << 20   // checksums.txt and its signature
	maxArchive = 200 << 20 // a release archive
)

// Apply installs the release tagged tag. Order matters: the signature over
// checksums.txt is checked first, then the archive against the checksum, and
// only then is anything written beside the binary. The swap is one rename.
func (a *Applier) Apply(ctx context.Context, tag string) error {
	name, err := ArchiveName(a.GOOS, a.GOARCH)
	if err != nil {
		return err
	}
	rawKey := a.Key
	if len(rawKey) == 0 {
		rawKey = SigningKey()
	}
	key, err := ParseKey(rawKey)
	if err != nil {
		return err
	}
	base := a.Base
	if base == "" {
		base = DownloadBase
	}
	client := a.HTTP
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	dir := strings.TrimSuffix(base, "/") + "/download/" + tag + "/"

	checksums, err := fetch(ctx, client, dir+"checksums.txt", maxSmall)
	if err != nil {
		return err
	}
	sig, err := fetch(ctx, client, dir+"checksums.txt.sig", maxSmall)
	if err != nil {
		return err
	}
	if err := VerifyChecksums(key, checksums, sig); err != nil {
		return err
	}
	want, ok := ChecksumFor(checksums, name)
	if !ok {
		return fmt.Errorf("%w: %s is not listed in checksums.txt", ErrChecksum, name)
	}
	archive, err := fetch(ctx, client, dir+name, maxArchive)
	if err != nil {
		return err
	}
	got := sha256.Sum256(archive)
	if hex.EncodeToString(got[:]) != want {
		return ErrChecksum
	}
	return replaceBinary(a.Exe, archive)
}

func fetch(ctx context.Context, client *http.Client, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", version.UserAgent())
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %d", path.Base(url), res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", path.Base(url), limit)
	}
	return body, nil
}

// replaceBinary unpacks the "listo" entry of a tar.gz into a temporary file in
// exe's own folder (same filesystem, so the rename is atomic) and renames it
// over exe. A running copy keeps its old inode on Unix, so this is safe while
// listo itself is the process doing it.
func replaceBinary(exe string, archive []byte) error {
	target := exe
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		target = resolved
	}
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".listo-update-*")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNotWritable, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if err := unpackBinary(tmp, archive); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		cleanup()
		return fmt.Errorf("%w: %v", ErrNotWritable, err)
	}
	return nil
}

func unpackBinary(dst io.Writer, archive []byte) error {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrChecksum, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return errors.New("the archive holds no listo binary")
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg || path.Base(h.Name) != "listo" {
			continue
		}
		if h.Size > maxArchive {
			return errors.New("the binary in the archive is too large")
		}
		_, err = io.Copy(dst, io.LimitReader(tr, maxArchive))
		return err
	}
}
