package engine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitmirror/internal/forge"
	"gitmirror/internal/forge/fake"
	"gitmirror/internal/state"
)

// useDst replaces the harness destination with a fake built from opts.
func (h *harness) useDst(opts fake.Options) {
	h.t.Helper()
	opts.Type, opts.GitBaseDir, opts.Username, opts.Token = "fakedst", h.dstBase, "u", "dsttoken-5678"
	h.dst = fake.New(opts)
	h.eng.Providers["dst"] = h.dst
}

func countCall(calls []string, name string) int {
	n := 0
	for _, c := range calls {
		if c == name {
			n++
		}
	}
	return n
}

// ---- default branch after the first push ----

// TestDefaultBranchPushedFirstIntoNewDestination: a forge that makes the
// first pushed branch the default (Gitea) must end up with the source's
// default even when settings are not mirrored, because the engine pushes
// that branch first into an empty destination.
func TestDefaultBranchPushedFirstIntoNewDestination(t *testing.T) {
	h := newHarness(t)
	h.useDst(fake.Options{DefaultFromFirstPush: true, ProtectDefaultBranch: true})
	s := h.addSource(forge.Repo{Name: "proj"}) // default main
	s.commit("a.txt")
	s.git("branch", "feature") // sorts before main
	s.push("main", "feature")

	p := h.pair(s, settings("git"))
	r := h.sync(p, Options{})
	wantStatus(t, r, state.RunOK)
	if got := h.dstRepo("mirrors", "proj").DefaultBranch; got != "main" {
		t.Fatalf("destination default branch = %q, want main (the source's)", got)
	}
	equalRefs(t, "dst refs", h.dstRefs("mirrors", "proj"), h.refs(s.bare))

	// Deleting the other branch at the source now propagates.
	s.push(":feature")
	r = h.sync(p, Options{})
	wantStatus(t, r, state.RunOK)
	equalRefs(t, "dst refs after delete", h.dstRefs("mirrors", "proj"), h.refs(s.bare))
}

// TestSettingsHealsDefaultBranchAfterRejectedPush: a mirror whose
// destination default branch is wrong cannot delete that branch (the forge
// refuses), so the push is rejected; the settings step must still correct
// the default branch (and nothing else), so the next run recovers.
func TestSettingsHealsDefaultBranchAfterRejectedPush(t *testing.T) {
	h := newHarness(t)
	h.useDst(fake.Options{ProtectDefaultBranch: true})
	s := h.addSource(forge.Repo{Name: "proj", Description: "old"})
	s.commit("a.txt")
	s.git("branch", "feature")
	s.push("main", "feature")
	p := h.pair(s, settings())
	wantStatus(t, h.sync(p, Options{}), state.RunOK)

	// Break the mirror the way Gitea did: default branch "feature".
	ctx := context.Background()
	br := "feature"
	if err := h.dst.UpdateRepoSettings(ctx, "mirrors", "proj", forge.Settings{DefaultBranch: &br}); err != nil {
		t.Fatal(err)
	}
	s.push(":feature")
	if err := h.src.AddRepo(forge.Repo{Owner: "alice", Name: "proj", DefaultBranch: "main", Description: "new"}); err != nil {
		t.Fatal(err)
	}

	r := h.sync(p, Options{})
	wantStatus(t, r, state.RunFailed)
	g := wantStep(t, r, "git", state.StepFailed)
	if !strings.Contains(g.Detail, "refusing to delete the default branch") {
		t.Fatalf("git detail lacks the remote's reason: %q", g.Detail)
	}
	if d := wantStep(t, r, "settings", state.StepOK).Detail; !strings.Contains(d, "default branch") {
		t.Fatalf("settings detail = %q", d)
	}
	wantStep(t, r, "releases", state.StepSkipped)
	wantStep(t, r, "wiki", state.StepSkipped)
	d := h.dstRepo("mirrors", "proj")
	if d.DefaultBranch != "main" {
		t.Fatalf("default branch not healed: %q", d.DefaultBranch)
	}
	if d.Description != "old" {
		t.Fatalf("other settings applied after a failed git step: description %q", d.Description)
	}

	r = h.sync(p, Options{})
	wantStatus(t, r, state.RunOK)
	equalRefs(t, "dst refs after heal", h.dstRefs("mirrors", "proj"), h.refs(s.bare))
	if d := h.dstRepo("mirrors", "proj"); d.Description != "new" {
		t.Fatalf("description after heal = %q", d.Description)
	}
}

// TestSettingsSkippedAfterRejectedPushWhenDefaultMatches: nothing to heal.
func TestSettingsSkippedAfterRejectedPushWhenDefaultMatches(t *testing.T) {
	h := newHarness(t)
	s := h.addSource(forge.Repo{Name: "proj"})
	s.commit("a.txt")
	s.commit("b.txt")
	s.push("main")
	p := h.pair(s, settings())
	wantStatus(t, h.sync(p, Options{}), state.RunOK)
	h.tgit(h.dstBase, "--git-dir="+bareDir(h.dstBase, "mirrors", "proj", false), "config", "receive.denyNonFastForwards", "true")
	s.git("reset", "-q", "--hard", "HEAD~1")
	s.commit("c.txt")
	s.git("push", "-q", "-f", "origin", "main")
	mark := len(h.dst.Calls())
	r := h.sync(p, Options{})
	wantStatus(t, r, state.RunFailed)
	wantStep(t, r, "settings", state.StepSkipped)
	if n := countCall(h.dst.Calls()[mark:], "UpdateRepoSettings"); n != 0 {
		t.Fatalf("UpdateRepoSettings called %d times with nothing to heal", n)
	}
}

// ---- release asset identity ----

func replaceSrcAsset(t *testing.T, h *harness, rel *forge.Release, name string, data []byte) {
	t.Helper()
	ctx := context.Background()
	cur := releaseByTag(t, h.src, "alice", "proj", rel.Tag)
	if err := h.src.DeleteAsset(ctx, "alice", "proj", rel.ID, assetByName(cur, name).ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.src.UploadAsset(ctx, "alice", "proj", rel.ID, forge.Asset{Name: name}, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseAssetSameSizeReplacementRecopied(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.addSource(forge.Repo{Name: "proj"})
	s.commit("a.txt")
	s.push("main")
	rel := h.seedRelease(h.src, "alice", "proj", forge.Release{Tag: "v1"}, map[string][]byte{"a.bin": blob(100, 1), "b.bin": blob(10, 2)})
	p := h.pair(s, settings())
	r := h.sync(p, Options{})
	wantStatus(t, r, state.RunOK)
	d := releaseByTag(t, h.dst, "mirrors", "proj", "v1")
	srcRel := releaseByTag(t, h.src, "alice", "proj", "v1")
	m, err := h.store.AssetMap(ctx, r.Key)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a.bin", "b.bin"} {
		if m[assetByName(d, n).ID] != assetByName(srcRel, n).ID {
			t.Fatalf("asset map %v lacks %s", m, n)
		}
	}

	newData := blob(100, 5) // same size, different content
	replaceSrcAsset(t, h, rel, "a.bin", newData)
	r = h.sync(p, Options{})
	wantStatus(t, r, state.RunOK)
	if det := wantStep(t, r, "releases", state.StepOK).Detail; !strings.Contains(det, "1 assets uploaded (1 replaced)") {
		t.Fatalf("releases detail = %q", det)
	}
	d = releaseByTag(t, h.dst, "mirrors", "proj", "v1")
	a := assetByName(d, "a.bin")
	if a == nil || !bytes.Equal(h.dst.AssetBytes("mirrors", "proj", d.ID, a.ID), newData) {
		t.Fatal("same-size replacement not re-copied")
	}
	srcRel = releaseByTag(t, h.src, "alice", "proj", "v1")
	m, _ = h.store.AssetMap(ctx, r.Key)
	if len(m) != 2 || m[a.ID] != assetByName(srcRel, "a.bin").ID {
		t.Fatalf("asset map not updated: %v", m)
	}

	r = h.sync(p, Options{})
	wantStep(t, r, "releases", state.StepUnchanged)
}

// TestReleaseAssetWithoutMappingAdoptedOnce: destination assets with no
// recorded source id (pre-upgrade state, adopted repos) that match by name
// and size are kept and mapped rather than re-uploaded.
func TestReleaseAssetWithoutMappingAdoptedOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.addSource(forge.Repo{Name: "proj"})
	s.commit("a.txt")
	s.push("main")
	rel := h.seedRelease(h.src, "alice", "proj", forge.Release{Tag: "v1"}, map[string][]byte{"a.bin": blob(100, 1)})
	p := h.pair(s, settings())
	r := h.sync(p, Options{})
	wantStatus(t, r, state.RunOK)
	m, _ := h.store.AssetMap(ctx, r.Key)
	for dst := range m {
		if err := h.store.DeleteAssetMapping(ctx, r.Key, dst); err != nil {
			t.Fatal(err)
		}
	}

	// Dry run: nothing planned, nothing recorded.
	r = h.sync(p, Options{DryRun: true})
	if hasAction(r, "asset") {
		t.Fatalf("dry run plans asset work for an unmapped matching asset: %q", r.Actions)
	}
	if m, _ := h.store.AssetMap(ctx, r.Key); len(m) != 0 {
		t.Fatalf("dry run recorded mappings: %v", m)
	}

	mark := len(h.dst.Calls())
	r = h.sync(p, Options{})
	wantStatus(t, r, state.RunOK)
	wantStep(t, r, "releases", state.StepUnchanged)
	calls := h.dst.Calls()[mark:]
	if countCall(calls, "UploadAsset")+countCall(calls, "DeleteAsset") != 0 {
		t.Fatalf("unmapped matching asset re-copied: %v", writeCalls(calls))
	}
	d := releaseByTag(t, h.dst, "mirrors", "proj", "v1")
	srcRel := releaseByTag(t, h.src, "alice", "proj", "v1")
	if m, _ := h.store.AssetMap(ctx, r.Key); m[assetByName(d, "a.bin").ID] != assetByName(srcRel, "a.bin").ID {
		t.Fatalf("mapping not recorded on adoption: %v", m)
	}

	// From now on identity is tracked.
	newData := blob(100, 3)
	replaceSrcAsset(t, h, rel, "a.bin", newData)
	wantStatus(t, h.sync(p, Options{}), state.RunOK)
	d = releaseByTag(t, h.dst, "mirrors", "proj", "v1")
	if a := assetByName(d, "a.bin"); a == nil || !bytes.Equal(h.dst.AssetBytes("mirrors", "proj", d.ID, a.ID), newData) {
		t.Fatal("same-size replacement after adoption not re-copied")
	}
}

// ---- wiki ----

// seedWiki creates the source wiki with one page and returns its bare path.
func (h *harness) seedWiki(t *testing.T) string {
	t.Helper()
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
	return wikiBare
}

// wikiOnly hides every optional interface of a provider except WikiProvider
// (in particular forge.WikiInitializer).
type wikiOnly struct {
	forge.Provider
	forge.WikiProvider
}

func TestWikiEnsureFailureIsPartial(t *testing.T) {
	h := newHarness(t)
	s := h.addSource(forge.Repo{Name: "proj"})
	s.commit("a.txt")
	s.push("main")
	h.seedWiki(t)
	h.dst.FailNext("EnsureWiki", errors.New("boom"))
	r := h.sync(h.pair(s, settings()), Options{})
	wantStatus(t, r, state.RunPartial)
	if d := wantStep(t, r, "wiki", state.StepFailed).Detail; !strings.Contains(d, "initialise destination wiki") || !strings.Contains(d, "boom") {
		t.Fatalf("wiki detail = %q", d)
	}
}

func TestWikiDryRunNeverEnsures(t *testing.T) {
	t.Run("NewDestination", func(t *testing.T) {
		h := newHarness(t)
		s := h.addSource(forge.Repo{Name: "proj"})
		s.commit("a.txt")
		s.push("main")
		h.seedWiki(t)
		r := h.sync(h.pair(s, settings()), Options{DryRun: true})
		wantStatus(t, r, state.RunOK)
		wantAction(t, r, "initialise destination wiki")
		if w := writeCalls(h.dst.Calls()); len(w) != 0 {
			t.Fatalf("dst writes in dry-run: %v", w)
		}
	})
	t.Run("ExistingDestinationWithoutWiki", func(t *testing.T) {
		h := newHarness(t)
		s := h.addSource(forge.Repo{Name: "proj"})
		s.commit("a.txt")
		s.push("main")
		h.seedWiki(t)
		wantStatus(t, h.sync(h.pair(s, settings("git")), Options{}), state.RunOK)
		mark := len(h.dst.Calls())
		r := h.sync(h.pair(s, settings()), Options{DryRun: true})
		wantStatus(t, r, state.RunOK)
		wantAction(t, r, "initialise destination wiki")
		wantStep(t, r, "wiki", state.StepSkipped)
		if w := writeCalls(h.dst.Calls()[mark:]); len(w) != 0 {
			t.Fatalf("dst writes in dry-run: %v", w)
		}
		if _, err := os.Stat(bareDir(h.dstBase, "mirrors", "proj", true)); err == nil {
			t.Fatal("dry run created the destination wiki")
		}
	})
}

func TestWikiFailureHintOnlyForDestinationSide(t *testing.T) {
	const hint = "destination wiki may not exist"
	t.Run("SourceWikiMissing", func(t *testing.T) {
		h := newHarness(t)
		s := h.addSource(forge.Repo{Name: "proj", HasWiki: true}) // no wiki repository
		s.commit("a.txt")
		s.push("main")
		// The forge says there is a wiki but its repository does not exist
		// (GitHub: wiki enabled, no page ever written): skipped, not failed.
		r := h.sync(h.pair(s, settings()), Options{})
		wantStatus(t, r, state.RunOK)
		if d := wantStep(t, r, "wiki", state.StepSkipped).Detail; d != "source wiki does not exist" {
			t.Fatalf("wiki detail = %q", d)
		}
	})
	t.Run("DestinationWikiMissing", func(t *testing.T) {
		h := newHarness(t)
		h.dst.AddOwner("mirrors", true)
		h.eng.Providers["dst"] = wikiOnly{h.dst, h.dst}
		s := h.addSource(forge.Repo{Name: "proj"})
		s.commit("a.txt")
		s.push("main")
		h.seedWiki(t)
		r := h.sync(h.pair(s, settings()), Options{})
		wantStatus(t, r, state.RunPartial)
		if d := wantStep(t, r, "wiki", state.StepFailed).Detail; !strings.Contains(d, hint) {
			t.Fatalf("destination-side failure lacks the hint: %q", d)
		}
	})
}
