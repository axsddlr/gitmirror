package server

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"gitmirror/internal/app"
	"gitmirror/internal/engine"
)

// Webhook results (the result label of gitmirror_webhooks_total).
const (
	whAccepted     = "accepted"
	whIgnored      = "ignored"
	whUnauthorized = "unauthorized"
	whBadRequest   = "bad_request"
	whTooLarge     = "too_large"
	whDisabled     = "disabled"
	whBusy         = "busy"
)

// metrics are the server's Prometheus collectors on a private registry.
// Label values are rule names, configured forge names, pair coordinates and
// fixed enums only; never tokens, secrets or request data.
type metrics struct {
	pairMu     sync.Mutex // serialises refreshPairs
	reg        *prometheus.Registry
	runs       *prometheus.CounterVec
	duration   *prometheus.HistogramVec
	pairs      *pairCollector
	queue      *prometheus.GaugeVec
	webhooks   *prometheus.CounterVec
	ruleErrors *prometheus.CounterVec
	rate       *prometheus.GaugeVec
}

func newMetrics() *metrics {
	m := &metrics{
		reg: prometheus.NewRegistry(),
		runs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gitmirror_sync_runs_total",
			Help: "Pair sync runs by rule and final status.",
		}, []string{"rule", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gitmirror_sync_duration_seconds",
			Help:    "Duration of one pair sync (sum of its step durations).",
			Buckets: []float64{0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800, 3600},
		}, []string{"rule"}),
		pairs: newPairCollector(),
		queue: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gitmirror_queue_depth",
			Help: "Jobs waiting in a lane (scheduled or webhook).",
		}, []string{"lane"}),
		webhooks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gitmirror_webhooks_total",
			Help: "Webhook deliveries by forge and result.",
		}, []string{"forge", "result"}),
		ruleErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gitmirror_rule_errors_total",
			Help: "Rule-level errors (listing failures, safety drops, failed jobs).",
		}, []string{"rule"}),
		rate: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gitmirror_forge_rate_limit_remaining",
			Help: "Remaining API quota per forge, when the forge reports one.",
		}, []string{"forge"}),
	}
	m.reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.runs, m.duration, m.pairs, m.queue, m.webhooks, m.ruleErrors, m.rate,
	)
	m.queue.WithLabelValues(laneScheduled).Set(0)
	m.queue.WithLabelValues(laneWebhook).Set(0)
	return m
}

type pairSample struct {
	labels   []string // rule, src, dst
	lastOK   float64
	failures float64
}

// pairCollector exposes the per-pair gauges from a snapshot that is replaced
// as a whole.
type pairCollector struct {
	mu          sync.RWMutex
	snap        []pairSample
	lastSuccess *prometheus.Desc
	failures    *prometheus.Desc
}

func newPairCollector() *pairCollector {
	labels := []string{"rule", "src", "dst"}
	return &pairCollector{
		lastSuccess: prometheus.NewDesc("gitmirror_pair_last_success_timestamp_seconds",
			"Unix time of the pair's last ok or partial run (0 = never).", labels, nil),
		failures: prometheus.NewDesc("gitmirror_pair_consecutive_failures",
			"Failed or aborted runs of the pair since its last success.", labels, nil),
	}
}

func (c *pairCollector) set(snap []pairSample) {
	c.mu.Lock()
	c.snap = snap
	c.mu.Unlock()
}

func (c *pairCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.lastSuccess
	ch <- c.failures
}

func (c *pairCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	snap := c.snap
	c.mu.RUnlock()
	for _, p := range snap {
		ch <- prometheus.MustNewConstMetric(c.lastSuccess, prometheus.GaugeValue, p.lastOK, p.labels...)
		ch <- prometheus.MustNewConstMetric(c.failures, prometheus.GaugeValue, p.failures, p.labels...)
	}
}

// observe records the outcome of one job.
func (m *metrics) observe(j Job, res *app.SyncResult, err error) {
	if err != nil {
		for _, r := range j.Rules {
			m.ruleErrors.WithLabelValues(r).Inc()
		}
	}
	if res == nil {
		return
	}
	for _, e := range res.Resolve.Errors {
		m.ruleErrors.WithLabelValues(e.Rule).Inc()
	}
	for _, list := range [][]engine.Report{res.Reports, res.Reconciled} {
		for _, r := range list {
			m.runs.WithLabelValues(r.Key.Rule, string(r.Status)).Inc()
			var d time.Duration
			for _, st := range r.Steps {
				d += st.Duration
			}
			m.duration.WithLabelValues(r.Key.Rule).Observe(d.Seconds())
		}
	}
}

// refreshPairs rebuilds the per-pair gauges from the state store, so pairs
// that were deleted disappear.
func (s *Server) refreshPairs(ctx context.Context) {
	// Hold the lock across the read so concurrent refreshes (one per lane)
	// apply in order and the last one to run wins with the freshest state.
	// The snapshot is swapped in atomically, so a scrape never sees the
	// gauges half rebuilt or empty.
	s.m.pairMu.Lock()
	defer s.m.pairMu.Unlock()
	ps, err := s.App.Store.Pairs(ctx)
	if err != nil {
		s.log().Warn("server: read pair states for metrics", "err", err)
		return
	}
	snap := make([]pairSample, 0, len(ps))
	for _, p := range ps {
		var ts float64
		if !p.LastSuccess.IsZero() {
			ts = float64(p.LastSuccess.UnixNano()) / 1e9
		}
		snap = append(snap, pairSample{
			labels:   []string{p.Key.Rule, p.Key.SrcForge + ":" + p.Key.SrcRepo, p.Key.DstForge + ":" + p.Key.DstRepo},
			lastOK:   ts,
			failures: float64(p.ConsecutiveFailures),
		})
	}
	s.m.pairs.set(snap)
}

// refreshRates queries every forge's API quota (skipping forges that report
// no limit) of the current configuration.
func (s *Server) refreshRates(ctx context.Context) {
	for name, p := range s.App.Providers() {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		r, err := p.RateLimit(rctx)
		cancel()
		if err != nil {
			s.log().Debug("server: rate limit", "forge", name, "err", err)
			continue
		}
		if r.Limit == 0 {
			continue
		}
		s.m.rate.WithLabelValues(name).Set(float64(r.Remaining))
	}
}
