package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gitmirror/internal/forge"
	"gitmirror/internal/state"
)

// releaseTally accumulates the outcome of the releases step.
type releaseTally struct {
	created, updated, deleted, left   int
	uploaded, replaced, assetsDeleted int
	drafts, failures                  int
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
		r.syncAssets(ctx, srp, drp, &sr, dr, t)
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

// syncAssets makes dr's assets match sr's.
func (r *pairRun) syncAssets(ctx context.Context, srp, drp forge.ReleaseProvider, sr, dr *forge.Release, t *releaseTally) {
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
			continue
		}
		if exists {
			r.action("replace asset %s/%s", sr.Tag, a.Name)
			if !r.dry() {
				if err := drp.DeleteAsset(ctx, r.p.DstOwner, r.p.DstName, dr.ID, cur.ID); err != nil {
					t.fail("delete outdated asset %s/%s: %v", sr.Tag, a.Name, err)
					continue
				}
			}
			t.replaced++
		} else {
			r.action("upload asset %s/%s", sr.Tag, a.Name)
		}
		if r.dry() {
			t.uploaded++
			continue
		}
		if err := r.copyAsset(ctx, srp, drp, dr.ID, a); err != nil {
			t.fail("upload asset %s/%s: %v", sr.Tag, a.Name, err)
			continue
		}
		t.uploaded++
	}
	for _, a := range dr.Assets {
		if want[a.Name] {
			continue
		}
		r.action("delete asset %s/%s", dr.Tag, a.Name)
		if !r.dry() {
			if err := drp.DeleteAsset(ctx, r.p.DstOwner, r.p.DstName, dr.ID, a.ID); err != nil {
				t.fail("delete asset %s/%s: %v", dr.Tag, a.Name, err)
				continue
			}
		}
		t.assetsDeleted++
	}
}

// copyAsset streams one asset from the source straight into the destination
// without buffering it in memory.
func (r *pairRun) copyAsset(ctx context.Context, srp, drp forge.ReleaseProvider, dstReleaseID int64, a forge.Asset) error {
	rc, err := srp.DownloadAsset(ctx, r.src.Owner, r.src.Name, a)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer rc.Close()
	_, err = drp.UploadAsset(ctx, r.p.DstOwner, r.p.DstName, dstReleaseID,
		forge.Asset{Name: a.Name, Size: a.Size, ContentType: a.ContentType}, rc)
	return err
}
