// Package control implements the LCM HTTP control plane (FR-021..FR-034).
//
// All LCM logic lives in `../pg-manager`'s `manager.Manager`. This
// package is a thin scaffold: HTTP routing, request-ID tagging,
// bearer-token auth (with hot rotation), dual-sink audit (slog + NATS),
// leader-routing (forward via NATS req/reply OR 307 redirect), TLS
// bring-up, and JSON response encoding.
//
// Constitutional invariants preserved here:
//   - Wire-protocol fidelity (I): the data-plane proxy is untouched.
//   - Fail-closed safety (II): every uncertain state returns a documented
//     non-200 response; no operation is silently skipped.
//   - Active/active correctness (III): mutating ops route to the leader.
//   - Thin scaffold (IV): no LCM mechanics live here — only request
//     plumbing.
//   - Stable observability (V): metric names and log-event names match
//     contracts/observability.md exactly.
package control

import (
	"context"
	"net/http"

	pgmanager "github.com/f1bonacc1/pg-manager"
	"github.com/f1bonacc1/pg-manager/manager"
	"github.com/f1bonacc1/pg-manager/upgrade"
)

// Engine is the slice of `manager.Manager` the control plane needs.
// Carving an interface here lets tests substitute a fake without
// standing up the full pg-manager stack and mirrors the constitutional
// "thin scaffold" rule — handlers MUST NOT touch any other engine
// surface than what's declared here (Constitution IV).
type Engine interface {
	Status(ctx context.Context) (pgmanager.Status, error)
	Diagnose(ctx context.Context) (pgmanager.Diagnosis, error)
	Switchover(ctx context.Context, target pgmanager.NodeID) error
	Failover(ctx context.Context) error
	Fence(ctx context.Context, target pgmanager.NodeID) error
	Unfence(ctx context.Context, target pgmanager.NodeID) error
	Promote(ctx context.Context) error
	UpdateTopology(ctx context.Context, t pgmanager.Topology, p pgmanager.Policy) error
	TriggerBackup(ctx context.Context) (pgmanager.BackupID, error)
	PrepareUpgrade(ctx context.Context, plan pgmanager.UpgradePlan) error
	ExecuteUpgrade(ctx context.Context, plan pgmanager.UpgradePlan, preSwap upgrade.PreSwap) error
	// Feature 003 / US6: operator-triggered restart of the LOCAL
	// PostgreSQL process. Returns the executor's error verbatim so
	// the handler can map to the right audit outcome.
	RestartPostgres(ctx context.Context) error
}

// Compile-time assertion: *manager.Manager satisfies Engine.
var _ Engine = (*manager.Manager)(nil)

// LeaderState reports whether THIS peer is currently the cluster leader
// and, when not, who is. The `leader_at_request` audit field (FR-034)
// is sourced from this view at the moment a forward is published.
type LeaderState interface {
	IsLeader() bool
	LeaderID() string   // empty when unknown
	LeaderAddr() string // host:port of the leader's control plane, "" if unknown
}

// AuditSink is the dual-sink audit emitter. The control plane MUST
// require both sinks to succeed before mutating ops are accepted
// (FR-028). The slog sink is the in-process structured-log writer; the
// NATS sink publishes on `pgman_proxy.<cluster_id>.audit.lcm`.
type AuditSink interface {
	Emit(ctx context.Context, rec AuditRecord) error
}

// AuditRecord is the JSON shape emitted on both audit sinks
// (contracts/lcm.md § Audit record).
type AuditRecord struct {
	Time            string `json:"time"`
	RequestID       string `json:"request_id"`
	Operation       string `json:"operation"`
	Target          string `json:"target,omitempty"`
	Actor           string `json:"actor"`
	SourceAddr      string `json:"source_addr"`
	Outcome         string `json:"outcome"`
	EngineLatencyMS int64  `json:"engine_latency_ms,omitempty"`
	TotalLatencyMS  int64  `json:"total_latency_ms"`
	ErrorCode       string `json:"error_code,omitempty"`
	ClusterID       string `json:"cluster_id"`
	NodeID          string `json:"node_id"`
	TraceID         string `json:"trace_id,omitempty"`
	SpanID          string `json:"span_id,omitempty"`
	// LeaderAtRequest is populated only on `forward`-mode requests; it
	// names the peer this node believed was leader at the moment the
	// forward was published (FR-034).
	LeaderAtRequest string `json:"leader_at_request,omitempty"`
}

// errorEnvelope is the JSON `error` block of a non-accepted response.
// Documented in contracts/lcm.md § Response envelope.
type errorEnvelope struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// envelope is the response wrapper for every non-redirect endpoint.
type envelope struct {
	Operation    string         `json:"operation"`
	RequestID    string         `json:"request_id"`
	Outcome      string         `json:"outcome"`
	EngineResult any            `json:"engine_result,omitempty"`
	Error        *errorEnvelope `json:"error,omitempty"`
}

// Outcome label constants (audit + envelope).
const (
	OutcomeAccepted = "accepted"
	OutcomeRejected = "rejected"
	OutcomeFailed   = "failed"
)

// Documented error codes (contracts/lcm.md § Error codes). Stable per
// Constitution V — renames are MINOR-version events.
const (
	CodeAuthRequired           = "auth_required"
	CodeAuthInvalid            = "auth_invalid"
	CodeNotLeader              = "not_leader"
	CodeClusterBootstrapping   = "cluster_bootstrapping"
	CodeLeadershipInTransition = "leadership_in_transition"
	CodeBackupExecutorMissing  = "backup_executor_missing"
	CodeAuditUnavailable       = "audit_unavailable"
	CodeEngineError            = "engine_error"
	CodeInvalidArgument        = "invalid_argument"
	CodeLeaderRouteTimeout     = "leader_route_timeout"
	// CodeAdvisoryOnly surfaces 003 § 2 — POST /v1/doctor/fix called
	// with a fix whose blast_radius is advisory has nothing to apply.
	CodeAdvisoryOnly = "advisory_only"
	CodeInternal     = "internal"

	// 003 / US6 — restart + setconfig codes
	// (contracts/control-plane-extensions.md § Error codes added by 003).
	CodeSupervisorNotDetected = "supervisor_not_detected"
	CodeWrongPeer             = "wrong_peer"
	CodeSetConfigKeyDisallowed = "set_config_key_disallowed"
)

// httpStatusForCode maps the documented error codes to their HTTP
// status. Codes that aren't supposed to surface on the response (e.g.
// `not_leader` is encoded as a 307 directly) map to 500 here as a
// conservative default.
func httpStatusForCode(code string) int {
	switch code {
	case CodeAuthRequired:
		return http.StatusUnauthorized
	case CodeAuthInvalid:
		return http.StatusForbidden
	case CodeClusterBootstrapping:
		return http.StatusConflict
	case CodeLeadershipInTransition, CodeAuditUnavailable:
		return http.StatusServiceUnavailable
	case CodeBackupExecutorMissing, CodeAdvisoryOnly, CodeSupervisorNotDetected:
		return http.StatusPreconditionFailed
	case CodeInvalidArgument, CodeWrongPeer, CodeSetConfigKeyDisallowed:
		return http.StatusBadRequest
	case CodeLeaderRouteTimeout:
		return http.StatusGatewayTimeout
	}
	return http.StatusInternalServerError
}
