package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// ErrLockTimeout means another process held the config directory lock for too
// long (a minute, or the caller's own deadline).
var ErrLockTimeout = errors.New("config: timed out waiting for another listo process")

const (
	lockFileName = ".lock"
	lockPoll     = 25 * time.Millisecond
	lockMaxWait  = time.Minute
)

// lockDir takes the exclusive advisory lock on <dir>/.lock (flock on Unix,
// LockFileEx on Windows, see lock_unix.go and lock_windows.go) and returns the
// function that releases it. The lock is released by the OS when the process
// dies, so a crash never leaves it held.
func lockDir(ctx context.Context, dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, lockFileName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, lockMaxWait)
		defer cancel()
	}
	for {
		ok, err := tryLock(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		if ok {
			return func() {
				unlock(f)
				f.Close()
			}, nil
		}
		select {
		case <-ctx.Done():
			f.Close()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, ErrLockTimeout
			}
			return nil, ctx.Err()
		case <-time.After(lockPoll):
		}
	}
}
