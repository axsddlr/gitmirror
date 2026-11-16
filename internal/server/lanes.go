package server

import (
	"context"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"gitmirror/internal/app"
	"gitmirror/internal/config"
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

// SyncNow queues a full sync of rule in the scheduled lane, as if its
// interval had elapsed (a rule already waiting is not queued twice). An
// unknown rule is an error wrapping app.ErrNoMatch. Safe to call before Run;
// the job then starts once Run does.
func (s *Server) SyncNow(rule string) error {
	s.init()
	if s.App.Config().RuleOrigin(rule) == "" {
		return fmt.Errorf("%w: unknown rule %q", app.ErrNoMatch, rule)
	}
	s.enqueueScheduled(rule)
	return nil
}

// SyncAll queues a full sync of every rule in the scheduled lane (see
// SyncNow).
func (s *Server) SyncAll() error {
	s.init()
	for _, r := range s.App.Config().Rules {
		s.enqueueScheduled(r.Name)
	}
	return nil
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

// runJob runs one job and records its outcome. Rules removed from the
// configuration since the job was queued are skipped.
func (s *Server) runJob(ctx context.Context, lane string, j Job) {
	log := s.log().With("lane", lane, "rules", j.Rules, "repos", j.Repos)
	if len(j.Rules) > 0 {
		cfg := s.App.Config()
		live := slices.DeleteFunc(slices.Clone(j.Rules), func(r string) bool { return cfg.RuleOrigin(r) == "" })
		if len(live) == 0 {
			log.Info("server: skipping job of removed rule(s)")
			return
		}
		j.Rules = live
	}
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
// failed or was incomplete keeps its previous pairs. Rules removed from the
// configuration meanwhile (the job outlived them) are not remembered.
func (s *Server) rememberResolve(rules []string, res resolve.Result) {
	cfg := s.App.Config()
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
		if cfg.RuleOrigin(r) == "" {
			continue
		}
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

// schedEntry is one rule's place in the schedule.
type schedEntry struct {
	next  time.Time
	every time.Duration
}

func ruleInterval(r config.Rule, d config.Defaults) time.Duration {
	if iv := r.Effective(d).Interval; iv > 0 {
		return iv
	}
	return defaultInterval
}

// scheduler enqueues each rule when due (first after its initial delay,
// then every jittered interval from that start) and prunes run history
// daily. Times are absolute on s.Clock. When the App's configuration
// changes it reschedules (see reschedule) and forgets removed rules (see
// applyRuleChanges).
func (s *Server) scheduler(ctx context.Context) {
	snap := s.App.Snapshot()
	now := s.Clock.Now()
	sched := map[string]*schedEntry{}
	order := s.reschedule(snap.Config, sched, now)
	nextPrune := now.Add(pruneEvery)
	var timer <-chan time.Time
	var timerAt time.Time
	for {
		now = s.Clock.Now()
		for _, name := range order {
			if e := sched[name]; !e.next.After(now) {
				s.enqueueScheduled(name)
				e.next = now.Add(s.jitter(e.every))
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
		for _, e := range sched {
			if e.next.Before(earliest) {
				earliest = e.next
			}
		}
		// Keep a pending timer that still matches, so a configuration
		// change that moves nothing does not stack up timers.
		if timer == nil || !earliest.Equal(timerAt) {
			timer, timerAt = s.Clock.After(earliest.Sub(now)), earliest
		}
		select {
		case <-s.stop:
			return
		case <-snap.Changed():
			old := snap.Config
			snap = s.App.Snapshot()
			order = s.reschedule(snap.Config, sched, s.Clock.Now())
			s.applyRuleChanges(old, snap.Config)
			if s.onReschedule != nil {
				next := make(map[string]time.Time, len(sched))
				for n, e := range sched {
					next[n] = e.next
				}
				s.onReschedule(next)
			}
		case <-timer:
			timer = nil
		}
	}
}

// reschedule updates sched for the rules of cfg and returns their names in
// config order. New rules are due after the initial delay (staggered among
// the rules added together); a rule whose interval changed runs no later
// than one new interval from now; other rules keep their next run time.
// Removed rules are dropped.
func (s *Server) reschedule(cfg *config.Config, sched map[string]*schedEntry, now time.Time) []string {
	order := make([]string, 0, len(cfg.Rules))
	var added []string
	for _, r := range cfg.Rules {
		order = append(order, r.Name)
		every := ruleInterval(r, cfg.Defaults)
		e, ok := sched[r.Name]
		switch {
		case !ok:
			sched[r.Name] = &schedEntry{every: every}
			added = append(added, r.Name)
		case e.every != every:
			e.every = every
			if n := now.Add(s.jitter(every)); n.Before(e.next) {
				e.next = n
			}
		}
	}
	for i, name := range added {
		e := sched[name]
		e.next = now.Add(s.initialDelay(i, len(added), e.every))
	}
	for name := range sched {
		if !slices.Contains(order, name) {
			delete(sched, name)
		}
	}
	return order
}

// applyRuleChanges reacts to a configuration change from old to cur:
// rules that were removed leave the scheduled queue, their pending webhook
// jobs and their remembered resolve; new rules get their metric labels.
// Jobs already running are not affected.
func (s *Server) applyRuleChanges(old, cur *config.Config) {
	for _, r := range cur.Rules {
		if old.RuleOrigin(r.Name) == "" {
			s.m.ruleErrors.WithLabelValues(r.Name).Add(0)
		}
	}
	removed := map[string]bool{}
	for _, r := range old.Rules {
		if cur.RuleOrigin(r.Name) == "" {
			removed[r.Name] = true
		}
	}
	if len(removed) == 0 {
		return
	}
	s.mu.Lock()
	s.schedQueue = slices.DeleteFunc(s.schedQueue, func(r string) bool { return removed[r] })
	s.whOrder = slices.DeleteFunc(s.whOrder, func(it *whItem) bool {
		if slices.ContainsFunc(it.job.Rules, func(r string) bool { return removed[r] }) {
			delete(s.whPending, it.key)
			return true
		}
		return false
	})
	for r := range removed {
		delete(s.lastResolve, r)
	}
	nq, nw := len(s.schedQueue), len(s.whOrder)
	s.mu.Unlock()
	s.m.queue.WithLabelValues(laneScheduled).Set(float64(nq))
	s.m.queue.WithLabelValues(laneWebhook).Set(float64(nw))
}
