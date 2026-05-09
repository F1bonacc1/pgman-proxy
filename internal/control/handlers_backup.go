// TriggerBackup handler (T060). Maps the engine's
// ErrBackupNotConfigured sentinel to the documented
// `backup_executor_missing` error code (FR-030, HTTP 412).

package control

import (
	"errors"
	"net/http"
	"time"

	pgmanager "github.com/f1bonacc1/pg-manager"
)

// triggerBackupResp is the engine_result body for /v1/backup.
type triggerBackupResp struct {
	BackupID string `json:"backup_id"`
}

func (s *Server) handleTriggerBackup(w http.ResponseWriter, r *http.Request, env *requestEnv) {
	if env.Forwarded {
		s.forwardEngineCall(w, r, env)
		return
	}
	engineStart := time.Now()
	id, err := s.engine.TriggerBackup(r.Context())
	engineLatency := time.Since(engineStart)
	if err != nil {
		if errors.Is(err, pgmanager.ErrBackupNotConfigured) {
			s.completeFail(r.Context(), w, r, env, "",
				CodeBackupExecutorMissing,
				"no BackupExecutor wired; configure backup.driver per FR-030",
				engineLatency, "")
			return
		}
		s.completeFail(r.Context(), w, r, env, "", CodeEngineError, err.Error(),
			engineLatency, "")
		return
	}
	s.completeOK(r.Context(), w, r, env, "", triggerBackupResp{BackupID: string(id)}, engineLatency)
}
