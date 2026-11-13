package gitx

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestLockSerializesConcurrentSyncs: two Cache values pointing at the same
// Dir must not run concurrently; the second waits (honouring its context).
func TestLockSerializesConcurrentSyncs(t *testing.T) {
	f := newFixture(t)
	f.commit("a.txt")
	f.push("main")

	release := make(chan struct{})
	firstIn := make(chan struct{})
	secondIn := make(chan struct{})
	c1 := &Cache{Dir: f.cache, R: f.r, afterLock: func() { close(firstIn); <-release }}
	c2 := &Cache{Dir: f.cache + string(os.PathSeparator) + ".", R: f.r, afterLock: func() { close(secondIn) }}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errs[0] = c1.Sync(context.Background(), f.srcRemote, f.dstRemote, SyncOptions{})
	}()
	<-firstIn
	go func() {
		defer wg.Done()
		_, errs[1] = c2.Sync(context.Background(), f.srcRemote, f.dstRemote, SyncOptions{})
	}()

	select {
	case <-secondIn:
		t.Fatal("second Sync entered while first held the lock")
	case <-time.After(400 * time.Millisecond):
	}
	close(release)
	select {
	case <-secondIn:
	case <-time.After(60 * time.Second):
		t.Fatal("second Sync never acquired the lock")
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
	}
	if _, err := os.Stat(f.cache + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("lock file left behind: %v", err)
	}
}

func TestLockWaitHonoursContext(t *testing.T) {
	f := newFixture(t)
	f.commit("a.txt")
	f.push("main")
	if err := os.MkdirAll(f.root+"/cache", 0o755); err != nil {
		t.Fatal(err)
	}
	// A fresh lock held by "another process".
	if err := os.WriteFile(f.cache+".lock", []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	f.resetCmds()
	_, err := f.c.Sync(ctx, f.srcRemote, f.dstRemote, SyncOptions{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if len(f.captured()) != 0 {
		t.Fatal("git ran without the lock")
	}
	if b, _ := os.ReadFile(f.cache + ".lock"); string(b) != "other" {
		t.Fatal("foreign lock file was touched")
	}
}

func TestStaleLockRemovedWithWarning(t *testing.T) {
	f := newFixture(t)
	f.commit("a.txt")
	f.push("main")
	if err := os.MkdirAll(f.root+"/cache", 0o755); err != nil {
		t.Fatal(err)
	}
	lock := f.cache + ".lock"
	if err := os.WriteFile(lock, []byte("dead"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Hour) // default Timeout 30m -> stale after 60m
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	f.c.Logger = slog.New(slog.NewTextHandler(&buf, nil))
	f.sync(SyncOptions{})
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "stale") {
		t.Fatalf("no stale-lock warning logged: %q", buf.String())
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatal("lock not released")
	}
}
