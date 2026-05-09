// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build integration

// US4 / T045 — Fresh-cluster bootstrap from empty PGDATA via the LCM
// control plane. The harness brings the topology up; this test
// verifies the cluster reaches a state where Status returns
// `outcome=accepted` and reports a primary, all without any
// out-of-band initdb invocation (FR-023, SC-009).

package integration

import (
	"context"
	"testing"
	"time"
)

func TestLCM_BootstrapFromEmptyPGDATA(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	peers := Peers()
	// Status is a read; allow_unauth_reads is false by default so we
	// MUST authenticate. Retry budget rides out the documented
	// `cluster_bootstrapping` window (FR-029).
	code, body := retryLCM(t, ctx, peers[0].Name, "GET", "/v1/status", "", 200, 4*time.Minute)
	env := expectLCM(t, code, body, 200, "accepted")
	if len(env.EngineResult) == 0 {
		t.Errorf("Status engine_result missing")
	}
}
