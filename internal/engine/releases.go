package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"gitmirror/internal/forge"
	"gitmirror/internal/state"
)

// releaseTally accumulates the outcome of the releases step.
type releaseTally struct {
	created, updated, deleted, left   int
	uploaded, replaced, assetsDeleted int
	drafts, failures                  int
	assetsLeft                        int // destination assets not uploaded by gitmirror
	notes, errs                       []string
}

func (t *releaseTally) changed() bool {
	return t.created+t.updated+t.deleted+t.uploaded+t.replaced+t.assetsDeleted > 0
}

func (t *releaseTally) fail(format string, args ...any) {
	t.failures++
	t.errs = append(t.errs, fmt.Sprintf(format, args...))
}

func (t *releaseTally) detail() string {
	parts := []string{fmt.Sprintf("%d created, %d updated, %d deleted releases; %d assets uploaded (%d replaced), %d assets deleted",
		t.created, t.updated, t.deleted, t.uploaded, t.replaced, t.assetsDeleted)}
	if t.drafts > 0 {
		parts = append(parts, fmt.Sprintf("%d drafts skipped", t.drafts))
	}
	if t.left > 0 {
		parts = append(parts, fmt.Sprintf("%d destination releases not managed by gitmirror left untouched", t.left))
	}
	if t.assetsLeft > 0 {
		parts = append(parts, fmt.Sprintf("%d destination assets not uploaded by gitmirror left untouched", t.assetsLeft))
	}
	parts = append(parts, t.notes...)
	if t.failures > 0 {
		parts = append(parts, fmt.Sprintf("%d failed: %s", t.failures, strings.Join(t.errs, "; ")))
	}
	return strings.Join(parts, "; ")
}

func (r *pairRun) syncReleases(ctx context.Context) {
	start := time.Now()
	srp, ok := r.srcP.(forge.ReleaseProvider)
	if !ok {
		r.skip(StepReleases, "unsupported by "+r.srcP.Type())
		return
	}
	drp, ok := r.dstP.(forge.ReleaseProvider)
	if !ok {
		r.skip(StepReleases, "unsupported by "+r.dstP.Type())
		return
	}
	srcRels, err := srp.ListReleases(ctx, r.src.Owner, r.src.Name)
	if err != nil {
		r.stepFailed(StepReleases, start, fmt.Errorf("list source releases: %w", err))
		return
	}
	var dstRels []forge.Release
	if r.dstExists {
		if dstRels, err = drp.ListReleases(ctx, r.p.DstOwner, r.p.DstName); err != nil {
			r.stepFailed(StepReleases, start, fmt.Errorf("list destination releases: %w", err))
			return
		}
	}
	relMap, err := r.e.Store.ReleaseMap(ctx, r.key)
	if err != nil {
		r.stepFailed(StepReleases, start, fmt.Errorf("read release map: %w", err))
		return
	}
	assetMap, err := r.e.Store.AssetMap(ctx, r.key) // dst asset id -> src asset id
	if err != nil {
		r.stepFailed(StepReleases, start, fmt.Errorf("read asset map: %w", err))
		return
	}
	ours := make(map[int64]int64, len(relMap)) // dst id -> src id
	for s, d := range relMap {
		ours[d] = s
	}
	byID := map[int64]*forge.Release{}
	byTag := map[string]*forge.Release{}
	for i := range dstRels {
		byID[dstRels[i].ID] = &dstRels[i]
		byTag[dstRels[i].Tag] = &dstRels[i]
	}

	// A destination release found only by its tag (no mapping) is taken over
	// only when gitmirror created the repository (the release comes from an
	// earlier run whose mapping was lost) or the rule opted in with adopt.
	trustTags := r.ownHow == state.OwnedCreated || r.p.Settings.Adopt
	t := &releaseTally{}
	matched := map[int64]bool{}
	// Oldest first, so the newest source release is also newest on the destination.
	for i := len(srcRels) - 1; i >= 0; i-- {
		sr := srcRels[i]
		if sr.Draft {
			t.drafts++
			if id, ok := relMap[sr.ID]; ok {
				matched[id] = true // never delete the mirror of a release that went back to draft
			}
			continue
		}
		var dr *forge.Release
		if id, ok := relMap[sr.ID]; ok {
			dr = byID[id]
		}
		if dr == nil {
			if byTag[sr.Tag] != nil && !trustTags {
				matched[byTag[sr.Tag].ID] = true // reported here, not as "left"
				t.notes = append(t.notes, fmt.Sprintf("destination release %s exists and was not created by gitmirror; skipped", sr.Tag))
				continue
			}
			dr = byTag[sr.Tag]
		}
		if dr != nil {
			matched[dr.ID] = true
			if dr.Name != sr.Name || dr.Body != sr.Body || dr.Prerelease != sr.Prerelease {
				r.action("update release %s", sr.Tag)
				if r.dry() {
					t.updated++
				} else if _, err := drp.UpdateRelease(ctx, r.p.DstOwner, r.p.DstName, forge.Release{
					ID: dr.ID, Tag: dr.Tag, Name: sr.Name, Body: sr.Body, Target: dr.Target, Prerelease: sr.Prerelease,
				}); err != nil {
					t.fail("update release %s: %v", sr.Tag, err)
				} else {
					t.updated++
				}
			}
		} else {
			r.action("create release %s (%d assets)", sr.Tag, len(sr.Assets))
			if r.dry() {
				t.created++
				continue
			}
			nr, err := drp.CreateRelease(ctx, r.p.DstOwner, r.p.DstName, forge.Release{
				Tag: sr.Tag, Name: sr.Name, Body: sr.Body, Target: sr.Target, Prerelease: sr.Prerelease,
			})
			if err != nil {
				t.fail("create release %s: %v", sr.Tag, err)
				continue
			}
			t.created++
			matched[nr.ID] = true
			if err := r.e.Store.SetReleaseMapping(context.WithoutCancel(ctx), r.key, sr.ID, nr.ID); err != nil {
				t.fail("record release mapping for %s: %v", sr.Tag, err)
			}
			dr = nr
		}
		r.syncAssets(ctx, srp, drp, &sr, dr, assetMap, t)
	}

	for i := range dstRels {
		d := &dstRels[i]
		if matched[d.ID] {
			continue
		}
		srcID, mine := ours[d.ID]
		if !r.p.Settings.DeleteMissingReleases || !mine {
			t.left++
			continue
		}
		r.action("delete release %s", d.Tag)
		if r.dry() {
			t.deleted++
			continue
		}
		if err := drp.DeleteRelease(ctx, r.p.DstOwner, r.p.DstName, d.ID); err != nil {
			t.fail("delete release %s: %v", d.Tag, err)
			continue
		}
		t.deleted++
		if err := r.e.Store.DeleteReleaseMapping(context.WithoutCancel(ctx), r.key, srcID); err != nil {
			t.fail("forget release mapping for %s: %v", d.Tag, err)
		}
		for _, a := range d.Assets {
			if err := r.e.Store.DeleteAssetMapping(context.WithoutCancel(ctx), r.key, a.ID); err != nil {
				t.fail("forget asset mapping for %s/%s: %v", d.Tag, a.Name, err)
			}
		}
	}

	switch {
	case t.failures > 0:
		r.stepFailed(StepReleases, start, fmt.Errorf("%s", t.detail()))
	case t.changed():
		r.record(StepReleases, start, state.StepOK, t.detail())
	default:
		r.record(StepReleases, start, state.StepUnchanged, t.detail())
	}
}

// syncAssets makes dr's assets match sr's. Destination assets that are not
// in the source are deleted only when assetMap says gitmirror uploaded them.
// A destination asset is kept only
// when its name and size match and assetMap (destination asset id to source
// asset id, updated in place) says it was copied from the current source
// asset, so a source asset replaced by a same-size file is re-copied. A
// matching destination asset with no recorded origin (adopted repository,
// or state from before asset tracking) is trusted once and recorded.
func (r *pairRun) syncAssets(ctx context.Context, srp, drp forge.ReleaseProvider, sr, dr *forge.Release, assetMap map[int64]int64, t *releaseTally) {
	wctx := context.WithoutCancel(ctx)
	max := r.e.maxAssetSize()
	have := map[string]forge.Asset{}
	for _, a := range dr.Assets {
		have[a.Name] = a
	}
	want := map[string]bool{}
	for _, a := range sr.Assets {
		want[a.Name] = true
		if a.Size > max {
			t.notes = append(t.notes, fmt.Sprintf("asset %s/%s (%d bytes) exceeds the %d byte limit; skipped", sr.Tag, a.Name, a.Size, max))
			continue
		}
		cur, exists := have[a.Name]
		if exists && cur.Size == a.Size {
			srcID, known := assetMap[cur.ID]
			if known && srcID == a.ID {
				continue
			}
			if !known {
				if !r.dry() {
					if err := r.e.Store.SetAssetMapping(wctx, r.key, cur.ID, a.ID); err != nil {
						t.fail("record asset mapping for %s/%s: %v", sr.Tag, a.Name, err)
					} else {
						assetMap[cur.ID] = a.ID
					}
				}
				continue
			}
		}
		if exists {
			r.action("replace asset %s/%s", sr.Tag, a.Name)
			if !r.dry() {
				if err := drp.DeleteAsset(ctx, r.p.DstOwner, r.p.DstName, dr.ID, cur.ID); err != nil {
					t.fail("delete outdated asset %s/%s: %v", sr.Tag, a.Name, err)
					continue
				}
				if err := r.e.Store.DeleteAssetMapping(wctx, r.key, cur.ID); err != nil {
					t.fail("forget asset mapping for %s/%s: %v", sr.Tag, a.Name, err)
				}
				delete(assetMap, cur.ID)
			}
			t.replaced++
		} else {
			r.action("upload asset %s/%s", sr.Tag, a.Name)
		}
		if r.dry() {
			t.uploaded++
			continue
		}
		na, err := r.copyAsset(ctx, srp, drp, dr.ID, a)
		if err != nil {
			t.fail("upload asset %s/%s: %v", sr.Tag, a.Name, err)
			continue
		}
		t.uploaded++
		if na != nil {
			if err := r.e.Store.SetAssetMapping(wctx, r.key, na.ID, a.ID); err != nil {
				t.fail("record asset mapping for %s/%s: %v", sr.Tag, a.Name, err)
			} else {
				assetMap[na.ID] = a.ID
			}
		}
	}
	for _, a := range dr.Assets {
		if want[a.Name] {
			continue
		}
		if _, mine := assetMap[a.ID]; !mine {
			// Not uploaded by gitmirror (e.g. attached by hand): leave it.
			t.assetsLeft++
			continue
		}
		r.action("delete asset %s/%s", dr.Tag, a.Name)
		if !r.dry() {
			if err := drp.DeleteAsset(ctx, r.p.DstOwner, r.p.DstName, dr.ID, a.ID); err != nil {
				t.fail("delete asset %s/%s: %v", dr.Tag, a.Name, err)
				continue
			}
			if err := r.e.Store.DeleteAssetMapping(wctx, r.key, a.ID); err != nil {
				t.fail("forget asset mapping for %s/%s: %v", dr.Tag, a.Name, err)
			}
			delete(assetMap, a.ID)
		}
		t.assetsDeleted++
	}
}

// minAssetTimeout is the smallest total time limit of one asset copy; assets
// over 600 MiB get one second per MiB instead (see assetTimeout).
const minAssetTimeout = 10 * time.Minute

// assetTimeout is the total time limit for copying an asset of size bytes:
// max(minAssetTimeout, size at 1 MiB/s).
func assetTimeout(size int64) time.Duration {
	return max(minAssetTimeout, time.Duration(size>>20)*time.Second)
}

// errAssetStalled is the cancellation cause of an asset copy whose source
// download delivered no data for the idle timeout.
var errAssetStalled = errors.New("asset download stalled")

// idleWatch cancels an asset copy when its download delivers no data for
// the idle timeout. Every Read that returns data re-arms it.
type idleWatch struct {
	timer *time.Timer
	idle  time.Duration

	mu   sync.Mutex
	body io.ReadCloser
}

func (w *idleWatch) Read(p []byte) (int, error) {
	w.mu.Lock()
	body := w.body
	w.mu.Unlock()
	n, err := body.Read(p)
	if n > 0 {
		w.timer.Reset(w.idle)
	}
	return n, err
}

// copyAsset streams one asset from the source straight into the destination
// without buffering it in memory, returning the new destination asset. The
// copy runs under its own context: it is cancelled (aborting both the
// download and the upload) when the download delivers no data for the
// engine's AssetIdleTimeout, or when the copy exceeds assetTimeout.
func (r *pairRun) copyAsset(ctx context.Context, srp, drp forge.ReleaseProvider, dstReleaseID int64, a forge.Asset) (*forge.Asset, error) {
	idle := r.e.AssetIdleTimeout
	if idle <= 0 {
		idle = DefaultAssetIdleTimeout
	}
	total := assetTimeout(a.Size)
	cctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	actx, stop := context.WithTimeout(cctx, total)
	defer stop()
	w := &idleWatch{idle: idle}
	w.timer = time.AfterFunc(idle, func() {
		cancel(errAssetStalled)
		w.mu.Lock()
		body := w.body
		w.mu.Unlock()
		if body != nil {
			body.Close() // unblocks a Read that ignores the context
		}
	})
	defer w.timer.Stop()
	wrap := func(err error) error {
		switch {
		case errors.Is(context.Cause(cctx), errAssetStalled):
			return fmt.Errorf("%w: no data from the source for %s: %v", errAssetStalled, idle, err)
		case ctx.Err() == nil && errors.Is(actx.Err(), context.DeadlineExceeded):
			return fmt.Errorf("asset copy exceeded its %s time limit: %v", total, err)
		}
		return err
	}
	rc, err := srp.DownloadAsset(actx, r.src.Owner, r.src.Name, a)
	if err != nil {
		return nil, wrap(fmt.Errorf("download: %w", err))
	}
	w.mu.Lock()
	w.body = rc
	w.mu.Unlock()
	defer rc.Close()
	na, err := drp.UploadAsset(actx, r.p.DstOwner, r.p.DstName, dstReleaseID,
		forge.Asset{Name: a.Name, Size: a.Size, ContentType: a.ContentType}, w)
	if err != nil {
		return nil, wrap(err)
	}
	return na, nil
}
