// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build integration

// US4 / T047 — Fence / Unfence round-trip.

package integration

import (
	"context"
	"testing"
	"time"
)

func TestLCM_FenceUnfence_Roundtrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	peers := Peers()
	_, _ = retryLCM(t, ctx, peers[0].Name, "GET", "/v1/status", "", 200, 2*time.Minute)

	// Fence node-c (a follower).
	code, raw := retryLCM(t, ctx, peers[0].Name, "POST", "/v1/fence",
		`{"target":"node-c"}`, 200, 30*time.Second)
	expectLCM(t, code, raw, 200, "accepted")

	// Unfence — must succeed and not error if the node was fenced.
	code, raw = retryLCM(t, ctx, peers[0].Name, "POST", "/v1/unfence",
		`{"target":"node-c"}`, 200, 30*time.Second)
	expectLCM(t, code, raw, 200, "accepted")
}
