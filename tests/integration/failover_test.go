// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build integration

// US1 forced-failover: kill the current leader's container, wait for
// the cluster to elect a new leader, and verify writes through every
// surviving peer succeed within the SC-002 budget (5s p99 from
// leader-loss to first successful write through any peer).

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestFailover_NewLeaderReachableThroughEveryPeer validates SC-002:
// p99 leader-failure-to-first-successful-write ≤ 5s, observable via any
// peer that survived. The test is non-flaky because we only assert on
// the upper bound — actual measured latency is far lower in healthy
// clusters.
func TestFailover_NewLeaderReachableThroughEveryPeer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	peers := Peers()

	// Pre-failover schema fixture. Any peer works.
	if err := withConn(ctx, peers[0].DSN(), func(conn *pgx.Conn) error {
		_, err := conn.Exec(ctx,
			"CREATE TABLE IF NOT EXISTS failover_marker (id serial primary key, msg text)")
		return err
	}); err != nil {
		t.Fatalf("schema setup: %v", err)
	}

	leader, err := whoIsLeader(ctx, peers)
	if err != nil {
		t.Fatalf("identify leader before kill: %v", err)
	}
	t.Logf("leader before kill: %s", leader.Name)

	// SIGKILL the leader's container — exercises the harshest failover
	// path (no graceful Stop, no controlled lease release; the new
	// leader must wait for the lease TTL to expire).
	killCtx, cancelKill := context.WithTimeout(ctx, 30*time.Second)
	defer cancelKill()
	if err := runCompose(killCtx, "kill", "-s", "KILL", leader.Name); err != nil {
		t.Fatalf("compose kill %s: %v", leader.Name, err)
	}

	// SC-002 budget: 5s p99 from leader-loss to first successful write.
	// We sample every 100ms for up to 30s — well over the spec — and
	// fail only if no surviving peer can write within the documented
	// budget on any of the samples.
	survivors := otherPeers(peers, leader.Name)
	deadline := time.Now().Add(30 * time.Second)
	first := time.Time{}
	startKill := time.Now()
	for time.Now().Before(deadline) {
		for _, p := range survivors {
			if writeThrough(ctx, p) == nil {
				first = time.Now()
				break
			}
		}
		if !first.IsZero() {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context cancelled before failover: %v", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	if first.IsZero() {
		t.Fatalf("no surviving peer accepted writes within deadline")
	}
	elapsed := first.Sub(startKill)
	t.Logf("first surviving-peer write at %s after kill", elapsed)
	if elapsed > 5*time.Second {
		t.Errorf("SC-002: first write after kill took %s, exceeds 5s p99 budget", elapsed)
	}

	// And every surviving peer should accept writes once the cluster
	// has settled — proxies follow the new leader.
	for _, p := range survivors {
		t.Run("write_via_"+p.Name, func(t *testing.T) {
			err := writeThrough(ctx, p)
			if err != nil {
				t.Fatalf("post-failover write via %s: %v", p.Name, err)
			}
		})
	}
}

// whoIsLeader runs a query on every peer until one returns
// `pg_is_in_recovery() = false`. Returns the first such peer.
func whoIsLeader(ctx context.Context, peers []Peer) (*Peer, error) {
	for i := range peers {
		p := peers[i]
		var inRecovery bool
		err := withConn(ctx, p.DSN(), func(conn *pgx.Conn) error {
			return conn.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&inRecovery)
		})
		if err != nil {
			continue
		}
		if !inRecovery {
			return &p, nil
		}
	}
	return nil, fmt.Errorf("no peer reported leader role; cluster may not be ready")
}

func otherPeers(peers []Peer, exclude string) []Peer {
	out := make([]Peer, 0, len(peers))
	for _, p := range peers {
		if p.Name == exclude {
			continue
		}
		out = append(out, p)
	}
	return out
}

// writeThrough attempts a single INSERT through a peer. Returns nil on
// success; any wire-protocol or planner error counts as failure for
// failover-budget accounting.
func writeThrough(ctx context.Context, p Peer) error {
	connCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	conn, err := pgx.Connect(connCtx, p.DSN())
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	_, err = conn.Exec(ctx,
		"INSERT INTO failover_marker(msg) VALUES ($1)", p.Name)
	return err
}
