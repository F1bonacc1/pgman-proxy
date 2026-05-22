// PrepareUpgrade + ExecuteUpgrade handlers (T061).
//
// PrepareUpgrade is a dry run: it surfaces every reason the plan would
// be rejected without committing to anything. ExecuteUpgrade runs the
// local-node steps of plan; cross-node orchestration (visit
// EffectiveOrder, switchover before the primary's turn) is the host's
// responsibility (see upgrade.EffectiveOrder).
//
// `pre_swap` is the host-supplied callback that swaps binaries on
// disk. v1 wires only the no-op pre-swap (the actual binary swap is a
// host concern outside HTTP); future revisions may serialise an opaque
// payload that names a host-installed pre-swap implementation.

package control

import (
	"context"
	"net/http"
	"time"

	pgmanager "github.com/f1bonacc1/pg-manager"
)

// upgradeReq is the body shape for /v1/upgrade/prepare and
// /v1/upgrade/execute. RequestID is accepted-but-ignored — pgmctl
// stamps a client-side ULID into the body alongside the X-Request-Id
// header (FR-039); the server's envelope reuses its own ULID.
type upgradeReq struct {
	Plan      pgmanager.UpgradePlan `json:"plan"`
	RequestID string                `json:"request_id,omitempty"`
}

func (s *Server) handlePrepareUpgrade(w http.ResponseWriter, r *http.Request, env *requestEnv) {
	if env.Forwarded {
		s.forwardEngineCall(w, r, env)
		return
	}
	var body upgradeReq
	if err := decodeJSON(r, &body); err != nil {
		s.rejectInvalid(r.Context(), w, r, env, "decode body: "+err.Error())
		return
	}
	engineStart := time.Now()
	err := s.engine.PrepareUpgrade(r.Context(), body.Plan)
	s.finishMutation(w, r, env, "", engineStart, err)
}

func (s *Server) handleExecuteUpgrade(w http.ResponseWriter, r *http.Request, env *requestEnv) {
	if env.Forwarded {
		s.forwardEngineCall(w, r, env)
		return
	}
	var body upgradeReq
	if err := decodeJSON(r, &body); err != nil {
		s.rejectInvalid(r.Context(), w, r, env, "decode body: "+err.Error())
		return
	}
	// v1: no-op pre-swap. Custom binaries override this hook in
	// out-of-tree pgman-proxy builds; the HTTP surface deliberately
	// doesn't accept arbitrary pre-swap bytes (Constitution VII —
	// don't expand the trust boundary).
	preSwap := func(_ context.Context) error { return nil }
	engineStart := time.Now()
	err := s.engine.ExecuteUpgrade(r.Context(), body.Plan, preSwap)
	s.finishMutation(w, r, env, "", engineStart, err)
}
