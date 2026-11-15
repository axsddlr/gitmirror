package server

import (
	"errors"
	"slices"
	"testing"

	"gitmirror/internal/app"
)

func TestSyncNowQueuesKnownRulesOnce(t *testing.T) {
	f := newFixture(t)
	s := f.server()
	if err := s.SyncNow("nosuch"); !errors.Is(err, app.ErrNoMatch) {
		t.Fatalf("unknown rule: err = %v", err)
	}
	if err := s.SyncNow("r1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncNow("r1"); err != nil {
		t.Fatal(err)
	}
	if got := s.queuedRules(); !slices.Equal(got, []string{"r1"}) {
		t.Fatalf("queue = %v", got)
	}
	if err := s.SyncAll(); err != nil {
		t.Fatal(err)
	}
	if got := s.queuedRules(); !slices.Equal(got, []string{"r1", "r2"}) {
		t.Fatalf("queue after SyncAll = %v", got)
	}
}

func TestSyncNowRunsInScheduledLane(t *testing.T) {
	f := newFixture(t)
	f.addSource("alice", "a")
	s := f.server()
	f.start(s)
	if err := s.SyncNow("r1"); err != nil {
		t.Fatal(err)
	}
	jobs := f.waitJobs(1)
	if jobs[0].Lane != laneScheduled || !slices.Equal(jobs[0].Job.Rules, []string{"r1"}) {
		t.Fatalf("jobs = %+v", jobs)
	}
	if !f.dstHas("mirrors", "a") {
		t.Fatal("SyncNow did not mirror")
	}
}
