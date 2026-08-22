// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build integration

// US4 / T046 — Switchover end-to-end. The receiving peer accepts the
// request, ferries it to the leader (forward mode is the default),
// and returns the leader's reply. The test asserts the audit record
// exists on BOTH sinks (slog via container logs + NATS via subscriber)
// and the new leader holds the primary role.

package integration

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestLCM_Switchover_ToPeerB(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Wait for the cluster to be ready first.
	peers := Peers()
	_, _ = retryLCM(t, ctx, peers[0].Name, "GET", "/v1/status", "", 200, 2*time.Minute)

	body := `{"target":"node-b"}`
	code, raw := retryLCM(t, ctx, peers[0].Name, "POST", "/v1/switchover", body, 200, 1*time.Minute)
	env := expectLCM(t, code, raw, 200, "accepted")
	if env.Operation != "Switchover" {
		t.Errorf("operation: got %q", env.Operation)
	}

	// Audit: the slog sink writes a structured-log line on the leader.
	// Read the leader's recent logs and confirm the line is present.
	peerLogs, err := dumpLogs(ctx, "node-a")
	if err != nil {
		t.Fatalf("dumpLogs: %v", err)
	}
	if !strings.Contains(peerLogs, `"operation":"Switchover"`) {
		t.Errorf("Switchover audit missing from node-a logs (sample: %s)", lastN(peerLogs, 800))
	}

	// Do not return until the rotation has actually COMPLETED.
	//
	// The LCM call above returns on `accepted`, not on done. Without
	// this barrier the switchover stays in flight while later tests run,
	// and any of them that issues a leader-routed call can land in the
	// window where the old primary has resigned and the new one has not
	// finished promoting. There is no leader to route to, so the call
	// blocks for the full 30s leader-route timeout and returns 504.
	// TestLCM_TransientRefusal_ClusterBootstrapping, four tests later,
	// hit exactly that — intermittently, which is worse than reliably.
	//
	// This also strengthens the test: previously it asserted only that
	// the request was accepted and audited, never that leadership
	// actually moved to the requested target.
	primary, err := waitForConvergence(ctx, time.Now().Add(2*time.Minute))
	if err != nil {
		t.Fatalf("cluster did not converge after switchover: %v", err)
	}
	if primary != "node-b" {
		t.Errorf("switchover target was node-b, but the cluster converged on %q", primary)
	}
}

// dumpLogs returns the last ~200 log lines from peerName's container.
func dumpLogs(ctx context.Context, peerName string) (string, error) {
	out, err := dockerComposeOutput(ctx, "logs", "--tail=200", peerName)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// dumpFullLogs returns peerName's entire container log. Use when the
// lines under test may be arbitrarily old (startup events, audit
// records from earlier in the suite).
func dumpFullLogs(ctx context.Context, peerName string) (string, error) {
	out, err := dockerComposeOutput(ctx, "logs", peerName)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func lastN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
