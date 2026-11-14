package state

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestLFSDigestRoundTrip(t *testing.T) {
	s := openTemp(t)
	k := key("a")
	if err := s.SetLFSDigest(ctx, k, "d1"); err == nil {
		t.Fatal("SetLFSDigest on an unknown pair succeeded")
	}
	id, err := s.StartRun(ctx, k, "cli")
	if err != nil {
		t.Fatal(err)
	}
	ps, _, _ := s.Pair(ctx, k)
	if ps.LFSDigest != "" {
		t.Fatalf("new pair LFSDigest = %q", ps.LFSDigest)
	}
	if err := s.SetLFSDigest(ctx, k, "d1"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(ctx, id, RunOK, nil, "", "x", 3); err != nil {
		t.Fatal(err)
	}
	ps, _, _ = s.Pair(ctx, k)
	if ps.LFSDigest != "d1" || ps.LastDigest != "x" {
		t.Fatalf("pair = %+v, want LFSDigest d1 kept by FinishRun", ps)
	}
	all, err := s.Pairs(ctx)
	if err != nil || len(all) != 1 || all[0].LFSDigest != "d1" {
		t.Fatalf("Pairs = %+v, %v", all, err)
	}
}

// TestConcurrentOpenOfFreshDatabase races several Opens (as a read-only plan
// next to serve would) on a brand new database: every one must succeed and
// each migration must be applied exactly once.
func TestConcurrentOpenOfFreshDatabase(t *testing.T) {
	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for round := range 8 {
		p := filepath.Join(t.TempDir(), fmt.Sprintf("race%d.db", round))
		const n = 4
		var wg sync.WaitGroup
		stores := make([]*Store, n)
		errs := make([]error, n)
		start := make(chan struct{})
		for i := range n {
			wg.Go(func() {
				<-start
				stores[i], errs[i] = Open(ctx, p)
			})
		}
		close(start)
		wg.Wait()
		t.Cleanup(func() {
			for _, s := range stores {
				if s != nil {
					s.Close()
				}
			}
		})
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d: Open #%d: %v", round, i, err)
			}
		}
		var rows int
		if err := stores[0].db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != len(ms) {
			t.Fatalf("round %d: %d schema_migrations rows, want %d", round, rows, len(ms))
		}
	}
}

func TestStateDirPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	dir := filepath.Join(t.TempDir(), "new", "state")
	s, err := Open(ctx, filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p := fi.Mode().Perm(); p&0o077 != 0 {
		t.Fatalf("state dir mode = %v, want no group/other access", p)
	}
}
