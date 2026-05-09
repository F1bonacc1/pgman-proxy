// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build integration

// US1 NATS-outage: when a peer loses its NATS connection, the peer's
// /readyz MUST flip to 503 within the lease-renewal grace window
// (FR-011). The data-plane listener accepts connections only while
// /readyz is 200, so writes through that peer are refused.

package integration

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// TestNATSOutage_ReadinessFlipsTo503 isolates one peer from NATS by
// disconnecting that peer's compose service from the test network.
// The peer's /readyz must report 503 within the documented grace
// window. Other peers stay ready (whole-cluster NATS isn't down).
func TestNATSOutage_ReadinessFlipsTo503(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	peers := Peers()
	if len(peers) < 2 {
		t.Skip("requires multi-peer topology")
	}
	target := peers[1] // node-b — picking a non-leader-by-default

	// Disconnect target from its compose default network. `compose
	// network disconnect` is the v2-supported way to model a partial
	// NATS outage without restarting the peer.
	netName := harness.project + "_default"
	disconnectCtx, cancelDisc := context.WithTimeout(ctx, 30*time.Second)
	defer cancelDisc()
	cmdArgs := []string{"network", "disconnect", "-f", netName, target.Name}
	if err := runRawDocker(disconnectCtx, cmdArgs...); err != nil {
		t.Fatalf("disconnect %s from %s: %v", target.Name, netName, err)
	}
	defer reconnectPeer(target, netName)

	// Within 30s the peer's /readyz MUST flip to 503. The lease-renewal
	// grace window is governed by NATS heartbeat + LeadershipProvider
	// jitter inside pg-manager; 30s is comfortable headroom.
	deadline := time.Now().Add(30 * time.Second)
	hc := &http.Client{Timeout: 2 * time.Second}
	url := target.HealthURL + "/readyz"
	flipped := false
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := hc.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusServiceUnavailable {
				flipped = true
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !flipped {
		t.Fatalf("FR-011: /readyz on %s never flipped to 503 within deadline", target.Name)
	}

	// Other peers must remain ready — this is per-peer, not whole-cluster.
	for _, p := range peers {
		if p.Name == target.Name {
			continue
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.HealthURL+"/readyz", nil)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatalf("probe %s: %v", p.Name, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("peer %s flipped to %d (expected 200 — its NATS link is intact)",
				p.Name, resp.StatusCode)
		}
	}
}

// reconnectPeer re-attaches the target to its compose network so the
// rest of the test suite isn't poisoned. Best-effort.
func reconnectPeer(p Peer, network string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = runRawDocker(ctx, "network", "connect", network, p.Name)
}

// runRawDocker invokes `docker <args...>` directly (NOT through
// `docker compose`). Used for network manipulation that compose
// doesn't expose first-class.
func runRawDocker(ctx context.Context, args ...string) error {
	return execDocker(ctx, args...)
}
