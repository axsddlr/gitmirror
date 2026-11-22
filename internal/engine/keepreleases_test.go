package engine

import (
	"context"
	"slices"
	"testing"

	"gitmirror/internal/forge"
	"gitmirror/internal/state"
)

func dstTags(t *testing.T, h *harness) []string {
	t.Helper()
	rels, err := h.dst.ListReleases(context.Background(), "mirrors", "proj")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rels {
		out = append(out, r.Tag)
	}
	slices.Sort(out)
	return out
}

// KeepReleases copies only the newest N source releases and deletes older
// releases gitmirror copied earlier; releases it did not make stay.
func TestKeepReleases(t *testing.T) {
	h := newHarness(t)
	s := h.addSource(forge.Repo{Name: "proj"})
	s.commit("a.txt")
	s.push("main")
	for _, tag := range []string{"v1", "v2", "v3", "v4"} { // v4 is the newest
		h.seedRelease(h.src, "alice", "proj", forge.Release{Tag: tag, Name: tag}, nil)
	}
	p := h.pair(s, settings("git", "releases"))

	// No limit: everything is copied.
	wantStatus(t, h.sync(p, Options{}), state.RunOK)
	if got := dstTags(t, h); !slices.Equal(got, []string{"v1", "v2", "v3", "v4"}) {
		t.Fatalf("without a limit dst = %v", got)
	}
	h.seedRelease(h.dst, "mirrors", "proj", forge.Release{Tag: "manual", Name: "hand made"}, nil)

	// Keep 2: a dry run plans the deletions and changes nothing.
	p.Settings.KeepReleases = 2
	r := h.sync(p, Options{DryRun: true})
	wantStatus(t, r, state.RunOK)
	wantAction(t, r, "delete release v1")
	wantAction(t, r, "delete release v2")
	if got := dstTags(t, h); len(got) != 5 {
		t.Fatalf("dry run changed dst: %v", got)
	}

	r = h.sync(p, Options{})
	wantStatus(t, r, state.RunOK)
	if got := dstTags(t, h); !slices.Equal(got, []string{"manual", "v3", "v4"}) {
		t.Fatalf("keep 2: dst = %v", got)
	}
	if m, _ := h.store.ReleaseMap(context.Background(), r.Key); len(m) != 2 {
		t.Fatalf("release map = %v, want the 2 kept releases", m)
	}

	// A new source release pushes the oldest kept one out.
	h.seedRelease(h.src, "alice", "proj", forge.Release{Tag: "v5", Name: "v5"}, nil)
	wantStatus(t, h.sync(p, Options{}), state.RunOK)
	if got := dstTags(t, h); !slices.Equal(got, []string{"manual", "v4", "v5"}) {
		t.Fatalf("after v5: dst = %v", got)
	}
}

// With a limit on a fresh destination, older releases are never created.
func TestKeepReleasesFreshDestination(t *testing.T) {
	h := newHarness(t)
	s := h.addSource(forge.Repo{Name: "proj"})
	s.commit("a.txt")
	s.push("main")
	for _, tag := range []string{"v1", "v2", "v3"} {
		h.seedRelease(h.src, "alice", "proj", forge.Release{Tag: tag, Name: tag}, nil)
	}
	h.seedRelease(h.src, "alice", "proj", forge.Release{Tag: "v4-draft", Name: "draft", Draft: true}, nil)
	p := h.pair(s, settings("git", "releases"))
	p.Settings.KeepReleases = 1
	wantStatus(t, h.sync(p, Options{}), state.RunOK)
	if got := dstTags(t, h); !slices.Equal(got, []string{"v3"}) {
		t.Fatalf("keep 1: dst = %v (a draft does not count)", got)
	}
	if n := countCalls(h.dst.Calls(), "CreateRelease"); n != 1 {
		t.Fatalf("CreateRelease called %d times, want 1", n)
	}
}
