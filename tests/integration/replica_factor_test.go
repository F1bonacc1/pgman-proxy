// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build integration

// US1 (feature 002 / FR-011a / RD-004) — replication-factor
// derivation table. The DeriveReplicas function is unit-tested in
// internal/embedded; this integration test verifies the wiring all
// the way through to the actual JetStream KV bucket's Replicas field
// on a running cluster.
//
// The compose harness brings up a 3-peer cluster with declared_size=3,
// so the standard verification is `Replicas == 3`. Sizes 1, 2, and 5
// are exercised via in-process unit tests of the derivation function;
// running them here would require a per-test compose tear-up which is
// disproportionate to the value.

package integration

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestReplicaFactor_ThreePeer asserts the running 3-peer cluster's
// metrics expose `pgman_proxy_embedded_nats_replicas_factor=3` with
// `overridden="false"`.
func TestReplicaFactor_ThreePeer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	deadline := time.Now().Add(45 * time.Second)
	for _, p := range Peers() {
		var lastBody string
		met := false
		for time.Now().Before(deadline) {
			body, err := scrapeMetrics(ctx, p)
			if err == nil {
				lastBody = body
				if hasReplicaFactor(body, 3, false) {
					met = true
					break
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
		if !met {
			t.Errorf("peer %s: never reported replicas_factor=3 (overridden=false); last body excerpt:\n%s",
				p.Name, headLines(lastBody, 30))
		}
	}
}

// hasReplicaFactor scans the Prometheus body for a metric line of
// the shape:
//
//	pgman_proxy_embedded_nats_replicas_factor{...overridden="false"...} 3
//
// and reports whether the value matches.
func hasReplicaFactor(body string, want int, overridden bool) bool {
	wantValue := itoa(want)
	wantOverridden := `overridden="false"`
	if overridden {
		wantOverridden = `overridden="true"`
	}
	for _, line := range strings.Split(body, "\n") {
		s := strings.TrimSpace(line)
		if !strings.HasPrefix(s, "pgman_proxy_embedded_nats_replicas_factor") {
			continue
		}
		if !strings.Contains(s, wantOverridden) {
			continue
		}
		fields := strings.Fields(s)
		if len(fields) < 2 {
			continue
		}
		if fields[len(fields)-1] == wantValue {
			return true
		}
	}
	return false
}
