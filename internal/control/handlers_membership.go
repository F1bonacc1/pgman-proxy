// Membership-mutation handlers — Switchover, Failover, Promote, Fence,
// Unfence (T058), and the promotion authorization (pg-manager B-025).
// Switchover, Failover, Fence and Unfence are leader-only. Promote is
// refused locally. The promotion authorization is not leader-only: it
// exists for a cluster with no leader, and its record is cluster-wide,
// so any node can write it.
//
// Engine errors map to `engine_error` (HTTP 500). Body decode errors
// map to `invalid_argument` (HTTP 400).

package control

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	pgmanager "github.com/f1bonacc1/pg-manager"
)

// switchoverReq is the body for POST /v1/switchover. RequestID is
// accepted-but-ignored — pgmctl's mutating commands all stamp a
// client-side ULID into the body alongside the X-Request-Id header
// (FR-039 retry-correlation). The server's envelope reuses its own
// server-side ULID, so the body field is informational only.
type switchoverReq struct {
	Target    string `json:"target"`
	RequestID string `json:"request_id,omitempty"`
}

// fenceReq is the body for POST /v1/fence and /v1/unfence. RequestID
// is accepted-but-ignored (see switchoverReq).
type fenceReq struct {
	Target    string `json:"target"`
	RequestID string `json:"request_id,omitempty"`
}

func (s *Server) handleSwitchover(w http.ResponseWriter, r *http.Request, env *requestEnv) {
	if env.Forwarded {
		s.forwardEngineCall(w, r, env)
		return
	}
	var body switchoverReq
	if err := decodeJSON(r, &body); err != nil {
		s.rejectInvalid(r.Context(), w, r, env, "decode body: "+err.Error())
		return
	}
	if body.Target == "" {
		s.rejectInvalid(r.Context(), w, r, env, "target is required")
		return
	}
	engineStart := time.Now()
	err := s.engine.Switchover(r.Context(), pgmanager.NodeID(body.Target))
	s.finishMutation(w, r, env, body.Target, engineStart, err)
}

func (s *Server) handleFailover(w http.ResponseWriter, r *http.Request, env *requestEnv) {
	if env.Forwarded {
		s.forwardEngineCall(w, r, env)
		return
	}
	engineStart := time.Now()
	err := s.engine.Failover(r.Context())
	s.finishMutation(w, r, env, "", engineStart, err)
}

// promoteNotSupported is the refusal POST /v1/promote returns.
const promoteNotSupported = "promote is not supported: pg-manager's Manager.Promote never " +
	"acted on a request, so this endpoint used to report success without promoting. " +
	"To end a join window whose publisher is lost, use POST /v1/promotion-authorization " +
	"(pgmctl promote NODE --accept-data-loss). To move the primary, use POST /v1/switchover " +
	"or POST /v1/failover (both need a leader)"

// handlePromote refuses every request. pg-manager's Manager.Promote
// writes nothing and returns ErrPromoteDeprecated; accepting the call
// used to tell operators a promotion was under way when none was. The
// refusal is audited like any rejected mutation.
func (s *Server) handlePromote(w http.ResponseWriter, r *http.Request, env *requestEnv) {
	s.refuse(r.Context(), w, r, env.Op, env.ReqID, env.Start, env.Actor, CodeNotSupported, promoteNotSupported)
}

// promotionAuthorizationReq is the body for POST
// /v1/promotion-authorization. AcceptDataLoss must be true: the
// operation abandons every write the join-window publisher
// acknowledged that the target lacks. RequestID is accepted but the
// server's ULID is what pg-manager records and every event carries.
type promotionAuthorizationReq struct {
	Target         string `json:"target"`
	Reason         string `json:"reason"`
	TTLSeconds     int    `json:"ttl_seconds,omitempty"`
	AcceptDataLoss bool   `json:"accept_data_loss"`
	Replace        bool   `json:"replace,omitempty"`
	RequestID      string `json:"request_id,omitempty"`
}

// handleAuthorizePromotion writes pg-manager's promotion authorization
// on the receiving node (B-025). The engine checks every precondition
// against the cluster's StateStore; its refusals map to 409 codes the
// CLI can render.
func (s *Server) handleAuthorizePromotion(w http.ResponseWriter, r *http.Request, env *requestEnv) {
	var body promotionAuthorizationReq
	if err := decodeJSON(r, &body); err != nil {
		s.rejectInvalid(r.Context(), w, r, env, "decode body: "+err.Error())
		return
	}
	switch {
	case body.Target == "":
		s.rejectInvalid(r.Context(), w, r, env, "target is required")
		return
	case body.Reason == "":
		s.rejectInvalid(r.Context(), w, r, env, "reason is required")
		return
	case !body.AcceptDataLoss:
		s.rejectInvalid(r.Context(), w, r, env, fmt.Sprintf(
			"accept_data_loss must be true: promoting %s abandons every write the join-window publisher acknowledged that %s has not received",
			body.Target, body.Target))
		return
	case body.TTLSeconds < 0:
		s.rejectInvalid(r.Context(), w, r, env, "ttl_seconds must be >= 0")
		return
	}
	engineStart := time.Now()
	auth, err := s.engine.AuthorizeJoinWindowPromotion(r.Context(), pgmanager.PromotionAuthorizationRequest{
		Target:    pgmanager.NodeID(body.Target),
		Actor:     env.Actor,
		Reason:    body.Reason,
		RequestID: env.ReqID,
		TTL:       time.Duration(body.TTLSeconds) * time.Second,
		Replace:   body.Replace,
	})
	if err != nil {
		s.completeFail(r.Context(), w, r, env, body.Target, promotionErrorCode(err), err.Error(),
			time.Since(engineStart), "")
		return
	}
	s.completeOK(r.Context(), w, r, env, body.Target, auth, time.Since(engineStart))
}

// handleCancelPromotionAuthorization deletes the pending authorization.
// engine_result is the cancelled record, omitted when there was none.
func (s *Server) handleCancelPromotionAuthorization(w http.ResponseWriter, r *http.Request, env *requestEnv) {
	engineStart := time.Now()
	auth, err := s.engine.CancelPromotionAuthorization(r.Context())
	if err != nil {
		s.completeFail(r.Context(), w, r, env, "", CodeEngineError, err.Error(), time.Since(engineStart), "")
		return
	}
	if auth == nil {
		s.completeOK(r.Context(), w, r, env, "", nil, time.Since(engineStart))
		return
	}
	s.completeOK(r.Context(), w, r, env, string(auth.Target), auth, time.Since(engineStart))
}

// promotionErrorCode maps AuthorizeJoinWindowPromotion's errors to
// documented codes.
func promotionErrorCode(err error) string {
	switch {
	case errors.Is(err, pgmanager.ErrConfigInvalid):
		return CodeInvalidArgument
	case errors.Is(err, pgmanager.ErrNoJoinWindow):
		return CodeNoJoinWindow
	case errors.Is(err, pgmanager.ErrPromotionAuthorizationPending):
		return CodePromotionAuthorizationPending
	case errors.Is(err, pgmanager.ErrTargetIsPublisher),
		errors.Is(err, pgmanager.ErrTargetNotEligible),
		errors.Is(err, pgmanager.ErrPublisherNotSilent),
		errors.Is(err, pgmanager.ErrTargetIdentityMismatch),
		errors.Is(err, pgmanager.ErrTargetBehind):
		return CodePromotionRefused
	}
	return CodeEngineError
}

func (s *Server) handleFence(w http.ResponseWriter, r *http.Request, env *requestEnv) {
	if env.Forwarded {
		s.forwardEngineCall(w, r, env)
		return
	}
	var body fenceReq
	if err := decodeJSON(r, &body); err != nil {
		s.rejectInvalid(r.Context(), w, r, env, "decode body: "+err.Error())
		return
	}
	if body.Target == "" {
		s.rejectInvalid(r.Context(), w, r, env, "target is required")
		return
	}
	engineStart := time.Now()
	err := s.engine.Fence(r.Context(), pgmanager.NodeID(body.Target))
	s.finishMutation(w, r, env, body.Target, engineStart, err)
}

func (s *Server) handleUnfence(w http.ResponseWriter, r *http.Request, env *requestEnv) {
	if env.Forwarded {
		s.forwardEngineCall(w, r, env)
		return
	}
	var body fenceReq
	if err := decodeJSON(r, &body); err != nil {
		s.rejectInvalid(r.Context(), w, r, env, "decode body: "+err.Error())
		return
	}
	if body.Target == "" {
		s.rejectInvalid(r.Context(), w, r, env, "target is required")
		return
	}
	engineStart := time.Now()
	err := s.engine.Unfence(r.Context(), pgmanager.NodeID(body.Target))
	s.finishMutation(w, r, env, body.Target, engineStart, err)
}

// finishMutation writes the accepted/failed envelope + audit. Common
// across every membership op.
func (s *Server) finishMutation(w http.ResponseWriter, r *http.Request, env *requestEnv,
	target string, engineStart time.Time, err error,
) {
	engineLatency := time.Since(engineStart)
	if err != nil {
		s.completeFail(r.Context(), w, r, env, target, CodeEngineError, err.Error(),
			engineLatency, "")
		return
	}
	s.completeOK(r.Context(), w, r, env, target, nil, engineLatency)
}

// forwardEngineCall publishes the original request body via the
// LeaderRouter and copies the leader's reply back to the caller. Maps
// timeout to `leader_route_timeout` per FR-034.
func (s *Server) forwardEngineCall(w http.ResponseWriter, r *http.Request, env *requestEnv) {
	leaderID := s.router.LeaderID()
	body, err := readBoundedBody(r, 64<<10)
	if err != nil {
		s.rejectInvalid(r.Context(), w, r, env, "read body: "+err.Error())
		return
	}
	// Forward the originating client's identity + trace context so the
	// leader's audit record correlates with the forwarder's.
	tp := traceFromContext(r.Context())
	var traceparent string
	if tp.HasTrace() {
		traceparent = tp.Header()
	}
	engineStart := time.Now()
	reply, err := s.router.Forward(r.Context(), env.Op, env.ReqID, env.Actor, traceparent, body)
	engineLatency := time.Since(engineStart)
	if err != nil {
		if errors.Is(err, ErrLeaderRouteTimeout) {
			s.completeFail(r.Context(), w, r, env, "", CodeLeaderRouteTimeout,
				"forward to leader timed out", engineLatency, leaderID)
			return
		}
		s.completeFail(r.Context(), w, r, env, "", CodeEngineError, err.Error(),
			engineLatency, leaderID)
		return
	}
	// The leader's reply is already a fully-formed envelope; copy it
	// through verbatim. Local audit records the forwarded request so
	// every peer's audit trail captures every request.
	s.completeOK(r.Context(), w, r, env, "", rawJSON(reply), engineLatency)
}

// rawJSON is a marker used by the envelope to splice an already-encoded
// JSON payload (the leader's reply) into the local response without
// double-encoding.
type rawJSON []byte

// MarshalJSON makes rawJSON satisfy json.Marshaler; the value is emitted
// verbatim.
func (r rawJSON) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return []byte("null"), nil
	}
	return []byte(r), nil
}

// readBoundedBody returns the request body capped at maxBytes.
func readBoundedBody(r *http.Request, maxBytes int64) ([]byte, error) {
	body := http.MaxBytesReader(nil, r.Body, maxBytes)
	defer body.Close() //nolint:errcheck
	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 1024)
	for {
		n, err := body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			if err.Error() == "EOF" {
				return buf, nil
			}
			return nil, err
		}
	}
}
