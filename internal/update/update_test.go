package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDetectMethodOrder(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "listo")
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := DetectMethod(env(nil), exe); got != MethodUnknown {
		t.Fatalf("no signal at all must be unknown, got %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, MethodFile), []byte("script\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DetectMethod(env(nil), exe); got != MethodScript {
		t.Fatalf("the file beside the binary says script, got %q", got)
	}
	// The environment wins over the file.
	if got := DetectMethod(env(map[string]string{EnvInstallMethod: "npm"}), exe); got != MethodNPM {
		t.Fatalf("env must win, got %q", got)
	}
	if got := DetectMethod(env(map[string]string{EnvInstallMethod: "nonsense"}), exe); got != MethodScript {
		t.Fatalf("a bad env value is ignored, got %q", got)
	}
}

func TestDetectMethodHomebrewCellar(t *testing.T) {
	cellar := filepath.Join(t.TempDir(), "Cellar", "listo", "0.1.0", "bin")
	if err := os.MkdirAll(cellar, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(cellar, "listo")
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := DetectMethod(env(nil), exe); got != MethodBrew {
		t.Fatalf("got %q", got)
	}
}

func TestUpgradeCommands(t *testing.T) {
	cases := map[Method]string{
		MethodNPM:     "npm i -g listo-cli",
		MethodBrew:    "brew upgrade listo",
		MethodScript:  "listo update",
		MethodUnknown: "",
	}
	for m, want := range cases {
		if got := m.UpgradeCommand(); got != want {
			t.Errorf("%q: got %q want %q", m, got, want)
		}
	}
}

func TestNewer(t *testing.T) {
	yes := [][2]string{{"0.1.0", "v0.1.1"}, {"0.1.9", "0.2.0"}, {"v0.9.0", "1.0.0"}, {"0.1.0", "0.1.10"}}
	no := [][2]string{{"0.1.0", "0.1.0"}, {"0.2.0", "v0.1.9"}, {"0.1.0", "garbage"}, {"", "0.1.0"}, {"0.1.0", "0.2.0-rc1"}}
	for _, c := range yes {
		if !Newer(c[0], c[1]) {
			t.Errorf("%v should be newer", c)
		}
	}
	for _, c := range no {
		if Newer(c[0], c[1]) {
			t.Errorf("%v should not be newer", c)
		}
	}
}

func TestDueAndStamp(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	if !Due(dir, now) {
		t.Fatal("no stamp means due")
	}
	if err := Stamp(dir, now); err != nil {
		t.Fatal(err)
	}
	if Due(dir, now.Add(23*time.Hour)) {
		t.Fatal("23 hours later is not due")
	}
	if !Due(dir, now.Add(24*time.Hour)) {
		t.Fatal("24 hours later is due")
	}
	if !Due(dir, now.Add(-time.Hour)) {
		t.Fatal("a stamp in the future (clock moved back) is due")
	}
}

func releaseServer(t *testing.T, tag string, hits *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			atomic.AddInt32(hits, 1)
		}
		if r.Header.Get("User-Agent") == "" {
			t.Error("the lookup must send a User-Agent")
		}
		_, _ = w.Write([]byte(`{"tag_name":"` + tag + `"}`))
	}))
}

func TestCheckForNoticeOncePerDay(t *testing.T) {
	var hits int32
	srv := releaseServer(t, "v0.2.0", &hits)
	defer srv.Close()
	dir := t.TempDir()
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	opts := NoticeOptions{Dir: dir, Now: now, Current: "0.1.0", URL: srv.URL}

	if got := CheckForNotice(context.Background(), opts); got != "v0.2.0" {
		t.Fatalf("first check: %q", got)
	}
	opts.Now = now.Add(time.Hour)
	if got := CheckForNotice(context.Background(), opts); got != "" {
		t.Fatalf("a second check within a day must stay silent, got %q", got)
	}
	if hits != 1 {
		t.Fatalf("one call a day at most, got %d", hits)
	}
	opts.Now = now.Add(25 * time.Hour)
	if got := CheckForNotice(context.Background(), opts); got != "v0.2.0" {
		t.Fatalf("a day later: %q", got)
	}
	if hits != 2 {
		t.Fatalf("hits %d", hits)
	}
}

func TestCheckForNoticeSilentWhenCurrentOrFailing(t *testing.T) {
	srv := releaseServer(t, "v0.1.0", nil)
	defer srv.Close()
	now := time.Now()
	if got := CheckForNotice(context.Background(), NoticeOptions{Dir: t.TempDir(), Now: now, Current: "0.1.0", URL: srv.URL}); got != "" {
		t.Fatalf("up to date must be silent, got %q", got)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", 500) }))
	defer bad.Close()
	dir := t.TempDir()
	if got := CheckForNotice(context.Background(), NoticeOptions{Dir: dir, Now: now, Current: "0.1.0", URL: bad.URL}); got != "" {
		t.Fatalf("a failing lookup must be silent, got %q", got)
	}
	// The failed attempt still counts, so an offline computer is not retried on every command.
	if Due(dir, now.Add(time.Minute)) {
		t.Fatal("a failed check must still stamp")
	}

	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("not json")) }))
	defer junk.Close()
	if got := CheckForNotice(context.Background(), NoticeOptions{Dir: t.TempDir(), Now: now, Current: "0.1.0", URL: junk.URL}); got != "" {
		t.Fatalf("junk must be silent, got %q", got)
	}
	if got := CheckForNotice(context.Background(), NoticeOptions{Dir: t.TempDir(), Now: now, Current: "0.1.0", URL: "http://127.0.0.1:1/x"}); got != "" {
		t.Fatalf("refused connection must be silent, got %q", got)
	}
}

// ---- Apply ----

type fixtureRelease struct {
	key       *ecdsa.PrivateKey
	keyPEM    []byte
	archive   []byte
	checksums []byte
	sig       []byte
	name      string
	binary    []byte
}

func newKey(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func tarGz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func newRelease(t *testing.T, goos, goarch string) fixtureRelease {
	t.Helper()
	key, keyPEM := newKey(t)
	name, err := ArchiveName(goos, goarch)
	if err != nil {
		t.Fatal(err)
	}
	binary := []byte("#!/bin/sh\necho new listo\n")
	archive := tarGz(t, map[string][]byte{"LICENSE": []byte("MIT"), "listo": binary})
	sum := sha256.Sum256(archive)
	checksums := []byte(hex.EncodeToString(sum[:]) + "  " + name + "\n" +
		strings.Repeat("0", 64) + "  listo_windows_amd64.zip\n")
	digest := sha256.Sum256(checksums)
	sig, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return fixtureRelease{key: key, keyPEM: keyPEM, archive: archive, checksums: checksums, sig: sig, name: name, binary: binary}
}

func (r fixtureRelease) serve(t *testing.T, tag string) *httptest.Server {
	t.Helper()
	files := map[string][]byte{
		"/download/" + tag + "/checksums.txt":     r.checksums,
		"/download/" + tag + "/checksums.txt.sig": r.sig,
		"/download/" + tag + "/" + r.name:         r.archive,
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, ok := files[req.URL.Path]
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(body)
	}))
}

func installedExe(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "listo")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

func TestApplyReplacesTheBinaryAtomically(t *testing.T) {
	rel := newRelease(t, "linux", "amd64")
	srv := rel.serve(t, "v0.2.0")
	defer srv.Close()
	exe := installedExe(t)

	a := &Applier{Base: srv.URL, Key: rel.keyPEM, GOOS: "linux", GOARCH: "amd64", Exe: exe}
	if err := a.Apply(context.Background(), "v0.2.0"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(exe)
	if !bytes.Equal(got, rel.binary) {
		t.Fatalf("binary not replaced: %q", got)
	}
	info, _ := os.Stat(exe)
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", info.Mode())
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".listo-update-*"))
	if len(left) != 0 {
		t.Fatalf("temporary files left behind: %v", left)
	}
}

func TestApplyRefusesAndChangesNothing(t *testing.T) {
	type tamper func(*fixtureRelease)
	cases := map[string]struct {
		change tamper
		want   error
	}{
		"tampered archive": {func(r *fixtureRelease) {
			r.archive = append([]byte(nil), r.archive...)
			r.archive[len(r.archive)-1] ^= 0xff
		}, ErrChecksum},
		"tampered signature": {func(r *fixtureRelease) {
			r.sig = append([]byte(nil), r.sig...)
			r.sig[len(r.sig)-1] ^= 0xff
		}, ErrBadSignature},
		"tampered checksums": {func(r *fixtureRelease) { r.checksums = append([]byte("# x\n"), r.checksums...) }, ErrBadSignature},
		"signed by another key": {func(r *fixtureRelease) {
			other, _ := newKey(t)
			digest := sha256.Sum256(r.checksums)
			r.sig, _ = ecdsa.SignASN1(rand.Reader, other, digest[:])
		}, ErrBadSignature},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rel := newRelease(t, "darwin", "arm64")
			c.change(&rel)
			srv := rel.serve(t, "v0.2.0")
			defer srv.Close()
			exe := installedExe(t)
			a := &Applier{Base: srv.URL, Key: rel.keyPEM, GOOS: "darwin", GOARCH: "arm64", Exe: exe}
			err := a.Apply(context.Background(), "v0.2.0")
			if !errors.Is(err, c.want) {
				t.Fatalf("want %v, got %v", c.want, err)
			}
			got, _ := os.ReadFile(exe)
			if string(got) != "old binary" {
				t.Fatalf("the binary must stay as it was, got %q", got)
			}
		})
	}
}

func TestApplyPlaceholderKeyAndPlatform(t *testing.T) {
	rel := newRelease(t, "linux", "amd64")
	srv := rel.serve(t, "v0.2.0")
	defer srv.Close()
	exe := installedExe(t)

	a := &Applier{Base: srv.URL, Key: []byte("LISTO_CLI_PUBLIC_KEY_PLACEHOLDER\n"), GOOS: "linux", GOARCH: "amd64", Exe: exe}
	if err := a.Apply(context.Background(), "v0.2.0"); !errors.Is(err, ErrNoSigningKey) {
		t.Fatalf("placeholder must say there is no key, got %v", err)
	}
	// The shipped build carries the release public key, a real P-256 key.
	if _, err := ParseKey(SigningKey()); err != nil {
		t.Fatalf("the embedded release key must parse: %v", err)
	}
	a = &Applier{Base: srv.URL, Key: rel.keyPEM, GOOS: "windows", GOARCH: "amd64", Exe: exe}
	if err := a.Apply(context.Background(), "v0.2.0"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("windows: %v", err)
	}
	a = &Applier{Base: srv.URL, Key: rel.keyPEM, GOOS: "linux", GOARCH: "amd64", Exe: exe}
	if err := a.Apply(context.Background(), "v9.9.9"); err == nil {
		t.Fatal("an unknown tag must fail")
	}
}

func TestApplyUnwritableFolder(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	rel := newRelease(t, "linux", "amd64")
	srv := rel.serve(t, "v0.2.0")
	defer srv.Close()
	exe := installedExe(t)
	if err := os.Chmod(filepath.Dir(exe), 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Dir(exe), 0o755)
	a := &Applier{Base: srv.URL, Key: rel.keyPEM, GOOS: "linux", GOARCH: "amd64", Exe: exe}
	if err := a.Apply(context.Background(), "v0.2.0"); !errors.Is(err, ErrNotWritable) {
		t.Fatalf("got %v", err)
	}
}

// OpenSSL is what signs a release (GoReleaser calls it), so the Go verifier is
// proven against its real output, not only against Go's own signer.
func TestVerifiesWhatOpenSSLSigns(t *testing.T) {
	openssl, err := exec.LookPath("openssl")
	if err != nil {
		t.Skip("openssl is not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(openssl, args...).CombinedOutput(); err != nil {
			t.Fatalf("openssl %v: %v\n%s", args, err, out)
		}
	}
	priv, pub, file, sigFile := filepath.Join(dir, "k.pem"), filepath.Join(dir, "pub.pem"), filepath.Join(dir, "checksums.txt"), filepath.Join(dir, "checksums.txt.sig")
	run("ecparam", "-name", "prime256v1", "-genkey", "-noout", "-out", priv)
	run("ec", "-in", priv, "-pubout", "-out", pub)
	if err := os.WriteFile(file, []byte("abc  listo_linux_amd64.tar.gz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("dgst", "-sha256", "-sign", priv, "-out", sigFile, file)

	rawPub, _ := os.ReadFile(pub)
	key, err := ParseKey(rawPub)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(file)
	sig, _ := os.ReadFile(sigFile)
	if err := VerifyChecksums(key, body, sig); err != nil {
		t.Fatalf("a signature made by openssl must verify: %v", err)
	}
	if err := VerifyChecksums(key, append(body, 'x'), sig); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a changed file must not verify: %v", err)
	}
}

// The install script and the Go build carry the same release key, typed twice.
func TestInstallScriptKeyEqualsEmbeddedKey(t *testing.T) {
	script, err := os.ReadFile("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)\n[ \t]*PUBLIC_KEY='(.*?)'\n`).FindSubmatch(script)
	if m == nil {
		t.Fatal("install.sh has no PUBLIC_KEY='...' line")
	}
	if got, want := strings.TrimSpace(string(m[1])), strings.TrimSpace(string(SigningKey())); got != want {
		t.Fatalf("install.sh key and internal/update/signing_key.pem differ:\n%s\n---\n%s", got, want)
	}
}

func TestChecksumFor(t *testing.T) {
	body := []byte("AAAA  listo_linux_amd64.tar.gz\nbbbb  *listo_darwin_arm64.tar.gz\n\nnot a line\n")
	if got, ok := ChecksumFor(body, "listo_linux_amd64.tar.gz"); !ok || got != "aaaa" {
		t.Fatalf("%q %v", got, ok)
	}
	if got, ok := ChecksumFor(body, "listo_darwin_arm64.tar.gz"); !ok || got != "bbbb" {
		t.Fatalf("%q %v", got, ok)
	}
	if _, ok := ChecksumFor(body, "missing"); ok {
		t.Fatal("missing must be absent")
	}
}
