package cmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/listo-delivery/listo-cli/internal/update"
)

// latestServer answers the release lookup with tag and counts the calls.
func latestServer(t *testing.T, tag string, hits *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(hits, 1)
		_, _ = w.Write([]byte(`{"tag_name":"` + tag + `"}`))
	}))
	old := update.LatestURL
	update.LatestURL = srv.URL
	t.Cleanup(func() { srv.Close(); update.LatestURL = old })
	return srv
}

func TestUpdateNoticeNamesTheCommandForTheInstallMethod(t *testing.T) {
	cases := map[string]string{
		"npm":    "npm i -g listo-cli",
		"brew":   "brew upgrade listo",
		"script": "listo update",
	}
	for method, command := range cases {
		var hits int32
		latestServer(t, "v9.0.0", &hits)
		r := run(t, true, "", map[string]string{update.EnvInstallMethod: method}, "version")
		if r.code != 0 || !strings.Contains(r.err, "A new version of listo is available: 9.0.0") || !strings.Contains(r.err, "Upgrade with: "+command) {
			t.Fatalf("%s: %+v", method, r)
		}
		if strings.Contains(r.out, "new version") {
			t.Fatalf("the notice goes to stderr, not stdout: %q", r.out)
		}
	}
}

func TestUpdateNoticeUnknownMethodPointsToDocsAndSpeaksSpanish(t *testing.T) {
	var hits int32
	latestServer(t, "v9.0.0", &hits)
	r := run(t, true, "", map[string]string{"LANG": "es_MX.UTF-8"}, "version")
	if !strings.Contains(r.err, "Hay una versión nueva de listo: 9.0.0") || !strings.Contains(r.err, "https://docs.listo.delivery/es/guides/cli") {
		t.Fatalf("%+v", r)
	}
}

func TestUpdateNoticeOncePerDayAndSilencers(t *testing.T) {
	var hits int32
	latestServer(t, "v9.0.0", &hits)
	dir := t.TempDir()
	base := map[string]string{"LISTO_CONFIG_DIR": dir, update.EnvInstallMethod: "npm"}

	if r := run(t, true, "", base, "version"); !strings.Contains(r.err, "new version") {
		t.Fatalf("first run must notice: %+v", r)
	}
	if r := run(t, true, "", base, "version"); r.err != "" {
		t.Fatalf("second run inside a day must be silent: %q", r.err)
	}
	if hits != 1 {
		t.Fatalf("one API call, got %d", hits)
	}

	// A day later it speaks again.
	realNow := updateNow
	updateNow = func() time.Time { return time.Now().Add(25 * time.Hour) }
	defer func() { updateNow = realNow }()
	if r := run(t, true, "", base, "version"); !strings.Contains(r.err, "new version") {
		t.Fatalf("a day later: %+v", r)
	}
	updateNow = realNow

	silent := func(name string, tty bool, env map[string]string, args ...string) {
		t.Helper()
		before := atomic.LoadInt32(&hits)
		env["LISTO_CONFIG_DIR"] = t.TempDir() // a fresh dir: the stamp cannot be what silences it
		r := run(t, tty, "", env, args...)
		if r.err != "" || atomic.LoadInt32(&hits) != before {
			t.Fatalf("%s must be silent and make no call: err=%q hits %d->%d", name, r.err, before, hits)
		}
	}
	silent("--json", true, map[string]string{update.EnvInstallMethod: "npm"}, "version", "--json")
	silent("a piped stdout/stderr (no terminal)", false, map[string]string{update.EnvInstallMethod: "npm"}, "version")
	silent("LISTO_NO_UPDATE_NOTIFIER", true, map[string]string{update.EnvInstallMethod: "npm", update.EnvNoNotifier: "1"}, "version")
	silent("completion", true, map[string]string{update.EnvInstallMethod: "npm"}, "completion", "bash")
}

func TestUpdateNoticeSilentWhenTheLookupFailsOrIsCurrent(t *testing.T) {
	old := update.LatestURL
	defer func() { update.LatestURL = old }()
	update.LatestURL = "http://127.0.0.1:1/none"
	if r := run(t, true, "", map[string]string{update.EnvInstallMethod: "npm"}, "version"); r.code != 0 || r.err != "" {
		t.Fatalf("an unreachable API must not show: %+v", r)
	}
	var hits int32
	latestServer(t, "v0.0.1", &hits)
	if r := run(t, true, "", map[string]string{update.EnvInstallMethod: "npm"}, "version"); r.err != "" {
		t.Fatalf("an older release must not show: %+v", r)
	}
}

func TestUpdateCommandForPackageManagersMakesNoCall(t *testing.T) {
	var hits int32
	latestServer(t, "v9.0.0", &hits)
	for method, want := range map[string]string{
		"npm":  "listo was installed with npm. Update it with: npm i -g listo-cli",
		"brew": "listo was installed with Homebrew. Update it with: brew upgrade listo",
	} {
		r := run(t, true, "", map[string]string{update.EnvInstallMethod: method}, "update")
		if r.code != 0 || !strings.Contains(r.out, want) {
			t.Fatalf("%s: %+v", method, r)
		}
	}
	r := run(t, true, "", nil, "update")
	if r.code != 0 || !strings.Contains(r.out, "curl -fsSL https://cli.listo.delivery/install.sh | sh") {
		t.Fatalf("unknown: %+v", r)
	}
	if hits != 0 {
		t.Fatalf("no network call for a copy another tool owns, got %d", hits)
	}
}

func TestUpdateCheck(t *testing.T) {
	var hits int32
	latestServer(t, "v9.0.0", &hits)
	r := run(t, true, "", map[string]string{update.EnvInstallMethod: "npm"}, "update", "--check")
	if r.code != 0 || !strings.Contains(r.out, "A new version is available: 9.0.0") {
		t.Fatalf("%+v", r)
	}
	r = run(t, false, "", map[string]string{update.EnvInstallMethod: "npm"}, "update", "--check")
	var j map[string]any
	if err := json.Unmarshal([]byte(r.out), &j); err != nil || j["update_available"] != true || j["latest"] != "9.0.0" || j["updated"] != false {
		t.Fatalf("%q %v", r.out, err)
	}
}

// A script install, end to end: the real command downloads a signed release
// from a local server and replaces the "installed" binary.
func TestUpdateReplacesAScriptInstall(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("self-update is Unix only")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})

	name, err := update.ArchiveName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skip("no archive for this platform")
	}
	newBinary := []byte("the new listo")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "listo", Mode: 0o755, Size: int64(len(newBinary)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(newBinary)
	tw.Close()
	gz.Close()
	sum := sha256.Sum256(buf.Bytes())
	checksums := []byte(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	digest := sha256.Sum256(checksums)
	sig, _ := ecdsa.SignASN1(rand.Reader, key, digest[:])

	files := map[string][]byte{
		"/download/v9.0.0/checksums.txt":     checksums,
		"/download/v9.0.0/checksums.txt.sig": sig,
		"/download/v9.0.0/" + name:           buf.Bytes(),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := files[r.URL.Path]; ok {
			_, _ = w.Write(body)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	var hits int32
	latestServer(t, "v9.0.0", &hits)
	oldBase, oldExe, oldKey := update.DownloadBase, executablePath, updateKey
	defer func() { update.DownloadBase, executablePath, updateKey = oldBase, oldExe, oldKey }()
	update.DownloadBase = srv.URL
	updateKey = keyPEM

	exe := filepath.Join(t.TempDir(), "listo")
	if err := os.WriteFile(exe, []byte("the old listo"), 0o755); err != nil {
		t.Fatal(err)
	}
	executablePath = func() (string, error) { return exe, nil }
	env := map[string]string{update.EnvInstallMethod: "script"}

	r := run(t, true, "", env, "update")
	if r.code != 0 || !strings.Contains(r.out, "Updated listo from") || !strings.Contains(r.out, "to 9.0.0") {
		t.Fatalf("%+v", r)
	}
	if got, _ := os.ReadFile(exe); !bytes.Equal(got, newBinary) {
		t.Fatalf("binary not replaced: %q", got)
	}

	// A tampered archive: exit 1, the binary stays, the next step is named.
	if err := os.WriteFile(exe, []byte("the old listo"), 0o755); err != nil {
		t.Fatal(err)
	}
	files["/download/v9.0.0/"+name] = append([]byte("x"), buf.Bytes()...)
	r = run(t, true, "", env, "update")
	if r.code != 1 || !strings.Contains(r.err, "did not pass verification") || !strings.Contains(r.err, "Next step:") {
		t.Fatalf("%+v", r)
	}
	if got, _ := os.ReadFile(exe); string(got) != "the old listo" {
		t.Fatalf("a refused update must leave the binary: %q", got)
	}

	// Already current: nothing is downloaded.
	latestServer(t, "v0.0.1", &hits)
	r = run(t, true, "", env, "update")
	if r.code != 0 || !strings.Contains(r.out, "listo is up to date") {
		t.Fatalf("%+v", r)
	}
}

func TestUpdateWithAPlaceholderKeyRefuses(t *testing.T) {
	var hits int32
	latestServer(t, "v9.0.0", &hits)
	exe := filepath.Join(t.TempDir(), "listo")
	_ = os.WriteFile(exe, []byte("old"), 0o755)
	oldExe, oldKey := executablePath, updateKey
	defer func() { executablePath, updateKey = oldExe, oldKey }()
	executablePath = func() (string, error) { return exe, nil }
	updateKey = []byte("LISTO_CLI_PUBLIC_KEY_PLACEHOLDER\n")
	r := run(t, true, "", map[string]string{update.EnvInstallMethod: "script"}, "update")
	if r.code != 1 || !strings.Contains(r.err, "no release signing key") || !strings.Contains(r.err, "Next step:") {
		t.Fatalf("%+v", r)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old" {
		t.Fatal("binary changed")
	}
}
