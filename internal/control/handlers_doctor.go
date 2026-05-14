// Doctor endpoint handlers (T073).
//
// Three endpoints per contracts/control-plane-extensions.md § 2:
//
//   GET  /v1/doctor/checks  — return the server-published catalog
//   POST /v1/doctor/run     — execute one or all checks; return the report
//   POST /v1/doctor/fix     — apply a named fix
//
// `run` is audited but read-only (no mutation; FR-027 invariant
// enforced by doctor_checks_readonly_test.go). `fix` is mutating —
// for v1 we accept the request, but every registered fix is
// blast_radius=advisory until the apply path lands in US6, so the
// handler always responds 412 with `advisory_only` per the contract.

package control

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	pgmanager "github.com/f1bonacc1/pg-manager"
)

// enrichedEngine wraps the base Engine with a PeerAggregator so the
// doctor checks see the same cluster-wide Status snapshot that
// GET /v1/status returns. Without this shim, Engine.Status would
// return only the per-peer scalar view from pg-manager and every
// "look across peers" check would degenerate to a single-node lens
// (Instances empty, PrimaryNodeID empty, etc.). Bug surface fixed
// post-T071 once the inconsistency between `pgmctl health` and
// `pgmctl doctor` was observed in the live fixture.
type enrichedEngine struct {
	Engine
	agg PeerAggregator
}

// Status overrides the base Engine.Status: fetch the raw per-peer
// snapshot, then enrich it via the aggregator (when wired). Any
// EnrichStatus internal error returns the raw view unchanged — same
// fail-soft contract used by handleStatus.
func (e *enrichedEngine) Status(ctx context.Context) (pgmanager.Status, error) {
	st, err := e.Engine.Status(ctx)
	if err != nil {
		return st, err
	}
	if e.agg != nil {
		st = e.agg.EnrichStatus(ctx, st)
	}
	return st, nil
}

// docFixRequest is the wire body of POST /v1/doctor/fix.
type docFixRequest struct {
	Fix       string         `json:"fix"`
	Args      map[string]any `json:"args"`
	RequestID string         `json:"request_id"`
}

// handleDoctorChecks serves GET /v1/doctor/checks.
func (s *Server) handleDoctorChecks(w http.ResponseWriter, r *http.Request, env *requestEnv) {
	body := DoctorChecksResponse{
		APIVersion: "pgman-proxy/v1",
		Kind:       "DoctorChecks",
		Checks:     catalogFromChecks(DefaultDoctorChecks()),
	}
	s.completeOK(r.Context(), w, r, env, "", body, 0)
}

// docRunRequest is the wire body of POST /v1/doctor/run. `check` MAY
// be omitted (or empty) to run the full catalog.
type docRunRequest struct {
	Check string `json:"check"`
}

// handleDoctorRun serves POST /v1/doctor/run. Audited via the standard
// LCM envelope (the audit record carries operation=DoctorRun); the
// CheckFunc closures are required by FR-027 to be read-only.
func (s *Server) handleDoctorRun(w http.ResponseWriter, r *http.Request, env *requestEnv) {
	var body docRunRequest
	// Empty body == run all. parseJSONBody returns a typed error for
	// genuine decode failures; absent body is allowed.
	if r.ContentLength != 0 && r.Body != nil {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			s.completeFail(r.Context(), w, r, env, "", CodeInvalidArgument, "decode body: "+err.Error(), 0, "")
			return
		}
	}

	catalog := DefaultDoctorChecks()
	selected := catalog
	if body.Check != "" {
		match := findCheckByName(catalog, body.Check)
		if match == nil {
			s.completeFail(r.Context(), w, r, env, "", CodeInvalidArgument, "unknown check: "+body.Check, 0, "")
			return
		}
		selected = []NamedCheck{*match}
	}

	engineStart := time.Now()
	now := engineStart.UTC()
	runCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	results := runChecks(runCtx, &enrichedEngine{Engine: s.engine, agg: s.aggregator}, selected)
	engineLatency := time.Since(engineStart)

	report := DoctorReport{
		APIVersion: "pgman-proxy/v1",
		Kind:       "DoctorReport",
		CapturedAt: now,
		Summary:    summariseChecks(results),
		Checks:     results,
	}
	s.completeOK(r.Context(), w, r, env, "", report, engineLatency)
}

// handleDoctorFix serves POST /v1/doctor/fix. For the v1 doctor
// registry every published fix is blast_radius=advisory, so this
// handler always returns 412 `advisory_only` per the contract.
// Replaced by a real apply dispatch once /v1/restart + LCM-wrapper
// fixes land in US6.
func (s *Server) handleDoctorFix(w http.ResponseWriter, r *http.Request, env *requestEnv) {
	var body docFixRequest
	if r.ContentLength != 0 && r.Body != nil {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			s.completeFail(r.Context(), w, r, env, "", CodeInvalidArgument, "decode body: "+err.Error(), 0, "")
			return
		}
	}
	if body.Fix == "" {
		s.completeFail(r.Context(), w, r, env, "", CodeInvalidArgument, "missing fix name", 0, "")
		return
	}
	// 412 Precondition Failed + advisory_only per
	// control-plane-extensions.md § 2 / SuggestedFix.blast_radius.
	// httpStatusForCode maps CodeAdvisoryOnly to 412 automatically.
	s.completeFail(r.Context(), w, r, env, body.Fix,
		CodeAdvisoryOnly,
		"fix \""+body.Fix+"\" is advisory-only in this build (no apply path; recommendation only)",
		0, "")
}

// findCheckByName is a linear lookup over the registry — N is small
// (≤ 20) so a map adds more cost than it saves.
func findCheckByName(catalog []NamedCheck, name string) *NamedCheck {
	for i := range catalog {
		if catalog[i].Name == name {
			return &catalog[i]
		}
	}
	return nil
}
