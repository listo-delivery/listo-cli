// Package config is the per-computer state of the CLI: the credential of each
// mode and that mode's webhook signing secret.
//
// Layout. One Record per mode ("test", "live"). A Record holds at most one
// Credential, either a GRANT (access_token, access_token_expires_at,
// refresh_token, device_name, mode: what `listo login` stores) or a KEY
// (api_key, mode: `listo login --key`), plus the mode's webhook session secret
// ("whsec_" + 32 random bytes in hex), minted ONCE per computer per mode and kept
// beside the credential so `listen` prints the same secret on every start.
//
// Where it lives. The OS credential store first (macOS Keychain, Windows
// Credential Manager, the Linux Secret Service, through go-keyring), one item per
// mode in the service "listo-cli". When no store answers (a headless Linux box,
// a container) the records go to a 0600 credentials.json in the config directory
// and Save says which one it used, so the command can tell the person. ONE
// source is authoritative per mode: an entry in credentials.json (marked
// "holder": "file") outranks the OS store, so a write that had to fall back to the
// file is the one Load returns even when an older store copy could not be deleted;
// a write goes to the store first and only then removes the file entry (a "holder":
// "keyring" marker found in the file hands the mode back to the store). Every
// change to credentials.json is one atomic replace. The
// directory is $LISTO_CONFIG_DIR, else the OS user config dir plus "listo"
// ($XDG_CONFIG_HOME/listo on Linux). LISTO_NO_KEYRING=1 forces the file.
//
// Two terminals. Refresh tokens rotate and a token presented twice revokes the
// whole grant, so anything that reads a credential, calls the network and writes
// the answer back must do it inside WithLock and RE-READ the record after the
// lock is taken (the first terminal may have refreshed while this one waited):
//
//	err := store.WithLock(ctx, func(tx *config.Tx) error {
//		rec, err := tx.Load(config.Test) // read AFTER the lock
//		if err != nil || stillFresh(rec) {
//			return err
//		}
//		... exchange rec.Credential.RefreshToken ...
//		_, err = tx.Save(config.Test, rec)
//		return err
//	})
//
// Store.Load is a lock-free snapshot for readers that change nothing.
package config

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/zalando/go-keyring"
)

// Mode is the world a credential acts in.
type Mode string

const (
	Test Mode = "test"
	Live Mode = "live"
)

// Modes lists both modes, test first.
var Modes = []Mode{Test, Live}

// ModeFor is Live when live is true (the --live flag), else Test.
func ModeFor(live bool) Mode {
	if live {
		return Live
	}
	return Test
}

// Credential is what the CLI presents to the API.
type Credential struct {
	Mode Mode `json:"mode"`

	// A grant (listo login).
	AccessToken          string    `json:"access_token,omitempty"`
	AccessTokenExpiresAt time.Time `json:"access_token_expires_at,omitzero"`
	RefreshToken         string    `json:"refresh_token,omitempty"`
	DeviceName           string    `json:"device_name,omitempty"`

	// A key (listo login --key). Never both.
	APIKey string `json:"api_key,omitempty"`
}

// IsKey reports a stored API key; IsGrant a stored grant.
func (c *Credential) IsKey() bool { return c != nil && c.APIKey != "" }
func (c *Credential) IsGrant() bool {
	return c != nil && c.APIKey == "" && (c.RefreshToken != "" || c.AccessToken != "")
}

// Record is everything stored for one mode.
type Record struct {
	Credential    *Credential `json:"credential,omitempty"`
	WebhookSecret string      `json:"webhook_secret,omitempty"`

	// Backend says where Load found the record ("keyring" or "file"); never stored.
	Backend string `json:"-"`

	// Holder is the marker in credentials.json that says which backend is the
	// authority for the mode ("file", or "keyring" for an entry that only hands the
	// mode back to the OS store). Never stored in the OS store, never returned by Load.
	Holder string `json:"holder,omitempty"`
}

// Values of Record.Holder.
const (
	holderFile    = "file"
	holderKeyring = "keyring"
)

// Backends a record can live in.
const (
	BackendKeyring = "keyring"
	BackendFile    = "file"
)

// Environment variables.
const (
	EnvConfigDir = "LISTO_CONFIG_DIR"
	EnvNoKeyring = "LISTO_NO_KEYRING"
)

const (
	keyringService  = "listo-cli"
	credentialsFile = "credentials.json"
)

// Keyring is the slice of an OS credential store the CLI needs. System wraps
// go-keyring; tests pass a fake.
type Keyring interface {
	Get(service, user string) (string, error)
	Set(service, user, secret string) error
	Delete(service, user string) error
}

// ErrKeyringNotFound is what a Keyring returns for an item that is absent (as
// opposed to a store that does not work).
var ErrKeyringNotFound = errors.New("config: item not found in the credential store")

// SystemKeyring is the operating system's credential store.
type SystemKeyring struct{}

func (SystemKeyring) Get(service, user string) (string, error) {
	v, err := keyring.Get(service, user)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrKeyringNotFound
	}
	return v, err
}

func (SystemKeyring) Set(service, user, secret string) error {
	return keyring.Set(service, user, secret)
}

func (SystemKeyring) Delete(service, user string) error {
	err := keyring.Delete(service, user)
	if errors.Is(err, keyring.ErrNotFound) {
		return ErrKeyringNotFound
	}
	return err
}

// Options configure Open.
type Options struct {
	Dir     string  // the config directory; required (see DefaultDir)
	Keyring Keyring // nil means no OS store: the file is used
}

// Store reads and writes the Records. Build one with Open or OpenDefault.
type Store struct {
	dir string
	kr  Keyring

	// hook, set only by tests, is called around each visible change to
	// credentials.json ("file:replace" before it, "file:replaced" after); an error
	// from the first makes the change fail.
	hook func(step string) error
}

func (s *Store) at(step string) error {
	if s.hook == nil {
		return nil
	}
	return s.hook(step)
}

// Open creates the config directory (0700) if needed and returns a Store.
func Open(o Options) (*Store, error) {
	if o.Dir == "" {
		return nil, errors.New("config: no directory")
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: o.Dir, kr: o.Keyring}, nil
}

// DefaultDir is $LISTO_CONFIG_DIR, else the OS user config dir plus "listo".
func DefaultDir(getenv func(string) string) (string, error) {
	if d := getenv(EnvConfigDir); d != "" {
		return d, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "listo"), nil
}

// OpenDefault opens the Store of the running process: DefaultDir, with the OS
// credential store unless LISTO_NO_KEYRING is set.
func OpenDefault(getenv func(string) string) (*Store, error) {
	dir, err := DefaultDir(getenv)
	if err != nil {
		return nil, err
	}
	o := Options{Dir: dir}
	if getenv(EnvNoKeyring) == "" {
		o.Keyring = SystemKeyring{}
	}
	return Open(o)
}

// Dir is the config directory.
func (s *Store) Dir() string { return s.dir }

// CorruptError means credentials.json could not be read as JSON.
type CorruptError struct {
	Path string
	Err  error
}

func (e *CorruptError) Error() string {
	return fmt.Sprintf("config: %s is not valid: %v", e.Path, e.Err)
}
func (e *CorruptError) Unwrap() error { return e.Err }

// Load returns the Record of a mode (a zero Record when nothing is stored) as a
// snapshot, taking no lock. Use it to read; to change, use WithLock or the
// Update/Save/Delete helpers.
func (s *Store) Load(mode Mode) (Record, error) { return s.load(mode) }

// Tx is the store inside WithLock. Its methods take no lock of their own (the
// caller holds it), so they cannot deadlock against it.
type Tx struct{ s *Store }

func (t *Tx) Load(mode Mode) (Record, error)           { return t.s.load(mode) }
func (t *Tx) Save(mode Mode, r Record) (string, error) { return t.s.save(mode, r) }
func (t *Tx) Delete(mode Mode) error                   { return t.s.remove(mode) }

// WithLock runs fn holding the exclusive lock on the config directory. Another
// process (or goroutine) calling WithLock waits until fn returns. It gives up
// with ErrLockTimeout after a minute when ctx has no earlier deadline.
func (s *Store) WithLock(ctx context.Context, fn func(tx *Tx) error) error {
	unlock, err := lockDir(ctx, s.dir)
	if err != nil {
		return err
	}
	defer unlock()
	return fn(&Tx{s: s})
}

// Save stores a Record under the lock and returns where it went ("keyring" or
// "file"), for the line that tells the person.
func (s *Store) Save(ctx context.Context, mode Mode, r Record) (backend string, err error) {
	err = s.WithLock(ctx, func(tx *Tx) error {
		backend, err = tx.Save(mode, r)
		return err
	})
	return backend, err
}

// Delete removes a mode's Record from every place it may live (`listo logout`
// clears the credential and the webhook secret together).
func (s *Store) Delete(ctx context.Context, mode Mode) error {
	return s.WithLock(ctx, func(tx *Tx) error { return tx.Delete(mode) })
}

// Update is the read-modify-write helper: under the lock it loads the mode's
// Record, lets fn change it and saves it. It returns where the record went.
func (s *Store) Update(ctx context.Context, mode Mode, fn func(r *Record) error) (backend string, err error) {
	err = s.WithLock(ctx, func(tx *Tx) error {
		r, err := tx.Load(mode)
		if err != nil {
			return err
		}
		if err := fn(&r); err != nil {
			return err
		}
		backend, err = tx.Save(mode, r)
		return err
	})
	return backend, err
}

// MintWebhookSecret is "whsec_" plus 32 random bytes in hex.
func MintWebhookSecret() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "whsec_" + hex.EncodeToString(b[:])
}

// WebhookSecret returns the mode's session secret, minting and storing it the
// first time (under the lock, so two terminals starting `listen` together agree).
// It does not need a credential. `listo listen --print-secret` is this call.
func (s *Store) WebhookSecret(ctx context.Context, mode Mode) (string, error) {
	var secret string
	_, err := s.Update(ctx, mode, func(r *Record) error {
		if r.WebhookSecret == "" {
			r.WebhookSecret = MintWebhookSecret()
		}
		secret = r.WebhookSecret
		return nil
	})
	return secret, err
}

// --- storage ---

func (s *Store) filePath() string { return filepath.Join(s.dir, credentialsFile) }

func (s *Store) readFile() (map[Mode]Record, error) {
	raw, err := os.ReadFile(s.filePath())
	if errors.Is(err, os.ErrNotExist) {
		return map[Mode]Record{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[Mode]Record{}
	if len(raw) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, &CorruptError{Path: s.filePath(), Err: err}
	}
	return m, nil
}

// writeFile replaces credentials.json atomically with mode 0600; an empty map
// removes the file.
func (s *Store) writeFile(m map[Mode]Record) error {
	if len(m) == 0 {
		if err := s.at("file:replace"); err != nil {
			return err
		}
		err := os.Remove(s.filePath())
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		_ = s.at("file:replaced")
		return nil
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".credentials-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func() { _ = os.Remove(name) }
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := s.at("file:replace"); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(name, s.filePath()); err != nil {
		cleanup()
		return err
	}
	_ = s.at("file:replaced")
	return nil
}

// load follows the marker. One source is authoritative per mode at any moment:
// credentials.json holds an entry for the mode when (and only when) the FILE is
// the authority (Holder "file", or no Holder at all, which is a file written
// before the marker existed), and an entry marked "keyring" (or none) hands the
// mode to the OS store. A write that fell back to the file therefore wins over an
// older record the store could not be made to forget.
func (s *Store) load(mode Mode) (Record, error) {
	m, ferr := s.readFile()
	if ferr == nil {
		if e, ok := m[mode]; ok && e.Holder != holderKeyring {
			return fileRecord(e), nil
		}
	}
	if s.kr != nil {
		raw, err := s.kr.Get(keyringService, string(mode))
		if err == nil {
			var r Record
			if jerr := json.Unmarshal([]byte(raw), &r); jerr == nil {
				r.Backend = BackendKeyring
				r.Holder = ""
				return r, nil
			}
		}
		// Not found, unusable or a store that does not work: nothing to read there.
	}
	// Reaching here means the file did not name itself the authority and the store
	// had nothing; an unreadable file is the one thing worth reporting.
	return Record{}, ferr
}

// fileRecord is what a file entry means to a caller: the record, or the empty
// Record for an entry that only marks the file as the authority.
func fileRecord(e Record) Record {
	e.Holder = ""
	if e.Credential == nil && e.WebhookSecret == "" {
		return Record{}
	}
	e.Backend = BackendFile
	return e
}

func (s *Store) save(mode Mode, r Record) (string, error) {
	r.Backend = ""
	r.Holder = ""
	if r.Credential != nil {
		r.Credential.Mode = mode
	}
	if s.kr != nil {
		raw, err := json.Marshal(r)
		if err != nil {
			return "", err
		}
		// Write to the store FIRST: the credential leaves the file only once the new
		// record is safely in the store.
		if err := s.kr.Set(keyringService, string(mode), string(raw)); err == nil {
			// An older file entry would outrank the store, so remove it (one atomic
			// replace). If that replace fails the old entry stays the authority, so
			// Load still reads the old record and the caller is told. A file that
			// cannot be read cannot outrank anything.
			if m, ferr := s.readFile(); ferr == nil {
				if _, ok := m[mode]; ok {
					delete(m, mode)
					if err := s.writeFile(m); err != nil {
						return "", err
					}
				}
			}
			return BackendKeyring, nil
		}
		// No store answered: fall through to the file, which becomes the authority.
	}
	m, err := s.readFile()
	if err != nil {
		return "", err
	}
	r.Holder = holderFile
	m[mode] = r
	if err := s.writeFile(m); err != nil {
		return "", err
	}
	if s.kr != nil {
		// The file is the authority now, so an older store copy is dead whether or
		// not it can be deleted; removing it is only tidiness.
		_ = s.kr.Delete(keyringService, string(mode))
	}
	return BackendFile, nil
}

func (s *Store) remove(mode Mode) error {
	// Any outcome of the store's delete other than success or a definite "not
	// found" (a refusal, a store that is down) means it may still hold the record,
	// and that copy must not come back to life once the file entry is gone: the
	// file keeps an empty entry marked "file", the authority holding nothing.
	storeMayHold := false
	if s.kr != nil {
		if err := s.kr.Delete(keyringService, string(mode)); err != nil && !errors.Is(err, ErrKeyringNotFound) {
			storeMayHold = true
		}
	}
	m, err := s.readFile()
	if err != nil {
		return err
	}
	if storeMayHold {
		m[mode] = Record{Holder: holderFile}
		return s.writeFile(m)
	}
	if _, ok := m[mode]; ok {
		delete(m, mode)
		return s.writeFile(m)
	}
	return nil
}
