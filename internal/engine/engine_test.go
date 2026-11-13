package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gitmirror/internal/forge"
	"gitmirror/internal/forge/fake"
	"gitmirror/internal/resolve"
	"gitmirror/internal/state"
)

func TestNewDestinationCreatedPrivate(t *testing.T) {
	h := newHarness(t)
	s := h.addSource(forge.Repo{Name: "proj", Description: "the project", Homepage: "https://example.com",
		Topics: []string{"Go", "mirror"}, DefaultBranch: "develop"})
	s.commit("a.txt")
	s.push("main")
	s.git("checkout", "-q", "-b", "develop")
	s.commit("b.txt")
	s.push("develop")
	s.git("tag", "-a", "-m", "v1", "v1")
	s.push("v1")

	r := h.sync(h.pair(s, settings()), Options{})
	wantStatus(t, r, state.RunOK)
	if r.RunID == 0 {
		t.Fatal("RunID = 0 in a real run")
	}
	wantStep(t, r, "prepare", state.StepOK)
	wantStep(t, r, "git", state.StepOK)
	wantStep(t, r, "settings", state.StepOK)
	wantAction(t, r, "create repository dst:mirrors/proj (private)")

	equalRefs(t, "dst refs", h.dstRefs("mirrors", "proj"), h.refs(s.bare))
	d := h.dstRepo("mirrors", "proj")
	if !d.Private {
		t.Fatal("destination is public, want private")
	}
	if d.Description != "the project" || d.Homepage != "https://example.com" || d.DefaultBranch != "develop" {
		t.Fatalf("settings not applied: %+v", d)
	}
	if strings.Join(d.Topics, ",") != "go,mirror" {
		t.Fatalf("topics = %v, want [go mirror]", d.Topics)
	}
	how, ok, err := h.store.Owned(context.Background(), "dst", "mirrors/proj")
	if err != nil || !ok || how != state.OwnedCreated {
		t.Fatalf("ownership = %q %v %v, want created", how, ok, err)
	}
	ps, ok, _ := h.store.Pair(context.Background(), r.Key)
	if !ok || ps.LastRefCount != 3 || ps.LastDigest == "" {
		t.Fatalf("pair state = %+v", ps)
	}
	runs, _ := h.store.Runs(context.Background(), r.Key, 0)
	if len(runs) != 1 || runs[0].Status != state.RunOK || runs[0].Trigger != "cli" {
		t.Fatalf("runs = %+v", runs)
	}
	if step(r, "git").Duration <= 0 {
		t.Error("git step has no duration")
	}

	// Second run: nothing to do.
	r = h.sync(h.pair(s, settings()), Options{})
	wantStatus(t, r, state.RunOK)
	wantStep(t, r, "git", state.StepUnchanged)
	wantStep(t, r, "settings", state.StepUnchanged)
}

func TestVisibilityMatchSource(t *testing.T) {
	for _, private := range []bool{false, true} {
		t.Run(fmt.Sprintf("private=%v", private), func(t *testing.T) {
			h := newHarness(t)
			s := h.addSource(forge.Repo{Name: "proj", Private: private})
			s.commit("a.txt")
			s.push("main")
			rs := settings()
			rs.Visibility = "match-source"
			r := h.sync(h.pair(s, rs), Options{})
			wantStatus(t, r, state.RunOK)
			if d := h.dstRepo("mirrors", "proj"); d.Private != private {
				t.Fatalf("dst private = %v, want %v", d.Private, private)
			}
		})
	}
}

func TestPublicSourceDefaultVisibilityStaysPrivate(t *testing.T) {
	h := newHarness(t)
	s := h.addSource(forge.Repo{Name: "proj", Private: false})
	s.commit("a.txt")
	s.push("main")
	r := h.sync(h.pair(s, settings()), Options{})
	wantStatus(t, r, state.RunOK)
	if d := h.dstRepo("mirrors", "proj"); !d.Private {
		t.Fatal("visibility private produced a public destination")
	}
}

func TestUnownedDestinationWithoutAdoptMakesNoWrites(t *testing.T) {
	h := newHarness(t)
	s := h.addSource(forge.Repo{Name: "proj"})
	s.commit("a.txt")
	s.push("main")
	if err := h.dst.AddRepo(forge.Repo{Owner: "mirrors", Name: "proj", DefaultBranch: "main", Archived: true}); err != nil {
		t.Fatal(err)
	}
	r := h.sync(h.pair(s, settings()), Options{})
	wantStatus(t, r, state.RunFailed)
	if r.Err == nil || !strings.Contains(r.Err.Error(), "adopt: true") || !strings.Contains(r.Err.Error(), "mirrors/proj") {
		t.Fatalf("err = %v, want a hint naming the repo and adopt: true", r.Err)
	}
	if w := writeCalls(h.dst.Calls()); len(w) != 0 {
		t.Fatalf("dst writes on unowned repo: %v", w)
	}
	if n := len(h.dstRefs("mirrors", "proj")); n != 0 {
		t.Fatalf("dst has %d refs", n)
	}
	if _, ok, _ := h.store.Owned(context.Background(), "dst", "mirrors/proj"); ok {
		t.Fatal("ownership recorded without adopt")
	}

	rs := settings()
	rs.Adopt = true
	r = h.sync(h.pair(s, rs), Options{})
	wantStatus(t, r, state.RunOK)
	how, ok, _ := h.store.Owned(context.Background(), "dst", "mirrors/proj")
	if !ok || how != state.OwnedAdopted {
		t.Fatalf("ownership = %q %v, want adopted", how, ok)
	}
	equalRefs(t, "dst refs", h.dstRefs("mirrors", "proj"), h.refs(s.bare))
}

func TestEmptySourceGuard(t *testing.T) {
	h := newHarness(t)
	s := h.addSource(forge.Repo{Name: "proj"})
	s.commit("a.txt")
	s.push("main")
	s.git("tag", "v1")
	s.push("v1")
	p := h.pair(s, settings())
	wantStatus(t, h.sync(p, Options{}), state.RunOK)
	before := h.dstRefs("mirrors", "proj")
	if len(before) != 2 {
		t.Fatalf("fixture: dst refs = %v", before)
	}

	h.tgit(s.bare, "--git-dir="+s.bare, "update-ref", "-d", "refs/heads/main")
	h.tgit(s.bare, "--git-dir="+s.bare, "update-ref", "-d", "refs/tags/v1")
	if n := len(h.refs(s.bare)); n != 0 {
		t.Fatalf("fixture: source still has %d refs", n)
	}
	mark := len(h.dst.Calls())
	r := h.sync(p, Options{})
	wantStatus(t, r, state.RunAborted)
	if r.Err == nil || !strings.Contains(r.Err.Error(), "source has no refs but previously had 2; refusing to prune destination") {
		t.Fatalf("err = %v", r.Err)
	}
	equalRefs(t, "dst refs after guard", h.dstRefs("mirrors", "proj"), before)
	if slices.Contains(h.dst.Calls()[mark:], "ListReleases") {
		t.Fatal("releases step ran after the empty-source guard")
	}
	ps, _, _ := h.store.Pair(context.Background(), r.Key)
	if ps.LastRefCount != 2 {
		t.Fatalf("LastRefCount = %d, want 2 (guard must not reset it)", ps.LastRefCount)
	}
}

func TestBrandNewEmptySource(t *testing.T) {
	h := newHarness(t)
	s := h.addSource(forge.Repo{Name: "empty", DefaultBranch: "develop"})
	r := h.sync(h.pair(s, settings()), Options{})
	wantStatus(t, r, state.RunOK)
	wantStep(t, r, "git", state.StepUnchanged)
	if n := len(h.dstRefs("mirrors", "empty")); n != 0 {
		t.Fatalf("dst refs = %d, want 0", n)
	}
	if d := wantStep(t, r, "settings", state.StepUnchanged); !strings.Contains(d.Detail, "default branch") {
		t.Fatalf("settings detail = %q, want a note about the missing default branch", d.Detail)
	}
}

func TestForcePushAndBranchDeletionPropagate(t *testing.T) {
	h := newHarness(t)
	s := h.addSource(forge.Repo{Name: "proj"})
	s.commit("a.txt")
	s.commit("b.txt")
	s.push("main")
	s.git("branch", "feature")
	s.push("feature")
	p := h.pair(s, settings("git"))
	wantStatus(t, h.sync(p, Options{}), state.RunOK)

	s.git("reset", "-q", "--hard", "HEAD~1")
	newHead := s.commit("c.txt")
	s.push("--force", "main")
	s.push("--delete", "feature")
	r := h.sync(p, Options{})
	wantStatus(t, r, state.RunOK)
	if d := wantStep(t, r, "git", state.StepOK); d.Detail != "0 created, 1 updated, 1 deleted" {
		t.Fatalf("git detail = %q", d.Detail)
	}
	got := h.dstRefs("mirrors", "proj")
	if got["refs/heads/main"] != newHead {
		t.Fatalf("main = %s, want %s", got["refs/heads/main"], newHead)
	}
	if _, ok := got["refs/heads/feature"]; ok {
		t.Fatal("deleted branch still on destination")
	}
}

func TestArchivedDestinationUnarchivedForPushAndRestored(t *testing.T) {
	h := newHarness(t)
	s := h.addSource(forge.Repo{Name: "proj"})
	s.commit("a.txt")
	s.push("main")
	if err := h.dst.AddRepo(forge.Repo{Owner: "mirrors", Name: "proj", DefaultBranch: "main", Archived: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.MarkOwned(context.Background(), "dst", "mirrors/proj", state.OwnedCreated); err != nil {
		t.Fatal(err)
	}
	r := h.sync(h.pair(s, settings("git")), Options{})
	wantStatus(t, r, state.RunOK)
	equalRefs(t, "dst refs", h.dstRefs("mirrors", "proj"), h.refs(s.bare))
	if !h.dstRepo("mirrors", "proj").Archived {
		t.Fatal("destination not re-archived")
	}
	calls := h.dst.Calls()
	w := writeCalls(calls)
	if len(w) != 2 || w[0] != "UpdateRepoSettings" || w[1] != "UpdateRepoSettings" {
		t.Fatalf("dst writes = %v, want unarchive + re-archive", w)
	}
	if !(indexOf(calls, "UpdateRepoSettings") < indexOf(calls, "GitURL")) {
		t.Fatalf("unarchive did not happen before the push: %v", calls)
	}
	wantAction(t, r, "unarchive")
}

func TestArchivedSourceArchivesDestinationLast(t *testing.T) {
	h := newHarness(t)
	s := h.addSource(forge.Repo{Name: "proj", Archived: true})
	s.commit("a.txt")
	s.push("main")
	s.git("tag", "v1")
	s.push("v1")
	h.seedRelease(h.src, "alice", "proj", forge.Release{Tag: "v1", Name: "one"}, map[string][]byte{"a.bin": blob(10, 1)})
	s.repo.Archived = true
	r := h.sync(h.pair(s, settings()), Options{})
	wantStatus(t, r, state.RunOK)
	if !h.dstRepo("mirrors", "proj").Archived {
		t.Fatal("destination not archived")
	}
	calls := h.dst.Calls()
	last := lastIndexOf(calls, "UpdateRepoSettings")
	for _, c := range []string{"CreateRelease", "UploadAsset"} {
		if i := lastIndexOf(calls, c); i < 0 || i > last {
			t.Fatalf("%s (index %d) not before the final archive (index %d): %v", c, i, last, calls)
		}
	}
	w := writeCalls(calls)
	if w[len(w)-1] != "UpdateRepoSettings" {
		t.Fatalf("last write = %s, want UpdateRepoSettings: %v", w[len(w)-1], w)
	}
}

func TestReleasesMirrored(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.addSource(forge.Repo{Name: "proj"})
	s.commit("a.txt")
	s.push("main")
	aData, bData := blob(100, 1), blob(50, 2)
	r1 := h.seedRelease(h.src, "alice", "proj", forge.Release{Tag: "v1", Name: "One", Body: "first", Target: "main"},
		map[string][]byte{"a.bin": aData, "b.bin": bData})
	h.seedRelease(h.src, "alice", "proj", forge.Release{Tag: "v2", Name: "Draft", Draft: true}, nil)
	rs := settings()
	p := h.pair(s, rs)

	r := h.sync(p, Options{})
	wantStatus(t, r, state.RunOK)
	if d := wantStep(t, r, "releases", state.StepOK); !strings.Contains(d.Detail, "1 drafts skipped") {
		t.Fatalf("releases detail = %q", d.Detail)
	}
	wantAction(t, r, "create release v1 (2 assets)")
	d1 := releaseByTag(t, h.dst, "mirrors", "proj", "v1")
	if d1 == nil || d1.Name != "One" || d1.Body != "first" || d1.Draft {
		t.Fatalf("dst v1 = %+v", d1)
	}
	if releaseByTag(t, h.dst, "mirrors", "proj", "v2") != nil {
		t.Fatal("draft release mirrored")
	}
	for name, want := range map[string][]byte{"a.bin": aData, "b.bin": bData} {
		a := assetByName(d1, name)
		if a == nil || !bytes.Equal(h.dst.AssetBytes("mirrors", "proj", d1.ID, a.ID), want) {
			t.Fatalf("asset %s missing or bytes differ", name)
		}
	}
	m, _ := h.store.ReleaseMap(ctx, r.Key)
	if m[r1.ID] != d1.ID {
		t.Fatalf("release map = %v, want %d->%d", m, r1.ID, d1.ID)
	}

	// Unchanged second run.
	r = h.sync(p, Options{})
	wantStep(t, r, "releases", state.StepUnchanged)

	// Body change, asset replaced (size change), extra dst asset, manual dst release.
	r1.Body = "first, edited"
	if _, err := h.src.UpdateRelease(ctx, "alice", "proj", *r1); err != nil {
		t.Fatal(err)
	}
	srcRel := releaseByTag(t, h.src, "alice", "proj", "v1")
	if err := h.src.DeleteAsset(ctx, "alice", "proj", r1.ID, assetByName(srcRel, "a.bin").ID); err != nil {
		t.Fatal(err)
	}
	aData2 := blob(120, 9)
	if _, err := h.src.UploadAsset(ctx, "alice", "proj", r1.ID, forge.Asset{Name: "a.bin"}, bytes.NewReader(aData2)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.dst.UploadAsset(ctx, "mirrors", "proj", d1.ID, forge.Asset{Name: "extra.txt"}, strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	h.seedRelease(h.dst, "mirrors", "proj", forge.Release{Tag: "manual", Name: "hand made"}, nil)

	r = h.sync(p, Options{})
	wantStatus(t, r, state.RunOK)
	d1 = releaseByTag(t, h.dst, "mirrors", "proj", "v1")
	if d1.Body != "first, edited" {
		t.Fatalf("body = %q", d1.Body)
	}
	if a := assetByName(d1, "a.bin"); a == nil || !bytes.Equal(h.dst.AssetBytes("mirrors", "proj", d1.ID, a.ID), aData2) {
		t.Fatal("a.bin not replaced")
	}
	if assetByName(d1, "extra.txt") != nil {
		t.Fatal("extra dst asset not removed")
	}
	if assetByName(d1, "b.bin") == nil {
		t.Fatal("unchanged asset b.bin removed")
	}
	if releaseByTag(t, h.dst, "mirrors", "proj", "manual") == nil {
		t.Fatal("manual release removed without DeleteMissingReleases")
	}

	// Source release deleted: removed only with DeleteMissingReleases, and only ours.
	if err := h.src.DeleteRelease(ctx, "alice", "proj", r1.ID); err != nil {
		t.Fatal(err)
	}
	r = h.sync(p, Options{})
	wantStatus(t, r, state.RunOK)
	if releaseByTag(t, h.dst, "mirrors", "proj", "v1") == nil {
		t.Fatal("v1 deleted although DeleteMissingReleases is false")
	}
	p.Settings.DeleteMissingReleases = true
	r = h.sync(p, Options{})
	wantStatus(t, r, state.RunOK)
	wantAction(t, r, "delete release v1")
	if releaseByTag(t, h.dst, "mirrors", "proj", "v1") != nil {
		t.Fatal("mapped release v1 not deleted")
	}
	if releaseByTag(t, h.dst, "mirrors", "proj", "manual") == nil {
		t.Fatal("unmapped manual release deleted")
	}
	if m, _ := h.store.ReleaseMap(ctx, r.Key); len(m) != 0 {
		t.Fatalf("release map not cleaned: %v", m)
	}
}

func TestReleaseAssetOverMaxSizeSkipped(t *testing.T) {
	h := newHarness(t)
	h.eng.MaxAssetSize = 60
	s := h.addSource(forge.Repo{Name: "proj"})
	s.commit("a.txt")
	s.push("main")
	h.seedRelease(h.src, "alice", "proj", forge.Release{Tag: "v1"}, map[string][]byte{"big.bin": blob(100, 1), "small.bin": blob(50, 2)})
	r := h.sync(h.pair(s, settings()), Options{})
	wantStatus(t, r, state.RunOK)
	d := wantStep(t, r, "releases", state.StepOK)
	if !strings.Contains(d.Detail, "big.bin") {
		t.Fatalf("detail = %q, want a note about big.bin", d.Detail)
	}
	rel := releaseByTag(t, h.dst, "mirrors", "proj", "v1")
	if assetByName(rel, "big.bin") != nil || assetByName(rel, "small.bin") == nil {
		t.Fatalf("assets = %+v", rel.Assets)
	}
}

func TestReleaseUploadFailureIsPartial(t *testing.T) {
	h := newHarness(t)
	s := h.addSource(forge.Repo{Name: "proj"})
	s.commit("a.txt")
	s.push("main")
	h.seedRelease(h.src, "alice", "proj", forge.Release{Tag: "v1"}, map[string][]byte{"a.bin": blob(10, 1)})
	h.seedRelease(h.src, "alice", "proj", forge.Release{Tag: "v2"}, map[string][]byte{"b.bin": blob(10, 2)})
	h.dst.FailNext("UploadAsset", errors.New("boom"))
	p := h.pair(s, settings())
	r := h.sync(p, Options{})
	wantStatus(t, r, state.RunPartial)
	wantStep(t, r, "git", state.StepOK)
	if d := wantStep(t, r, "releases", state.StepFailed); !strings.Contains(d.Detail, "boom") {
		t.Fatalf("detail = %q", d.Detail)
	}
	n := 0
	for _, tag := range []string{"v1", "v2"} {
		if rel := releaseByTag(t, h.dst, "mirrors", "proj", tag); rel != nil {
			n += len(rel.Assets)
		}
	}
	if n != 1 {
		t.Fatalf("uploaded assets = %d, want 1 (other upload failed)", n)
	}
	// Next run heals.
	r = h.sync(p, Options{})
	wantStatus(t, r, state.RunOK)
	for _, tag := range []string{"v1", "v2"} {
		if rel := releaseByTag(t, h.dst, "mirrors", "proj", tag); rel == nil || len(rel.Assets) != 1 {
			t.Fatalf("release %s = %+v", tag, rel)
		}
	}
}

func TestWikiMirrored(t *testing.T) {
	h := newHarness(t)
	s := h.addSource(forge.Repo{Name: "proj"})
	s.commit("a.txt")
	s.push("main")
	if err := h.src.InitWiki("alice", "proj"); err != nil {
		t.Fatal(err)
	}
	wikiBare := bareDir(h.srcBase, "alice", "proj", true)
	wwork := filepath.Join(t.TempDir(), "wiki")
	h.tgit(filepath.Dir(wwork), "init", "-q", "-b", "master", wwork)
	if err := os.WriteFile(filepath.Join(wwork, "Home.md"), []byte("# Home\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.tgit(wwork, "add", "Home.md")
	h.tgit(wwork, "commit", "-q", "-m", "home")
	h.tgit(wwork, "push", "-q", fileURL(wikiBare), "master")
	p := h.pair(s, settings())

	// The destination wiki does not exist yet: failed with a hint, run partial.
	r := h.sync(p, Options{})
	wantStatus(t, r, state.RunPartial)
	if d := wantStep(t, r, "wiki", state.StepFailed); !strings.Contains(d.Detail, "create") {
		t.Fatalf("wiki detail = %q, want a hint to create a page first", d.Detail)
	}

	if err := h.dst.InitWiki("mirrors", "proj"); err != nil {
		t.Fatal(err)
	}
	r = h.sync(p, Options{})
	wantStatus(t, r, state.RunOK)
	wantStep(t, r, "wiki", state.StepOK)
	equalRefs(t, "wiki refs", h.refs(bareDir(h.dstBase, "mirrors", "proj", true)), h.refs(wikiBare))
}

func TestEmptySourceWikiSkipped(t *testing.T) {
	h := newHarness(t)
	s := h.addSource(forge.Repo{Name: "proj"})
	s.commit("a.txt")
	s.push("main")
	if err := h.src.InitWiki("alice", "proj"); err != nil {
		t.Fatal(err)
	}
	r := h.sync(h.pair(s, settings()), Options{})
	wantStatus(t, r, state.RunOK)
	if d := wantStep(t, r, "wiki", state.StepSkipped); d.Detail != "source wiki is empty" {
		t.Fatalf("wiki detail = %q", d.Detail)
	}
}

func TestNoWikiSkipped(t *testing.T) {
	h := newHarness(t)
	s := h.addSource(forge.Repo{Name: "proj"})
	s.commit("a.txt")
	s.push("main")
	r := h.sync(h.pair(s, settings()), Options{})
	wantStatus(t, r, state.RunOK)
	wantStep(t, r, "wiki", state.StepSkipped)
}

// TestLFSMissingSkipsLFSButSyncsGit hides git-lfs behind a failing shim
// placed first on PATH. (Emptying PATH is not usable on Windows: Git for
// Windows' transport crashes without its own bin directory on PATH.)
func TestLFSMissingSkipsLFSButSyncsGit(t *testing.T) {
	h := newHarness(t)
	shim := t.TempDir()
	if err := os.WriteFile(filepath.Join(shim, "git-lfs"), []byte("#!/bin/sh\necho 'git-lfs: not installed' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pathEnv := "PATH=" + shim + string(os.PathListSeparator) + os.Getenv("PATH")
	h.eng.Git.Env = append(h.eng.Git.Env, pathEnv)
	probe := exec.Command("git", "lfs", "version")
	probe.Env = append(cleanHostEnv(), pathEnv)
	if out, err := probe.CombinedOutput(); err == nil && strings.HasPrefix(string(out), "git-lfs/") {
		t.Skip("git-lfs still resolves with a failing shim first on PATH; cannot simulate missing git-lfs")
	}
	s := h.addSource(forge.Repo{Name: "proj"})
	s.commit("a.txt")
	s.push("main")
	r := h.sync(h.pair(s, settings("git", "lfs")), Options{})
	wantStatus(t, r, state.RunOK)
	wantStep(t, r, "git", state.StepOK)
	if d := wantStep(t, r, "lfs", state.StepSkipped); d.Detail != "git-lfs not installed" {
		t.Fatalf("lfs detail = %q", d.Detail)
	}
	equalRefs(t, "dst refs", h.dstRefs("mirrors", "proj"), h.refs(s.bare))
}

func TestLFSAvailableRecordsStep(t *testing.T) {
	h := newHarness(t)
	s := h.addSource(forge.Repo{Name: "proj"})
	s.commit("a.txt")
	s.push("main")
	r := h.sync(h.pair(s, settings("git", "lfs")), Options{})
	wantStatus(t, r, state.RunOK)
	if st := step(r, "lfs"); st.Status != state.StepOK && st.Status != state.StepUnchanged {
		t.Fatalf("lfs step = %+v", st)
	}
}

func TestDryRunMakesNoWrites(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.addSource(forge.Repo{Name: "proj", Description: "d", Topics: []string{"x"}})
	s.commit("a.txt")
	s.push("main")
	s.git("tag", "v1")
	s.push("v1")
	h.seedRelease(h.src, "alice", "proj", forge.Release{Tag: "v1"}, map[string][]byte{"a.bin": blob(10, 1)})
	srcMark := len(h.src.Calls())
	p := h.pair(s, settings())

	r := h.sync(p, Options{DryRun: true})
	wantStatus(t, r, state.RunOK)
	if r.RunID != 0 {
		t.Fatalf("RunID = %d in dry-run", r.RunID)
	}
	wantAction(t, r, "create repository dst:mirrors/proj (private)")
	wantAction(t, r, "push all 2 refs")
	wantAction(t, r, "create release v1 (1 assets)")
	if w := writeCalls(h.dst.Calls()); len(w) != 0 {
		t.Fatalf("dst writes in dry-run: %v", w)
	}
	if w := writeCalls(h.src.Calls()[srcMark:]); len(w) != 0 {
		t.Fatalf("src writes in dry-run: %v", w)
	}
	if runs, _ := h.store.RecentRuns(ctx, 0); len(runs) != 0 {
		t.Fatalf("runs recorded in dry-run: %+v", runs)
	}
	if ps, _ := h.store.Pairs(ctx); len(ps) != 0 {
		t.Fatalf("pairs recorded in dry-run: %+v", ps)
	}
	if _, ok, _ := h.store.Owned(ctx, "dst", "mirrors/proj"); ok {
		t.Fatal("ownership recorded in dry-run")
	}

	// Existing destination, new commit at the source.
	wantStatus(t, h.sync(p, Options{}), state.RunOK)
	runsBefore, _ := h.store.RecentRuns(ctx, 0)
	before := h.dstRefs("mirrors", "proj")
	s.commit("b.txt")
	s.push("main")
	if err := h.src.AddRepo(forge.Repo{Owner: "alice", Name: "proj", DefaultBranch: "main", Description: "new"}); err != nil {
		t.Fatal(err)
	}
	dstMark, srcMark := len(h.dst.Calls()), len(h.src.Calls())
	r = h.sync(p, Options{DryRun: true})
	wantStatus(t, r, state.RunOK)
	wantAction(t, r, "push 0 created, 1 updated, 0 deleted refs")
	wantAction(t, r, "description")
	if w := writeCalls(h.dst.Calls()[dstMark:]); len(w) != 0 {
		t.Fatalf("dst writes in dry-run: %v", w)
	}
	if w := writeCalls(h.src.Calls()[srcMark:]); len(w) != 0 {
		t.Fatalf("src writes in dry-run: %v", w)
	}
	equalRefs(t, "dst refs after dry-run", h.dstRefs("mirrors", "proj"), before)
	if runs, _ := h.store.RecentRuns(ctx, 0); len(runs) != len(runsBefore) {
		t.Fatalf("dry-run recorded a run")
	}
}

// panicky is a provider whose GetRepo panics.
type panicky struct{ *fake.Provider }

func (panicky) GetRepo(context.Context, string, string) (*forge.Repo, error) { panic("kaboom") }

func TestRunConcurrentOrderAndPanicIsolation(t *testing.T) {
	h := newHarness(t)
	h.eng.Concurrency = 4
	h.eng.Providers["boom"] = panicky{h.src}
	var pairs []resolve.Pair
	for i := 0; i < 6; i++ {
		s := h.addSource(forge.Repo{Name: fmt.Sprintf("p%d", i)})
		s.commit("a.txt")
		s.push("main")
		p := h.pair(s, settings("git", "settings"))
		if i == 3 {
			p.SrcForge = "boom"
		}
		pairs = append(pairs, p)
	}
	reps := h.eng.Run(context.Background(), pairs, Options{Trigger: "schedule"})
	if len(reps) != len(pairs) {
		t.Fatalf("%d reports for %d pairs", len(reps), len(pairs))
	}
	for i, r := range reps {
		if r.Key != pairs[i].Key() {
			t.Fatalf("report %d key = %v, want %v", i, r.Key, pairs[i].Key())
		}
		if i == 3 {
			wantStatus(t, r, state.RunFailed)
			if r.Err == nil || !strings.Contains(r.Err.Error(), "kaboom") {
				t.Fatalf("panic err = %v", r.Err)
			}
			continue
		}
		wantStatus(t, r, state.RunOK)
	}
	runs, _ := h.store.RecentRuns(context.Background(), 0)
	if len(runs) != 6 {
		t.Fatalf("runs = %d, want 6", len(runs))
	}
	for _, r := range runs {
		if r.Status == state.RunRunning {
			t.Fatalf("run %d left running", r.ID)
		}
		if r.Trigger != "schedule" {
			t.Fatalf("trigger = %q", r.Trigger)
		}
	}
}

func TestRunSamePairSerialized(t *testing.T) {
	h := newHarness(t)
	h.eng.Concurrency = 4
	s := h.addSource(forge.Repo{Name: "proj"})
	s.commit("a.txt")
	s.push("main")
	p := h.pair(s, settings("git", "settings"))
	reps := h.eng.Run(context.Background(), []resolve.Pair{p, p, p}, Options{})
	for _, r := range reps {
		wantStatus(t, r, state.RunOK)
	}
	n := 0
	for _, c := range h.dst.Calls() {
		if c == "CreateRepo" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("CreateRepo called %d times, want 1: %v", n, h.dst.Calls())
	}
}

func TestRunCancelledContext(t *testing.T) {
	h := newHarness(t)
	s := h.addSource(forge.Repo{Name: "proj"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reps := h.eng.Run(ctx, []resolve.Pair{h.pair(s, settings()), h.pair(s, settings())}, Options{})
	for _, r := range reps {
		wantStatus(t, r, state.RunAborted)
		if !errors.Is(r.Err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", r.Err)
		}
	}
	if w := writeCalls(h.dst.Calls()); len(w) != 0 {
		t.Fatalf("writes after cancel: %v", w)
	}
}

func TestMissingProviderFails(t *testing.T) {
	h := newHarness(t)
	s := h.addSource(forge.Repo{Name: "proj"})
	p := h.pair(s, settings())
	p.DstForge = "nope"
	r := h.sync(p, Options{})
	wantStatus(t, r, state.RunFailed)
	wantStep(t, r, "prepare", state.StepFailed)
	runs, _ := h.store.Runs(context.Background(), r.Key, 0)
	if len(runs) != 1 || runs[0].Status != state.RunFailed {
		t.Fatalf("runs = %+v", runs)
	}
}

func TestSourceNotFoundFails(t *testing.T) {
	h := newHarness(t)
	p := resolve.Pair{Rule: "r1", Settings: settings(), SrcForge: "src", Src: forge.Repo{Owner: "alice", Name: "ghost"},
		DstForge: "dst", DstOwner: "mirrors", DstName: "ghost"}
	r := h.sync(p, Options{})
	wantStatus(t, r, state.RunFailed)
	if r.Err == nil || !strings.Contains(r.Err.Error(), "source repository not found") {
		t.Fatalf("err = %v", r.Err)
	}
	if w := writeCalls(h.dst.Calls()); len(w) != 0 {
		t.Fatalf("dst writes: %v", w)
	}
}
