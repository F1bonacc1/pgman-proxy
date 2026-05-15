// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

package cluster

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/f1bonacc1/pgman-proxy/internal/obs"
)

// captureEvent runs handleCoordinationEvent for one synthetic message
// and returns the parsed JSON log record produced by the call. Lets
// tests assert on field presence + value without string-matching.
func captureEvent(t *testing.T, subject string, payload any) map[string]any {
	t.Helper()
	buf := &obs.SafeBuffer{}
	logger := obs.NewLogger(buf, "info", "test-cluster", "test-node", "test")
	metrics := obs.NewMetrics("test-cluster", "test-node")

	var data []byte
	if payload != nil {
		var err error
		data, err = json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
	}
	handleCoordinationEvent(&nats.Msg{Subject: subject, Data: data}, "test-node", logger, metrics, nil)

	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("expected one log line, got none")
	}
	if strings.Contains(line, "\n") {
		t.Fatalf("expected exactly one log line, got %d:\n%s", strings.Count(line, "\n")+1, line)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("parse log line %q: %v", line, err)
	}
	return rec
}

// TestHandleCoordinationEvent_PayloadFlattenedAndPromoted pins the
// enrichment contract: a pg-manager auto_rebootstrap.detected payload
// carries an EventHeader plus per-event fields, and our log line
// surfaces ALL of it from one record — emitter_* fields at top level
// (so an operator can grep node/term/role without parsing nested
// JSON) and the full payload nested under `payload` for forensics.
func TestHandleCoordinationEvent_PayloadFlattenedAndPromoted(t *testing.T) {
	payload := map[string]any{
		// EventHeader fields — should be promoted to `emitter_*` top-level.
		"cluster_id":  "pgman-pc",
		"node_id":     "node-a",
		"state":       "running",
		"role":        "standby",
		"term":        7,
		"occurred_at": "2026-05-11T20:28:19Z",
		// AutoRebootstrapDetected per-event fields — surface via `payload`.
		"condition":         map[string]any{"kind": "stale_wal", "lag_bytes": 4096},
		"consecutive_ticks": 3,
	}
	rec := captureEvent(t, "pgmanager.pgman-pc.auto_rebootstrap.detected", payload)

	if got := rec["msg"]; got != "coordination event" {
		t.Errorf(`msg = %q, want "coordination event"`, got)
	}
	if got := rec["outcome"]; got != "delivered" {
		t.Errorf(`outcome = %q, want "delivered"`, got)
	}
	if got := rec["emitter_node_id"]; got != "node-a" {
		t.Errorf("emitter_node_id = %v, want node-a", got)
	}
	if got := rec["emitter_state"]; got != "running" {
		t.Errorf("emitter_state = %v, want running", got)
	}
	if got := rec["emitter_role"]; got != "standby" {
		t.Errorf("emitter_role = %v, want standby", got)
	}
	if got := rec["emitter_term"]; got != float64(7) {
		t.Errorf("emitter_term = %v, want 7", got)
	}
	// payload is nested intact so per-event specifics (condition,
	// consecutive_ticks, …) remain greppable for forensics.
	p, ok := rec["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload missing or wrong type: %T", rec["payload"])
	}
	if got := p["consecutive_ticks"]; got != float64(3) {
		t.Errorf("payload.consecutive_ticks = %v, want 3", got)
	}
	if _, ok := p["condition"].(map[string]any); !ok {
		t.Errorf("payload.condition missing or wrong type: %T", p["condition"])
	}
}

// TestHandleCoordinationEvent_RefusedSubjectGetsOutcome pins that
// refused subjects show up with outcome=refused both in the metric
// label and the log line — that's the field operators grep when
// hunting failover refusals during a chaos run.
func TestHandleCoordinationEvent_RefusedSubjectGetsOutcome(t *testing.T) {
	payload := map[string]any{
		"node_id": "node-c",
		"term":    11,
		"reason":  "missing_quorum_snapshot",
	}
	rec := captureEvent(t, "pgmanager.pgman-pc.auto_demote.refused", payload)

	if got := rec["outcome"]; got != "refused" {
		t.Errorf(`outcome = %q, want "refused"`, got)
	}
	p := rec["payload"].(map[string]any)
	if got := p["reason"]; got != "missing_quorum_snapshot" {
		t.Errorf("payload.reason = %v, want missing_quorum_snapshot", got)
	}
}

// TestHandleCoordinationEvent_EmptyPayload pins the fall-back path:
// pg-manager events without a payload (or with an unparseable body)
// must still produce a log line with the basic subject/outcome/size
// fields — no panic, no missing log entry.
func TestHandleCoordinationEvent_EmptyPayload(t *testing.T) {
	rec := captureEvent(t, "pgmanager.pgman-pc.divergence.parked", nil)

	if rec["subject"] != "pgmanager.pgman-pc.divergence.parked" {
		t.Errorf("subject wrong: %v", rec["subject"])
	}
	if rec["outcome"] != "delivered" {
		t.Errorf("outcome wrong: %v", rec["outcome"])
	}
	if rec["payload_size_bytes"] != float64(0) {
		t.Errorf("payload_size_bytes = %v, want 0", rec["payload_size_bytes"])
	}
	if _, ok := rec["payload"]; ok {
		t.Errorf("payload field should be absent for empty body")
	}
	if _, ok := rec["emitter_node_id"]; ok {
		t.Errorf("emitter_node_id should be absent when no payload")
	}
}

// TestHandleCoordinationEvent_MalformedPayloadIsTolerated pins that a
// non-JSON body (defensive: should never happen, but the subscription
// MUST NOT panic on it) falls back to the basic log line cleanly.
// fakeHistorySink captures PublishEvent calls so the test can assert
// the cluster handler routes coordination events into the history
// JetStream when a sink is wired.
type fakeHistorySink struct {
	captured []struct {
		category string
		evType   string
		nodeID   string
		details  map[string]any
	}
}

func (f *fakeHistorySink) PublishEvent(_ context.Context, category, evType, nodeID string, details map[string]any) (string, error) {
	f.captured = append(f.captured, struct {
		category string
		evType   string
		nodeID   string
		details  map[string]any
	}{category, evType, nodeID, details})
	return "fake-id", nil
}

// TestHandleCoordinationEvent_PublishesToHistorySink — regression for
// the "pgmctl explain leader-election always NA" bug. When the sink
// is wired, every coordination event must land in history with the
// pg-manager topic constant as the `type`. Without this the explain
// command (and operators) never see leadership transitions.
func TestHandleCoordinationEvent_PublishesToHistorySink(t *testing.T) {
	buf := &obs.SafeBuffer{}
	logger := obs.NewLogger(buf, "info", "test-cluster", "test-node", "test")
	metrics := obs.NewMetrics("test-cluster", "test-node")
	sink := &fakeHistorySink{}

	payload, _ := json.Marshal(map[string]any{
		"node_id":   "node-b",
		"term":      42,
		"new_leader": "node-b",
	})
	handleCoordinationEvent(&nats.Msg{
		Subject: "pgmanager.pgman-pc.leader_changed",
		Data:    payload,
	}, "node-b", logger, metrics, sink)

	if len(sink.captured) != 1 {
		t.Fatalf("expected 1 history publish, got %d", len(sink.captured))
	}
	got := sink.captured[0]
	if got.category != "event" {
		t.Errorf("category = %q, want event", got.category)
	}
	if got.evType != "leader_changed" {
		t.Errorf("evType = %q, want leader_changed", got.evType)
	}
	if got.nodeID != "node-b" {
		t.Errorf("nodeID = %q, want node-b (from payload.node_id)", got.nodeID)
	}
	if got.details["subject"] != "pgmanager.pgman-pc.leader_changed" {
		t.Errorf("subject not preserved in details: %+v", got.details)
	}
}

// TestHandleCoordinationEvent_FiltersOutSiblingEmitters — regression
// for the N-fold duplication bug observed in the live fixture. When
// the payload's node_id names a DIFFERENT peer, the local subscriber
// must NOT publish to history (only slog). pg-manager broadcasts each
// event on a single NATS subject; the emitter peer owns the history
// publish so the stream lands one record per event, not N.
func TestHandleCoordinationEvent_FiltersOutSiblingEmitters(t *testing.T) {
	buf := &obs.SafeBuffer{}
	logger := obs.NewLogger(buf, "info", "test-cluster", "node-a", "test")
	metrics := obs.NewMetrics("test-cluster", "node-a")
	sink := &fakeHistorySink{}

	// Event emitted by node-b; this subscriber's selfNodeID is node-a.
	// History publish MUST be suppressed; slog still records the receipt.
	payload, _ := json.Marshal(map[string]any{
		"node_id":    "node-b",
		"new_leader": "node-b",
	})
	handleCoordinationEvent(&nats.Msg{
		Subject: "pgmanager.pgman-pc.leader_changed",
		Data:    payload,
	}, "node-a", logger, metrics, sink)

	if got := len(sink.captured); got != 0 {
		t.Errorf("sibling-emitted event must NOT publish to history; got %d records", got)
	}
	if !strings.Contains(buf.String(), "leader_changed") {
		t.Errorf("slog line should still record the sibling event")
	}
}

// TestHandleCoordinationEvent_MissingNodeIDPublishesAnyway — when the
// payload doesn't carry an emitter node_id (cluster-level events), we
// publish on every observer. Better to risk duplicates than to drop
// the record entirely. Documents the design choice.
func TestHandleCoordinationEvent_MissingNodeIDPublishesAnyway(t *testing.T) {
	buf := &obs.SafeBuffer{}
	logger := obs.NewLogger(buf, "info", "test-cluster", "node-a", "test")
	metrics := obs.NewMetrics("test-cluster", "node-a")
	sink := &fakeHistorySink{}

	payload, _ := json.Marshal(map[string]any{"some_field": "value"})
	handleCoordinationEvent(&nats.Msg{
		Subject: "pgmanager.pgman-pc.failover_quorum_published",
		Data:    payload,
	}, "node-a", logger, metrics, sink)

	if got := len(sink.captured); got != 1 {
		t.Errorf("event without node_id must publish; got %d records", got)
	}
}

// TestHandleCoordinationEvent_SynthesizesLeaderChanged — pg-manager
// declares LeaderChangedEvent but never emits it (zombie type). The
// cluster handler synthesizes proxy.leader_changed from
// state_transition records whose reason is became_leader / lost_leader
// so operators can query for the leadership edge by event type.
func TestHandleCoordinationEvent_SynthesizesLeaderChanged(t *testing.T) {
	logger := obs.NewLogger(&obs.SafeBuffer{}, "info", "test-cluster", "node-c", "test")
	metrics := obs.NewMetrics("test-cluster", "node-c")
	sink := &fakeHistorySink{}

	payload, _ := json.Marshal(map[string]any{
		"node_id": "node-c",
		"from":    3,
		"to":      4,
		"reason":  "became_leader",
		"term":    1,
	})
	handleCoordinationEvent(&nats.Msg{
		Subject: "pgmanager.pgman-pc.state_transition",
		Data:    payload,
	}, "node-c", logger, metrics, sink)

	if len(sink.captured) != 2 {
		t.Fatalf("expected 2 records (state_transition + proxy.leader_changed), got %d", len(sink.captured))
	}
	gotTypes := []string{sink.captured[0].evType, sink.captured[1].evType}
	wantTypes := []string{"state_transition", "proxy.leader_changed"}
	for i, want := range wantTypes {
		if gotTypes[i] != want {
			t.Errorf("record[%d].evType = %q, want %q", i, gotTypes[i], want)
		}
	}
	leaderRec := sink.captured[1]
	if leaderRec.details["new_leader"] != "node-c" {
		t.Errorf("proxy.leader_changed: new_leader = %v, want node-c", leaderRec.details["new_leader"])
	}
	if leaderRec.details["derived_from"] != "state_transition" {
		t.Errorf("proxy.leader_changed: must carry derived_from marker")
	}
}

// TestSynthesizeLeaderChange_IgnoresNonLeaderReasons — startup,
// auto_rebootstrap, and other non-leadership state_transition reasons
// must NOT spawn a synthesized proxy.leader_changed record.
func TestSynthesizeLeaderChange_IgnoresNonLeaderReasons(t *testing.T) {
	for _, reason := range []string{"startup_with_pgdata", "auto_rebootstrap", "promote_requested"} {
		got := synthesizeLeaderChange(map[string]any{
			"node_id": "node-a",
			"reason":  reason,
		})
		if got != nil {
			t.Errorf("reason=%q must NOT synthesize a leader_changed record, got %+v", reason, got)
		}
	}
}

// TestSubjectTail_RetainsMultiSegmentTopics — `auto_rebootstrap.detected`
// must survive verbatim so the history record's type matches the
// pg-manager topic constant.
func TestSubjectTail_RetainsMultiSegmentTopics(t *testing.T) {
	cases := map[string]string{
		"pgmanager.pgman-pc.leader_changed":              "leader_changed",
		"pgmanager.pgman-pc.auto_rebootstrap.detected":   "auto_rebootstrap.detected",
		"pgmanager.pgman-pc.divergence.flagged":          "divergence.flagged",
		"pgmanager.pgman-pc.conninfo.reconciled":         "conninfo.reconciled",
	}
	for subj, want := range cases {
		if got := subjectTail(subj); got != want {
			t.Errorf("subjectTail(%q) = %q, want %q", subj, got, want)
		}
	}
}

func TestHandleCoordinationEvent_MalformedPayloadIsTolerated(t *testing.T) {
	buf := &obs.SafeBuffer{}
	logger := obs.NewLogger(buf, "info", "test-cluster", "test-node", "test")
	metrics := obs.NewMetrics("test-cluster", "test-node")

	handleCoordinationEvent(&nats.Msg{
		Subject: "pgmanager.pgman-pc.auto_rebootstrap.detected",
		Data:    []byte("not-json"),
	}, "test-node", logger, metrics, nil)

	line := strings.TrimSpace(buf.String())
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("parse log line %q: %v", line, err)
	}
	if rec["payload_size_bytes"] != float64(len("not-json")) {
		t.Errorf("payload_size_bytes = %v, want %d", rec["payload_size_bytes"], len("not-json"))
	}
	if _, ok := rec["payload"]; ok {
		t.Errorf("payload field should be absent when body fails to parse")
	}
}
