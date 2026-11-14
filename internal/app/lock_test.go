package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitmirror/internal/state"
)

func TestBuildLocksStateDir(t *testing.T) {
	f := newFixture(t)
	a := f.build()
	if _, err := os.Stat(filepath.Join(f.stateDir, "gitmirror.lock")); err != nil {
		t.Fatalf("lock file: %v", err)
	}

	b, err := Build(context.Background(), f.config(), f.options())
	if err == nil {
		b.Close()
		t.Fatal("second Build on a locked state dir succeeded")
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("err = %v, want ErrLocked", err)
	}
	if want := "another gitmirror process is using state dir " + f.config().StateDir; err.Error() != want {
		t.Fatalf("err = %q, want %q", err, want)
	}

	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	c, err := Build(context.Background(), f.config(), f.options())
	if err != nil {
		t.Fatalf("Build after Close: %v", err)
	}
	c.Close()
}

func TestBuildLockTimeoutWaitsForRelease(t *testing.T) {
	f := newFixture(t)
	a := f.build()

	type result struct {
		a   *App
		err error
	}
	got := make(chan result, 1)
	start := time.Now()
	go func() {
		o := f.options()
		o.LockTimeout = 20 * time.Second
		b, err := Build(context.Background(), f.config(), o)
		got <- result{b, err}
	}()
	time.Sleep(5 * time.Millisecond)
	select {
	case r := <-got:
		t.Fatalf("Build returned while locked: %v", r.err)
	default:
	}
	a.Close()
	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("Build with LockTimeout: %v", r.err)
		}
		r.a.Close()
	case <-time.After(15 * time.Second):
		t.Fatal("Build with LockTimeout did not return after the holder closed")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestBuildLockTimeoutExpires(t *testing.T) {
	f := newFixture(t)
	f.build()
	o := f.options()
	o.LockTimeout = 300 * time.Millisecond
	start := time.Now()
	_, err := Build(context.Background(), f.config(), o)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("err = %v, want ErrLocked", err)
	}
	if d := time.Since(start); d < 250*time.Millisecond {
		t.Fatalf("gave up after %v, before LockTimeout", d)
	}
}

func TestBuildLockTimeoutHonoursContext(t *testing.T) {
	f := newFixture(t)
	f.build()
	o := f.options()
	o.LockTimeout = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := Build(ctx, f.config(), o)
	if !errors.Is(err, ErrLocked) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want ErrLocked and DeadlineExceeded", err)
	}
}

func TestReadOnlyBuildIgnoresLockAndKeepsRunningRuns(t *testing.T) {
	f := newFixture(t)
	holder := f.build()
	ctx := context.Background()
	k := srcKey("r1", "alice", "a", "mirrors")
	id, err := holder.Store.StartRun(ctx, k, "schedule")
	if err != nil {
		t.Fatal(err)
	}

	o := f.options()
	o.ReadOnly = true
	ro, err := Build(ctx, f.config(), o)
	if err != nil {
		t.Fatalf("ReadOnly Build while locked: %v", err)
	}
	runs, err := ro.Store.Runs(ctx, k, 0)
	if err != nil || len(runs) != 1 || runs[0].ID != id || runs[0].Status != state.RunRunning {
		t.Fatalf("runs = %+v, err %v; the holder's run must stay running", runs, err)
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}

	// Closing the read-only App must not release the holder's lock.
	_, err = Build(ctx, f.config(), f.options())
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("err = %v, want ErrLocked after ReadOnly Close", err)
	}
	runs, _ = holder.Store.Runs(ctx, k, 0)
	if len(runs) != 1 || runs[0].Status != state.RunRunning {
		t.Fatalf("runs = %+v after failed Build; must not abandon", runs)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	f := newFixture(t)
	a := f.build()
	a.Close()
	if err := a.Close(); err != nil && !strings.Contains(err.Error(), "closed") {
		t.Fatalf("second Close: %v", err)
	}
}
