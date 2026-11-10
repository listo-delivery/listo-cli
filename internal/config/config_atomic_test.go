package config

import (
	"errors"
	"testing"
)

// seedFileAuthorityOverStaleStore leaves rt_K in the store and rt_A in the file
// as the authority, the state a fallback write leaves behind.
func seedFileAuthorityOverStaleStore(t *testing.T, s *Store, kr *fakeKeyring) {
	t.Helper()
	if _, err := s.Save(ctx(), Test, grantWith("at_K", "rt_K")); err != nil {
		t.Fatal(err)
	}
	kr.failSet, kr.failDelete = true, true
	if where, err := s.Save(ctx(), Test, grantWith("at_A", "rt_A")); err != nil || where != BackendFile {
		t.Fatalf("seeding the file authority: %q, %v", where, err)
	}
	kr.failSet, kr.failDelete = false, false
	if got, _ := s.Load(Test); got.Credential == nil || got.Credential.RefreshToken != "rt_A" {
		t.Fatalf("the premise failed: %+v", got)
	}
}

func refreshOf(r Record) string {
	if r.Credential == nil {
		return ""
	}
	return r.Credential.RefreshToken
}

// Logout while the store is unreachable must not leave the store's old record to
// come back when the store does.
func TestLogoutWithTheStoreDownDoesNotLetTheOldRecordReturn(t *testing.T) {
	kr := newFakeKeyring()
	s, _ := Open(Options{Dir: t.TempDir(), Keyring: kr})
	seedFileAuthorityOverStaleStore(t, s, kr)

	kr.fail = true // every call fails during the logout
	if err := s.Delete(ctx(), Test); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Load(Test); got.Credential != nil {
		t.Fatalf("still logged in after logout: %+v", got)
	}
	kr.fail = false // the store comes back holding rt_K
	if _, ok := kr.items["listo-cli/test"]; !ok {
		t.Fatal("the premise failed: the store should still hold the old record")
	}
	if got, _ := s.Load(Test); got.Credential != nil || got.WebhookSecret != "" {
		t.Fatalf("the store's old record came back after logout: %+v", got)
	}
	// A later login still works and wins.
	if where, err := s.Save(ctx(), Test, grantWith("at_n", "rt_n")); err != nil || where != BackendKeyring {
		t.Fatalf("Save = %q, %v", where, err)
	}
	if got, _ := s.Load(Test); refreshOf(got) != "rt_n" {
		t.Fatalf("Load = %+v", got)
	}
}

// The store cannot be written and the file cannot be replaced: the old file
// record is still what Load returns, and Save says it failed.
func TestSaveThatCannotReachEitherPlaceKeepsTheOldRecordAndReportsIt(t *testing.T) {
	kr := newFakeKeyring()
	s, _ := Open(Options{Dir: t.TempDir(), Keyring: kr})
	seedFileAuthorityOverStaleStore(t, s, kr)

	kr.failSet = true
	s.hook = func(step string) error {
		if step == "file:replace" {
			return errors.New("disk full")
		}
		return nil
	}
	if _, err := s.Save(ctx(), Test, grantWith("at_B", "rt_B")); err == nil {
		t.Fatal("Save must report that nothing could be written")
	}
	if got, _ := s.Load(Test); refreshOf(got) != "rt_A" {
		t.Fatalf("Load = %+v, want the old file record rt_A", got)
	}
}

// The store takes the new record but the file entry cannot be removed: the old
// file entry stays the authority (the store cannot outrank it), Load reads that
// one record, and Save reports the failure.
func TestStoreWrittenButFileEntryNotRemovedReadsBackOneRecordAndReports(t *testing.T) {
	kr := newFakeKeyring()
	s, _ := Open(Options{Dir: t.TempDir(), Keyring: kr})
	seedFileAuthorityOverStaleStore(t, s, kr)

	s.hook = func(step string) error {
		if step == "file:replace" {
			return errors.New("read-only file system")
		}
		return nil
	}
	if _, err := s.Save(ctx(), Test, grantWith("at_B", "rt_B")); err == nil {
		t.Fatal("Save must report the failed file replace")
	}
	if got, _ := s.Load(Test); refreshOf(got) != "rt_A" {
		t.Fatalf("Load = %+v, want the old file record rt_A", got)
	}
	// Once the file can be written again, the next save lands in the store and wins.
	s.hook = nil
	if where, err := s.Save(ctx(), Test, grantWith("at_C", "rt_C")); err != nil || where != BackendKeyring {
		t.Fatalf("Save = %q, %v", where, err)
	}
	if got, _ := s.Load(Test); refreshOf(got) != "rt_C" {
		t.Fatalf("Load = %+v", got)
	}
}

type crashed struct{}

// A process dying at ANY step of a save leaves a state from which Load returns
// the old record or the new one, never a stale third record and never nothing.
func TestACrashAtAnyStepOfASaveReadsOldOrNew(t *testing.T) {
	for _, storeWritable := range []bool{true, false} {
		finished := false
		for target := 1; target < 60 && !finished; target++ {
			kr := newFakeKeyring()
			s, _ := Open(Options{Dir: t.TempDir(), Keyring: kr})
			seedFileAuthorityOverStaleStore(t, s, kr)
			kr.failSet = !storeWritable
			kr.failDelete = !storeWritable

			n, inHook := 0, false
			step := func(string) error {
				if inHook {
					return nil
				}
				inHook = true
				defer func() { inHook = false }()
				n++
				if n == target {
					panic(crashed{})
				}
				return nil
			}
			kr.onCall = func(op string) { _ = step(op) }
			s.hook = step

			didCrash := func() (c bool) {
				defer func() {
					if r := recover(); r != nil {
						if _, ok := r.(crashed); !ok {
							panic(r)
						}
						c = true
					}
				}()
				_, _ = s.Save(ctx(), Test, grantWith("at_B", "rt_B"))
				return false
			}()

			s.hook, kr.onCall = nil, nil
			got, err := s.Load(Test)
			if rt := refreshOf(got); err != nil || (rt != "rt_A" && rt != "rt_B") {
				t.Fatalf("storeWritable=%v crash at step %d: Load = %+v, %v; want rt_A or rt_B", storeWritable, target, got, err)
			}
			finished = !didCrash
		}
		if !finished {
			t.Fatalf("storeWritable=%v: the save never completed within the sweep", storeWritable)
		}
	}
}

// With no secret service at all, a lock-free reader running at every step of a
// save never sees a mode that had a record come back empty.
func TestAHeadlessSaveNeverShowsALockFreeReaderNoSession(t *testing.T) {
	kr := newFakeKeyring()
	kr.fail = true
	s, _ := Open(Options{Dir: t.TempDir(), Keyring: kr})
	if _, err := s.Save(ctx(), Test, grantWith("at_A", "rt_A")); err != nil {
		t.Fatal(err)
	}

	reads, inHook := 0, false
	check := func(step string) {
		if inHook {
			return
		}
		inHook = true
		defer func() { inHook = false }()
		reads++
		got, err := s.Load(Test)
		if rt := refreshOf(got); err != nil || (rt != "rt_A" && rt != "rt_B") {
			t.Errorf("at %q a reader saw %+v, %v", step, got, err)
		}
	}
	kr.onCall = check
	s.hook = func(step string) error { check(step); return nil }

	if where, err := s.Save(ctx(), Test, grantWith("at_B", "rt_B")); err != nil || where != BackendFile {
		t.Fatalf("Save = %q, %v", where, err)
	}
	if reads < 4 {
		t.Fatalf("the reader only ran %d times; the hook is not wired", reads)
	}
	s.hook, kr.onCall = nil, nil
	if got, _ := s.Load(Test); refreshOf(got) != "rt_B" {
		t.Fatalf("Load = %+v", got)
	}
}
