package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitmirror/internal/forge"
)

// addTaggedSource seeds owner/name with main plus n tags; it returns the work
// tree whose origin is the source.
func (e *testEnv) addTaggedSource(owner, name string, n int) (string, []string) {
	e.t.Helper()
	if err := e.src.AddRepo(forge.Repo{Owner: owner, Name: name, DefaultBranch: "main"}); err != nil {
		e.t.Fatal(err)
	}
	work := filepath.Join(e.t.TempDir(), "work")
	e.git(e.t.TempDir(), "init", "-q", "-b", "main", work)
	e.git(work, "remote", "add", "origin", e.src.GitURL(owner, name))
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte(name+"\n"), 0o644); err != nil {
		e.t.Fatal(err)
	}
	e.git(work, "add", "a.txt")
	e.git(work, "commit", "-q", "-m", "init")
	var tags []string
	for i := range n {
		tag := fmt.Sprintf("t%02d", i)
		e.git(work, "tag", tag)
		tags = append(tags, tag)
	}
	e.git(work, append([]string{"push", "-q", "origin", "main"}, tags...)...)
	return work, tags
}

func (e *testEnv) dstRefCount(owner, name string) int {
	e.t.Helper()
	cmd := exec.Command("git", "ls-remote", "--refs", e.dst.GitURL(owner, name))
	cmd.Env = append(os.Environ(), e.gitEnv...)
	out, err := cmd.Output()
	if err != nil {
		e.t.Fatalf("ls-remote: %v", err)
	}
	return len(strings.Fields(string(out))) / 2
}

func TestSyncAllowPruneBypassesMassDeletionGuard(t *testing.T) {
	e := newTestEnv(t)
	work, tags := e.addTaggedSource("alice", "a", 10)
	code, out, errOut := e.cli("sync")
	wantCode(t, code, 0, out, errOut)
	if n := e.dstRefCount("mirrors", "a"); n != 11 {
		t.Fatalf("dst refs = %d, want 11", n)
	}

	e.git(work, append([]string{"push", "-q", "origin", "--delete"}, tags[:8]...)...)
	code, out, errOut = e.cli("sync")
	wantCode(t, code, 1, out, errOut)
	if !strings.Contains(out, "aborted") || !strings.Contains(out, "would delete 8 of 11 destination refs") || !strings.Contains(out, "--allow-prune") {
		t.Fatalf("guarded sync output:\n%s", out)
	}
	if n := e.dstRefCount("mirrors", "a"); n != 11 {
		t.Fatalf("dst refs after guarded sync = %d, want 11", n)
	}

	code, out, errOut = e.cli("--json", "sync", "--allow-prune")
	wantCode(t, code, 0, out, errOut)
	if !strings.Contains(out, "deletion guard bypassed (--allow-prune)") {
		t.Fatalf("--allow-prune output lacks the bypass note:\n%s", out)
	}
	if n := e.dstRefCount("mirrors", "a"); n != 3 {
		t.Fatalf("dst refs after --allow-prune = %d, want 3", n)
	}
}

func TestSyncHelpListsAllowPrune(t *testing.T) {
	code, out, errOut := runCLI([]string{"sync", "--help"}, nil)
	wantCode(t, code, 0, out, errOut)
	if !strings.Contains(out, "--allow-prune") {
		t.Fatalf("sync --help lacks --allow-prune:\n%s", out)
	}
}
