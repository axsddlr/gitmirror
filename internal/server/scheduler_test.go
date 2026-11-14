package server

import (
	"slices"
	"testing"
	"time"
)

func countRule(jobs []doneJob, lane, rule string) int {
	n := 0
	for _, j := range jobs {
		if j.Lane == lane && slices.Contains(j.Job.Rules, rule) {
			n++
		}
	}
	return n
}

func TestSchedulerRunsEachRuleAtStartThenEveryInterval(t *testing.T) {
	f := newFixture(t)
	f.addSource("alice", "a")
	f.addSource("bob", "c")
	s := f.server()
	f.start(s)

	if n := len(f.doneJobs()); n != 0 {
		t.Fatalf("%d jobs ran before InitialDelay", n)
	}
	f.initialSync(s)
	jobs := f.doneJobs()
	if len(jobs) != 2 || countRule(jobs, laneScheduled, "r1") != 1 || countRule(jobs, laneScheduled, "r2") != 1 {
		t.Fatalf("jobs after start = %+v", jobs)
	}
	for _, j := range jobs {
		if len(j.Job.Repos) != 0 || j.Job.Trigger != "schedule" || len(j.Job.Rules) != 1 {
			t.Fatalf("scheduled job = %+v", j.Job)
		}
	}
	if !f.dstHas("mirrors", "a") || !f.dstHas("bobmirror", "c") {
		t.Fatal("initial sync did not mirror")
	}

	// Interval is 1h with +-10% jitter: nothing is due before 54m.
	waitFor(t, 10*time.Second, func() bool { return f.clock.Waiters() == 1 }, "scheduler idle")
	f.clock.Advance(50 * time.Minute)
	waitFor(t, 10*time.Second, func() bool { return f.clock.Waiters() == 1 }, "scheduler idle")
	if n := len(f.doneJobs()); n != 2 {
		t.Fatalf("%d jobs after 50m, want 2", n)
	}
	// Everything is due by 66m.
	f.clock.Advance(20 * time.Minute)
	jobs = f.waitJobs(4)
	if countRule(jobs, laneScheduled, "r1") != 2 || countRule(jobs, laneScheduled, "r2") != 2 {
		t.Fatalf("jobs after 70m = %+v", jobs)
	}
	runs := f.runs(key("r1", "alice", "a", "mirrors"))
	if len(runs) != 2 || runs[0].Trigger != "schedule" {
		t.Fatalf("runs = %+v", runs)
	}
}

func TestDefaultInitialDelayStaggersRules(t *testing.T) {
	s := &Server{}
	if d := s.initialDelay(0, 2, time.Hour); d != 0 {
		t.Fatalf("rule 0 delay = %v", d)
	}
	if d := s.initialDelay(1, 2, time.Hour); d != 15*time.Second {
		t.Fatalf("rule 1 delay = %v", d)
	}
	if d := s.initialDelay(1, 2, 10*time.Second); d != 5*time.Second {
		t.Fatalf("rule 1 delay (short interval) = %v", d)
	}
	s.InitialDelay = time.Minute
	if d := s.initialDelay(1, 2, time.Hour); d != time.Minute {
		t.Fatalf("explicit delay = %v", d)
	}
}

func TestJitterWithinTenPercent(t *testing.T) {
	s := &Server{Clock: newFakeClock()}
	s.init()
	for range 1000 {
		d := s.jitter(time.Hour)
		if d < 54*time.Minute || d > 66*time.Minute {
			t.Fatalf("jitter(1h) = %v", d)
		}
	}
}

func TestScheduledLaneCoalescesQueuedRule(t *testing.T) {
	f := newFixture(t)
	s := f.server()
	s.init()
	s.enqueueScheduled("r1")
	s.enqueueScheduled("r1")
	s.enqueueScheduled("r2")
	s.enqueueScheduled("r1")
	if got := s.queuedRules(); !slices.Equal(got, []string{"r1", "r2"}) {
		t.Fatalf("queue = %v, want [r1 r2]", got)
	}
}

func TestScheduledLaneRerunsRunningRuleOnce(t *testing.T) {
	f := newFixture(t)
	f.addSource("alice", "a")
	s := f.server()
	s.InitialDelay = 24 * time.Hour // keep the scheduler out of the way
	f.start(s)

	f.src.arm()
	s.enqueueScheduled("r1")
	select {
	case <-f.src.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("job did not start")
	}
	// r1 is running: three triggers yield exactly one pending rerun.
	s.enqueueScheduled("r1")
	s.enqueueScheduled("r1")
	s.enqueueScheduled("r1")
	if got := s.queuedRules(); !slices.Equal(got, []string{"r1"}) {
		t.Fatalf("queue while running = %v, want [r1]", got)
	}
	f.src.open()
	jobs := f.waitJobs(2)
	waitFor(t, 10*time.Second, func() bool { return len(s.queuedRules()) == 0 && !s.busy(laneScheduled) }, "lane idle")
	jobs = f.doneJobs()
	if len(jobs) != 2 || countRule(jobs, laneScheduled, "r1") != 2 {
		t.Fatalf("jobs = %+v, want r1 exactly twice", jobs)
	}
}
