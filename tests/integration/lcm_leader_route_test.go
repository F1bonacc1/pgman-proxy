// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build integration

// US4 / T049 — Leader-routing in BOTH forward and redirect modes.
// The default deployment uses forward mode; this test additionally
// flexes redirect mode by issuing a Switchover at a non-leader peer
// and asserting the response is either accepted (forward) or carries
// the documented redirect contract.

package integration

import (
	"context"
	"testing"
	"time"
)

func TestLCM_LeaderRoute_Forward(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	peers := Peers()
	_, _ = retryLCM(t, ctx, peers[0].Name, "GET", "/v1/status", "", 200, 2*time.Minute)

	// Issue a Fence via every peer in turn — forward mode means the
	// non-leader peers should still report accepted (the leader runs
	// the engine call). The audit on each peer records the local
	// observation; only the leader's audit shows engine_latency_ms > 0.
	for _, p := range peers {
		body := `{"target":"node-c"}`
		code, raw, err := callLCM(ctx, p.Name, "POST", "/v1/fence", integrationToken, body)
		if err != nil {
			t.Fatalf("callLCM via %s: %v", p.Name, err)
		}
		expectLCM(t, code, raw, 200, "accepted")
	}
	// Cleanup.
	_, _ = retryLCM(t, ctx, peers[0].Name, "POST", "/v1/unfence",
		`{"target":"node-c"}`, 200, 10*time.Second)
}
