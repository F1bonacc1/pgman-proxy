// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build integration

// US1 performance baseline: SC-003 caps proxy-hop overhead at <1ms p99
// for simple-query latency (no transactions, no large payloads). The
// budget compares end-to-end latency through the proxy to direct-PG
// latency on the same host. We measure here as a baseline; the
// constitutional alarm threshold (>10% drift) is checked in CI by
// task T076 once a recorded number exists.

package integration

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestPerf_ProxyHopOverhead measures the p99 of `SELECT 1` round-trip
// latency through the proxy and emits the value via t.Logf. The test
// PASSES if the harness can complete the run; it does not enforce the
// 1ms cap here — that's polish-phase work (T076). Failing on absolute
// latency from a developer laptop is too noisy to be useful.
func TestPerf_ProxyHopOverhead(t *testing.T) {
	if testing.Short() {
		t.Skip("perf measurement skipped in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const samples = 1000
	peer := Peers()[0]

	conn, err := pgx.Connect(ctx, peer.DSN())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())

	// Warm-up: 50 round-trips so the prepared-statement cache is hot.
	for i := 0; i < 50; i++ {
		var n int
		if err := conn.QueryRow(ctx, "SELECT 1").Scan(&n); err != nil {
			t.Fatalf("warmup: %v", err)
		}
	}

	durs := make([]time.Duration, 0, samples)
	for i := 0; i < samples; i++ {
		start := time.Now()
		var n int
		if err := conn.QueryRow(ctx, "SELECT 1").Scan(&n); err != nil {
			t.Fatalf("sample %d: %v", i, err)
		}
		durs = append(durs, time.Since(start))
	}

	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	p50 := durs[len(durs)*50/100]
	p99 := durs[len(durs)*99/100]
	t.Logf("SELECT 1 round-trip via %s: p50=%s, p99=%s, samples=%d",
		peer.Name, p50, p99, samples)

	// The constitution sets a 10% regression alarm; absolute thresholds
	// belong in CI artefacts where the baseline can be tracked over
	// time. T076 records the first run's number into the README.
	if p99 > 100*time.Millisecond {
		t.Errorf("p99 %s exceeds sanity ceiling 100ms — proxy may be misconfigured", p99)
	}
}
