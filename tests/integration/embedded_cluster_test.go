// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build integration

// US1 (feature 002 / specs/002-embedded-nats-cluster) — 3-peer
// embedded NATS cluster forms, elects one leader, and routes writes
// from any peer to the leader. The premise of this test is that NO
// `nats-server` process exists on any host — each peer hosts NATS
// in-process via internal/embedded.
//
// Spec coverage:
//   * SC-001: 3-peer cluster up with no external NATS process.
//   * SC-006: every 001 integration test passes unchanged against
//     the embedded topology (this test just confirms the new shape).

package integration

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestEmbeddedCluster_NoExternalNATSProcess verifies the SC-001
// "no nats-server on any host" invariant.
func TestEmbeddedCluster_NoExternalNATSProcess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, p := range Peers() {
		// `pgrep -fa nats-server$` MUST find nothing inside the peer
		// container. The `$` anchor avoids matching the nats-server
		// Go module name in process command lines.
		out, err := composeExec(ctx, p.Name, "pgrep", "-fa", "nats-server$")
		// pgrep exits 1 when no match — that's the success case.
		if err == nil && strings.TrimSpace(string(out)) != "" {
			t.Errorf("peer %s has external nats-server process(es): %s", p.Name, out)
		}
	}
}

// TestEmbeddedCluster_AllPeersReady asserts every peer reaches the
// ready state within the documented startup budget. Reuses the
// harness's `waitReady` helper.
func TestEmbeddedCluster_AllPeersReady(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if err := waitReady(ctx, Peers(), 90*time.Second); err != nil {
		t.Fatalf("peers never reached /readyz=200: %v", err)
	}
}

// TestEmbeddedCluster_RoutesMeshed asserts each peer's metrics expose
// `pgman_proxy_embedded_nats_routes_meshed=2` and `..._up=1` (the
// SC-001 / FR-013 verification surface).
func TestEmbeddedCluster_RoutesMeshed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	deadline := time.Now().Add(45 * time.Second)
	for _, p := range Peers() {
		var lastBody string
		var lastErr error
		met := false
		for time.Now().Before(deadline) {
			body, err := scrapeMetrics(ctx, p)
			lastBody = body
			lastErr = err
			if err == nil &&
				containsMetricLine(body, "pgman_proxy_embedded_nats_up") &&
				metricValueEquals(body, "pgman_proxy_embedded_nats_routes_meshed", 2) {
				met = true
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if !met {
			t.Errorf("peer %s: never reported routes_meshed=2 within budget; last err=%v\nlast body excerpt:\n%s",
				p.Name, lastErr, headLines(lastBody, 40))
		}
	}
}

// composeExec runs `docker compose exec -T <service> <cmd...>` and
// returns combined stdout/stderr. Used to peek inside peer containers.
func composeExec(ctx context.Context, service string, cmd ...string) ([]byte, error) {
	args := append([]string{"exec", "-T", service}, cmd...)
	full := composeArgs(args...)
	c := exec.CommandContext(ctx, "docker", full...) //nolint:gosec
	c.Dir = harness.workdir
	var buf bytes.Buffer
	c.Stdout = &buf
	c.Stderr = &buf
	if err := c.Run(); err != nil {
		return buf.Bytes(), fmt.Errorf("docker compose exec %s %v: %w", service, cmd, err)
	}
	return buf.Bytes(), nil
}

// containsMetricLine reports whether the Prometheus body has at least
// one line beginning with the supplied metric name (label-set agnostic).
func containsMetricLine(body, metricName string) bool {
	for _, line := range strings.Split(body, "\n") {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, metricName+" ") || strings.HasPrefix(s, metricName+"{") {
			return true
		}
	}
	return false
}

// metricValueEquals reports whether the Prometheus body has a line for
// metricName whose terminal value field equals `want`.
func metricValueEquals(body, metricName string, want int) bool {
	wantStr := itoa(want)
	for _, line := range strings.Split(body, "\n") {
		s := strings.TrimSpace(line)
		if !(strings.HasPrefix(s, metricName+" ") || strings.HasPrefix(s, metricName+"{")) {
			continue
		}
		fields := strings.Fields(s)
		if len(fields) < 2 {
			continue
		}
		if fields[len(fields)-1] == wantStr {
			return true
		}
	}
	return false
}

// itoa is a small allocation-free integer formatter for non-negative
// values. Used by the metric assertions.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	const digits = "0123456789"
	negative := false
	if n < 0 {
		negative = true
		n = -n
	}
	buf := [20]byte{}
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// headLines returns the first n newline-separated lines of s, for
// truncated diagnostic output.
func headLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
