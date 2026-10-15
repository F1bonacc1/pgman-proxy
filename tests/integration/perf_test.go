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

// perfLeg is one connection path timed by TestPerf_ProxyHopOverhead.
type perfLeg struct {
	name string
	dsn  string
	conn *pgx.Conn
	durs []time.Duration
}

// TestPerf_ProxyHopOverhead measures `SELECT 1` round-trip latency on
// three paths to the elected primary's Postgres and logs p50/p99 for
// each via t.Logf:
//
//   - direct:    straight to the primary's Postgres port (the baseline)
//   - local:     through the primary's own proxy (the SC-003 hop)
//   - forwarded: through a standby's proxy, which forwards to the
//     primary across the container network (informational; it is the
//     path a client takes when it lands on a non-leader peer)
//
// The proxy targets the elected leader's Postgres from every peer, so
// the test first waits for a stable primary and builds the legs around
// it. Samples are interleaved one query per leg per iteration, with the
// starting leg rotated, so CPU-frequency and thermal drift land on all
// legs equally. Overhead is reported as the difference of percentiles
// (local p99 - direct p99), not the percentile of paired differences.
//
// The test PASSES if the harness can complete the run; it does not
// enforce the 1ms cap here — that's polish-phase work (T076). Failing
// on absolute latency from a developer laptop is too noisy to be useful.
func TestPerf_ProxyHopOverhead(t *testing.T) {
	if testing.Short() {
		t.Skip("perf measurement skipped in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// 10k samples per leg puts p99 on the 100th-worst sample instead
	// of the 10th, which is what makes a 10% drift gate meaningful.
	const (
		warmup  = 200
		samples = 10000
	)

	// Measuring while an earlier test's failover or rebootstrap is
	// still settling would time the churn, not the proxy.
	primaryName, err := waitForConvergence(ctx, time.Now().Add(3*time.Minute))
	if err != nil {
		t.Fatalf("cluster not converged before measuring: %v", err)
	}
	var primary, standby Peer
	for _, p := range Peers() {
		if p.Name == primaryName {
			primary = p
		} else if standby.Name == "" {
			standby = p
		}
	}

	legs := []*perfLeg{
		{name: "direct", dsn: primary.DirectDSN()},
		{name: "local", dsn: primary.DSN()},
		{name: "forwarded", dsn: standby.DSN()},
	}
	for _, l := range legs {
		conn, err := pgx.Connect(ctx, l.dsn)
		if err != nil {
			t.Fatalf("connect %s leg: %v", l.name, err)
		}
		defer conn.Close(context.Background())
		l.conn = conn
		l.durs = make([]time.Duration, 0, samples)
	}

	// Warm-up so every connection's prepared-statement cache is hot.
	for _, l := range legs {
		for i := 0; i < warmup; i++ {
			var n int
			if err := l.conn.QueryRow(ctx, "SELECT 1").Scan(&n); err != nil {
				t.Fatalf("warmup %s: %v", l.name, err)
			}
		}
	}

	for i := 0; i < samples; i++ {
		for j := range legs {
			l := legs[(i+j)%len(legs)]
			start := time.Now()
			var n int
			if err := l.conn.QueryRow(ctx, "SELECT 1").Scan(&n); err != nil {
				t.Fatalf("%s sample %d: %v", l.name, i, err)
			}
			l.durs = append(l.durs, time.Since(start))
		}
	}

	p50 := make(map[string]time.Duration, len(legs))
	p99 := make(map[string]time.Duration, len(legs))
	for _, l := range legs {
		sort.Slice(l.durs, func(i, j int) bool { return l.durs[i] < l.durs[j] })
		p50[l.name] = l.durs[len(l.durs)*50/100]
		p99[l.name] = l.durs[len(l.durs)*99/100]
	}
	t.Logf("SELECT 1 round-trip, primary=%s, standby=%s, samples=%d per leg",
		primary.Name, standby.Name, samples)
	for _, l := range legs {
		t.Logf("  %-9s p50=%s p99=%s", l.name, p50[l.name], p99[l.name])
	}
	t.Logf("  hop overhead (local - direct): p50=%s p99=%s",
		p50["local"]-p50["direct"], p99["local"]-p99["direct"])
	t.Logf("  forward overhead (forwarded - direct): p50=%s p99=%s",
		p50["forwarded"]-p50["direct"], p99["forwarded"]-p99["direct"])

	// The constitution sets a 10% regression alarm; absolute thresholds
	// belong in CI artefacts where the baseline can be tracked over
	// time. T076 records the first run's number into the README.
	for _, name := range []string{"local", "forwarded"} {
		if p99[name] > 100*time.Millisecond {
			t.Errorf("%s p99 %s exceeds sanity ceiling 100ms — proxy may be misconfigured", name, p99[name])
		}
	}
}
