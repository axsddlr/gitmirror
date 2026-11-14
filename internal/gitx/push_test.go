package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeHook installs an sh hook in a bare repository. Git for Windows runs
// sh hooks through its bundled sh.
func writeHook(t *testing.T, bare, name, body string) {
	t.Helper()
	p := filepath.Join(bare, "hooks", name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// recordPushes installs a post-receive hook in dst that appends, per push,
// the received ref names followed by a "--" separator line.
func recordPushes(t *testing.T, dst string) string {
	t.Helper()
	log := filepath.Join(t.TempDir(), "pushes.log")
	writeHook(t, dst, "post-receive", fmt.Sprintf(
		"while read old new ref; do echo \"$ref\" >> '%s'; done\necho -- >> '%s'\n",
		filepath.ToSlash(log), filepath.ToSlash(log)))
	return log
}

// pushGroups returns the ref names of each recorded push, in order.
func pushGroups(t *testing.T, log string) [][]string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("no pushes recorded: %v", err)
	}
	var groups [][]string
	var cur []string
	for _, l := range strings.Split(strings.ReplaceAll(string(b), "\r", ""), "\n") {
		switch l {
		case "":
		case "--":
			groups = append(groups, cur)
			cur = nil
		default:
			cur = append(cur, l)
		}
	}
	return groups
}

func TestSyncFirstBranchPushedFirstIntoEmptyDestination(t *testing.T) {
	f := newFixture(t)
	f.commit("a.txt")
	f.git("tag", "v1")
	f.git("branch", "feature")
	f.git("branch", "alpha")
	f.push("main", "feature", "alpha", "v1")
	log := recordPushes(t, f.dst)

	f.resetCmds()
	res := f.sync(SyncOptions{Prune: true, FirstBranch: "main"})
	equalMaps(t, "dst refs", refsOf(t, f.env, f.dst), refsOf(t, f.env, f.src))
	equalStrings(t, "Created", res.Created, []string{"refs/heads/alpha", "refs/heads/feature", "refs/heads/main", "refs/tags/v1"})

	groups := pushGroups(t, log)
	if len(groups) != 2 {
		t.Fatalf("pushes = %q, want 2 (default branch alone, then the rest)", groups)
	}
	if strings.Join(groups[0], ",") != "refs/heads/main" {
		t.Fatalf("first push = %q, want only refs/heads/main", groups[0])
	}
	equalStrings(t, "second push", groups[1], []string{"refs/heads/alpha", "refs/heads/feature", "refs/tags/v1"})

	var pushes []string
	for _, c := range f.captured() {
		if cmdMentions(c, "push") {
			pushes = append(pushes, strings.Join(c.Args, " "))
		}
	}
	if len(pushes) != 2 || !strings.HasSuffix(pushes[0], "-- "+f.dstRemote.URL+" +refs/heads/main:refs/heads/main") {
		t.Fatalf("push commands = %q", pushes)
	}
}

func TestSyncFirstBranchOnlyIntoEmptyDestination(t *testing.T) {
	f := newFixture(t)
	f.commit("a.txt")
	f.git("branch", "feature")
	f.push("main", "feature")
	f.sync(SyncOptions{})
	log := recordPushes(t, f.dst)

	// The destination has refs now: one normal push, even with FirstBranch.
	f.commit("b.txt")
	f.git("checkout", "-q", "-b", "zz")
	f.commit("c.txt")
	f.push("main", "zz")
	f.sync(SyncOptions{FirstBranch: "main"})
	if g := pushGroups(t, log); len(g) != 1 {
		t.Fatalf("pushes into a non-empty destination = %q, want 1", g)
	}
	equalMaps(t, "dst refs", refsOf(t, f.env, f.dst), refsOf(t, f.env, f.src))
}

func TestSyncFirstBranchMissingAtSourceIsIgnored(t *testing.T) {
	f := newFixture(t)
	f.commit("a.txt")
	f.push("main")
	log := recordPushes(t, f.dst)
	f.sync(SyncOptions{FirstBranch: "develop"})
	if g := pushGroups(t, log); len(g) != 1 {
		t.Fatalf("pushes = %q, want 1", g)
	}
	equalMaps(t, "dst refs", refsOf(t, f.env, f.dst), refsOf(t, f.env, f.src))
}

func TestSyncFirstBranchDryRunPushesNothing(t *testing.T) {
	f := newFixture(t)
	f.commit("a.txt")
	f.git("branch", "feature")
	f.push("main", "feature")
	f.resetCmds()
	f.sync(SyncOptions{DryRun: true, FirstBranch: "main"})
	if ranSub(f.captured(), "push") {
		t.Fatal("dry run pushed")
	}
	if n := len(refsOf(t, f.env, f.dst)); n != 0 {
		t.Fatalf("dry run wrote %d refs", n)
	}
}

func TestSyncPushRejectionIncludesRemoteReason(t *testing.T) {
	f := newFixture(t)
	f.commit("a.txt")
	f.push("main")
	const tok = "hooktoken-4242"
	f.dstRemote.Token = tok
	writeHook(t, f.dst, "pre-receive",
		"cat >/dev/null\necho 'GL-HOOK-ERR: branch main is protected'\necho \"leaked "+tok+" here\" >&2\nexit 1\n")
	_, err := f.c.Sync(context.Background(), f.srcRemote, f.dstRemote, SyncOptions{})
	if !errors.Is(err, ErrPushRejected) {
		t.Fatalf("err = %v, want ErrPushRejected", err)
	}
	if !errors.Is(err, ErrDestination) {
		t.Fatalf("err = %v, want ErrDestination", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "refs/heads/main") {
		t.Fatalf("error does not name the rejected ref: %v", err)
	}
	if !strings.Contains(msg, "GL-HOOK-ERR: branch main is protected") {
		t.Fatalf("error does not carry the remote's reason: %v", err)
	}
	if strings.Contains(msg, tok) || !strings.Contains(msg, "leaked *** here") {
		t.Fatalf("remote reason not scrubbed: %v", err)
	}
}

func TestRemoteReason(t *testing.T) {
	if got := remoteReason(""); got != "" {
		t.Fatalf("empty stderr: %q", got)
	}
	got := remoteReason("To x\nremote: one        \r\nerror: failed to push\nremote:\nremote:   two\n")
	if got != "one | two" {
		t.Fatalf("got %q", got)
	}
	var b strings.Builder
	for i := range 30 {
		fmt.Fprintf(&b, "remote: line %02d\n", i)
	}
	got = remoteReason(b.String())
	if strings.Contains(got, "line 19") || !strings.HasPrefix(got, "line 20") || !strings.HasSuffix(got, "line 29") {
		t.Fatalf("want the last 10 lines, got %q", got)
	}
	long := "remote: " + strings.Repeat("x", 5000) + "\n"
	if got := remoteReason(long); len(got) > maxRemoteReason+len("...") || !strings.HasSuffix(got, "xxx") {
		t.Fatalf("not bounded: %d bytes", len(got))
	}
}

func TestSyncDestinationErrorsMarked(t *testing.T) {
	f := newFixture(t)
	f.commit("a.txt")
	f.push("main")
	bad := Remote{URL: fileURL(t, filepath.Join(f.root, "missing.git"))}
	_, err := f.c.Sync(context.Background(), f.srcRemote, bad, SyncOptions{})
	if err == nil || !errors.Is(err, ErrDestination) {
		t.Fatalf("missing destination: err = %v, want ErrDestination", err)
	}
	_, err = f.c.Sync(context.Background(), bad, f.dstRemote, SyncOptions{})
	if err == nil || errors.Is(err, ErrDestination) {
		t.Fatalf("missing source: err = %v, must not be ErrDestination", err)
	}
}
