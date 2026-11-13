package state

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

var ctx = context.Background()

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "sub", "dir", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func key(n string) PairKey {
	return PairKey{Rule: "r", SrcForge: "gh", SrcRepo: "o/" + n, DstForge: "cb", DstRepo: "o/" + n}
}

// fixedClock makes the store clock advance 1s per call from a base time.
func fixedClock(s *Store) {
	var mu sync.Mutex
	cur := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
	s.SetClock(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		cur = cur.Add(time.Second)
		return cur
	})
}

func TestOpenCreatesAndReopens(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a", "b", "s.db")
	s, err := Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkOwned(ctx, "f", "o/r", OwnedCreated); err != nil {
		t.Fatal(err)
	}
	var n1 int
	s.db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&n1)
	s.Close()
	s, err = Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n2 int
	s.db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&n2)
	if n1 == 0 || n1 != n2 {
		t.Fatalf("migrations count %d -> %d", n1, n2)
	}
	if _, ok, _ := s.Owned(ctx, "f", "o/r"); !ok {
		t.Fatal("data lost on reopen")
	}
}

func TestNewerSchemaRefused(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.db")
	s, err := Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES (9999, 0)`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	_, err = Open(ctx, p)
	if err == nil || !strings.Contains(err.Error(), "database schema is newer than this binary") {
		t.Fatalf("got %v", err)
	}
}

func TestPairKeyNoCollisions(t *testing.T) {
	keys := []PairKey{
		{Rule: "a/b", SrcForge: "c"},
		{Rule: "a", SrcForge: "b/c"},
		{Rule: "a:b", SrcForge: "c"},
		{Rule: "a", SrcForge: "b:c"},
		{Rule: "a", SrcForge: "b", SrcRepo: "c"},
		{Rule: "a", SrcForge: "", SrcRepo: "bc"},
	}
	seen := map[string]PairKey{}
	for _, k := range keys {
		if o, ok := seen[k.String()]; ok {
			t.Fatalf("collision %+v vs %+v", k, o)
		}
		seen[k.String()] = k
	}
}

func TestOwnership(t *testing.T) {
	s := openTemp(t)
	if _, ok, err := s.Owned(ctx, "cb", "O/Repo"); err != nil || ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if err := s.MarkOwned(ctx, "cb", "O/Repo", OwnedCreated); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkOwned(ctx, "cb", "O/Repo", OwnedCreated); err != nil {
		t.Fatal(err)
	}
	how, ok, err := s.Owned(ctx, "cb", "o/repo")
	if err != nil || !ok || how != OwnedCreated {
		t.Fatalf("%v %v %v", how, ok, err)
	}
	if _, ok, _ := s.Owned(ctx, "other", "o/repo"); ok {
		t.Fatal("forge must be part of key")
	}
	if err := s.MarkOwned(ctx, "cb", "o/repo", OwnedAdopted); err != nil {
		t.Fatal(err)
	}
	how, _, _ = s.Owned(ctx, "cb", "O/REPO")
	if how != OwnedAdopted {
		t.Fatalf("want adopted got %v", how)
	}
	if err := s.ReleaseOwnership(ctx, "cb", "O/REPO"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Owned(ctx, "cb", "o/repo"); ok {
		t.Fatal("still owned")
	}
	if err := s.ReleaseOwnership(ctx, "cb", "o/repo"); err != nil {
		t.Fatal("release of unowned should be nil:", err)
	}
}

func TestFinishRunSuccessAndFailure(t *testing.T) {
	s := openTemp(t)
	fixedClock(s)
	k := key("x")
	if _, ok, _ := s.Pair(ctx, k); ok {
		t.Fatal("pair should not exist")
	}
	id, err := s.StartRun(ctx, k, "manual")
	if err != nil {
		t.Fatal(err)
	}
	ps, ok, _ := s.Pair(ctx, k)
	if !ok || ps.LastAttempt.IsZero() || !ps.LastSuccess.IsZero() {
		t.Fatalf("after start: %+v ok=%v", ps, ok)
	}
	if err := s.FinishRun(ctx, id, RunFailed, nil, "boom", "ignored", 5); err != nil {
		t.Fatal(err)
	}
	ps, _, _ = s.Pair(ctx, k)
	if ps.ConsecutiveFailures != 1 || ps.LastError != "boom" || ps.LastDigest != "" {
		t.Fatalf("%+v", ps)
	}
	id, _ = s.StartRun(ctx, k, "cron")
	steps := []StepResult{{Step: "git", Status: StepOK, Detail: "pushed", Duration: 1500 * time.Millisecond}}
	if err := s.FinishRun(ctx, id, RunOK, steps, "", "d1", 7); err != nil {
		t.Fatal(err)
	}
	ps, _, _ = s.Pair(ctx, k)
	if ps.ConsecutiveFailures != 0 || ps.LastError != "" || ps.LastDigest != "d1" || ps.LastRefCount != 7 || ps.LastSuccess.IsZero() {
		t.Fatalf("%+v", ps)
	}
	if ps.LastSuccess.Location() != time.UTC {
		t.Fatal("expected UTC")
	}
	id, _ = s.StartRun(ctx, k, "cron")
	if err := s.FinishRun(ctx, id, RunPartial, nil, "", "", 0); err != nil {
		t.Fatal(err)
	}
	ps, _, _ = s.Pair(ctx, k)
	if ps.LastDigest != "d1" || ps.LastRefCount != 7 || ps.ConsecutiveFailures != 0 {
		t.Fatalf("partial: %+v", ps)
	}
	for _, st := range []RunStatus{RunFailed, RunAborted} {
		id, _ = s.StartRun(ctx, k, "cron")
		if err := s.FinishRun(ctx, id, st, nil, "e", "d2", 9); err != nil {
			t.Fatal(err)
		}
	}
	ps, _, _ = s.Pair(ctx, k)
	if ps.ConsecutiveFailures != 2 || ps.LastDigest != "d1" || ps.LastRefCount != 7 {
		t.Fatalf("%+v", ps)
	}
}

func TestFinishRunErrors(t *testing.T) {
	s := openTemp(t)
	if err := s.FinishRun(ctx, 999, RunOK, nil, "", "", 0); err == nil {
		t.Fatal("unknown id should error")
	}
	id, _ := s.StartRun(ctx, key("a"), "m")
	if err := s.FinishRun(ctx, id, RunOK, nil, "", "d", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(ctx, id, RunFailed, nil, "x", "", 0); err == nil {
		t.Fatal("double finish should error")
	}
	ps, _, _ := s.Pair(ctx, key("a"))
	if ps.ConsecutiveFailures != 0 {
		t.Fatal("failed double finish must not change state")
	}
}

func TestRunsOrderingLimitAndSteps(t *testing.T) {
	s := openTemp(t)
	fixedClock(s)
	k := key("a")
	steps := []StepResult{
		{Step: "git", Status: StepOK, Detail: "d", Duration: time.Second},
		{Step: "lfs", Status: StepSkipped},
		{Step: "wiki", Status: StepFailed, Detail: "nope", Duration: 3 * time.Millisecond},
		{Step: "settings", Status: StepUnchanged},
	}
	var ids []int64
	for i := 0; i < 5; i++ {
		id, _ := s.StartRun(ctx, k, fmt.Sprintf("t%d", i))
		if err := s.FinishRun(ctx, id, RunPartial, steps, "", "", 0); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	runs, err := s.Runs(ctx, k, 3)
	if err != nil || len(runs) != 3 {
		t.Fatalf("%v %d", err, len(runs))
	}
	if runs[0].ID != ids[4] || runs[1].ID != ids[3] || runs[2].ID != ids[2] {
		t.Fatalf("order: %v", runs)
	}
	r := runs[0]
	if r.Trigger != "t4" || r.Status != RunPartial || r.Key != k {
		t.Fatalf("%+v", r)
	}
	if !reflect.DeepEqual(r.Steps, steps) {
		t.Fatalf("steps %+v", r.Steps)
	}
	if !r.Finished.After(r.Started) || r.Started.Nanosecond() != 123456789 {
		t.Fatalf("times %v %v", r.Started, r.Finished)
	}
	all, _ := s.Runs(ctx, k, 0)
	if len(all) != 5 {
		t.Fatalf("limit 0 should mean all, got %d", len(all))
	}
}

func TestRecentRuns(t *testing.T) {
	s := openTemp(t)
	fixedClock(s)
	var last int64
	for i := 0; i < 3; i++ {
		for _, n := range []string{"a", "b"} {
			last, _ = s.StartRun(ctx, key(n), "m")
		}
	}
	runs, err := s.RecentRuns(ctx, 4)
	if err != nil || len(runs) != 4 {
		t.Fatalf("%v %d", err, len(runs))
	}
	if runs[0].ID != last || runs[0].Key != key("b") || runs[0].Status != RunRunning {
		t.Fatalf("%+v", runs[0])
	}
	if !runs[0].Finished.IsZero() {
		t.Fatal("running run must have zero Finished")
	}
}

func TestAbandonRunning(t *testing.T) {
	s := openTemp(t)
	a, _ := s.StartRun(ctx, key("a"), "m")
	s.StartRun(ctx, key("b"), "m")
	done, _ := s.StartRun(ctx, key("c"), "m")
	s.FinishRun(ctx, done, RunOK, nil, "", "d", 1)
	n, err := s.AbandonRunning(ctx)
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	runs, _ := s.Runs(ctx, key("a"), 1)
	if runs[0].ID != a || runs[0].Status != RunAborted || runs[0].Error != "interrupted" || runs[0].Finished.IsZero() {
		t.Fatalf("%+v", runs[0])
	}
	if n, _ := s.AbandonRunning(ctx); n != 0 {
		t.Fatalf("second call %d", n)
	}
}

func TestPruneRuns(t *testing.T) {
	s := openTemp(t)
	fixedClock(s)
	ids := map[string][]int64{}
	for i := 0; i < 5; i++ {
		for _, n := range []string{"a", "b"} {
			id, _ := s.StartRun(ctx, key(n), "m")
			ids[n] = append(ids[n], id)
		}
	}
	got, err := s.PruneRuns(ctx, 2)
	if err != nil || got != 6 {
		t.Fatalf("pruned %d err %v", got, err)
	}
	for _, n := range []string{"a", "b"} {
		runs, _ := s.Runs(ctx, key(n), 0)
		if len(runs) != 2 || runs[0].ID != ids[n][4] || runs[1].ID != ids[n][3] {
			t.Fatalf("%s: %+v", n, runs)
		}
	}
}

func TestPairsSorted(t *testing.T) {
	s := openTemp(t)
	for _, n := range []string{"c", "a", "b"} {
		s.StartRun(ctx, key(n), "m")
	}
	ps, err := s.Pairs(ctx)
	if err != nil || len(ps) != 3 {
		t.Fatalf("%v %d", err, len(ps))
	}
	for i := 1; i < len(ps); i++ {
		if ps[i-1].Key.String() >= ps[i].Key.String() {
			t.Fatal("not sorted")
		}
	}
}

func TestReleaseMap(t *testing.T) {
	s := openTemp(t)
	k := key("a")
	m, err := s.ReleaseMap(ctx, k)
	if err != nil || len(m) != 0 || m == nil {
		t.Fatalf("%v %v", m, err)
	}
	s.SetReleaseMapping(ctx, k, 1, 10)
	s.SetReleaseMapping(ctx, k, 2, 20)
	s.SetReleaseMapping(ctx, k, 1, 11)
	s.SetReleaseMapping(ctx, key("b"), 1, 99)
	m, _ = s.ReleaseMap(ctx, k)
	if !reflect.DeepEqual(m, map[int64]int64{1: 11, 2: 20}) {
		t.Fatalf("%v", m)
	}
	if err := s.DeleteReleaseMapping(ctx, k, 1); err != nil {
		t.Fatal(err)
	}
	m, _ = s.ReleaseMap(ctx, k)
	if !reflect.DeepEqual(m, map[int64]int64{2: 20}) {
		t.Fatalf("%v", m)
	}
	mb, _ := s.ReleaseMap(ctx, key("b"))
	if mb[1] != 99 {
		t.Fatal("other pair affected")
	}
}

func TestFixedClockExact(t *testing.T) {
	s := openTemp(t)
	want := time.Date(2030, 5, 6, 7, 8, 9, 987654321, time.FixedZone("x", 3600))
	s.SetClock(func() time.Time { return want })
	id, _ := s.StartRun(ctx, key("a"), "m")
	s.FinishRun(ctx, id, RunOK, nil, "", "d", 1)
	ps, _, _ := s.Pair(ctx, key("a"))
	if !ps.LastSuccess.Equal(want) || !ps.LastAttempt.Equal(want) {
		t.Fatalf("%v %v", ps.LastSuccess, ps.LastAttempt)
	}
}

func TestConcurrentRuns(t *testing.T) {
	s := openTemp(t)
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := key(fmt.Sprintf("p%d", i))
			for j := 0; j < 5; j++ {
				id, err := s.StartRun(ctx, k, "w")
				if err != nil {
					errs <- err
					return
				}
				if err := s.FinishRun(ctx, id, RunOK, []StepResult{{Step: "git", Status: StepOK}}, "", fmt.Sprintf("d%d", j), j); err != nil {
					errs <- err
					return
				}
				if err := s.SetReleaseMapping(ctx, k, int64(j), int64(j)); err != nil {
					errs <- err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	ps, err := s.Pairs(ctx)
	if err != nil || len(ps) != n {
		t.Fatalf("%v %d", err, len(ps))
	}
	for _, p := range ps {
		if p.LastDigest != "d4" || p.ConsecutiveFailures != 0 {
			t.Fatalf("%+v", p)
		}
	}
	runs, _ := s.RecentRuns(ctx, 1000)
	if len(runs) != n*5 {
		t.Fatalf("runs %d", len(runs))
	}
}
