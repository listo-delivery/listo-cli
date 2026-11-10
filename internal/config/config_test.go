package config

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"
)

type fakeKeyring struct {
	mu    sync.Mutex
	items map[string]string
	fail  bool // every call fails like a headless machine with no store

	failSet    bool // only Set fails: a store that reads but cannot be written
	failDelete bool // only Delete fails: an item that cannot be forgotten

	// onCall, when set, is called (without the lock held, so it may Load) before
	// and after every operation: "get:before", "set:after", ...
	onCall func(op string)
}

func newFakeKeyring() *fakeKeyring { return &fakeKeyring{items: map[string]string{}} }

func (f *fakeKeyring) trace(op string) {
	if f.onCall != nil {
		f.onCall(op)
	}
}

func (f *fakeKeyring) Get(service, user string) (string, error) {
	f.trace("get:before")
	defer f.trace("get:after")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return "", errors.New("no secret service")
	}
	v, ok := f.items[service+"/"+user]
	if !ok {
		return "", ErrKeyringNotFound
	}
	return v, nil
}

func (f *fakeKeyring) Set(service, user, secret string) error {
	f.trace("set:before")
	defer f.trace("set:after")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail || f.failSet {
		return errors.New("no secret service")
	}
	f.items[service+"/"+user] = secret
	return nil
}

func (f *fakeKeyring) Delete(service, user string) error {
	f.trace("delete:before")
	defer f.trace("delete:after")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail || f.failDelete {
		return errors.New("no secret service")
	}
	if _, ok := f.items[service+"/"+user]; !ok {
		return ErrKeyringNotFound
	}
	delete(f.items, service+"/"+user)
	return nil
}

func ctx() context.Context { return context.Background() }

func grant() Record {
	return Record{Credential: &Credential{
		AccessToken: "at_1", RefreshToken: "rt_1", DeviceName: "MacBook de Ana",
		AccessTokenExpiresAt: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC),
	}}
}

func TestFileBackendRoundTripAndPermissions(t *testing.T) {
	s, err := Open(Options{Dir: filepath.Join(t.TempDir(), "listo")})
	if err != nil {
		t.Fatal(err)
	}
	where, err := s.Save(ctx(), Test, grant())
	if err != nil || where != BackendFile {
		t.Fatalf("Save = %q, %v; want the file backend", where, err)
	}
	got, err := s.Load(Test)
	if err != nil {
		t.Fatal(err)
	}
	if got.Backend != BackendFile || got.Credential == nil || got.Credential.RefreshToken != "rt_1" || got.Credential.Mode != Test {
		t.Fatalf("round trip lost data: %+v %+v", got, got.Credential)
	}
	if !got.Credential.AccessTokenExpiresAt.Equal(grant().Credential.AccessTokenExpiresAt) {
		t.Fatalf("expiry changed: %v", got.Credential.AccessTokenExpiresAt)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(s.Dir(), "credentials.json"))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("credentials.json mode %v, want 0600", fi.Mode().Perm())
		}
		di, err := os.Stat(s.Dir())
		if err != nil {
			t.Fatal(err)
		}
		if di.Mode().Perm() != 0o700 {
			t.Fatalf("config dir mode %v, want 0700", di.Mode().Perm())
		}
	}
	if live, _ := s.Load(Live); live.Credential != nil {
		t.Fatal("the live record must be empty")
	}
}

func TestKeyBeatsNothingAndModesAreIndependent(t *testing.T) {
	s, _ := Open(Options{Dir: t.TempDir()})
	if _, err := s.Save(ctx(), Live, Record{Credential: &Credential{APIKey: "lst_live_abc"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx(), Test, grant()); err != nil {
		t.Fatal(err)
	}
	live, _ := s.Load(Live)
	test, _ := s.Load(Test)
	if !live.Credential.IsKey() || live.Credential.IsGrant() {
		t.Fatalf("live must be a key: %+v", live.Credential)
	}
	if !test.Credential.IsGrant() || test.Credential.IsKey() {
		t.Fatalf("test must be a grant: %+v", test.Credential)
	}
}

func TestKeyringIsUsedFirstAndDropsTheFileCopy(t *testing.T) {
	kr := newFakeKeyring()
	dir := t.TempDir()
	fileOnly, _ := Open(Options{Dir: dir})
	if _, err := fileOnly.Save(ctx(), Test, grant()); err != nil {
		t.Fatal(err)
	}
	s, _ := Open(Options{Dir: dir, Keyring: kr})
	// Before the move the record is found in the file.
	if r, _ := s.Load(Test); r.Backend != BackendFile {
		t.Fatalf("expected the file copy to be found first, got %q", r.Backend)
	}
	where, err := s.Save(ctx(), Test, grant())
	if err != nil || where != BackendKeyring {
		t.Fatalf("Save = %q, %v; want keyring", where, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "credentials.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the file copy must be removed once the store holds it: %v", err)
	}
	if r, _ := s.Load(Test); r.Backend != BackendKeyring || r.Credential.RefreshToken != "rt_1" {
		t.Fatalf("expected the keyring copy, got %+v", r)
	}
}

func TestFallsBackToTheFileWhenNoStoreAnswers(t *testing.T) {
	kr := newFakeKeyring()
	kr.fail = true
	s, _ := Open(Options{Dir: t.TempDir(), Keyring: kr})
	where, err := s.Save(ctx(), Test, grant())
	if err != nil || where != BackendFile {
		t.Fatalf("Save = %q, %v; want the file fallback", where, err)
	}
	r, err := s.Load(Test)
	if err != nil || r.Backend != BackendFile || r.Credential.AccessToken != "at_1" {
		t.Fatalf("Load = %+v, %v", r, err)
	}
}

func grantWith(access, refresh string) Record {
	r := grant()
	r.Credential.AccessToken = access
	r.Credential.RefreshToken = refresh
	return r
}

// An older record sits in the store, the store then refuses both a write and a
// delete: the newer file record is the authority, before and after the store
// recovers, and the next successful store write takes the mode back.
func TestAFileFallbackBeatsAnOlderStoreRecordThatCannotBeDeleted(t *testing.T) {
	kr := newFakeKeyring()
	dir := t.TempDir()
	s, _ := Open(Options{Dir: dir, Keyring: kr})
	if where, err := s.Save(ctx(), Test, grantWith("at_old", "rt_old")); err != nil || where != BackendKeyring {
		t.Fatalf("seeding the store: %q, %v", where, err)
	}

	kr.failSet, kr.failDelete = true, true
	where, err := s.Save(ctx(), Test, grantWith("at_new", "rt_new"))
	if err != nil || where != BackendFile {
		t.Fatalf("Save = %q, %v; want the file fallback", where, err)
	}
	got, err := s.Load(Test)
	if err != nil || got.Backend != BackendFile || got.Credential.RefreshToken != "rt_new" {
		t.Fatalf("Load preferred the stale store record: %+v, %v", got, err)
	}
	if _, stale := kr.items["listo-cli/test"]; !stale {
		t.Fatal("the premise failed: the older store record should still be there")
	}

	// The store comes back: the file still decides until a write reaches the store.
	kr.failSet, kr.failDelete = false, false
	if got, _ := s.Load(Test); got.Backend != BackendFile || got.Credential.RefreshToken != "rt_new" {
		t.Fatalf("recovery of the store changed the authority: %+v", got)
	}
	if where, err := s.Save(ctx(), Test, grantWith("at_newest", "rt_newest")); err != nil || where != BackendKeyring {
		t.Fatalf("Save after recovery = %q, %v; want keyring", where, err)
	}
	got, _ = s.Load(Test)
	if got.Backend != BackendKeyring || got.Credential.RefreshToken != "rt_newest" {
		t.Fatalf("expected the store's new record, got %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "credentials.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the file entry must be gone once the store holds the mode: %v", err)
	}
}

func TestStoreWriteFailureWithASuccessfulDeleteLeavesTheFileRecord(t *testing.T) {
	kr := newFakeKeyring()
	s, _ := Open(Options{Dir: t.TempDir(), Keyring: kr})
	if _, err := s.Save(ctx(), Test, grantWith("at_old", "rt_old")); err != nil {
		t.Fatal(err)
	}
	kr.failSet = true
	if where, err := s.Save(ctx(), Test, grantWith("at_new", "rt_new")); err != nil || where != BackendFile {
		t.Fatalf("Save = %q, %v", where, err)
	}
	if _, stale := kr.items["listo-cli/test"]; stale {
		t.Fatal("the older store record should have been deleted")
	}
	if got, _ := s.Load(Test); got.Backend != BackendFile || got.Credential.RefreshToken != "rt_new" {
		t.Fatalf("Load = %+v", got)
	}
}

// The other mode must not be touched by one mode's fallback.
func TestAFallbackIsPerMode(t *testing.T) {
	kr := newFakeKeyring()
	s, _ := Open(Options{Dir: t.TempDir(), Keyring: kr})
	if _, err := s.Save(ctx(), Live, Record{Credential: &Credential{APIKey: "lst_live_abc"}}); err != nil {
		t.Fatal(err)
	}
	kr.failSet = true
	if _, err := s.Save(ctx(), Test, grant()); err != nil {
		t.Fatal(err)
	}
	live, _ := s.Load(Live)
	test, _ := s.Load(Test)
	if live.Backend != BackendKeyring || live.Credential.APIKey != "lst_live_abc" || test.Backend != BackendFile {
		t.Fatalf("modes crossed: live %+v test %+v", live, test)
	}
}

// A refresh rotates the token under the lock; when the store cannot be written
// the rotated token lands in the file and the next reader sees it, not the
// older store copy (a stale refresh token would revoke the whole grant).
func TestARefreshUnderTheLockThroughTheFallbackIsWhatTheNextReaderSees(t *testing.T) {
	kr := newFakeKeyring()
	s, _ := Open(Options{Dir: t.TempDir(), Keyring: kr})
	if _, err := s.Save(ctx(), Test, grantWith("at_1", "rt_1")); err != nil {
		t.Fatal(err)
	}
	kr.failSet, kr.failDelete = true, true
	err := s.WithLock(ctx(), func(tx *Tx) error {
		rec, err := tx.Load(Test)
		if err != nil {
			return err
		}
		if rec.Credential.RefreshToken != "rt_1" {
			t.Errorf("the refresh started from %q", rec.Credential.RefreshToken)
		}
		rec.Credential.AccessToken, rec.Credential.RefreshToken = "at_2", "rt_2"
		where, err := tx.Save(Test, rec)
		if where != BackendFile {
			t.Errorf("Save = %q, want the file", where)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// A second terminal taking the lock afterwards reads the rotated token.
	err = s.WithLock(ctx(), func(tx *Tx) error {
		rec, err := tx.Load(Test)
		if err != nil {
			return err
		}
		if rec.Credential.RefreshToken != "rt_2" || rec.Backend != BackendFile {
			t.Errorf("the second terminal read %+v", rec)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A "keyring" marker left behind in the file (its removal failed) hands the mode
// to the store, and a file written before the marker existed is the file's.
func TestMarkersInTheFile(t *testing.T) {
	kr := newFakeKeyring()
	dir := t.TempDir()
	s, _ := Open(Options{Dir: dir, Keyring: kr})
	if _, err := s.Save(ctx(), Test, grantWith("at_s", "rt_s")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(path, []byte(`{"test":{"holder":"keyring"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Load(Test); got.Backend != BackendKeyring || got.Credential.RefreshToken != "rt_s" {
		t.Fatalf("a keyring marker must hand the mode to the store: %+v", got)
	}
	legacy := `{"test":{"credential":{"mode":"test","access_token":"at_f","refresh_token":"rt_f"}}}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Load(Test); got.Backend != BackendFile || got.Credential.RefreshToken != "rt_f" {
		t.Fatalf("an entry with no marker is the file's: %+v", got)
	}
	plain, _ := Open(Options{Dir: dir})
	if got, _ := plain.Load(Test); got.Backend != BackendFile || got.Credential.RefreshToken != "rt_f" {
		t.Fatalf("without a store the file answers as before: %+v", got)
	}
}

// A store that will not forget an item it can still read must not bring the
// record back from the dead after logout.
func TestDeleteThatTheStoreRefusesDoesNotResurrectTheRecord(t *testing.T) {
	kr := newFakeKeyring()
	s, _ := Open(Options{Dir: t.TempDir(), Keyring: kr})
	if _, err := s.Save(ctx(), Test, grant()); err != nil {
		t.Fatal(err)
	}
	kr.failDelete = true
	if err := s.Delete(ctx(), Test); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Load(Test); got.Credential != nil || got.WebhookSecret != "" {
		t.Fatalf("logout left %+v readable", got)
	}
	// And a later login works and wins.
	if where, err := s.Save(ctx(), Test, grantWith("at_n", "rt_n")); err != nil || where != BackendKeyring {
		t.Fatalf("Save = %q, %v", where, err)
	}
}

func TestDeleteClearsEveryPlaceAndTheWebhookSecret(t *testing.T) {
	kr := newFakeKeyring()
	dir := t.TempDir()
	s, _ := Open(Options{Dir: dir, Keyring: kr})
	secret, err := s.WebhookSecret(ctx(), Test)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(ctx(), Test, func(r *Record) error { r.Credential = grant().Credential; return nil }); err != nil {
		t.Fatal(err)
	}
	// Also plant a stale copy in the file to prove Delete reaches it.
	fileOnly, _ := Open(Options{Dir: dir})
	if _, err := fileOnly.Save(ctx(), Test, grant()); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx(), Test); err != nil {
		t.Fatal(err)
	}
	r, _ := s.Load(Test)
	if r.Credential != nil || r.WebhookSecret != "" {
		t.Fatalf("Delete left %+v", r)
	}
	again, _ := s.WebhookSecret(ctx(), Test)
	if again == secret {
		t.Fatal("after logout a new secret must be minted")
	}
}

func TestWebhookSecretIsMintedOncePerMode(t *testing.T) {
	s, _ := Open(Options{Dir: t.TempDir()})
	a, err := s.WebhookSecret(ctx(), Test)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`\Awhsec_[0-9a-f]{64}\z`).MatchString(a) {
		t.Fatalf("secret %q is not whsec_ + 32 random bytes in hex", a)
	}
	b, _ := s.WebhookSecret(ctx(), Test)
	live, _ := s.WebhookSecret(ctx(), Live)
	if a != b {
		t.Fatal("the secret must not change between calls")
	}
	if a == live {
		t.Fatal("each mode has its own secret")
	}
	// A credential saved later keeps the secret beside it.
	if _, err := s.Update(ctx(), Test, func(r *Record) error { r.Credential = grant().Credential; return nil }); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.Load(Test); r.WebhookSecret != a || r.Credential == nil {
		t.Fatalf("record lost the secret or the credential: %+v", r)
	}
}

func TestCorruptFileIsATypedError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _ := Open(Options{Dir: dir})
	_, err := s.Load(Test)
	var ce *CorruptError
	if !errors.As(err, &ce) {
		t.Fatalf("got %v, want *CorruptError", err)
	}
}

func TestDefaultDirHonoursTheOverrideAndXDG(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if d, _ := DefaultDir(env(map[string]string{EnvConfigDir: "/tmp/x"})); d != "/tmp/x" {
		t.Fatalf("override ignored: %q", d)
	}
	if runtime.GOOS == "linux" {
		t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
		if d, _ := DefaultDir(env(nil)); d != "/tmp/xdg/listo" {
			t.Fatalf("XDG_CONFIG_HOME ignored: %q", d)
		}
	}
}

func TestWithLockSerialisesGoroutines(t *testing.T) {
	s, _ := Open(Options{Dir: t.TempDir()})
	const n = 12
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Update(ctx(), Test, func(r *Record) error { return bump(r) }); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	r, _ := s.Load(Test)
	if r.Credential == nil || r.Credential.DeviceName != strconv.Itoa(n) {
		t.Fatalf("lost updates: counter %+v", r.Credential)
	}
}

func bump(r *Record) error {
	if r.Credential == nil {
		r.Credential = &Credential{Mode: Test, DeviceName: "0"}
	}
	n, _ := strconv.Atoi(r.Credential.DeviceName)
	// Widen the window in which a missing lock would lose an update.
	time.Sleep(2 * time.Millisecond)
	r.Credential.DeviceName = strconv.Itoa(n + 1)
	return nil
}

func TestWithLockTimesOutWhileHeld(t *testing.T) {
	s, _ := Open(Options{Dir: t.TempDir()})
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.WithLock(ctx(), func(*Tx) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	c, cancel := context.WithTimeout(ctx(), 100*time.Millisecond)
	defer cancel()
	err := s.WithLock(c, func(*Tx) error { t.Error("ran while the lock was held"); return nil })
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("got %v, want ErrLockTimeout", err)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the holder never finished")
	}
}

// TestTwoProcessesNeverLoseAnUpdate is the two-terminals case of section 4.2:
// separate OS processes, real flock, one shared directory.
func TestTwoProcessesNeverLoseAnUpdate(t *testing.T) {
	if os.Getenv("LISTO_LOCK_HELPER_DIR") != "" {
		return
	}
	dir := t.TempDir()
	const procs, each = 3, 15
	var wg sync.WaitGroup
	for i := 0; i < procs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestLockHelperProcess$")
			cmd.Env = append(os.Environ(), "LISTO_LOCK_HELPER_DIR="+dir, "LISTO_LOCK_HELPER_N="+strconv.Itoa(each))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("helper failed: %v\n%s", err, out)
			}
		}()
	}
	wg.Wait()
	s, _ := Open(Options{Dir: dir})
	r, _ := s.Load(Test)
	if r.Credential == nil || r.Credential.DeviceName != strconv.Itoa(procs*each) {
		t.Fatalf("lost updates across processes: %+v", r.Credential)
	}
}

func TestLockHelperProcess(t *testing.T) {
	dir := os.Getenv("LISTO_LOCK_HELPER_DIR")
	if dir == "" {
		t.Skip("helper for TestTwoProcessesNeverLoseAnUpdate")
	}
	n, _ := strconv.Atoi(os.Getenv("LISTO_LOCK_HELPER_N"))
	s, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := s.Update(ctx(), Test, func(r *Record) error { return bump(r) }); err != nil {
			t.Fatal(err)
		}
	}
}
