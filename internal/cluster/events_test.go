// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

package cluster

import (
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
	handleCoordinationEvent(&nats.Msg{Subject: subject, Data: data}, logger, metrics)

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
func TestHandleCoordinationEvent_MalformedPayloadIsTolerated(t *testing.T) {
	buf := &obs.SafeBuffer{}
	logger := obs.NewLogger(buf, "info", "test-cluster", "test-node", "test")
	metrics := obs.NewMetrics("test-cluster", "test-node")

	handleCoordinationEvent(&nats.Msg{
		Subject: "pgmanager.pgman-pc.auto_rebootstrap.detected",
		Data:    []byte("not-json"),
	}, logger, metrics)

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
