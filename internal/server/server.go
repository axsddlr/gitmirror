// Package server is gitmirror's long-running daemon: an HTTP server for
// webhooks, health and metrics, a per-rule scheduler and two job lanes that
// run App.Sync.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"gitmirror/internal/app"
	"gitmirror/internal/resolve"
)

// Clock abstracts time for the scheduler, debounce and maintenance, so tests
// can advance it without sleeping.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Defaults for the zero Server fields.
const (
	DefaultDebounce        = 5 * time.Second
	DefaultShutdownTimeout = 10 * time.Minute

	maxStagger      = 30 * time.Second
	httpShutdown    = 10 * time.Second
	pruneEvery      = 24 * time.Hour
	pruneKeep       = 100
	maxWebhookBody  = 25 << 20 // default Server.MaxWebhookBody (GitHub's payload limit)
	maxWebhooks     = 8        // default Server.MaxConcurrentWebhooks
	defaultInterval = time.Hour
)

// Job is one App.Sync call.
type Job struct {
	Rules   []string
	Repos   []string
	Trigger string
}

// Server runs the gitmirror daemon. Set the exported fields before calling
// Handler or Run; they must not change afterwards.
type Server struct {
	App    *app.App
	Logger *slog.Logger
	// Clock drives the scheduler, webhook debounce and daily maintenance;
	// nil means the real clock.
	Clock Clock
	// Debounce is the webhook coalescing window; 0 means DefaultDebounce.
	Debounce time.Duration
	// ShutdownTimeout is how long running jobs may continue after shutdown
	// begins before their context is cancelled; 0 means
	// DefaultShutdownTimeout. It is measured in real time.
	ShutdownTimeout time.Duration
	// InitialDelay is the wait before each rule's first scheduled run; 0
	// staggers the rules evenly over 0..min(interval, 30s).
	InitialDelay time.Duration
	// MaxWebhookBody is the largest accepted webhook body in bytes; 0 means
	// 25 MiB (GitHub's payload limit).
	MaxWebhookBody int64
	// MaxConcurrentWebhooks bounds webhook requests whose body is being read
	// (before signature verification); further requests get 503 with
	// Retry-After: 1. 0 means 8.
	MaxConcurrentWebhooks int
	MetricsListener       net.Listener
	// UIListener, when set, serves UIHandler (the web UI) on its own
	// listener; Run starts and gracefully shuts it down with the others.
	UIListener net.Listener
	UIHandler  http.Handler

	// onJobDone, when set (tests), is called after every job.
	onJobDone func(lane string, j Job)
	// onReschedule, when set (tests), is called by the scheduler with every
	// rule's next run time after it applied a configuration change.
	onReschedule func(next map[string]time.Time)

	once    sync.Once
	m       *metrics
	handler http.Handler
	rng     *rand.Rand // scheduler goroutine only
	stop    chan struct{}
	whSem   chan struct{}
	badSig  sync.Map // forge -> *badSigLog

	ready    atomic.Bool
	stopping atomic.Bool
	addr     atomic.Pointer[net.Addr]

	mu          sync.Mutex
	lastResolve map[string][]resolve.Pair // rule -> pairs of its last full resolve
	schedQueue  []string                  // rules waiting in the scheduled lane
	schedWake   chan struct{}
	whPending   map[string]*whItem // (rule, repo) -> pending webhook job
	whOrder     []*whItem
	whWake      chan struct{}
	busyLanes   map[string]bool
}

type whItem struct {
	key string
	job Job
	due time.Time
}

func (s *Server) init() {
	s.once.Do(func() {
		if s.Clock == nil {
			s.Clock = realClock{}
		}
		if s.Debounce <= 0 {
			s.Debounce = DefaultDebounce
		}
		if s.ShutdownTimeout <= 0 {
			s.ShutdownTimeout = DefaultShutdownTimeout
		}
		now := s.Clock.Now().UnixNano()
		s.rng = rand.New(rand.NewPCG(uint64(now), uint64(now>>1)^0x9e3779b97f4a7c15))
		s.m = newMetrics()
		s.stop = make(chan struct{})
		if s.MaxConcurrentWebhooks <= 0 {
			s.MaxConcurrentWebhooks = maxWebhooks
		}
		if s.MaxWebhookBody <= 0 {
			s.MaxWebhookBody = maxWebhookBody
		}
		s.whSem = make(chan struct{}, s.MaxConcurrentWebhooks)
		s.lastResolve = map[string][]resolve.Pair{}
		s.schedWake = make(chan struct{}, 1)
		s.whPending = map[string]*whItem{}
		s.whWake = make(chan struct{}, 1)
		s.busyLanes = map[string]bool{}
		if s.App != nil {
			for _, r := range s.App.Config().Rules {
				s.m.ruleErrors.WithLabelValues(r.Name).Add(0)
			}
		}

		mux := http.NewServeMux()
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
			text(w, http.StatusOK, "ok")
		})
		mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
			if s.ready.Load() {
				text(w, http.StatusOK, "ready")
				return
			}
			text(w, http.StatusServiceUnavailable, "not ready")
		})
		mux.HandleFunc("POST /webhook/{forge}", s.handleWebhook)
		s.handler = mux
	})
}

func (s *Server) log() *slog.Logger {
	if s.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Logger
}

func text(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	fmt.Fprintln(w, msg)
}

// Handler returns the main HTTP routes: GET /healthz, GET /readyz and
// POST /webhook/{forge}. /metrics is not served here (see MetricsListener). Other methods on these paths get 405.
func (s *Server) Handler() http.Handler {
	s.init()
	return s.handler
}

// MetricsHandler returns the handler for GET /metrics, which Run serves only
// on MetricsListener.
func (s *Server) MetricsHandler() http.Handler {
	s.init()
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.m.reg, promhttp.HandlerOpts{}))
	return mux
}

// Addr returns the listen address once Run has started, else nil.
func (s *Server) Addr() net.Addr {
	if p := s.addr.Load(); p != nil {
		return *p
	}
	return nil
}

// Run serves HTTP on ln and runs the scheduler and the two job lanes until
// ctx is done, then shuts down gracefully: readiness fails, the scheduler
// stops, webhooks get 503, the HTTP server drains (10s), running jobs get
// ShutdownTimeout to finish before their context is cancelled, and Run waits
// for them. It returns nil on a clean shutdown. Run must be called once.
func (s *Server) Run(ctx context.Context, ln net.Listener) error {
	s.init()
	if s.UIListener != nil && s.UIHandler == nil {
		return errors.New("server: UIListener is set without UIHandler")
	}
	addr := ln.Addr()
	s.addr.Store(&addr)
	hs := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          slog.NewLogLogger(s.log().Handler(), slog.LevelWarn),
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- hs.Serve(ln) }()
	var ms *http.Server
	metricsErr := make(chan error, 1)
	if s.MetricsListener != nil {
		ms = &http.Server{
			Handler:           s.MetricsHandler(),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       60 * time.Second,
			WriteTimeout:      60 * time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    1 << 20,
			ErrorLog:          slog.NewLogLogger(s.log().Handler(), slog.LevelWarn),
		}
		go func() { metricsErr <- ms.Serve(s.MetricsListener) }()
		s.log().Info("server: metrics listening", "addr", s.MetricsListener.Addr().String())
	}
	var us *http.Server
	uiErr := make(chan error, 1)
	if s.UIListener != nil {
		us = &http.Server{
			Handler:           s.UIHandler,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       60 * time.Second,
			WriteTimeout:      60 * time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    1 << 20,
			ErrorLog:          slog.NewLogLogger(s.log().Handler(), slog.LevelWarn),
		}
		go func() { uiErr <- us.Serve(s.UIListener) }()
		s.log().Info("server: web UI listening", "addr", s.UIListener.Addr().String())
	}

	// Jobs outlive ctx: they are cancelled only after ShutdownTimeout.
	jobCtx, cancelJobs := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelJobs()
	s.refreshPairs(jobCtx)

	var workers, sched sync.WaitGroup
	workers.Go(func() { s.scheduledWorker(jobCtx) })
	workers.Go(func() { s.webhookWorker(jobCtx) })
	sched.Go(func() { s.scheduler(jobCtx) })
	s.ready.Store(true)
	s.log().Info("server: started", "addr", addr.String())

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		runErr = fmt.Errorf("server: %w", err)
		serveErr <- nil
	case err := <-metricsErr:
		runErr = fmt.Errorf("server: metrics: %w", err)
		metricsErr <- nil
	case err := <-uiErr:
		runErr = fmt.Errorf("server: web UI: %w", err)
		uiErr <- nil
	}

	s.log().Info("server: shutting down")
	s.ready.Store(false)
	s.stopping.Store(true)
	close(s.stop)
	sched.Wait()

	shCtx, cancel := context.WithTimeout(context.Background(), httpShutdown)
	if err := hs.Shutdown(shCtx); err != nil {
		s.log().Warn("server: http shutdown", "err", err)
		hs.Close()
	}
	if ms != nil {
		if err := ms.Shutdown(shCtx); err != nil {
			s.log().Warn("server: metrics shutdown", "err", err)
			ms.Close()
		}
		if err := <-metricsErr; err != nil && !errors.Is(err, http.ErrServerClosed) && runErr == nil {
			runErr = fmt.Errorf("server: metrics: %w", err)
		}
	}
	if us != nil {
		if err := us.Shutdown(shCtx); err != nil {
			s.log().Warn("server: web UI shutdown", "err", err)
			us.Close()
		}
		if err := <-uiErr; err != nil && !errors.Is(err, http.ErrServerClosed) && runErr == nil {
			runErr = fmt.Errorf("server: web UI: %w", err)
		}
	}
	cancel()
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) && runErr == nil {
		runErr = fmt.Errorf("server: %w", err)
	}

	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	t := time.NewTimer(s.ShutdownTimeout)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		s.log().Warn("server: shutdown timeout reached; cancelling running jobs", "timeout", s.ShutdownTimeout)
		cancelJobs()
		<-done
	}
	s.log().Info("server: stopped")
	return runErr
}

// stopped reports whether shutdown has begun.
func (s *Server) stopped() bool {
	select {
	case <-s.stop:
		return true
	default:
		return false
	}
}

func wake(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
