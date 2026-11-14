package engine

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gitmirror/internal/config"
	"gitmirror/internal/forge"
	"gitmirror/internal/forge/fake"
	"gitmirror/internal/resolve"
	"gitmirror/internal/state"
)

// Tests for the data-loss and concurrency review fixes (batch 2).

func reconcileSettings(policy string) config.RuleSettings {
	rs := settings("settings")
	rs.OnSourceDeleted = policy
	rs.DeletionGrace = time.Hour
	return rs
}

// ---- finding 1: a stored pair whose destination is live is never acted on ----

func TestReconcileTransferredSourceKeepsLiveDestination(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	clk := h.useClock()
	rs := reconcileSettings("delete")
	old := h.pair(h.addSource(forge.Repo{Name: "x"}), rs)
	wantStatus(t, h.sync(old, Options{}), state.RunOK)

	// The source is transferred alice/x -> bob/x: the old name is gone and
	// the rule now resolves a new pair into the same destination.
	h.src.RemoveRepo("alice", "x")
	live := h.pair(h.addSource(forge.Repo{Owner: "bob", Name: "x"}), rs)
	wantStatus(t, h.sync(live, Options{}), state.RunOK)

	all := map[string]config.RuleSettings{"r1": rs}
	complete := map[string]bool{"r1": true}
	mark := len(h.dst.Calls())
	for i := range 4 {
		reps := h.eng.Reconcile(ctx, []resolve.Pair{live}, complete, nil, all, Options{})
		if len(reps) != 0 {
			t.Fatalf("cycle %d: reports %+v", i, reps)
		}
		clk.Advance(50 * time.Minute)
	}
	if w := writeCalls(h.dst.Calls()[mark:]); len(w) != 0 {
		t.Fatalf("destination writes for a stale pair of a live destination: %v", w)
	}
	if r := h.dstRepo("mirrors", "x"); r.Archived {
		t.Fatal("live destination archived")
	}
	if _, ok := h.missingSince(old.Key()); ok {
		t.Fatal("stale pair has a missing record")
	}
}

func TestReconcileRepointedRuleKeepsLiveDestination(t *testing.T) {
	for _, via := range []string{"current", "dropped"} {
		t.Run(via, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			clk := h.useClock()
			rs := reconcileSettings("archive")
			oldSrc := h.addSource(forge.Repo{Name: "x"})
			old := h.pair(oldSrc, rs)
			old.DstName = "m"
			wantStatus(t, h.sync(old, Options{}), state.RunOK)
			// The rule is re-pointed at alice/y, still into mirrors/m
			// (written with different case), and alice/x is deleted.
			live := h.pair(h.addSource(forge.Repo{Name: "y"}), rs)
			live.DstOwner, live.DstName = "Mirrors", "M"
			wantStatus(t, h.sync(live, Options{}), state.RunOK)
			h.src.RemoveRepo("alice", "x")

			var current, dropped []resolve.Pair
			if via == "current" {
				current = []resolve.Pair{live}
			} else {
				dropped = []resolve.Pair{live}
			}
			all := map[string]config.RuleSettings{"r1": rs}
			for range 4 {
				if reps := h.eng.Reconcile(ctx, current, map[string]bool{"r1": true}, dropped, all, Options{}); len(reps) != 0 {
					t.Fatalf("reports %+v", reps)
				}
				clk.Advance(50 * time.Minute)
			}
			if r := h.dstRepo("mirrors", "m"); r.Archived {
				t.Fatal("live destination archived")
			}
		})
	}
}

// A `sync --rule r1` run resolves only r1. If another rule (r2, not resolved
// this run) now mirrors into the destination that r1's stale pair names, that
// destination must still count as live.
func TestReconcileDestinationLiveInUnresolvedRule(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	clk := h.useClock()
	rs := reconcileSettings("archive")
	oldSrc := h.addSource(forge.Repo{Name: "x"})
	old := h.pair(oldSrc, rs)
	old.DstName = "m"
	wantStatus(t, h.sync(old, Options{}), state.RunOK)
	live := h.pair(h.addSource(forge.Repo{Name: "y"}), rs)
	live.Rule = "r2"
	live.DstName = "m"
	wantStatus(t, h.sync(live, Options{}), state.RunOK)
	h.src.RemoveRepo("alice", "x")

	all := map[string]config.RuleSettings{"r1": rs, "r2": rs}
	for range 4 {
		// Only r1 was resolved (and completely, with no pairs left).
		if reps := h.eng.Reconcile(ctx, nil, map[string]bool{"r1": true}, nil, all, Options{}); len(reps) != 0 {
			t.Fatalf("reports %+v", reps)
		}
		clk.Advance(50 * time.Minute)
	}
	if r := h.dstRepo("mirrors", "m"); r.Archived {
		t.Fatal("destination mirrored by rule r2 was archived by r1's stale pair")
	}
}

func TestReconcileRechecksSourceUnderPairLock(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	clk := h.useClock()
	rs := reconcileSettings("archive")
	s := h.addSource(forge.Repo{Name: "x"})
	p := h.pair(s, rs)
	wantStatus(t, h.sync(p, Options{}), state.RunOK)
	all := map[string]config.RuleSettings{"r1": rs}
	complete := map[string]bool{"r1": true}

	h.src.RemoveRepo("alice", "x")
	h.eng.Reconcile(ctx, nil, complete, nil, all, Options{})
	clk.Advance(61 * time.Minute)
	// The source is back (a webhook run saw it), but the first, unlocked
	// check still answers not found.
	if err := h.src.AddRepo(s.repo); err != nil {
		t.Fatal(err)
	}
	h.src.FailNext("GetRepo", fmt.Errorf("fake: stale answer: %w", forge.ErrNotFound))
	reps := h.eng.Reconcile(ctx, nil, complete, nil, all, Options{})
	if len(reps) != 0 {
		t.Fatalf("reports %+v", reps)
	}
	if r := h.dstRepo("mirrors", "x"); r.Archived {
		t.Fatal("destination archived although the source exists under the lock")
	}
	if _, ok := h.missingSince(p.Key()); ok {
		t.Fatal("missing record kept although the source exists")
	}
}

// ---- finding 2: ownership is bound to the destination's identity ----

func ownership(t *testing.T, h *harness, repo string) state.OwnershipRecord {
	t.Helper()
	rec, ok, err := h.store.OwnershipOf(context.Background(), "dst", repo)
	if err != nil || !ok {
		t.Fatalf("OwnershipOf %s = %v, %v", repo, ok, err)
	}
	return rec
}

func TestOwnershipRecordsIdentity(t *testing.T) {
	h := newHarness(t)
	h.eng.ForgeURLs = map[string]string{"dst": "https://dst.example"}
	p := h.pair(h.addSource(forge.Repo{Name: "x"}), settings("settings"))
	wantStatus(t, h.sync(p, Options{}), state.RunOK)
	rec := ownership(t, h, "mirrors/x")
	if id := h.dstRepo("mirrors", "x").ID; id == 0 || rec.RepoID != id || rec.ForgeURL != "https://dst.example" || rec.How != state.OwnedCreated {
		t.Fatalf("ownership = %+v, destination id %d", rec, id)
	}
}

func TestRecreatedDestinationRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	clk := h.useClock()
	rs := reconcileSettings("delete")
	p := h.pair(h.addSource(forge.Repo{Name: "x", Description: "src"}), rs)
	wantStatus(t, h.sync(p, Options{}), state.RunOK)

	// The user deletes the mirror and re-creates it by hand.
	h.dst.RemoveRepo("mirrors", "x")
	if err := h.dst.AddRepo(forge.Repo{Owner: "mirrors", Name: "x", Description: "mine", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	mark := len(h.dst.Calls())
	r := h.sync(p, Options{})
	wantStatus(t, r, state.RunFailed)
	if d := wantStep(t, r, StepPrepare, state.StepFailed).Detail; !strings.Contains(d, "replaced") || !strings.Contains(d, "adopt: true") {
		t.Fatalf("prepare detail %q", d)
	}
	if w := writeCalls(h.dst.Calls()[mark:]); len(w) != 0 {
		t.Fatalf("writes to a replaced destination: %v", w)
	}

	// on_source_deleted: delete must never delete the user's repository.
	h.src.RemoveRepo("alice", "x")
	all := map[string]config.RuleSettings{"r1": rs}
	h.eng.Reconcile(ctx, nil, map[string]bool{"r1": true}, nil, all, Options{})
	clk.Advance(61 * time.Minute)
	reps := h.eng.Reconcile(ctx, nil, map[string]bool{"r1": true}, nil, all, Options{})
	if len(reps) != 1 || !strings.Contains(reps[0].Steps[0].Detail, "not owned") {
		t.Fatalf("reconcile reports %+v", reps)
	}
	if n := countCalls(h.dst.Calls(), "DeleteRepo"); n != 0 {
		t.Fatalf("DeleteRepo called %d times", n)
	}
	if got := h.dstRepo("mirrors", "x"); got.Description != "mine" || got.Archived {
		t.Fatalf("user's repository changed: %+v", got)
	}
	events, err := h.store.AuditLog(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Action == state.AuditDelete || ev.Action == state.AuditArchive {
			t.Fatalf("destructive audit event recorded: %+v", ev)
		}
	}
}

func TestForgeURLChangeRefused(t *testing.T) {
	h := newHarness(t)
	h.eng.ForgeURLs = map[string]string{"dst": "https://old.example"}
	p := h.pair(h.addSource(forge.Repo{Name: "x"}), settings("settings"))
	wantStatus(t, h.sync(p, Options{}), state.RunOK)
	h.eng.ForgeURLs["dst"] = "https://new.example"
	mark := len(h.dst.Calls())
	r := h.sync(p, Options{})
	wantStatus(t, r, state.RunFailed)
	if d := wantStep(t, r, StepPrepare, state.StepFailed).Detail; !strings.Contains(d, "forge URL") {
		t.Fatalf("prepare detail %q", d)
	}
	if w := writeCalls(h.dst.Calls()[mark:]); len(w) != 0 {
		t.Fatalf("writes after a forge URL change: %v", w)
	}
}

func TestOwnershipIdentityBackfilledOnUpgrade(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.eng.ForgeURLs = map[string]string{"dst": "https://dst.example"}
	if err := h.dst.AddRepo(forge.Repo{Owner: "mirrors", Name: "x", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	// An ownership row written by an older version: no identity.
	if err := h.store.MarkOwned(ctx, "dst", "mirrors/x", state.OwnedCreated); err != nil {
		t.Fatal(err)
	}
	p := h.pair(h.addSource(forge.Repo{Name: "x"}), settings("settings"))
	// A dry run backfills nothing.
	wantStatus(t, h.sync(p, Options{DryRun: true}), state.RunOK)
	if rec := ownership(t, h, "mirrors/x"); rec.RepoID != 0 || rec.ForgeURL != "" {
		t.Fatalf("dry run backfilled %+v", rec)
	}
	wantStatus(t, h.sync(p, Options{}), state.RunOK)
	want := h.dstRepo("mirrors", "x").ID
	if rec := ownership(t, h, "mirrors/x"); rec.RepoID != want || want == 0 || rec.ForgeURL != "https://dst.example" || rec.How != state.OwnedCreated {
		t.Fatalf("after backfill %+v (destination id %d)", rec, want)
	}
	// From now on a replacement is detected.
	h.dst.RemoveRepo("mirrors", "x")
	if err := h.dst.AddRepo(forge.Repo{Owner: "mirrors", Name: "x", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, h.sync(p, Options{}), state.RunFailed)
}

func TestAdoptReplacedDestinationRecordsNewIdentity(t *testing.T) {
	h := newHarness(t)
	rs := settings("settings")
	p := h.pair(h.addSource(forge.Repo{Name: "x"}), rs)
	wantStatus(t, h.sync(p, Options{}), state.RunOK)
	h.dst.RemoveRepo("mirrors", "x")
	if err := h.dst.AddRepo(forge.Repo{Owner: "mirrors", Name: "x", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	p.Settings.Adopt = true
	r := h.sync(p, Options{})
	wantStatus(t, r, state.RunOK)
	if d := wantStep(t, r, StepPrepare, state.StepOK).Detail; !strings.Contains(d, "adopted") {
		t.Fatalf("prepare detail %q", d)
	}
	rec := ownership(t, h, "mirrors/x")
	if rec.How != state.OwnedAdopted || rec.RepoID != h.dstRepo("mirrors", "x").ID {
		t.Fatalf("ownership after adopt %+v", rec)
	}
	p.Settings.Adopt = false
	wantStatus(t, h.sync(p, Options{}), state.RunOK)
}

// ---- finding 3: releases and assets gitmirror did not create ----

func TestUnmappedReleaseOnAdoptedDestinationNotOverwritten(t *testing.T) {
	h := newHarness(t)
	s := h.addSource(forge.Repo{Name: "x"})
	if err := h.dst.AddRepo(forge.Repo{Owner: "mirrors", Name: "x", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	rs := settings("releases")
	rs.Adopt = true
	p := h.pair(s, rs)
	wantStatus(t, h.sync(p, Options{}), state.RunOK) // adopted, no releases yet

	// The user's own release on the destination, and a source release
	// with the same tag; the rule no longer has adopt.
	h.seedRelease(h.dst, "mirrors", "x", forge.Release{Tag: "v1", Name: "mine", Body: "hand made"}, map[string][]byte{"own.bin": blob(10, 1)})
	h.seedRelease(h.src, "alice", "x", forge.Release{Tag: "v1", Name: "upstream", Body: "src"}, map[string][]byte{"a.bin": blob(20, 2)})
	p.Settings.Adopt = false
	mark := len(h.dst.Calls())
	r := h.sync(p, Options{})
	wantStatus(t, r, state.RunOK)
	if d := step(r, StepReleases).Detail; !strings.Contains(d, "destination release v1 exists and was not created by gitmirror") {
		t.Fatalf("releases detail %q", d)
	}
	if w := writeCalls(h.dst.Calls()[mark:]); len(w) != 0 {
		t.Fatalf("writes to an unmanaged release: %v", w)
	}
	got := releaseByTag(t, h.dst, "mirrors", "x", "v1")
	if got.Name != "mine" || got.Body != "hand made" || len(got.Assets) != 1 || got.Assets[0].Name != "own.bin" {
		t.Fatalf("user's release changed: %+v", got)
	}
}

func TestUnmappedReleaseMatchedWithAdoptOrCreated(t *testing.T) {
	t.Run("adopt", func(t *testing.T) {
		h := newHarness(t)
		s := h.addSource(forge.Repo{Name: "x"})
		if err := h.dst.AddRepo(forge.Repo{Owner: "mirrors", Name: "x", DefaultBranch: "main"}); err != nil {
			t.Fatal(err)
		}
		h.seedRelease(h.dst, "mirrors", "x", forge.Release{Tag: "v1", Name: "old"}, nil)
		h.seedRelease(h.src, "alice", "x", forge.Release{Tag: "v1", Name: "upstream"}, nil)
		rs := settings("releases")
		rs.Adopt = true
		wantStatus(t, h.sync(h.pair(s, rs), Options{}), state.RunOK)
		if got := releaseByTag(t, h.dst, "mirrors", "x", "v1"); got.Name != "upstream" {
			t.Fatalf("adopt: release not updated: %+v", got)
		}
	})
	t.Run("created", func(t *testing.T) {
		h := newHarness(t)
		s := h.addSource(forge.Repo{Name: "x"})
		p := h.pair(s, settings("releases"))
		wantStatus(t, h.sync(p, Options{}), state.RunOK) // created by gitmirror
		// A release whose mapping was lost (e.g. created by an earlier run
		// whose state was reset).
		h.seedRelease(h.dst, "mirrors", "x", forge.Release{Tag: "v1", Name: "old"}, nil)
		h.seedRelease(h.src, "alice", "x", forge.Release{Tag: "v1", Name: "upstream"}, nil)
		wantStatus(t, h.sync(p, Options{}), state.RunOK)
		if got := releaseByTag(t, h.dst, "mirrors", "x", "v1"); got.Name != "upstream" {
			t.Fatalf("created: release not updated: %+v", got)
		}
	})
}

func TestUnmappedDestinationAssetNotDeleted(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.addSource(forge.Repo{Name: "x"})
	h.seedRelease(h.src, "alice", "x", forge.Release{Tag: "v1", Name: "v1"},
		map[string][]byte{"a.bin": blob(20, 1), "b.bin": blob(30, 2)})
	p := h.pair(s, settings("releases"))
	wantStatus(t, h.sync(p, Options{}), state.RunOK)
	dr := releaseByTag(t, h.dst, "mirrors", "x", "v1")
	if len(dr.Assets) != 2 {
		t.Fatalf("assets %+v", dr.Assets)
	}
	// The user uploads an extra asset by hand; the source drops b.bin.
	if _, err := h.dst.UploadAsset(ctx, "mirrors", "x", dr.ID, forge.Asset{Name: "hand.bin"}, strings.NewReader("by hand")); err != nil {
		t.Fatal(err)
	}
	srel := releaseByTag(t, h.src, "alice", "x", "v1")
	if err := h.src.DeleteAsset(ctx, "alice", "x", srel.ID, assetByName(srel, "b.bin").ID); err != nil {
		t.Fatal(err)
	}
	r := h.sync(p, Options{})
	wantStatus(t, r, state.RunOK)
	d := step(r, StepReleases).Detail
	if !strings.Contains(d, "1 assets deleted") || !strings.Contains(d, "1 destination assets not uploaded by gitmirror left untouched") {
		t.Fatalf("releases detail %q", d)
	}
	dr = releaseByTag(t, h.dst, "mirrors", "x", "v1")
	if assetByName(dr, "hand.bin") == nil {
		t.Fatalf("hand-uploaded asset deleted: %+v", dr.Assets)
	}
	if assetByName(dr, "b.bin") != nil {
		t.Fatalf("mirrored asset dropped at the source not deleted: %+v", dr.Assets)
	}
}

// ---- finding 4: a stalled asset download times out ----

// stallSrc is a source whose asset downloads deliver no data until the
// download's context is cancelled.
type stallSrc struct {
	*fake.Provider
	downloads atomic.Int32
}

type stalledBody struct{ ctx context.Context }

func (b stalledBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b stalledBody) Close() error { return nil }

func (s *stallSrc) DownloadAsset(ctx context.Context, owner, name string, a forge.Asset) (io.ReadCloser, error) {
	s.downloads.Add(1)
	return stalledBody{ctx: ctx}, nil
}

func TestStalledAssetDownloadTimesOut(t *testing.T) {
	h := newHarness(t)
	s := h.addSource(forge.Repo{Name: "x"})
	h.seedRelease(h.src, "alice", "x", forge.Release{Tag: "v1", Name: "v1"}, map[string][]byte{"a.bin": blob(20, 1)})
	src := &stallSrc{Provider: h.src}
	h.eng.Providers["src"] = src
	h.eng.AssetIdleTimeout = 200 * time.Millisecond
	p := h.pair(s, settings("releases"))
	done := make(chan Report, 1)
	start := time.Now()
	go func() { done <- h.sync(p, Options{}) }()
	var r Report
	select {
	case r = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("sync blocked on a stalled asset download")
	}
	if el := time.Since(start); el > 15*time.Second {
		t.Fatalf("stalled download took %v", el)
	}
	wantStatus(t, r, state.RunPartial)
	if d := wantStep(t, r, StepReleases, state.StepFailed).Detail; !strings.Contains(d, "stalled") {
		t.Fatalf("releases detail %q", d)
	}
	if src.downloads.Load() != 1 {
		t.Fatalf("downloads = %d", src.downloads.Load())
	}
}

// ---- finding 5: deletion grace needs continuous observation ----

func TestDeletionGraceRestartsAfterObservationGap(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	clk := h.useClock()
	rs := reconcileSettings("archive") // interval 1h: max gap 2h
	p := h.pair(h.addSource(forge.Repo{Name: "x"}), rs)
	wantStatus(t, h.sync(p, Options{}), state.RunOK)
	all := map[string]config.RuleSettings{"r1": rs}
	complete := map[string]bool{"r1": true}

	h.src.RemoveRepo("alice", "x")
	h.eng.Reconcile(ctx, nil, complete, nil, all, Options{})
	// Downtime: the next observation comes 5h later. A single not-found
	// after the gap must not apply the policy.
	clk.Advance(5 * time.Hour)
	reps := h.eng.Reconcile(ctx, nil, complete, nil, all, Options{})
	if len(reps) != 1 || !strings.Contains(reps[0].Steps[0].Detail, "within deletion grace") {
		t.Fatalf("after gap: %+v", reps)
	}
	if h.dstRepo("mirrors", "x").Archived {
		t.Fatal("archived after a single observation following a gap")
	}
	if since, ok := h.missingSince(p.Key()); !ok || !since.Equal(clk.Now()) {
		t.Fatalf("missing since %v %v, want restarted at %v", since, ok, clk.Now())
	}
	// A dry run in the same situation also reports the restarted clock.
	clk.Advance(3 * time.Hour)
	reps = h.eng.Reconcile(ctx, nil, complete, nil, all, Options{DryRun: true})
	if len(reps) != 1 || !strings.Contains(reps[0].Steps[0].Detail, "within deletion grace") {
		t.Fatalf("dry run after gap: %+v", reps)
	}
	clk.Advance(-3 * time.Hour)
	// Continuous observation for the grace period applies the policy.
	clk.Advance(40 * time.Minute)
	h.eng.Reconcile(ctx, nil, complete, nil, all, Options{})
	clk.Advance(40 * time.Minute)
	h.eng.Reconcile(ctx, nil, complete, nil, all, Options{})
	if !h.dstRepo("mirrors", "x").Archived {
		t.Fatal("not archived after continuous observation for the grace period")
	}
}

// ---- finding 6: a crash between unarchive and re-archive is repaired ----

// panicReleases panics in the source's ListReleases while armed.
type panicReleases struct {
	*fake.Provider
	armed atomic.Bool
}

func (p *panicReleases) ListReleases(ctx context.Context, owner, name string) ([]forge.Release, error) {
	if p.armed.Load() {
		panic("simulated crash")
	}
	return p.Provider.ListReleases(ctx, owner, name)
}

func TestCrashAfterUnarchiveReArchivedNextRun(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.addSource(forge.Repo{Name: "x"})
	src := &panicReleases{Provider: h.src}
	h.eng.Providers["src"] = src
	p := h.pair(s, settings("releases"))
	wantStatus(t, h.sync(p, Options{}), state.RunOK)
	tr := true
	if err := h.dst.UpdateRepoSettings(ctx, "mirrors", "x", forge.Settings{Archived: &tr}); err != nil {
		t.Fatal(err)
	}

	src.armed.Store(true)
	r := h.sync(p, Options{})
	wantStatus(t, r, state.RunFailed)
	if h.dstRepo("mirrors", "x").Archived {
		t.Fatal("simulation: destination should be left unarchived by the crash")
	}
	if ps, _, _ := h.store.Pair(ctx, p.Key()); !ps.Unarchived {
		t.Fatal("unarchived marker not persisted before unarchiving")
	}

	// A dry run reports the repair but writes nothing.
	src.armed.Store(false)
	mark := len(h.dst.Calls())
	dr := h.sync(p, Options{DryRun: true})
	wantAction(t, dr, "re-archive")
	if w := writeCalls(h.dst.Calls()[mark:]); len(w) != 0 {
		t.Fatalf("dry run writes: %v", w)
	}

	r = h.sync(p, Options{})
	wantStatus(t, r, state.RunOK)
	if !h.dstRepo("mirrors", "x").Archived {
		t.Fatalf("destination not re-archived after the crash; steps %+v", r.Steps)
	}
	if ps, _, _ := h.store.Pair(ctx, p.Key()); ps.Unarchived {
		t.Fatal("marker not cleared after re-archiving")
	}
}

// ---- finding 7: an adopted destination is never made public by default ----

func TestAdoptedPrivateDestinationNotPublished(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(fmt.Sprintf("allow_publish=%v", allow), func(t *testing.T) {
			h := newHarness(t)
			s := h.addSource(forge.Repo{Name: "x", Private: false})
			if err := h.dst.AddRepo(forge.Repo{Owner: "mirrors", Name: "x", Private: true, DefaultBranch: "main"}); err != nil {
				t.Fatal(err)
			}
			rs := settings("settings")
			rs.Visibility = "match-source"
			rs.Adopt = true
			rs.AllowPublish = allow
			r := h.sync(h.pair(s, rs), Options{})
			wantStatus(t, r, state.RunOK)
			got := h.dstRepo("mirrors", "x")
			if got.Private == allow {
				t.Fatalf("private = %v with allow_publish %v; settings %+v", got.Private, allow, step(r, StepSettings))
			}
			if !allow && !strings.Contains(step(r, StepSettings).Detail, "allow_publish") {
				t.Fatalf("settings detail %q", step(r, StepSettings).Detail)
			}
		})
	}
}
