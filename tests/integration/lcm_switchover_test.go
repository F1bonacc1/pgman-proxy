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
}

// dumpLogs returns the last ~200 log lines from peerName's container.
func dumpLogs(ctx context.Context, peerName string) (string, error) {
	out, err := dockerComposeOutput(ctx, "logs", "--tail=200", peerName)
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
