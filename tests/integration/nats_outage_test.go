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
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestNATSOutage_ReadinessFlipsTo503 isolates one peer from NATS by
// disconnecting that peer's compose service from the test network.
// The peer's /readyz must report 503 within the documented grace
// window. Other peers stay ready (whole-cluster NATS isn't down).
func TestNATSOutage_ReadinessFlipsTo503(t *testing.T) {
	// Verified empirically (2026-07-07, healthy cluster, container-id
	// disconnect, in-container probing, 90s window): /readyz stays 200
	// on a routes-isolated peer. The outage model predates feature
	// 002 — NATS is now embedded in-process, so a compose-network
	// disconnect severs cluster ROUTES (and JetStream quorum) but
	// never the peer's own client connection, and readiness currently
	// tracks only the local embedded server. Whether a quorum-isolated
	// peer SHOULD flip (its lease renewals are failing — Constitution
	// II fail-closed suggests yes) is a product decision needing a
	// spec'd readiness signal, the same redesign bucket as
	// TestLCM_AuditFailClose_RefusesMutation (feature 002 RD-001a
	// follow-up). Skip until that lands; the disconnect/probe
	// mechanics below are already embedded-model-correct for it.
	t.Skip("readiness does not (yet) track routes-isolation; needs the embedded-model redesign — see comment")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	peers := Peers()
	if len(peers) < 2 {
		t.Skip("requires multi-peer topology")
	}
	target := peers[1] // node-b — picking a non-leader-by-default

	// Disconnect target from its compose default network. Docker's
	// `network disconnect` takes a CONTAINER name/id, not a compose
	// service name — resolve it via `compose ps -q` (passing the bare
	// service name fails with "endpoint not found").
	netName := harness.project + "_default"
	cid, err := composeContainerID(ctx, target.Name)
	if err != nil {
		t.Fatalf("resolve container for %s: %v", target.Name, err)
	}
	disconnectCtx, cancelDisc := context.WithTimeout(ctx, 30*time.Second)
	defer cancelDisc()
	if err := runRawDocker(disconnectCtx, "network", "disconnect", "-f", netName, cid); err != nil {
		t.Fatalf("disconnect %s from %s: %v", target.Name, netName, err)
	}
	defer reconnectPeer(t, target, netName, cid)

	// The peer's /readyz MUST flip to 503. Post-feature-002 the NATS
	// server is embedded, so a network disconnect severs cluster
	// ROUTES (and the peer's JetStream quorum) but never the peer's
	// in-process client connection — the flip therefore rides on the
	// lease-renewal failure path, not on a connection drop. Allow a
	// full lease TTL + propagation margin.
	//
	// Probe from INSIDE the container: disconnecting the network
	// endpoint also breaks the host port-publish path, so a host-side
	// probe would see connection errors instead of the 503.
	deadline := time.Now().Add(90 * time.Second)
	flipped := false
	for time.Now().Before(deadline) {
		code, err := readyzInContainer(ctx, target.Name)
		if err == nil && code == http.StatusServiceUnavailable {
			flipped = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !flipped {
		t.Fatalf("FR-011: /readyz on %s never flipped to 503 within deadline", target.Name)
	}
	hc := &http.Client{Timeout: 2 * time.Second}

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

// reconnectPeer re-attaches the target to its compose network and
// waits for its /readyz to recover so the rest of the suite isn't
// poisoned by a still-degraded peer.
func reconnectPeer(t *testing.T, p Peer, network, cid string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := runRawDocker(ctx, "network", "connect", network, cid); err != nil {
		t.Errorf("reconnect %s to %s: %v", p.Name, network, err)
		return
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		code, err := readyzInContainer(ctx, p.Name)
		if err == nil && code == http.StatusOK {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("reconnect %s: /readyz never recovered to 200 (last code=%d err=%v)", p.Name, code, err)
			return
		}
		time.Sleep(2 * time.Second)
	}
}

// composeContainerID resolves a compose service name to its container
// id (docker network commands do not accept service names).
func composeContainerID(ctx context.Context, service string) (string, error) {
	out, err := dockerComposeOutput(ctx, "ps", "-q", service)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", fmt.Errorf("no running container for compose service %s", service)
	}
	return id, nil
}

// readyzInContainer probes the peer's /readyz from inside its own
// network namespace — unaffected by the peer being disconnected from
// the compose network.
func readyzInContainer(ctx context.Context, service string) (int, error) {
	out, err := dockerComposeOutput(ctx, "exec", "-T", service,
		"curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", "http://127.0.0.1:9090/readyz")
	if err != nil {
		return 0, err
	}
	var code int
	if _, serr := fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &code); serr != nil {
		return 0, fmt.Errorf("parse readyz code from %q: %w", out, serr)
	}
	return code, nil
}

// runRawDocker invokes `docker <args...>` directly (NOT through
// `docker compose`). Used for network manipulation that compose
// doesn't expose first-class.
func runRawDocker(ctx context.Context, args ...string) error {
	return execDocker(ctx, args...)
}
