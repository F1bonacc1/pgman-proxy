// Read handlers — Status, Diagnose (T057). Not leader-only;
// allow_unauth_reads can let them through without a credential.

package control

import (
	"net/http"
	"time"
)

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request, env *requestEnv) {
	engineStart := time.Now()
	st, err := s.engine.Status(r.Context())
	engineLatency := time.Since(engineStart)
	if err != nil {
		s.completeFail(r.Context(), w, r, env, "", CodeEngineError, err.Error(), engineLatency, "")
		return
	}
	// Feature 002 (contracts/observability.md § Status response): augment
	// the engine status with a `cluster.embedded_nats` sub-block when
	// the host has wired an embedded-server snapshot callback. Single-
	// peer test paths that omit the embedded server simply return the
	// engine status verbatim.
	if s.embeddedSnapshot != nil {
		envelope := struct {
			Engine       any `json:"engine"`
			EmbeddedNATS any `json:"embedded_nats"`
		}{
			Engine:       st,
			EmbeddedNATS: s.embeddedSnapshot(),
		}
		s.completeOK(r.Context(), w, r, env, "", envelope, engineLatency)
		return
	}
	s.completeOK(r.Context(), w, r, env, "", st, engineLatency)
}

func (s *Server) handleDiagnose(w http.ResponseWriter, r *http.Request, env *requestEnv) {
	engineStart := time.Now()
	dg, err := s.engine.Diagnose(r.Context())
	engineLatency := time.Since(engineStart)
	if err != nil {
		s.completeFail(r.Context(), w, r, env, "", CodeEngineError, err.Error(), engineLatency, "")
		return
	}
	s.completeOK(r.Context(), w, r, env, "", dg, engineLatency)
}
