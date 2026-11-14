package gitx

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const lockPollInterval = 100 * time.Millisecond

// procLocks serializes Syncs of the same cache within this process; each
// key maps to a 1-slot semaphore so waiting can honour a context.
var procLocks = struct {
	mu sync.Mutex
	m  map[string]chan struct{}
}{m: map[string]chan struct{}{}}

func lockKey(dir string) string {
	k := filepath.Clean(dir)
	if runtime.GOOS == "windows" {
		k = strings.ToLower(k)
	}
	return k
}

// lock takes the in-process lock for dir and then the cross-process lock
// file dir+".lock" (O_CREATE|O_EXCL). It waits for both until ctx is done.
// A lock file older than 2*timeout is treated as stale, removed and logged.
// While held, the lock file's mtime is refreshed so long syncs don't look
// stale to other processes.
func (c *Cache) lock(ctx context.Context, dir string, timeout time.Duration) (func(), error) {
	key := lockKey(dir)
	procLocks.mu.Lock()
	sem, ok := procLocks.m[key]
	if !ok {
		sem = make(chan struct{}, 1)
		procLocks.m[key] = sem
	}
	procLocks.mu.Unlock()

	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("gitx: waiting for cache lock: %w", ctx.Err())
	}
	unlockProc := func() { <-sem }

	path := dir + ".lock"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		unlockProc()
		return nil, fmt.Errorf("gitx: lock: %w", err)
	}
	stale := 2 * timeout
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.WriteString(strconv.Itoa(os.Getpid()) + " " + time.Now().UTC().Format(time.RFC3339) + "\n")
			_ = f.Close()
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			unlockProc()
			return nil, fmt.Errorf("gitx: lock: %w", err)
		}
		if fi, serr := os.Stat(path); serr == nil && time.Since(fi.ModTime()) > stale {
			c.logger().Warn("gitx: removing stale cache lock", "lock", path, "age", time.Since(fi.ModTime()).Round(time.Second).String())
			if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
				unlockProc()
				return nil, fmt.Errorf("gitx: removing stale lock: %w", rerr)
			}
			continue
		}
		select {
		case <-ctx.Done():
			unlockProc()
			return nil, fmt.Errorf("gitx: waiting for cache lock %s: %w", path, ctx.Err())
		case <-time.After(lockPollInterval):
		}
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		interval := timeout / 4
		if interval > time.Minute {
			interval = time.Minute
		}
		if interval < time.Second {
			interval = time.Second
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				now := time.Now()
				_ = os.Chtimes(path, now, now)
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			<-done
			_ = os.Remove(path)
			unlockProc()
		})
	}, nil
}
