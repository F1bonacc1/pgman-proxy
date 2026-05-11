package cluster

import (
	"encoding/json"
	"fmt"
	"strings"

	pgmanager "github.com/f1bonacc1/pg-manager"
	"github.com/nats-io/nats.go"

	"github.com/f1bonacc1/pgman-proxy/internal/obs"
)

// SubscribeCoordinationEvents subscribes to the pg-manager coordination
// event family on NATS and surfaces every message via the metrics +
// structured-log surfaces (FR-006). Returns the active subscription so
// the caller can Unsubscribe at shutdown.
//
// The subjects subscribed to are documented in
// contracts/observability.md § NATS topics — consumed:
//
//	pgmanager.<cluster_id>.auto_rebootstrap.>
//	pgmanager.<cluster_id>.auto_demote.>
//	pgmanager.<cluster_id>.divergence.>
//	pgmanager.<cluster_id>.conninfo.reconciled
//
// pgman-proxy MUST NOT publish on these subjects (Constitution IV).
func SubscribeCoordinationEvents(
	conn *nats.Conn,
	clusterID string,
	logger *obs.Logger,
	metrics *obs.MetricSet,
) ([]*nats.Subscription, error) {
	families := []string{
		"auto_rebootstrap",
		"auto_demote",
		"divergence",
		"conninfo",
	}
	subs := make([]*nats.Subscription, 0, len(families))
	for _, family := range families {
		subject := fmt.Sprintf("pgmanager.%s.%s.>", clusterID, family)
		sub, err := conn.Subscribe(subject, func(m *nats.Msg) {
			handleCoordinationEvent(m, logger, metrics)
		})
		if err != nil {
			for _, prev := range subs {
				_ = prev.Unsubscribe()
			}
			return nil, fmt.Errorf("subscribe %q: %w", subject, err)
		}
		subs = append(subs, sub)
	}
	return subs, nil
}

func handleCoordinationEvent(m *nats.Msg, logger *obs.Logger, metrics *obs.MetricSet) {
	subject := m.Subject
	outcome := classifyOutcome(subject)
	metrics.CoordinationEventsTotal.WithLabelValues(subject, outcome).Inc()
	fields := []pgmanager.Field{
		{Key: "subject", Value: subject},
		{Key: "outcome", Value: outcome},
		{Key: "payload_size_bytes", Value: len(m.Data)},
	}
	// Trace-context propagation per contracts/observability.md: read
	// `traceparent` from the NATS header (when present) and surface it
	// on the log line.
	if m.Header != nil {
		if tp := obs.ParseTraceParent(m.Header.Get("traceparent")); tp.HasTrace() {
			fields = append(fields,
				pgmanager.Field{Key: "trace_id", Value: tp.TraceID},
				pgmanager.Field{Key: "span_id", Value: tp.SpanID})
		}
	}
	// Surface the JSON payload as structured fields so a single log
	// line carries the per-event context (term, role, reason, from/to,
	// new_leader, condition, …) that pg-manager already publishes. The
	// payload schema varies per subject (see pg-manager observability/
	// events.go), so decode into a generic map and flatten under a
	// `payload` key. Header fields common to most pg-manager events —
	// node_id, state, role, term — are promoted to `emitter_*` so they
	// don't collide with the proxy's own node_id field on the record.
	if len(m.Data) > 0 {
		var payload map[string]any
		if err := json.Unmarshal(m.Data, &payload); err == nil && len(payload) > 0 {
			fields = appendEmitterFields(fields, payload)
			fields = append(fields, pgmanager.Field{Key: "payload", Value: payload})
		}
	}
	logger.Info("coordination event", fields...)
}

// appendEmitterFields promotes the pg-manager EventHeader fields that
// are useful for at-a-glance log skimming (term, state, role, the
// emitting node) to top-level so an operator does not have to dig
// into the `payload` map to answer "who emitted this in what term".
// Fields not present in the payload are silently skipped — most
// pg-manager events embed EventHeader but a few (PeerSlotsEnsured,
// StateTransition) carry their own schema.
func appendEmitterFields(fields []pgmanager.Field, payload map[string]any) []pgmanager.Field {
	emitterKeys := []struct {
		payloadKey string
		logKey     string
	}{
		{"node_id", "emitter_node_id"},
		{"state", "emitter_state"},
		{"role", "emitter_role"},
		{"term", "emitter_term"},
	}
	for _, k := range emitterKeys {
		if v, ok := payload[k.payloadKey]; ok && v != nil && v != "" {
			fields = append(fields, pgmanager.Field{Key: k.logKey, Value: v})
		}
	}
	return fields
}

// classifyOutcome derives the `outcome` label for the
// pgman_proxy_coordination_events_total counter. Refusals are tagged
// `refused`; failures `failed`; everything else `delivered`.
func classifyOutcome(subject string) string {
	switch {
	case strings.HasSuffix(subject, ".refused"):
		return "refused"
	case strings.HasSuffix(subject, ".failed"):
		return "failed"
	default:
		return "delivered"
	}
}
