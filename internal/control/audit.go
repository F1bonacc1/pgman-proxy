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

	"github.com/f1bonacc1/pgman-proxy/internal/obs"
)

// NATSPublisher is the slice of *nats.Conn the audit emitter needs.
// Carving an interface lets tests inject a fake without booting NATS.
type NATSPublisher interface {
	Publish(subject string, data []byte) error
}

// Audit is the dual-sink emitter. Construct via NewAudit; pass into
// every handler.
type Audit struct {
	subject string
	logger  *obs.Logger
	bus     NATSPublisher
	metrics *obs.MetricSet

	// natsFailures counts back-to-back NATS publish failures. The
	// fail-closed gate (FR-028) trips whenever this is >0; a successful
	// publish resets it. We track it as a counter for the metric and
	// as a boolean for the gate.
	natsFailures atomic.Uint64
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
			// firstErr is always nil at this branch (the marshal-error
			// path returned via the outer else-if), so we always assign.
			firstErr = fmt.Errorf("audit nats: %w", err)
		} else {
			a.natsFailures.Store(0)
		}
	}
	return firstErr
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
// False whenever the most recent NATS publish failed.
func (a *Audit) Healthy() bool {
	return a.natsFailures.Load() == 0
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
