package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gitmirror/internal/forge"
	"gitmirror/internal/state"
)

// addTaggedSource seeds owner/name with main plus n tags and returns the
// work tree (remote origin = the source).
func (f *fixture) addTaggedSource(owner, name string, n int) (work string, tags []string) {
	f.t.Helper()
	if err := f.src.AddRepo(forge.Repo{Owner: owner, Name: name, DefaultBranch: "main"}); err != nil {
		f.t.Fatal(err)
	}
	work = filepath.Join(f.t.TempDir(), "work")
	f.git(f.t.TempDir(), "init", "-q", "-b", "main", work)
	f.git(work, "remote", "add", "origin", f.src.GitURL(owner, name))
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte(name+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.git(work, "add", "a.txt")
	f.git(work, "commit", "-q", "-m", "init")
	for i := range n {
		tag := fmt.Sprintf("t%02d", i)
		f.git(work, "tag", tag)
		tags = append(tags, tag)
	}
	f.git(work, append([]string{"push", "-q", "origin", "main"}, tags...)...)
	return work, tags
}

func TestSyncWithAllowPruneBypassesDeletionGuard(t *testing.T) {
	f := newFixture(t)
	work, tags := f.addTaggedSource("alice", "a", 10)
	a := f.build()
	ctx := context.Background()
	req := SyncRequest{Repos: []string{"alice/a"}, Trigger: "cli"}
	res, err := a.SyncWith(ctx, req)
	if err != nil || len(res.Reports) != 1 || res.Reports[0].Status != state.RunOK {
		t.Fatalf("initial sync: %+v %v", res, err)
	}

	f.git(work, append([]string{"push", "-q", "origin", "--delete"}, tags[:8]...)...)
	res, err = a.SyncWith(ctx, req)
	if err != nil || res.Reports[0].Status != state.RunAborted {
		t.Fatalf("guarded sync: status %v err %v (report err %v)", res.Reports[0].Status, err, res.Reports[0].Err)
	}

	req.AllowPrune = true
	res, err = a.SyncWith(ctx, req)
	if err != nil || res.Reports[0].Status != state.RunOK {
		t.Fatalf("--allow-prune sync: %+v %v", res.Reports[0], err)
	}
	var detail string
	for _, s := range res.Reports[0].Steps {
		if s.Step == "git" {
			detail = s.Detail
		}
	}
	if !strings.Contains(detail, "deletion guard bypassed") {
		t.Fatalf("git step detail = %q", detail)
	}
}

func TestStateDirAndLockFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	f := newFixture(t)
	f.build()
	for _, c := range []struct {
		path string
		mask os.FileMode
	}{
		{f.stateDir, 0o077},
		{filepath.Join(f.stateDir, "gitmirror.lock"), 0o177},
	} {
		fi, err := os.Stat(c.path)
		if err != nil {
			t.Fatal(err)
		}
		if p := fi.Mode().Perm(); p&c.mask != 0 {
			t.Errorf("%s mode = %v, too permissive", c.path, p)
		}
	}
}
