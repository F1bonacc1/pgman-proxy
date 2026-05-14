// History query handler — GET /v1/history (feature 003 / FR-016a).
//
// Wraps internal/history.Run via the HistoryQuerier interface so the
// control package doesn't take a hard JetStream dependency: runtime
// supplies the closure that consumes a live JetStream context.

package control

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/f1bonacc1/pgman-proxy/internal/history"
)

// HistoryQuerier abstracts the JetStream-backed history lookup so the
// control plane stays clean across the two construction paths used in
// tests (no JetStream) and in production (real JetStream).
//
// Production wiring: pass a closure that calls history.Run(ctx, js,
// cfg.ClusterID, q) — see runtime/start.go.
//
// Test wiring: supply a stub that returns canned results.
type HistoryQuerier interface {
	Query(ctx context.Context, q history.Query) (history.Result, error)
}

func (s *Server) handleHistoryQuery(w http.ResponseWriter, r *http.Request, env *requestEnv) {
	if s.history == nil {
		// History stream not wired (single-peer dev path); answer
		// with an empty result so pgmctl's `events` command renders
		// "no events" cleanly rather than 500-ing.
		s.completeOK(r.Context(), w, r, env, "", history.Result{
			APIVersion: "pgman-proxy/v1",
			Kind:       "HistoryQueryResult",
		}, 0)
		return
	}

	q, code, errMsg := parseHistoryQuery(r)
	if errMsg != "" {
		engineLatency := time.Duration(0)
		s.completeFail(r.Context(), w, r, env, "", code, errMsg, engineLatency, "")
		return
	}

	engineStart := time.Now()
	res, err := s.history.Query(r.Context(), q)
	engineLatency := time.Since(engineStart)
	if err != nil {
		s.completeFail(r.Context(), w, r, env, "", CodeEngineError, err.Error(), engineLatency, "")
		return
	}
	s.completeOK(r.Context(), w, r, env, "", res, engineLatency)
}

// parseHistoryQuery validates and normalises the query string. Returns
// (q, CodeInvalidArgument, "<reason>") on validation failure.
func parseHistoryQuery(r *http.Request) (history.Query, string, string) {
	values := r.URL.Query()
	q := history.Query{
		Limit: 1000,
	}

	if v := values.Get("since"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return q, CodeInvalidArgument, "invalid since: " + err.Error()
		}
		if d < 0 {
			return q, CodeInvalidArgument, "since must be non-negative"
		}
		q.Since = d
	} else {
		// Default window matches contracts/cli-commands.md § events.
		q.Since = 30 * time.Minute
	}

	if v := values.Get("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return q, CodeInvalidArgument, "invalid until: " + err.Error()
		}
		q.Until = t
	}

	if v := values.Get("category"); v != "" {
		switch v {
		case "event":
			q.Category = history.CategoryEvent
		case "audit":
			q.Category = history.CategoryAudit
		case "":
			// both
		default:
			return q, CodeInvalidArgument, "invalid category " + v + " (want event|audit)"
		}
	}

	for _, t := range values["type"] {
		if t = strings.TrimSpace(t); t != "" {
			q.Types = append(q.Types, t)
		}
	}
	for _, n := range values["node"] {
		if n = strings.TrimSpace(n); n != "" {
			q.Nodes = append(q.Nodes, n)
		}
	}

	if v := values.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return q, CodeInvalidArgument, "invalid limit: must be a non-negative integer"
		}
		if n > 0 {
			q.Limit = n
		}
	}

	if v := values.Get("cursor"); v != "" {
		q.Cursor = v
	}
	return q, "", ""
}
