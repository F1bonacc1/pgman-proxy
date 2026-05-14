package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	pgmanager "github.com/f1bonacc1/pg-manager"
	"github.com/nats-io/nats.go"

	"github.com/f1bonacc1/pgman-proxy/internal/obs"
)

// HistorySink is the narrow contract this package needs from the
// history publisher: append an event record to the cluster history
// stream. Carved into an interface so the cluster package doesn't
// import internal/history directly (keeps the import graph one-way:
// runtime → cluster, history).
type HistorySink interface {
	PublishEvent(ctx context.Context, category, evType, nodeID string, details map[string]any) (string, error)
}

// SubscribeCoordinationEvents subscribes to the pg-manager coordination
// event family on NATS and surfaces every message via the metrics +
// structured-log surfaces (FR-006). When historySink is non-nil, the
// same records are appended to the cluster history JetStream so
// GET /v1/history sees them (003 FR-007). Returns the active
// subscriptions so the caller can Unsubscribe at shutdown.
//
// Subjects subscribed to (documented in pg-manager observability/events.go):
//
//	pgmanager.<cluster_id>.state_transition         — every state-machine edge
//	pgmanager.<cluster_id>.leader_changed           — leadership-lease change
//	pgmanager.<cluster_id>.primary_changed          — PG primary identity change
//	pgmanager.<cluster_id>.fenced_node              — fence applied
//	pgmanager.<cluster_id>.unfenced_node            — fence cleared
//	pgmanager.<cluster_id>.failover_quorum_published
//	pgmanager.<cluster_id>.failover_refused
//	pgmanager.<cluster_id>.auto_rebootstrap.>
//	pgmanager.<cluster_id>.auto_demote.>
//	pgmanager.<cluster_id>.divergence.>
//	pgmanager.<cluster_id>.conninfo.reconciled
//
// pgman-proxy MUST NOT publish on these subjects (Constitution IV) —
// it is a passive observer.
func SubscribeCoordinationEvents(
	conn *nats.Conn,
	clusterID string,
	logger *obs.Logger,
	metrics *obs.MetricSet,
	historySink HistorySink,
) ([]*nats.Subscription, error) {
	// Exact-topic subjects (single-segment): each is a leaf event the
	// pg-manager state machine emits. Subscribing to the parent
	// wildcard `pgmanager.<cluster>.>` would also collect lcm.request
	// / audit traffic, which we already capture through other paths.
	exactTopics := []string{
		"state_transition",
		"leader_changed",
		"primary_changed",
		"fenced_node",
		"unfenced_node",
		"failover_quorum_published",
		"failover_refused",
		"pg_config_change",
		"slot_dropped",
		"peer_slots_ensured",
		"backup_started",
		"backup_completed",
		"backup_failed",
		"upgrade_started",
		"upgrade_completed",
		"upgrade_failed",
	}
	// Wildcard families with multi-segment sub-topics
	// (`pgmanager.<cluster>.auto_rebootstrap.detected`, …).
	wildcardFamilies := []string{
		"auto_rebootstrap",
		"auto_demote",
		"divergence",
		"conninfo",
	}

	subs := make([]*nats.Subscription, 0, len(exactTopics)+len(wildcardFamilies))
	for _, topic := range exactTopics {
		subject := fmt.Sprintf("pgmanager.%s.%s", clusterID, topic)
		sub, err := conn.Subscribe(subject, func(m *nats.Msg) {
			handleCoordinationEvent(m, logger, metrics, historySink)
		})
		if err != nil {
			for _, prev := range subs {
				_ = prev.Unsubscribe()
			}
			return nil, fmt.Errorf("subscribe %q: %w", subject, err)
		}
		subs = append(subs, sub)
	}
	for _, family := range wildcardFamilies {
		subject := fmt.Sprintf("pgmanager.%s.%s.>", clusterID, family)
		sub, err := conn.Subscribe(subject, func(m *nats.Msg) {
			handleCoordinationEvent(m, logger, metrics, historySink)
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

func handleCoordinationEvent(m *nats.Msg, logger *obs.Logger, metrics *obs.MetricSet, historySink HistorySink) {
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
	var payload map[string]any
	if len(m.Data) > 0 {
		_ = json.Unmarshal(m.Data, &payload)
	}
	if len(payload) > 0 {
		fields = appendEmitterFields(fields, payload)
		fields = append(fields, pgmanager.Field{Key: "payload", Value: payload})
	}
	logger.Info("coordination event", fields...)

	// History sink — append every coordination event to the cluster
	// history JetStream so /v1/history (003 FR-007) sees it. The
	// event type is the third subject segment (e.g. "leader_changed"
	// or "auto_rebootstrap.detected") so consumers can filter by type.
	if historySink != nil {
		evType := subjectTail(subject)
		emitterNode, _ := payload["node_id"].(string)
		details := map[string]any{
			"subject": subject,
			"outcome": outcome,
		}
		if len(payload) > 0 {
			details["payload"] = payload
		}
		// Best-effort: a history-sink failure must NOT block the rest
		// of the handler; the slog line above remains authoritative.
		_, _ = historySink.PublishEvent(context.Background(), "event", evType, emitterNode, details)
	}
}

// subjectTail returns everything after the second dot in a subject of
// the form `pgmanager.<cluster_id>.<topic>`. For multi-segment topics
// like `auto_rebootstrap.detected`, returns the full sub-path so the
// history record's `type` is exactly the topic constant pg-manager
// defines in observability/events.go.
func subjectTail(subject string) string {
	parts := strings.SplitN(subject, ".", 3)
	if len(parts) < 3 {
		return subject
	}
	return parts[2]
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
