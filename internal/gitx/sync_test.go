package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncInitialMirror(t *testing.T) {
	f := newFixture(t)
	f.commit("a.txt")
	f.git("tag", "v1")
	f.git("tag", "-a", "v2", "-m", "annotated")
	f.git("checkout", "-q", "-b", "feature")
	f.commit("b.txt")
	f.push("main", "feature", "v1", "v2")

	res := f.sync(SyncOptions{})

	want := refsOf(t, f.env, f.src)
	if len(want) != 4 {
		t.Fatalf("fixture: src refs = %v", want)
	}
	equalMaps(t, "dst refs", refsOf(t, f.env, f.dst), want)
	equalStrings(t, "Created", res.Created, []string{"refs/heads/feature", "refs/heads/main", "refs/tags/v1", "refs/tags/v2"})
	if len(res.Updated) != 0 || len(res.Deleted) != 0 {
		t.Fatalf("Updated=%v Deleted=%v, want none", res.Updated, res.Deleted)
	}
	if !res.Changed {
		t.Fatal("Changed = false on initial mirror")
	}
	if len(res.SrcRefs) != 4 || len(res.DstRefsBefore) != 0 {
		t.Fatalf("SrcRefs=%v DstRefsBefore=%v", res.SrcRefs, res.DstRefsBefore)
	}
	if typ := tgit(t, f.env, f.dst, "cat-file", "-t", "refs/tags/v2"); typ != "tag" {
		t.Fatalf("annotated tag on dst has type %q, want tag", typ)
	}
	if typ := tgit(t, f.env, f.dst, "cat-file", "-t", "refs/tags/v1"); typ != "commit" {
		t.Fatalf("lightweight tag on dst has type %q, want commit", typ)
	}
}

func TestSyncIncrementalCommit(t *testing.T) {
	f := newFixture(t)
	f.commit("a.txt")
	f.push("main")
	f.sync(SyncOptions{})

	h := f.commit("b.txt")
	f.push("main")
	res := f.sync(SyncOptions{})

	equalStrings(t, "Updated", res.Updated, []string{"refs/heads/main"})
	equalStrings(t, "Created", res.Created, nil)
	if got := refsOf(t, f.env, f.dst)["refs/heads/main"]; got != h {
		t.Fatalf("dst main = %s, want %s", got, h)
	}
}

func TestSyncForcePushPropagates(t *testing.T) {
	f := newFixture(t)
	f.commit("a.txt")
	old := f.commit("b.txt")
	f.push("main")
	f.sync(SyncOptions{})

	f.git("reset", "-q", "--hard", "HEAD~1")
	rewritten := f.commit("c.txt")
	f.git("push", "-q", "-f", "origin", "main")
	res := f.sync(SyncOptions{})

	if rewritten == old {
		t.Fatal("fixture: history not rewritten")
	}
	equalStrings(t, "Updated", res.Updated, []string{"refs/heads/main"})
	if got := refsOf(t, f.env, f.dst)["refs/heads/main"]; got != rewritten {
		t.Fatalf("dst main = %s, want rewritten %s", got, rewritten)
	}
}

func TestSyncBranchDeleteNeedsPrune(t *testing.T) {
	f := newFixture(t)
	f.commit("a.txt")
	f.push("main")
	f.git("checkout", "-q", "-b", "feature")
	f.commit("b.txt")
	f.push("feature")
	f.sync(SyncOptions{Prune: true})

	f.git("push", "-q", "origin", "--delete", "feature")

	res := f.sync(SyncOptions{})
	if _, ok := refsOf(t, f.env, f.dst)["refs/heads/feature"]; !ok {
		t.Fatal("feature deleted on dst without Prune")
	}
	if res.Changed || len(res.Deleted) != 0 {
		t.Fatalf("without Prune: Changed=%v Deleted=%v", res.Changed, res.Deleted)
	}

	res = f.sync(SyncOptions{Prune: true})
	equalStrings(t, "Deleted", res.Deleted, []string{"refs/heads/feature"})
	if !res.Changed {
		t.Fatal("Changed = false after prune delete")
	}
	if _, ok := refsOf(t, f.env, f.dst)["refs/heads/feature"]; ok {
		t.Fatal("feature still on dst after Prune")
	}
	equalMaps(t, "dst refs", refsOf(t, f.env, f.dst), refsOf(t, f.env, f.src))
}

func TestSyncTagDeleteWithPrune(t *testing.T) {
	f := newFixture(t)
	f.commit("a.txt")
	f.git("tag", "-a", "v1", "-m", "v1")
	f.git("tag", "v2")
	f.push("main", "v1", "v2")
	f.sync(SyncOptions{Prune: true})

	f.git("push", "-q", "origin", "--delete", "refs/tags/v1")
	res := f.sync(SyncOptions{Prune: true})

	equalStrings(t, "Deleted", res.Deleted, []string{"refs/tags/v1"})
	got := refsOf(t, f.env, f.dst)
	if _, ok := got["refs/tags/v1"]; ok {
		t.Fatal("tag v1 still on dst")
	}
	if _, ok := got["refs/tags/v2"]; !ok {
		t.Fatal("tag v2 missing on dst")
	}
}

func TestSyncEmptySourceErrors(t *testing.T) {
	f := newFixture(t)
	f.commit("a.txt")
	f.git("push", "-q", fileURL(t, f.dst), "main")
	before := refsOf(t, f.env, f.dst)

	f.resetCmds()
	_, err := f.c.Sync(context.Background(), f.srcRemote, f.dstRemote, SyncOptions{Prune: true})
	if !errors.Is(err, ErrEmptySource) {
		t.Fatalf("err = %v, want ErrEmptySource", err)
	}
	equalMaps(t, "dst refs", refsOf(t, f.env, f.dst), before)
	if ranSub(f.captured(), "push") {
		t.Fatal("push ran despite empty source")
	}
	if _, err := os.Stat(f.cache); !os.IsNotExist(err) {
		t.Fatalf("cache created on empty-source failure: %v", err)
	}
}

func TestSyncAllowEmptySourceIsNoop(t *testing.T) {
	f := newFixture(t)
	res := f.sync(SyncOptions{AllowEmptySource: true})
	if res.Changed || len(res.SrcRefs) != 0 {
		t.Fatalf("empty->empty: %+v", res)
	}

	f.commit("a.txt")
	f.git("push", "-q", fileURL(t, f.dst), "main")
	before := refsOf(t, f.env, f.dst)
	f.resetCmds()
	res = f.sync(SyncOptions{AllowEmptySource: true, Prune: true})
	if res.Changed || len(res.Deleted) != 0 {
		t.Fatalf("empty source must never delete dst refs: %+v", res)
	}
	if len(res.DstRefsBefore) != 1 {
		t.Fatalf("DstRefsBefore = %v", res.DstRefsBefore)
	}
	equalMaps(t, "dst refs", refsOf(t, f.env, f.dst), before)
	if ranSub(f.captured(), "push") {
		t.Fatal("push ran for empty source")
	}
}

func TestSyncSecondRunUnchanged(t *testing.T) {
	f := newFixture(t)
	f.commit("a.txt")
	f.git("tag", "-a", "v1", "-m", "v1")
	f.push("main", "v1")
	f.sync(SyncOptions{Prune: true})

	f.resetCmds()
	res := f.sync(SyncOptions{Prune: true})
	if res.Changed || len(res.Created)+len(res.Updated)+len(res.Deleted) != 0 {
		t.Fatalf("second run: %+v", res)
	}
	if Digest(res.SrcRefs) != Digest(res.DstRefsBefore) {
		t.Fatal("src/dst digests differ on unchanged run")
	}
	if ranSub(f.captured(), "push") {
		t.Fatal("push ran although nothing changed")
	}
}

func TestSyncDryRun(t *testing.T) {
	f := newFixture(t)
	f.commit("a.txt")
	f.push("main")
	f.git("checkout", "-q", "-b", "feature")
	f.commit("b.txt")
	f.push("feature")

	f.resetCmds()
	res := f.sync(SyncOptions{DryRun: true})
	equalStrings(t, "Created", res.Created, []string{"refs/heads/main", "refs/heads/feature"})
	if !res.Changed {
		t.Fatal("dry run: Changed = false")
	}
	if n := len(refsOf(t, f.env, f.dst)); n != 0 {
		t.Fatalf("dry run wrote %d refs to dst", n)
	}
	if ranSub(f.captured(), "push") {
		t.Fatal("dry run invoked push")
	}

	f.sync(SyncOptions{})
	f.git("checkout", "-q", "main")
	f.commit("c.txt")
	f.push("main")
	f.git("push", "-q", "origin", "--delete", "feature")
	f.git("tag", "v1")
	f.push("v1")
	snapshot := refsOf(t, f.env, f.dst)

	f.resetCmds()
	res = f.sync(SyncOptions{DryRun: true, Prune: true})
	equalStrings(t, "Created", res.Created, []string{"refs/tags/v1"})
	equalStrings(t, "Updated", res.Updated, []string{"refs/heads/main"})
	equalStrings(t, "Deleted", res.Deleted, []string{"refs/heads/feature"})
	equalMaps(t, "dst after dry run", refsOf(t, f.env, f.dst), snapshot)
	if ranSub(f.captured(), "push") {
		t.Fatal("dry run invoked push")
	}
}

func TestSyncSkipsPullRefs(t *testing.T) {
	f := newFixture(t)
	f.commit("a.txt")
	f.push("main")
	f.git("checkout", "-q", "-b", "pr")
	f.commit("pr.txt")
	f.git("push", "-q", "origin", "pr:refs/pull/1/head")
	if _, ok := allRefs(t, f.env, f.src)["refs/pull/1/head"]; !ok {
		t.Fatal("fixture: pull ref missing on src")
	}

	res := f.sync(SyncOptions{Prune: true})
	for name := range allRefs(t, f.env, f.dst) {
		if strings.HasPrefix(name, "refs/pull/") {
			t.Fatalf("pull ref %s pushed to dst", name)
		}
	}
	for _, r := range res.SrcRefs {
		if strings.HasPrefix(r.Name, "refs/pull/") {
			t.Fatalf("SrcRefs contains %s", r.Name)
		}
	}
	equalMaps(t, "dst refs", refsOf(t, f.env, f.dst), refsOf(t, f.env, f.src))
}

func TestSyncCorruptCacheSelfHeals(t *testing.T) {
	t.Run("garbage directory", func(t *testing.T) {
		f := newFixture(t)
		f.commit("a.txt")
		f.push("main")
		if err := os.MkdirAll(f.cache, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.cache, "HEAD"), []byte("garbage\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.cache, "junk.txt"), []byte("junk"), 0o644); err != nil {
			t.Fatal(err)
		}

		f.sync(SyncOptions{})
		equalMaps(t, "dst refs", refsOf(t, f.env, f.dst), refsOf(t, f.env, f.src))
		if _, err := os.Stat(filepath.Join(f.cache, "junk.txt")); !os.IsNotExist(err) {
			t.Fatal("corrupt cache was not re-initialised")
		}
	})
	t.Run("objects removed", func(t *testing.T) {
		f := newFixture(t)
		f.commit("a.txt")
		f.push("main")
		f.sync(SyncOptions{})
		if err := os.RemoveAll(filepath.Join(f.cache, "objects")); err != nil {
			t.Fatal(err)
		}
		f.commit("b.txt")
		f.push("main")
		f.sync(SyncOptions{})
		equalMaps(t, "dst refs", refsOf(t, f.env, f.dst), refsOf(t, f.env, f.src))
	})
}

func TestSyncPushRejectionListsRefs(t *testing.T) {
	f := newFixture(t)
	tgit(t, f.env, f.dst, "config", "receive.denyNonFastForwards", "true")
	f.commit("a.txt")
	f.commit("b.txt")
	f.push("main")
	f.sync(SyncOptions{})

	f.git("reset", "-q", "--hard", "HEAD~1")
	f.commit("c.txt")
	f.git("push", "-q", "-f", "origin", "main")
	_, err := f.c.Sync(context.Background(), f.srcRemote, f.dstRemote, SyncOptions{})
	if !errors.Is(err, ErrPushRejected) {
		t.Fatalf("err = %v, want ErrPushRejected", err)
	}
	if !strings.Contains(err.Error(), "refs/heads/main") {
		t.Fatalf("error does not name the rejected ref: %v", err)
	}
}

func TestSyncRetriesWithoutAtomic(t *testing.T) {
	f := newFixture(t)
	tgit(t, f.env, f.dst, "config", "receive.advertiseAtomic", "false")
	f.commit("a.txt")
	f.push("main")

	f.resetCmds()
	f.sync(SyncOptions{})
	equalMaps(t, "dst refs", refsOf(t, f.env, f.dst), refsOf(t, f.env, f.src))

	var pushes [][]string
	for _, c := range f.captured() {
		if cmdMentions(c, "push") {
			pushes = append(pushes, c.Args)
		}
	}
	if len(pushes) != 2 {
		t.Fatalf("push invocations = %d (%v), want 2 (atomic then plain)", len(pushes), pushes)
	}
	if !strings.Contains(strings.Join(pushes[0], " "), "--atomic") || strings.Contains(strings.Join(pushes[1], " "), "--atomic") {
		t.Fatalf("push args = %v", pushes)
	}
}

func TestSyncCommandShapes(t *testing.T) {
	f := newFixture(t)
	f.commit("a.txt")
	f.push("main")
	f.resetCmds()
	f.sync(SyncOptions{Prune: true})

	var fetch, push []string
	for _, c := range f.captured() {
		if cmdMentions(c, "fetch") {
			fetch = c.Args[1:]
		}
		if cmdMentions(c, "push") {
			push = c.Args[1:]
		}
	}
	joined := strings.Join(fetch, " ")
	for _, want := range []string{"-c credential.helper=", "fetch --prune --no-tags --force -- " + f.srcRemote.URL + " +refs/heads/*:refs/heads/* +refs/tags/*:refs/tags/*"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("fetch args %q missing %q", joined, want)
		}
	}
	if strings.Contains(joined, "--mirror") {
		t.Fatal("fetch uses --mirror")
	}
	joined = strings.Join(push, " ")
	for _, want := range []string{"-c credential.helper=", "push --porcelain --force --prune --atomic -- " + f.dstRemote.URL + " +refs/heads/*:refs/heads/* +refs/tags/*:refs/tags/*"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("push args %q missing %q", joined, want)
		}
	}
}

// tokenCollisionFixture builds refs whose names and hashes contain tok.
func tokenCollisionFixture(t *testing.T) *fixture {
	f := newFixture(t)
	f.commit("a.txt")
	f.git("tag", "v1")
	f.git("tag", "-a", "v2", "-m", "annotated")
	f.git("checkout", "-q", "-b", "feature-v1")
	f.commit("b.txt")
	f.push("main", "feature-v1", "v1", "v2")
	return f
}

func TestSyncTokenCollidingWithRefNames(t *testing.T) {
	for _, tok := range []string{"v1", "a", "refs", "e"} {
		t.Run(tok, func(t *testing.T) {
			f := tokenCollisionFixture(t)
			f.srcRemote.Token = tok
			f.dstRemote.Token = tok
			res := f.sync(SyncOptions{})
			want := refsOf(t, f.env, f.src)
			equalMaps(t, "dst refs", refsOf(t, f.env, f.dst), want)
			equalStrings(t, "Created", res.Created, []string{"refs/heads/feature-v1", "refs/heads/main", "refs/tags/v1", "refs/tags/v2"})
			if len(res.SrcRefs) != 4 {
				t.Fatalf("SrcRefs = %v", res.SrcRefs)
			}
			for _, r := range res.SrcRefs {
				if want[r.Name] != r.Hash {
					t.Fatalf("SrcRef %+v does not match src %v", r, want)
				}
			}
		})
	}
}

func TestLsRemoteTokenCollidingWithRefs(t *testing.T) {
	for _, tok := range []string{"v1", "a", "refs/tags"} {
		f := tokenCollisionFixture(t)
		rem := f.srcRemote
		rem.Token = tok
		got, err := f.r.LsRemote(context.Background(), rem)
		if err != nil {
			t.Fatal(err)
		}
		want := refsOf(t, f.env, f.src)
		m := map[string]string{}
		for _, r := range got {
			m[r.Name] = r.Hash
		}
		if len(got) != len(want) {
			t.Fatalf("token %q: got %v, want %v", tok, got, want)
		}
		equalMaps(t, "ls-remote "+tok, m, want)
	}
}

func TestPushRejectionParsedRawErrorScrubbed(t *testing.T) {
	f := newFixture(t)
	tgit(t, f.env, f.dst, "config", "receive.denyNonFastForwards", "true")
	f.git("checkout", "-q", "-b", "secretbranch-x")
	f.commit("a.txt")
	f.commit("b.txt")
	f.push("secretbranch-x")
	f.sync(SyncOptions{})

	f.git("reset", "-q", "--hard", "HEAD~1")
	f.commit("c.txt")
	f.git("push", "-q", "-f", "origin", "secretbranch-x")
	f.dstRemote.Token = "secretbranch"
	_, err := f.c.Sync(context.Background(), f.srcRemote, f.dstRemote, SyncOptions{})
	if !errors.Is(err, ErrPushRejected) {
		t.Fatalf("err = %v, want ErrPushRejected", err)
	}
	if strings.Contains(err.Error(), "secretbranch") {
		t.Fatalf("error leaks token: %v", err)
	}
	if !strings.Contains(err.Error(), "refs/heads/***-x") {
		t.Fatalf("error missing scrubbed rejected ref: %v", err)
	}
}

func TestParsePorcelainKeepsRawNames(t *testing.T) {
	rej, n := parsePorcelain("To x\n!\trefs/heads/t:refs/heads/t\t[rejected] (non-fast-forward)\n=\trefs/tags/v1:refs/tags/v1\t[up to date]\nDone\n")
	if n != 2 || len(rej) != 1 || !strings.HasPrefix(rej[0], "refs/heads/t ") {
		t.Fatalf("rej=%v n=%d", rej, n)
	}
}
