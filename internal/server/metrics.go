package server

import (
	"context"
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
	reg         *prometheus.Registry
	runs        *prometheus.CounterVec
	duration    *prometheus.HistogramVec
	lastSuccess *prometheus.GaugeVec
	failures    *prometheus.GaugeVec
	queue       *prometheus.GaugeVec
	webhooks    *prometheus.CounterVec
	ruleErrors  *prometheus.CounterVec
	rate        *prometheus.GaugeVec
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
		lastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gitmirror_pair_last_success_timestamp_seconds",
			Help: "Unix time of the pair's last ok or partial run (0 = never).",
		}, []string{"rule", "src", "dst"}),
		failures: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gitmirror_pair_consecutive_failures",
			Help: "Failed or aborted runs of the pair since its last success.",
		}, []string{"rule", "src", "dst"}),
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
		m.runs, m.duration, m.lastSuccess, m.failures, m.queue, m.webhooks, m.ruleErrors, m.rate,
	)
	m.queue.WithLabelValues(laneScheduled).Set(0)
	m.queue.WithLabelValues(laneWebhook).Set(0)
	return m
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
	ps, err := s.App.Store.Pairs(ctx)
	if err != nil {
		s.log().Warn("server: read pair states for metrics", "err", err)
		return
	}
	s.m.lastSuccess.Reset()
	s.m.failures.Reset()
	for _, p := range ps {
		src := p.Key.SrcForge + ":" + p.Key.SrcRepo
		dst := p.Key.DstForge + ":" + p.Key.DstRepo
		var ts float64
		if !p.LastSuccess.IsZero() {
			ts = float64(p.LastSuccess.UnixNano()) / 1e9
		}
		s.m.lastSuccess.WithLabelValues(p.Key.Rule, src, dst).Set(ts)
		s.m.failures.WithLabelValues(p.Key.Rule, src, dst).Set(float64(p.ConsecutiveFailures))
	}
}

// refreshRates queries every forge's API quota (skipping forges that report
// no limit).
func (s *Server) refreshRates(ctx context.Context) {
	for name, p := range s.App.Providers {
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
