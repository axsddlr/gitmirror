package server

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"gitmirror/internal/config"
	"gitmirror/internal/forge"
)

// handleWebhook serves POST /webhook/{forge}. Responses never echo config
// details or parse errors; details go to the debug log (no secrets).
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("forge")
	// One configuration snapshot for the whole request.
	snap := s.App.Snapshot()
	cfg := snap.Config
	_, known := cfg.Forges[name]
	label := name
	if !known {
		label = "unknown" // never use request data as a label value
	}
	count := func(result string) { s.m.webhooks.WithLabelValues(label, result).Inc() }
	log := s.log().With("forge", label)

	if s.stopping.Load() {
		text(w, http.StatusServiceUnavailable, "shutting down")
		return
	}
	if cfg.Server.WebhookSecret == "" {
		count(whDisabled)
		text(w, http.StatusNotFound, "webhooks are not configured")
		return
	}
	if !known {
		count(whIgnored)
		text(w, http.StatusNotFound, "not found")
		return
	}
	wp, ok := snap.Providers[name].(forge.WebhookProvider)
	if !ok {
		count(whIgnored)
		text(w, http.StatusNotFound, "not found")
		return
	}

	// Bound unauthenticated memory: the body is read before the signature is
	// checked, so limit how many reads run at once and shed the rest.
	select {
	case s.whSem <- struct{}{}:
		defer func() { <-s.whSem }()
	default:
		count(whBusy)
		w.Header().Set("Retry-After", "1")
		text(w, http.StatusServiceUnavailable, "busy")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.MaxWebhookBody))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			count(whTooLarge)
			text(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		count(whBadRequest)
		text(w, http.StatusBadRequest, "bad request")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	ev, err := wp.ParseWebhook(r, cfg.Server.WebhookSecret)
	switch {
	case errors.Is(err, forge.ErrBadSignature):
		count(whUnauthorized)
		s.logBadSignature(log, name, r.RemoteAddr)
		text(w, http.StatusUnauthorized, "unauthorized")
		return
	case err != nil:
		count(whBadRequest)
		log.Debug("server: unparsable webhook", "err", err)
		text(w, http.StatusBadRequest, "bad request")
		return
	}
	if ev.Kind == forge.EventOther {
		count(whIgnored)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if s.route(cfg, name, ev) {
		count(whAccepted)
		log.Info("server: webhook accepted", "kind", ev.Kind, "repo", ev.Owner+"/"+ev.Name)
		text(w, http.StatusAccepted, "accepted")
		return
	}
	count(whIgnored)
	log.Debug("server: webhook matched no rule", "kind", ev.Kind, "repo", ev.Owner+"/"+ev.Name)
	text(w, http.StatusAccepted, "ignored")
}

// route turns an event into jobs and reports whether any were queued.
// A repo found in a rule's last resolve gets a targeted webhook-lane sync
// (no reconcile). Otherwise every owner rule for that owner and every
// starred_by rule on the forge gets a full scheduled-lane sync, which picks
// up new repositories. A repo: rule naming the event's repo gets a targeted
// webhook-lane sync even before any full sync has run. Only rules of cfg
// (the request's configuration snapshot) are considered.
func (s *Server) route(cfg *config.Config, forgeName string, ev *forge.Event) bool {
	full := ev.Owner + "/" + ev.Name
	type target struct{ rule, repo string }
	var targets []target
	s.mu.Lock()
	for rule, pairs := range s.lastResolve {
		if cfg.RuleOrigin(rule) == "" {
			continue // removed since its last resolve
		}
		for _, p := range pairs {
			if p.SrcForge == forgeName && strings.EqualFold(p.Src.FullName(), full) {
				targets = append(targets, target{rule, p.Src.FullName()})
				break
			}
		}
	}
	s.mu.Unlock()
	if len(targets) > 0 {
		sort.Slice(targets, func(i, j int) bool { return targets[i].rule < targets[j].rule })
		for _, t := range targets {
			s.enqueueWebhook(t.rule, t.repo)
		}
		return true
	}
	queued := false
	for _, r := range cfg.Rules {
		if r.From.Forge != forgeName {
			continue
		}
		if r.From.Repo != "" {
			// A repo: rule has no listing to learn from: sync just that repo.
			if strings.EqualFold(r.From.Repo, full) {
				s.enqueueWebhook(r.Name, r.From.Repo)
				queued = true
			}
			continue
		}
		if (r.From.Owner != "" && strings.EqualFold(r.From.Owner, ev.Owner)) || r.From.StarredBy != "" {
			s.enqueueScheduled(r.Name)
			queued = true
		}
	}
	return queued
}

// badSigWindow is the minimum gap between "bad signature" log lines per forge.
const badSigWindow = time.Minute

type badSigLog struct {
	mu         sync.Mutex
	last       time.Time
	logged     bool
	suppressed int
}

// logBadSignature logs a rejected delivery at most once per badSigWindow per
// forge (an unauthenticated sender must not be able to flood the log); the
// next line reports how many were suppressed.
func (s *Server) logBadSignature(log *slog.Logger, forgeLabel, remote string) {
	v, _ := s.badSig.LoadOrStore(forgeLabel, &badSigLog{})
	b := v.(*badSigLog)
	now := s.Clock.Now()
	b.mu.Lock()
	if b.logged && now.Sub(b.last) < badSigWindow {
		b.suppressed++
		b.mu.Unlock()
		return
	}
	n := b.suppressed
	b.suppressed, b.last, b.logged = 0, now, true
	b.mu.Unlock()
	if n > 0 {
		log.Warn("server: webhook with bad signature", "remote", remote, "suppressed", n)
		return
	}
	log.Warn("server: webhook with bad signature", "remote", remote)
}
