// Restart handler — POST /v1/restart (feature 003 / US6).
//
// Two targets:
//
//   - target=postgres: call Engine.RestartPostgres on the LOCAL
//     manager. Matches Promote's "act on the receiving peer" model
//     (the proxy is NOT leader-only for postgres restart in v1).
//   - target=proxy: respond 200 then enter the drain/exit flow that
//     a process supervisor (tini / systemd / k8s) will bring back.
//     Pre-flight rejects if no supervisor was detected.
//
// Audit emission goes through the existing dual-sink pipeline.

package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	pgmanager "github.com/f1bonacc1/pg-manager"
)

// RestartTarget enumerates the documented values of the `target`
// field. Local enum so json.Unmarshal can validate without leaking
// the constants into Engine.
type RestartTarget string

const (
	RestartTargetPostgres RestartTarget = "postgres"
	RestartTargetProxy    RestartTarget = "proxy"
)

// RestartRequest is the wire form of POST /v1/restart.
type RestartRequest struct {
	TargetNode string        `json:"target_node"`
	Target     RestartTarget `json:"target"`
	RequestID  string        `json:"request_id,omitempty"`
}

// ProxySelfTerminator is the contract the receiving peer presents to
// the restart handler: "I'm about to exit; the supervisor will
// bring me back." When nil, restart with target=proxy is refused.
type ProxySelfTerminator interface {
	// SupervisorPresence returns the documented presence label
	// ("none", "tini", "systemd", "process-compose", "kubernetes",
	// "assumed"). "none" means no supervisor detected and
	// proxy.assume_supervised was not set; the handler refuses.
	SupervisorPresence() string
	// LocalNodeID returns the peer's own node id so the handler can
	// validate the request body's target_node before draining.
	LocalNodeID() string
	// SelfTerminate is invoked AFTER the 200 envelope has been
	// flushed to the client. It MUST drain the data plane, close
	// every listener, and call os.Exit(0). Returning from this
	// method without exiting leaves the cluster with a half-dead
	// peer.
	SelfTerminate(ctx context.Context, reason string)
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request, env *requestEnv) {
	body, err := decodeBody[RestartRequest](r)
	if err != nil {
		s.rejectInvalid(r.Context(), w, r, env, err.Error())
		return
	}
	switch body.Target {
	case RestartTargetPostgres:
		s.handleRestartPostgres(w, r, env, body)
	case RestartTargetProxy:
		s.handleRestartProxy(w, r, env, body)
	default:
		s.rejectInvalid(r.Context(), w, r, env, "invalid target: must be 'postgres' or 'proxy'")
	}
}

func (s *Server) handleRestartPostgres(w http.ResponseWriter, r *http.Request, env *requestEnv, body RestartRequest) {
	// target=postgres semantics: restart THIS node's postgres. If
	// target_node is set, it MUST match — otherwise we silently
	// restart the wrong postgres (operator probably meant a
	// different peer's). target_node="" is permitted as a
	// convenience for "restart wherever the request lands".
	if body.TargetNode != "" && s.proxy != nil && body.TargetNode != s.proxy.LocalNodeID() {
		s.refuse(r.Context(), w, r, env.Op, env.ReqID, env.Start, env.Actor,
			CodeWrongPeer, "target_node "+body.TargetNode+" does not match receiving peer "+s.proxy.LocalNodeID())
		return
	}
	engineStart := time.Now()
	err := s.engine.RestartPostgres(r.Context())
	engineLatency := time.Since(engineStart)
	if err != nil {
		s.completeFail(r.Context(), w, r, env, body.TargetNode, CodeEngineError, err.Error(), engineLatency, "")
		return
	}
	result := map[string]any{
		"target":      "postgres",
		"target_node": s.nodeID,
	}
	s.completeOK(r.Context(), w, r, env, body.TargetNode, result, engineLatency)
}

// handleRestartProxy implements the self-terminate flow. The
// receiving peer:
//
//  1. Validates target_node matches itself (else wrong_peer / 400).
//  2. Validates SupervisorPresence != "none" (else
//     supervisor_not_detected / 412).
//  3. Writes the 200 envelope + audit record FIRST.
//  4. Spawns a background goroutine to drain + exit AFTER the
//     response is flushed.
func (s *Server) handleRestartProxy(w http.ResponseWriter, r *http.Request, env *requestEnv, body RestartRequest) {
	if s.proxy == nil {
		s.refuse(r.Context(), w, r, env.Op, env.ReqID, env.Start, env.Actor,
			CodeInternal, "proxy self-terminator not wired")
		return
	}
	localID := s.proxy.LocalNodeID()
	if body.TargetNode == "" || body.TargetNode != localID {
		s.refuse(r.Context(), w, r, env.Op, env.ReqID, env.Start, env.Actor,
			CodeWrongPeer, "target_node "+body.TargetNode+" does not match receiving peer "+localID)
		return
	}
	presence := s.proxy.SupervisorPresence()
	if presence == "none" {
		s.refuse(r.Context(), w, r, env.Op, env.ReqID, env.Start, env.Actor,
			CodeSupervisorNotDetected,
			"no process supervisor detected on this peer; set proxy.assume_supervised=true to override")
		return
	}
	// Accept FIRST: write the envelope + audit record before any
	// drain begins. The operator's last contact with the doomed
	// peer is the success envelope.
	result := map[string]any{
		"target":               "proxy",
		"target_node":          localID,
		"supervisor_presence":  presence,
	}
	s.completeOK(r.Context(), w, r, env, localID, result, 0)
	// Drain + exit on a separate goroutine so the response writer's
	// Close has time to flush. The context here is detached from
	// r.Context() because r.Context() will be cancelled the moment
	// the handler returns.
	go func() {
		// Tiny pause to let the kernel write out the response
		// buffer before the listener socket closes.
		time.Sleep(50 * time.Millisecond)
		drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.proxy.SelfTerminate(drainCtx, "operator_restart")
	}()
}

// decodeBody decodes JSON into T with strict field handling. Returns
// the user-facing error message verbatim — the wrap() middleware
// turns it into a 400 invalid_argument envelope.
func decodeBody[T any](r *http.Request) (T, error) {
	var v T
	if r.Body == nil {
		return v, errors.New("missing request body")
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, err
	}
	return v, nil
}

// Suppress unused-import warning in builds that drop the convenience
// pgmanager reference. (Kept as a tiny seam — RestartRequest may
// grow a NodeID field validated against pgmanager.NodeID.)
var _ = pgmanager.NodeID("")
