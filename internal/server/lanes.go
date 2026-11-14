package server

import (
	"context"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"gitmirror/internal/app"
	"gitmirror/internal/resolve"
)

// Lanes. Each has one worker; the engine serialises work on a pair across
// lanes.
const (
	laneScheduled = "scheduled" // full rule syncs (with reconcile)
	laneWebhook   = "webhook"   // targeted repo syncs (never reconcile)
)

// enqueueScheduled queues a full sync of rule. A rule already waiting is not
// queued twice. The queue never holds the running job, so a trigger while
// the rule runs queues exactly one rerun that starts when it finishes.
func (s *Server) enqueueScheduled(rule string) {
	s.mu.Lock()
	if !slices.Contains(s.schedQueue, rule) {
		s.schedQueue = append(s.schedQueue, rule)
	}
	n := len(s.schedQueue)
	s.mu.Unlock()
	s.m.queue.WithLabelValues(laneScheduled).Set(float64(n))
	wake(s.schedWake)
}

// queuedRules returns the scheduled lane's waiting rules in order.
func (s *Server) queuedRules() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.schedQueue)
}

// enqueueWebhook queues a targeted sync of repo under rule after Debounce.
// Deliveries for the same (rule, repo) arriving while one is pending collapse
// into it; the window is not extended, so a busy repo still syncs.
func (s *Server) enqueueWebhook(rule, repo string) {
	key := rule + "\x00" + strings.ToLower(repo)
	s.mu.Lock()
	if _, ok := s.whPending[key]; !ok {
		it := &whItem{
			key: key,
			job: Job{Rules: []string{rule}, Repos: []string{repo}, Trigger: "webhook"},
			due: s.Clock.Now().Add(s.Debounce),
		}
		s.whPending[key] = it
		s.whOrder = append(s.whOrder, it)
	}
	n := len(s.whOrder)
	s.mu.Unlock()
	s.m.queue.WithLabelValues(laneWebhook).Set(float64(n))
	wake(s.whWake)
}

// webhookDepth is the number of pending webhook jobs.
func (s *Server) webhookDepth() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.whOrder)
}

func (s *Server) busy(lane string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busyLanes[lane]
}

func (s *Server) setBusy(lane string, b bool) {
	s.mu.Lock()
	s.busyLanes[lane] = b
	s.mu.Unlock()
}

func (s *Server) scheduledWorker(ctx context.Context) {
	for {
		if s.stopped() {
			return
		}
		s.mu.Lock()
		if len(s.schedQueue) == 0 {
			s.mu.Unlock()
			select {
			case <-s.stop:
				return
			case <-s.schedWake:
			}
			continue
		}
		rule := s.schedQueue[0]
		s.schedQueue = s.schedQueue[1:]
		n := len(s.schedQueue)
		s.busyLanes[laneScheduled] = true
		s.mu.Unlock()
		s.m.queue.WithLabelValues(laneScheduled).Set(float64(n))
		s.runJob(ctx, laneScheduled, Job{Rules: []string{rule}, Trigger: "schedule"})
		s.setBusy(laneScheduled, false)
	}
}

func (s *Server) webhookWorker(ctx context.Context) {
	for {
		if s.stopped() {
			return
		}
		s.mu.Lock()
		now := s.Clock.Now()
		var item *whItem
		var next time.Time
		for i, it := range s.whOrder {
			if !it.due.After(now) {
				item = it
				s.whOrder = slices.Delete(s.whOrder, i, i+1)
				delete(s.whPending, it.key)
				break
			}
			if next.IsZero() || it.due.Before(next) {
				next = it.due
			}
		}
		n := len(s.whOrder)
		if item != nil {
			s.busyLanes[laneWebhook] = true
		}
		s.mu.Unlock()
		if item != nil {
			s.m.queue.WithLabelValues(laneWebhook).Set(float64(n))
			s.runJob(ctx, laneWebhook, item.job)
			s.setBusy(laneWebhook, false)
			continue
		}
		var timer <-chan time.Time
		if !next.IsZero() {
			timer = s.Clock.After(next.Sub(now))
		}
		select {
		case <-s.stop:
			return
		case <-s.whWake:
		case <-timer:
		}
	}
}

// runJob runs one job and records its outcome.
func (s *Server) runJob(ctx context.Context, lane string, j Job) {
	log := s.log().With("lane", lane, "rules", j.Rules, "repos", j.Repos)
	start := time.Now()
	var res *app.SyncResult
	var err error
	func() {
		defer func() {
			if v := recover(); v != nil {
				err = fmt.Errorf("panic: %v", v)
				log.Error("server: panic in job", "panic", v, "stack", string(debug.Stack()))
			}
		}()
		res, err = s.App.Sync(ctx, j.Rules, j.Repos, false, j.Trigger)
	}()
	if err != nil {
		log.Error("server: job failed", "err", err)
	} else {
		log.Info("server: job done", "pairs", len(res.Reports), "reconciled", len(res.Reconciled),
			"rule_errors", len(res.Resolve.Errors), "elapsed", time.Since(start).Round(time.Millisecond))
		for _, e := range res.Resolve.Errors {
			log.Warn("server: rule error", "rule", e.Rule, "err", e.Err)
		}
	}
	if err == nil && len(j.Repos) == 0 {
		s.rememberResolve(j.Rules, res.Resolve)
	}
	s.m.observe(j, res, err)
	mctx := context.WithoutCancel(ctx)
	s.refreshPairs(mctx)
	if lane == laneScheduled {
		s.refreshRates(mctx)
	}
	if s.onJobDone != nil {
		s.onJobDone(lane, j)
	}
}

// rememberResolve stores, per rule of a full (unfiltered) sync, the pairs
// that resolve produced; webhooks are routed with it. Filtered syncs carry
// only the filtered pairs, so they never update it. A rule whose resolve
// failed or was incomplete keeps its previous pairs.
func (s *Server) rememberResolve(rules []string, res resolve.Result) {
	by := map[string][]resolve.Pair{}
	for _, p := range res.Pairs {
		by[p.Rule] = append(by[p.Rule], p)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	failed := map[string]bool{}
	for _, e := range res.Errors {
		failed[e.Rule] = true
	}
	for _, r := range rules {
		// A rule that errored or listed incompletely keeps its last good
		// pairs, so its webhooks still route; it only gets this partial
		// result if it has nothing better.
		if _, have := s.lastResolve[r]; have && (failed[r] || !res.Complete[r]) {
			continue
		}
		s.lastResolve[r] = by[r]
	}
}

// ---- scheduler ----

// initialDelay is the wait before rule i's (of n) first run.
func (s *Server) initialDelay(i, n int, interval time.Duration) time.Duration {
	if s.InitialDelay > 0 {
		return s.InitialDelay
	}
	if n <= 0 {
		return 0
	}
	span := min(interval, maxStagger)
	return span * time.Duration(i) / time.Duration(n)
}

// jitter returns interval +-10%.
func (s *Server) jitter(interval time.Duration) time.Duration {
	f := 0.9 + 0.2*s.rng.Float64()
	return time.Duration(float64(interval) * f)
}

// scheduler enqueues each rule when due (first after its initial delay,
// then every jittered interval from that start) and prunes run history
// daily. Times are absolute on s.Clock.
func (s *Server) scheduler(ctx context.Context) {
	rules := s.App.Config.Rules
	now := s.Clock.Now()
	next := make([]time.Time, len(rules))
	every := make([]time.Duration, len(rules))
	for i, r := range rules {
		every[i] = r.Effective(s.App.Config.Defaults).Interval
		if every[i] <= 0 {
			every[i] = defaultInterval
		}
		next[i] = now.Add(s.initialDelay(i, len(rules), every[i]))
	}
	nextPrune := now.Add(pruneEvery)
	for {
		now = s.Clock.Now()
		for i, r := range rules {
			if !next[i].After(now) {
				s.enqueueScheduled(r.Name)
				next[i] = now.Add(s.jitter(every[i]))
			}
		}
		if !nextPrune.After(now) {
			if n, err := s.App.Store.PruneRuns(ctx, pruneKeep); err != nil {
				s.log().Warn("server: prune run history", "err", err)
			} else {
				s.log().Info("server: pruned run history", "deleted", n)
			}
			nextPrune = now.Add(pruneEvery)
		}
		earliest := nextPrune
		for _, t := range next {
			if t.Before(earliest) {
				earliest = t
			}
		}
		select {
		case <-s.stop:
			return
		case <-s.Clock.After(earliest.Sub(now)):
		}
	}
}
