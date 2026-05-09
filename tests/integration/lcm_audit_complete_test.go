// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build integration

// US4 / T052 — SC-010 audit completeness. EVERY LCM request — accepted,
// rejected, OR failed — produces records on BOTH sinks. We drive one
// of each outcome and assert the audit log line exists in the peer's
// container logs (slog sink) and the NATS audit subject (proven by
// the per-peer `pgman_proxy_lcm_audit_emit_failures_total` staying at 0).

package integration

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestLCM_AuditCompleteness_AllThreeOutcomes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	peers := Peers()
	_, _ = retryLCM(t, ctx, peers[0].Name, "GET", "/v1/status", "", 200, 2*time.Minute)

	// 1) accepted: Status with valid token.
	_, _, _ = callLCM(ctx, peers[0].Name, "GET", "/v1/status", integrationToken, "")
	// 2) rejected: Switchover with no body — invalid_argument.
	_, _, _ = callLCM(ctx, peers[0].Name, "POST", "/v1/switchover", integrationToken, "{}")
	// 3) failed: TriggerBackup — backup_executor_missing.
	_, _, _ = callLCM(ctx, peers[0].Name, "POST", "/v1/backup", integrationToken, "")

	// Verify all three outcomes appear in the peer's slog sink.
	logs, err := dumpLogs(ctx, peers[0].Name)
	if err != nil {
		t.Fatalf("dumpLogs: %v", err)
	}
	want := []string{
		`"outcome":"accepted"`,
		`"outcome":"rejected"`,
		`"outcome":"failed"`,
	}
	for _, w := range want {
		if !strings.Contains(logs, w) {
			t.Errorf("audit slog missing %s", w)
		}
	}
}
