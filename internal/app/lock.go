package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrLocked is wrapped by Build errors caused by another process holding the
// state dir lock.
var ErrLocked = errors.New("state dir is locked")

// lockFileName is the lock file inside the state dir. The lock is an OS file
// lock (LockFileEx / flock), released by the OS when the process dies, so the
// file itself is never removed and a leftover file is not a stale lock.
const lockFileName = "gitmirror.lock"

// lockRetry is the poll interval while waiting for Options.LockTimeout.
const lockRetry = 250 * time.Millisecond

// errWouldBlock is returned by tryLock when another handle holds the lock.
var errWouldBlock = errors.New("lock is held")

type lockedError struct{ dir string }

func (e *lockedError) Error() string {
	return "another gitmirror process is using state dir " + e.dir
}

func (e *lockedError) Unwrap() error { return ErrLocked }

// dirLock is an exclusive OS-level lock on <state dir>/gitmirror.lock.
type dirLock struct{ f *os.File }

// acquireLock takes the state dir lock. With timeout 0 it fails immediately
// when the lock is held; otherwise it retries every lockRetry until timeout
// elapses or ctx is done.
func acquireLock(ctx context.Context, dir string, timeout time.Duration) (*dirLock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, lockFileName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	deadline := time.Now().Add(timeout)
	for {
		err := tryLock(f)
		if err == nil {
			return &dirLock{f: f}, nil
		}
		if !errors.Is(err, errWouldBlock) {
			f.Close()
			return nil, fmt.Errorf("lock state dir %s: %w", dir, err)
		}
		left := time.Until(deadline)
		if timeout <= 0 || left <= 0 {
			f.Close()
			return nil, &lockedError{dir: dir}
		}
		t := time.NewTimer(min(lockRetry, left))
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			f.Close()
			return nil, fmt.Errorf("%w: %w", &lockedError{dir: dir}, ctx.Err())
		}
	}
}

// release unlocks and closes the lock file. Safe on a nil lock.
func (l *dirLock) release() error {
	if l == nil {
		return nil
	}
	uerr := unlock(l.f)
	return errors.Join(uerr, l.f.Close())
}
