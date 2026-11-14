package engine

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gitmirror/internal/forge"
	"gitmirror/internal/state"
)

// tagSource seeds a source with main plus n tags, mirrors it once and returns
// the tag names.
func tagSource(t *testing.T, h *harness, s *srcRepo, n int) []string {
	t.Helper()
	s.commit("a.txt")
	var tags []string
	for i := range n {
		tag := fmt.Sprintf("t%02d", i)
		s.git("tag", tag)
		tags = append(tags, tag)
	}
	s.push(append([]string{"main"}, tags...)...)
	wantStatus(t, h.sync(h.pair(s, settings("git")), Options{}), state.RunOK)
	return tags
}

func deleteAtSource(s *srcRepo, tags ...string) {
	s.git(append([]string{"push", "-q", "origin", "--delete"}, tags...)...)
}

func TestMassRefDeletionAbortsRun(t *testing.T) {
	h := newHarness(t)
	s := h.addSource(forge.Repo{Name: "proj"})
	tags := tagSource(t, h, s, 10)
	p := h.pair(s, settings("git", "settings", "releases"))
	before := h.dstRefs("mirrors", "proj")
	psBefore, _, _ := h.store.Pair(context.Background(), p.Key())

	deleteAtSource(s, tags[:8]...)
	for _, dry := range []bool{true, false} {
		mark := len(h.dst.Calls())
		r := h.sync(p, Options{DryRun: dry})
		wantStatus(t, r, state.RunAborted)
		if r.Err == nil || !strings.Contains(r.Err.Error(), "would delete 8 of 11 destination refs") {
			t.Fatalf("dry=%v: err = %v", dry, r.Err)
		}
		g := wantStep(t, r, StepGit, state.StepFailed)
		if !strings.Contains(g.Detail, "--allow-prune") {
			t.Fatalf("dry=%v: git step detail %q does not mention --allow-prune", dry, g.Detail)
		}
		wantStep(t, r, StepSettings, state.StepSkipped)
		equalRefs(t, "dst refs after guard", h.dstRefs("mirrors", "proj"), before)
		if slices.Contains(h.dst.Calls()[mark:], "ListReleases") {
			t.Fatalf("dry=%v: releases step ran after the deletion guard", dry)
		}
	}
	ps, _, _ := h.store.Pair(context.Background(), p.Key())
	if ps.LastDigest != psBefore.LastDigest || ps.LastRefCount != 11 {
		t.Fatalf("pair state changed by an aborted run: %+v", ps)
	}

	// --allow-prune lets the run through and says so in the step detail.
	r := h.sync(p, Options{AllowPrune: true})
	wantStatus(t, r, state.RunOK)
	g := wantStep(t, r, StepGit, state.StepOK)
	if !strings.Contains(g.Detail, "8 deleted") || !strings.Contains(g.Detail, "deletion guard bypassed (--allow-prune)") {
		t.Fatalf("git detail = %q", g.Detail)
	}
	equalRefs(t, "dst refs", h.dstRefs("mirrors", "proj"), h.refs(s.bare))
}

func TestRefDeletionsWithinLimitsProceed(t *testing.T) {
	cases := []struct {
		name           string
		del            int // of 10 tags (+ main = 11 refs)
		count, percent int
	}{
		{"few refs (5 of 11, count limit 5)", 5, 5, 50},
		{"low percent (6 of 11 = 54%, percent limit 60)", 6, 5, 60},
		{"zero settings mean defaults", 4, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			s := h.addSource(forge.Repo{Name: "proj"})
			tags := tagSource(t, h, s, 10)
			deleteAtSource(s, tags[:tc.del]...)
			rs := settings("git")
			rs.MaxRefDeletions, rs.MaxRefDeletionsPercent = tc.count, tc.percent
			r := h.sync(h.pair(s, rs), Options{})
			wantStatus(t, r, state.RunOK)
			if g := wantStep(t, r, StepGit, state.StepOK); !strings.Contains(g.Detail, fmt.Sprintf("%d deleted", tc.del)) || strings.Contains(g.Detail, "bypassed") {
				t.Fatalf("git detail = %q", g.Detail)
			}
			equalRefs(t, "dst refs", h.dstRefs("mirrors", "proj"), h.refs(s.bare))
		})
	}
}

func TestRefDeletionGuardCountZeroAndPercent(t *testing.T) {
	h := newHarness(t)
	s := h.addSource(forge.Repo{Name: "proj"})
	tags := tagSource(t, h, s, 3)
	deleteAtSource(s, tags[:2]...) // 2 of 4 = 50%: not more than 50
	rs := settings("git")
	rs.MaxRefDeletions, rs.MaxRefDeletionsPercent = 0, 50
	wantStatus(t, h.sync(h.pair(s, rs), Options{}), state.RunOK)

	deleteAtSource(s, tags[2]) // 1 of 2 = 50%: allowed; percent 49 trips it
	rs.MaxRefDeletionsPercent = 49
	r := h.sync(h.pair(s, rs), Options{})
	wantStatus(t, r, state.RunAborted)
	if !strings.Contains(r.Err.Error(), "would delete 1 of 2 destination refs") {
		t.Fatalf("err = %v", r.Err)
	}
}

func requireLFS(t *testing.T) {
	t.Helper()
	if err := exec.Command("git", "lfs", "version").Run(); err != nil {
		t.Skipf("git-lfs not installed: %v", err)
	}
}

func lfsObjectCount(t *testing.T, bare string) int {
	t.Helper()
	n := 0
	_ = filepath.WalkDir(filepath.Join(bare, "lfs", "objects"), func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && len(d.Name()) == 64 {
			n++
		}
		return nil
	})
	return n
}

// TestLFSHealsAfterGitOnlyRuns: refs were mirrored while lfs was not
// included; enabling lfs later copies the objects although no ref changed.
func TestLFSHealsAfterGitOnlyRuns(t *testing.T) {
	requireLFS(t)
	h := newHarness(t)
	s := h.addSource(forge.Repo{Name: "proj"})
	s.git("lfs", "install", "--local")
	s.git("lfs", "track", "*.bin")
	b := make([]byte, 2048)
	rand.Read(b)
	if err := os.WriteFile(filepath.Join(s.work, "a.bin"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	s.git("add", ".gitattributes", "a.bin")
	s.git("commit", "-q", "-m", "lfs")
	s.push("main")
	if n := lfsObjectCount(t, s.bare); n != 1 {
		t.Fatalf("fixture: source has %d LFS objects", n)
	}
	dstBare := bareDir(h.dstBase, "mirrors", "proj", false)

	wantStatus(t, h.sync(h.pair(s, settings("git")), Options{}), state.RunOK)
	if n := lfsObjectCount(t, dstBare); n != 0 {
		t.Fatalf("fixture: dst has %d LFS objects after a git-only run", n)
	}

	p := h.pair(s, settings("git", "lfs"))
	dry := h.sync(p, Options{DryRun: true})
	wantStatus(t, dry, state.RunOK)
	wantAction(t, dry, "transfer LFS objects")
	if n := lfsObjectCount(t, dstBare); n != 0 {
		t.Fatal("dry run transferred LFS objects")
	}

	r := h.sync(p, Options{})
	wantStatus(t, r, state.RunOK)
	wantStep(t, r, StepGit, state.StepUnchanged)
	wantStep(t, r, StepLFS, state.StepOK)
	if n := lfsObjectCount(t, dstBare); n != 1 {
		t.Fatalf("dst has %d LFS objects, want 1 (healed)", n)
	}
	ps, _, _ := h.store.Pair(context.Background(), r.Key)
	if ps.LFSDigest == "" || ps.LFSDigest != ps.LastDigest {
		t.Fatalf("LFSDigest = %q, LastDigest = %q", ps.LFSDigest, ps.LastDigest)
	}

	r = h.sync(p, Options{})
	wantStatus(t, r, state.RunOK)
	wantStep(t, r, StepLFS, state.StepUnchanged)
}
