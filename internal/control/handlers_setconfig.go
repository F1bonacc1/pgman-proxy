// SetConfig handler — POST /v1/config/set (feature 003 / US6).
//
// SIGHUP-trigger wrapper for the 002 hot-reload allow-list. The
// endpoint validates the requested key against the allow-list,
// invokes the host-provided Reloader (which re-reads YAML/env and
// applies the diff via embedded.Reload), then emits an audit record.
//
// The set of keys the operator may declare changed is:
//
//   - cluster.route_peers — the route-list for the embedded NATS
//     cluster (RD-001a).
//   - cluster.password — the cluster credential's secret handle
//     (env var or file path), rotated out-of-band.
//
// Anything else returns 400 set_config_key_disallowed (FR-014a).
// The endpoint MUST NOT carry the literal value — operators stage
// the change in YAML / env / secret store and call this to trigger
// the reload.

package control

import (
	"context"
	"net/http"
	"time"
)

// Reloader is the host-provided trigger for a SIGHUP-equivalent
// in-process reload pass. Implementations re-read YAML / env, compute
// the diff against the running embedded NATS server, and apply only
// allow-listed changes (route-peers / cluster password). Returns the
// error verbatim so the handler can map to engine_error / 5xx.
type Reloader interface {
	Reload(ctx context.Context) error
}

// SetConfigRequest is the wire form of POST /v1/config/set.
type SetConfigRequest struct {
	Key       string `json:"key"`
	RequestID string `json:"request_id,omitempty"`
}

// setConfigAllowList is the closed set of keys accepted by
// /v1/config/set (003 / contracts/control-plane-extensions.md
// § Error codes added by 003).
var setConfigAllowList = map[string]bool{
	"cluster.route_peers": true,
	"cluster.password":    true,
}

func (s *Server) handleSetConfig(w http.ResponseWriter, r *http.Request, env *requestEnv) {
	body, err := decodeBody[SetConfigRequest](r)
	if err != nil {
		s.rejectInvalid(r.Context(), w, r, env, err.Error())
		return
	}
	if body.Key == "" {
		s.rejectInvalid(r.Context(), w, r, env, "missing field 'key'")
		return
	}
	if !setConfigAllowList[body.Key] {
		s.refuse(r.Context(), w, r, env.Op, env.ReqID, env.Start, env.Actor,
			CodeSetConfigKeyDisallowed,
			"key "+body.Key+" is not in the hot-reload allow-list (peer routes + cluster password only)")
		return
	}
	if s.reloader == nil {
		s.completeFail(r.Context(), w, r, env, body.Key, CodeInternal,
			"reloader not wired", 0, "")
		return
	}
	engineStart := time.Now()
	err = s.reloader.Reload(r.Context())
	engineLatency := time.Since(engineStart)
	if err != nil {
		s.completeFail(r.Context(), w, r, env, body.Key, CodeEngineError, err.Error(), engineLatency, "")
		return
	}
	result := map[string]any{
		"key":     body.Key,
		"applied": true,
	}
	s.completeOK(r.Context(), w, r, env, body.Key, result, engineLatency)
}
