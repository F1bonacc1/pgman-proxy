// UpdateTopology handler (T059). Leader-only; routes through forward
// or 307 redirect when received at a non-leader.

package control

import (
	"net/http"
	"time"

	pgmanager "github.com/f1bonacc1/pg-manager"
)

// updateTopologyReq is the body shape for POST /v1/topology. Topology
// and Policy use pg-manager's wire shapes verbatim — renames upstream
// propagate as MINOR-version events here.
type updateTopologyReq struct {
	Topology pgmanager.Topology `json:"topology"`
	Policy   pgmanager.Policy   `json:"policy"`
}

func (s *Server) handleUpdateTopology(w http.ResponseWriter, r *http.Request, env *requestEnv) {
	if env.Forwarded {
		s.forwardEngineCall(w, r, env)
		return
	}
	var body updateTopologyReq
	if err := decodeJSON(r, &body); err != nil {
		s.rejectInvalid(r.Context(), w, r, env, "decode body: "+err.Error())
		return
	}
	engineStart := time.Now()
	err := s.engine.UpdateTopology(r.Context(), body.Topology, body.Policy)
	s.finishMutation(w, r, env, string(body.Topology.NodeID), engineStart, err)
}
