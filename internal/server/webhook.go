package server

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"

	"gitmirror/internal/forge"
)

// handleWebhook serves POST /webhook/{forge}. Responses never echo config
// details or parse errors; details go to the debug log (no secrets).
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("forge")
	cfg := s.App.Config
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
	wp, ok := s.App.Providers[name].(forge.WebhookProvider)
	if !ok {
		count(whIgnored)
		text(w, http.StatusNotFound, "not found")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
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
		log.Warn("server: webhook with bad signature", "remote", r.RemoteAddr)
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
	if s.route(name, ev) {
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
// up new repositories.
func (s *Server) route(forgeName string, ev *forge.Event) bool {
	full := ev.Owner + "/" + ev.Name
	type target struct{ rule, repo string }
	var targets []target
	s.mu.Lock()
	for rule, pairs := range s.lastResolve {
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
	for _, r := range s.App.Config.Rules {
		if r.From.Forge != forgeName {
			continue
		}
		if (r.From.Owner != "" && strings.EqualFold(r.From.Owner, ev.Owner)) || r.From.StarredBy != "" {
			s.enqueueScheduled(r.Name)
			queued = true
		}
	}
	return queued
}
