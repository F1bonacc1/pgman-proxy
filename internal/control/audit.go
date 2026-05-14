// Dual-sink audit emitter (T055 / FR-027 / FR-028).
//
// Every LCM request MUST appear on BOTH sinks before the engine runs
// for a mutating op. If either sink fails to accept the record, the
// request is rejected with `audit_unavailable` (FR-028). Read-only ops
// (Status / Diagnose) audit best-effort; their semantics are weaker so
// a transient NATS hiccup doesn't black-hole observability calls.
//
// Sinks:
//   * slog — the in-process structured logger; backed by os.Stderr.
//     A failure here means the slog writer is down (rare; usually
//     stderr is closed).
//   * NATS — `pgman_proxy.<cluster_id>.audit.lcm`. A failure here means
//     the NATS connection is broken or the request couldn't be
//     serialised.

package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"

	pgmanager "github.com/f1bonacc1/pg-manager"

	"github.com/f1bonacc1/pgman-proxy/internal/history"
	"github.com/f1bonacc1/pgman-proxy/internal/obs"
)

// NATSPublisher is the slice of *nats.Conn the audit emitter needs.
// Carving an interface lets tests inject a fake without booting NATS.
type NATSPublisher interface {
	Publish(subject string, data []byte) error
}

// HistoryPublisher is the slice of *history.Publisher the audit
// emitter needs. Carved into an interface so tests can inject a fake
// without booting JetStream; the concrete *history.Publisher satisfies
// it natively.
type HistoryPublisher interface {
	PublishEvent(ctx context.Context, category history.Category, evType, nodeID string, details map[string]any) (string, error)
}

// Audit is the multi-sink emitter. Construct via NewAudit; pass into
// every handler. Feature 003 added a third sink (the cluster's history
// JetStream); a publish failure on EITHER NATS-subject OR history sink
// trips the fail-closed gate (FR-028).
type Audit struct {
	subject string
	logger  *obs.Logger
	bus     NATSPublisher
	history HistoryPublisher
	metrics *obs.MetricSet

	// natsFailures counts back-to-back NATS publish failures. The
	// fail-closed gate (FR-028) trips whenever this is >0; a successful
	// publish resets it. We track it as a counter for the metric and
	// as a boolean for the gate.
	natsFailures atomic.Uint64

	// historyFailures mirrors natsFailures for the history-stream sink.
	// Feature 003: GET /v1/history is the operator-visible audit trail,
	// so a publish failure here is just as fatal to "everyone can see
	// every mutating op" as a NATS-subject failure.
	historyFailures atomic.Uint64
}

// NewAudit constructs an Audit emitter targeting the documented
// subject for the supplied cluster ID.
func NewAudit(clusterID string, logger *obs.Logger, bus NATSPublisher, metrics *obs.MetricSet) *Audit {
	return &Audit{
		subject: "pgman_proxy." + clusterID + ".audit.lcm",
		logger:  logger,
		bus:     bus,
		metrics: metrics,
	}
}

// WithHistory wires the cluster history publisher as a third audit
// sink. nil is accepted (tests that don't boot JetStream skip it). Must
// be called before the first Emit if used; the field is read without
// locking on the publish hot path.
func (a *Audit) WithHistory(p HistoryPublisher) *Audit {
	a.history = p
	return a
}

// Emit writes the record to both sinks. Returns the FIRST sink error
// encountered so the caller can decide fail-closed semantics. The
// metric counter is incremented per failure regardless of whether the
// caller treats the failure as fatal.
func (a *Audit) Emit(_ context.Context, rec AuditRecord) error {
	var firstErr error

	// slog sink — structured-log line at level info / warn / error
	// PLUS the documented per-request `control_plane request` event
	// (contracts/observability.md § Required event names).
	a.logSlog(rec)
	a.logControlPlaneRequest(rec)

	// NATS sink — JSON-encoded message on the documented subject.
	payload, err := json.Marshal(rec)
	if err != nil {
		a.bumpFailure("slog")
		// Marshal failure means the record itself is malformed — bug,
		// not infra issue. Log it loud and propagate.
		a.logger.Error("lcm audit emit failed",
			pgmanager.Field{Key: "sink", Value: "slog"},
			pgmanager.Field{Key: "request_id", Value: rec.RequestID},
			pgmanager.Field{Key: "error", Value: err.Error()})
		firstErr = fmt.Errorf("audit marshal: %w", err)
	} else if a.bus != nil { //nolint:gocritic // explicit: marshal-ok branch only publishes when bus is wired
		if err := a.bus.Publish(a.subject, payload); err != nil {
			a.bumpFailure("nats")
			a.logger.Error("lcm audit emit failed",
				pgmanager.Field{Key: "sink", Value: "nats"},
				pgmanager.Field{Key: "request_id", Value: rec.RequestID},
				pgmanager.Field{Key: "subject", Value: a.subject},
				pgmanager.Field{Key: "error", Value: err.Error()})
			firstErr = fmt.Errorf("audit nats: %w", err)
		} else {
			a.natsFailures.Store(0)
		}
	}

	// History sink — appends to the cluster's JetStream history stream
	// so GET /v1/history (003 FR-007) sees every mutating op. Wired
	// only when a publisher was attached via WithHistory.
	if a.history != nil {
		details := auditRecordToDetails(rec)
		if _, herr := a.history.PublishEvent(context.Background(), history.CategoryAudit, "lcm_audit", rec.NodeID, details); herr != nil {
			a.bumpHistoryFailure()
			a.logger.Error("lcm audit emit failed",
				pgmanager.Field{Key: "sink", Value: "history"},
				pgmanager.Field{Key: "request_id", Value: rec.RequestID},
				pgmanager.Field{Key: "error", Value: herr.Error()})
			if firstErr == nil {
				firstErr = fmt.Errorf("audit history: %w", herr)
			}
		} else {
			a.historyFailures.Store(0)
		}
	}
	return firstErr
}

// auditRecordToDetails projects an AuditRecord onto the
// HistoryEvent.Details map. The history stream is operator-visible so
// we publish every non-empty field — operators reading
// GET /v1/history want the same shape they'd see in slog.
func auditRecordToDetails(rec AuditRecord) map[string]any {
	d := map[string]any{
		"request_id": rec.RequestID,
		"operation":  rec.Operation,
		"actor":      rec.Actor,
		"outcome":    rec.Outcome,
	}
	if rec.Target != "" {
		d["target"] = rec.Target
	}
	if rec.SourceAddr != "" {
		d["source_addr"] = rec.SourceAddr
	}
	if rec.TotalLatencyMS != 0 {
		d["total_latency_ms"] = rec.TotalLatencyMS
	}
	if rec.EngineLatencyMS != 0 {
		d["engine_latency_ms"] = rec.EngineLatencyMS
	}
	if rec.ErrorCode != "" {
		d["error_code"] = rec.ErrorCode
	}
	if rec.LeaderAtRequest != "" {
		d["leader_at_request"] = rec.LeaderAtRequest
	}
	if rec.Time != "" {
		d["audit_time"] = rec.Time
	}
	return d
}

// logControlPlaneRequest emits the per-request `control_plane request`
// log event documented in contracts/observability.md. This is distinct
// from the `lcm audit` emission below — the audit line carries every
// field, while this one is the structured per-request access log.
func (a *Audit) logControlPlaneRequest(rec AuditRecord) {
	fields := []pgmanager.Field{
		{Key: "operation", Value: rec.Operation},
		{Key: "outcome", Value: rec.Outcome},
		{Key: "actor", Value: rec.Actor},
		{Key: "request_id", Value: rec.RequestID},
		{Key: "total_latency_ms", Value: rec.TotalLatencyMS},
	}
	if rec.Target != "" {
		fields = append(fields, pgmanager.Field{Key: "target", Value: rec.Target})
	}
	if rec.EngineLatencyMS != 0 {
		fields = append(fields, pgmanager.Field{Key: "engine_latency_ms", Value: rec.EngineLatencyMS})
	}
	if rec.ErrorCode != "" {
		fields = append(fields, pgmanager.Field{Key: "error_code", Value: rec.ErrorCode})
	}
	a.logger.Info("control_plane request", fields...)
}

// Healthy reports whether mutating ops should be allowed (FR-028).
// False whenever the most recent NATS publish OR history publish
// failed. The slog sink is best-effort and never gates Healthy.
func (a *Audit) Healthy() bool {
	return a.natsFailures.Load() == 0 && a.historyFailures.Load() == 0
}

// ErrAuditUnavailable is the typed sentinel returned (and surfaced via
// HTTP `audit_unavailable`) when the audit pipeline can't accept a
// record. Mutating ops fail closed on this (FR-028).
var ErrAuditUnavailable = errors.New("audit: pipeline unavailable")

// bumpFailure increments both the in-memory counter and the
// `pgman_proxy_lcm_audit_emit_failures_total` Prometheus counter.
func (a *Audit) bumpFailure(sink string) {
	a.natsFailures.Add(1)
	if a.metrics != nil {
		a.metrics.LCMAuditEmitFailuresTot.WithLabelValues(sink).Inc()
	}
}

// bumpHistoryFailure mirrors bumpFailure for the JetStream history sink.
// Kept on a separate counter so Healthy() can still recognise the NATS
// subject as a known-good state if only the history sink is wedged
// (and vice-versa).
func (a *Audit) bumpHistoryFailure() {
	a.historyFailures.Add(1)
	if a.metrics != nil {
		a.metrics.LCMAuditEmitFailuresTot.WithLabelValues("history").Inc()
	}
}

// logSlog writes the audit record as one structured log line. Outcome
// drives the level: accepted -> info, rejected -> warn, failed -> error.
func (a *Audit) logSlog(rec AuditRecord) {
	fields := []pgmanager.Field{
		{Key: "request_id", Value: rec.RequestID},
		{Key: "operation", Value: rec.Operation},
		{Key: "outcome", Value: rec.Outcome},
		{Key: "actor", Value: rec.Actor},
		{Key: "source_addr", Value: rec.SourceAddr},
		{Key: "total_latency_ms", Value: rec.TotalLatencyMS},
	}
	if rec.Target != "" {
		fields = append(fields, pgmanager.Field{Key: "target", Value: rec.Target})
	}
	if rec.EngineLatencyMS != 0 {
		fields = append(fields, pgmanager.Field{Key: "engine_latency_ms", Value: rec.EngineLatencyMS})
	}
	if rec.ErrorCode != "" {
		fields = append(fields, pgmanager.Field{Key: "error_code", Value: rec.ErrorCode})
	}
	if rec.LeaderAtRequest != "" {
		fields = append(fields, pgmanager.Field{Key: "leader_at_request", Value: rec.LeaderAtRequest})
	}
	switch rec.Outcome {
	case OutcomeAccepted:
		a.logger.Info("lcm audit", fields...)
	case OutcomeRejected:
		a.logger.Warn("lcm audit", fields...)
	default:
		a.logger.Error("lcm audit", fields...)
	}
}
