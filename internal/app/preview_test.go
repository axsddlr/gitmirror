package app

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitmirror/internal/state"
)

// treeOf lists every file and directory under dir, files with their size
// and modification time.
func treeOf(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// NTFS updates directory times lazily: compare names only.
			out[p] = "dir"
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		out[p] = fi.ModTime().Format(time.RFC3339Nano) + " " + fi.Mode().String() + " " + strconv.FormatInt(fi.Size(), 10)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPreviewDoesNotFetch(t *testing.T) {
	f := newUIFixture(t)
	f.addUISource("carol", "c1")
	a := f.build(f.options())
	ctx := context.Background()
	if err := a.UpdateUI(ctx, addUISourceForgeAndRule); err != nil {
		t.Fatal(err)
	}
	if res, err := a.Sync(ctx, []string{"ui1"}, nil, false, "cli"); err != nil || len(res.Reports) != 1 || res.Reports[0].Status != state.RunOK {
		t.Fatalf("first sync: %v %+v", err, res)
	}
	cache := a.Engine.CacheDir
	before := treeOf(t, cache)
	if len(before) < 2 {
		t.Fatalf("cache is empty after a sync: %v", before)
	}
	time.Sleep(20 * time.Millisecond) // let a rewrite show in the modification times
	same := func(*UIConfig) error { return nil }
	res, err := a.Preview(ctx, same, "ui1")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Reports) != 1 || res.Reports[0].Status != state.RunOK || strings.Contains(strings.Join(res.Reports[0].Actions, "\n"), "create repository") {
		t.Fatalf("preview of an existing destination: %+v", res.Reports)
	}
	after := treeOf(t, cache)
	for p, v := range after {
		if before[p] != v {
			t.Errorf("Preview changed the cache: %s: %q -> %q", p, before[p], v)
		}
	}
	if len(after) != len(before) {
		t.Errorf("Preview changed the cache: %d entries, was %d", len(after), len(before))
	}
	if t.Failed() {
		t.FailNow()
	}

	// Another sync holds the pair's cache lock: Preview does not wait for it.
	var lock string
	for p := range before {
		if strings.HasSuffix(p, ".git") && filepath.Dir(p) == cache {
			lock = p + ".lock"
		}
	}
	if lock == "" {
		t.Fatalf("no cache repository in %v", before)
	}
	if err := os.WriteFile(lock, []byte("1 test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(lock) })
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	start := time.Now()
	res, err = a.Preview(pctx, same, "ui1")
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 3*time.Second || len(res.Reports) != 1 || res.Reports[0].Status != state.RunOK {
		t.Fatalf("preview while the cache is locked took %v: %+v", took, res.Reports)
	}
}
