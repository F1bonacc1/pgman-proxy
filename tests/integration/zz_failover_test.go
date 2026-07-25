// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build integration

// US1 forced-failover: kill the current PRIMARY's container, wait for
// the cluster to elect a new leader, and verify writes through every
// surviving peer succeed within the SC-002 budget (45s p99 from
// leader-loss to first successful write through any peer).
//
// FILENAME NOTE: the `zz_` prefix is deliberate — Go runs tests in
// file order, and this test's aftermath (a SIGKILLed primary walking
// pg-manager's deposed-primary rejoin path) converges on a variable
// timeline. Running it LAST means nothing downstream inherits a
// mid-convergence topology. Keep it the final test file in this
// package.

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// failoverBudget is SC-002's p99 bound from leader-loss to first
// successful write (constitution v1.3.0: 45s). The p99 is dominated
// by two structural terms, NOT compose-timer tuning:
//
//   - When the killed primary also hosts the embedded-NATS JetStream
//     RAFT leader for the leadership KV bucket (~1/3 of boots — RAFT
//     placement is a dice roll), the survivors spend 5-10s in
//     JetStream re-election before any lease read works at all
//     (symptom: "singleton heartbeat Get failed: context deadline
//     exceeded" storms on both survivors). NATS election timeouts are
//     not tunable.
//   - After promote, the new primary keeps synchronous_standby_names
//     covering the peer pool, so the FIRST commit blocks (~4s) until
//     a standby's walreceiver re-attaches. That is correct
//     no-data-loss behavior, not a bug.
//
// Measured on a workstation: ~7s fast path; 16.4s and 29.1s in the
// colocated case. 45s adds margin for shared GitHub-hosted runners.
// Do NOT chase a smaller number by tightening the compose timers:
// sub-second lease TTLs and 1s failover delay met 5s on the fast path
// locally but risk spurious failovers under CI load, and no timer
// touches the two terms above.
const failoverBudget = 45 * time.Second

// TestFailover_NewLeaderReachableThroughEveryPeer validates SC-002:
// p99 leader-failure-to-first-successful-write ≤ failoverBudget,
// observable via any peer that survived. The test is non-flaky because
// we only assert on the upper bound — actual measured latency is
// lower in healthy clusters.
func TestFailover_NewLeaderReachableThroughEveryPeer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	peers := Peers()

	// The harness readiness gate covers /readyz, not the data plane —
	// on a fresh boot the proxy listener answers before the primary
	// election finishes routing. The full suite never notices (this
	// file runs last), but wait explicitly so `-run TestFailover`
	// works standalone too.
	waitForDataPlane(t, ctx, 2*time.Minute)

	// Pre-failover schema fixture. Any peer works.
	if err := withConn(ctx, peers[0].DSN(), func(conn *pgx.Conn) error {
		_, err := conn.Exec(ctx,
			"CREATE TABLE IF NOT EXISTS failover_marker (id serial primary key, msg text)")
		return err
	}); err != nil {
		t.Fatalf("schema setup: %v", err)
	}

	// The boot-time election decides the primary — it is NOT always
	// node-a. Identify it via each node's LOCAL postgres; asking
	// through the proxy listener is wrong because the proxy routes
	// queries to the primary wherever it lives, so every peer would
	// answer "not in recovery" and the kill would sometimes hit a
	// standby (observed: a "5.6ms failover", i.e. no failover at all).
	// Crucially, gate on FULL convergence — exactly one stable primary
	// with both standbys streaming — not on the first node answering
	// "not in recovery": an earlier test's planned disruption
	// (switchover, upgrade-execute) may still be settling, and a
	// just-demoted ex-primary keeps answering "f" until its demote
	// lands. Killing that stale victim measures nothing (observed: a
	// 48ms "failover" — the real primary never died) and, worse,
	// SIGKILLing a node mid-demote left it unable to rejoin at all.
	primaryName, err := waitForConvergence(ctx, time.Now().Add(3*time.Minute))
	if err != nil {
		// A persistent dual-primary here is a REAL product incident
		// (an ex-primary that never demoted after an earlier test's
		// planned disruption), not a test-logic problem. The compose
		// topology is gone by the time anyone reads a CI failure, so
		// capture each peer's control-plane view of itself now.
		for _, p := range peers {
			code, body, cerr := callLCM(ctx, p.Name, "GET", "/v1/status", integrationToken, "")
			t.Logf("pre-kill diagnostics %s: /v1/status code=%d err=%v body=%s", p.Name, code, cerr, body)
		}
		t.Fatalf("cluster not converged before kill: %v", err)
	}
	var leader *Peer
	for i := range peers {
		if peers[i].Name == primaryName {
			leader = &peers[i]
		}
	}
	if leader == nil {
		t.Fatalf("converged primary %q not in the peer list", primaryName)
	}
	t.Logf("primary before kill: %s", leader.Name)

	// Bring the killed peer back once this test finishes: the shared
	// harness boots the topology once per `go test` invocation, so a
	// peer left dead here fails every later test that needs it.
	// Registered before the kill so restoration runs even if an
	// assertion below fails first.
	t.Cleanup(func() { restorePeer(t, leader.Name) })

	// SIGKILL the leader's container — exercises the harshest failover
	// path (no graceful Stop, no controlled lease release; the new
	// leader must wait for the lease TTL to expire).
	killCtx, cancelKill := context.WithTimeout(ctx, 30*time.Second)
	defer cancelKill()
	if err := runCompose(killCtx, "kill", "-s", "KILL", leader.Name); err != nil {
		t.Fatalf("compose kill %s: %v", leader.Name, err)
	}

	// SC-002 budget: failoverBudget from leader-loss to first
	// successful write. We sample every 100ms for up to 90s — well over
	// the spec — and fail only if no surviving peer can write within
	// the documented budget on any of the samples.
	survivors := otherPeers(peers, leader.Name)
	deadline := time.Now().Add(90 * time.Second)
	first := time.Time{}
	startKill := time.Now()
	for time.Now().Before(deadline) {
		// Probe every survivor CONCURRENTLY: a failed attempt blocks
		// for its full 1s bound, so probing serially would lag the
		// measurement by up to len(survivors)×bound behind the actual
		// promotion and overstate SC-002.
		results := make(chan error, len(survivors))
		for _, p := range survivors {
			go func(p Peer) { results <- writeThrough(ctx, p) }(p)
		}
		for range survivors {
			if <-results == nil && first.IsZero() {
				first = time.Now()
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
	if elapsed > failoverBudget {
		t.Errorf("SC-002: first write after kill took %s, exceeds %s p99 budget", elapsed, failoverBudget)
	}

	// And every surviving peer should accept writes once the cluster
	// has settled — proxies follow the new leader. Retried briefly:
	// right after the first successful write, the OTHER survivor's
	// proxy may still be flipping its route to the new primary.
	for _, p := range survivors {
		t.Run("write_via_"+p.Name, func(t *testing.T) {
			var err error
			for end := time.Now().Add(15 * time.Second); time.Now().Before(end); {
				if err = writeThrough(ctx, p); err == nil {
					return
				}
				time.Sleep(500 * time.Millisecond)
			}
			t.Fatalf("post-failover write via %s: %v", p.Name, err)
		})
	}
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
// failover-budget accounting. The 1s bound covers the WHOLE attempt —
// with the primary dead, the proxy accepts the connection and then
// parks the query, so an unbounded Exec would hang the sampling loop
// on its first iteration and never observe the promotion. (Kept at 1s,
// not tighter: on a loaded CI runner a legitimate connect+INSERT can
// take hundreds of ms, and a bound it can never meet would make the
// sampling loop blind to a completed promotion.)
func writeThrough(ctx context.Context, p Peer) error {
	attemptCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	conn, err := pgx.Connect(attemptCtx, p.DSN())
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	_, err = conn.Exec(attemptCtx,
		"INSERT INTO failover_marker(msg) VALUES ($1)", p.Name)
	return err
}

// restorePeer restarts a container a test stopped or killed, then
// blocks until the node is a functioning cluster member again: its
// control plane answers, its local postmaster accepts connections
// (the node rejoins as a standby; pg-manager's auto-demote path may
// wipe + basebackup first), and every peer's proxy listener serves
// queries. Uses t.Errorf, not Fatal — it runs inside t.Cleanup.
func restorePeer(t *testing.T, name string) {
	t.Helper()
	// Generous budget: a deposed primary's rejoin can involve a wipe +
	// basebackup before it resumes streaming.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	if err := runCompose(ctx, "start", name); err != nil {
		t.Errorf("restore %s: compose start: %v", name, err)
		return
	}
	deadline := time.Now().Add(7 * time.Minute)

	// Control plane back up on the restored node.
	for {
		code, _, err := callLCM(ctx, name, "GET", "/v1/status", integrationToken, "")
		if err == nil && code == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("restore %s: control plane never answered (code=%d err=%v)", name, code, err)
			return
		}
		time.Sleep(2 * time.Second)
	}

	// Local postmaster accepting again (rejoin may basebackup first).
	for {
		_, err := dockerComposeOutput(ctx, "exec", "-T", name,
			"psql", "-U", "postgres", "-h", "/var/run/postgresql", "-tAc", "SELECT 1")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("restore %s: local postmaster never accepted connections: %v", name, err)
			return
		}
		time.Sleep(2 * time.Second)
	}

	// Whole data plane serving again.
	for _, p := range Peers() {
		for {
			var one int
			err := withConn(ctx, p.DSN(), func(conn *pgx.Conn) error {
				return conn.QueryRow(ctx, "SELECT 1").Scan(&one)
			})
			if err == nil && one == 1 {
				break
			}
			if time.Now().After(deadline) {
				t.Errorf("restore %s: data plane via %s never recovered: %v", name, p.Name, err)
				return
			}
			time.Sleep(2 * time.Second)
		}
	}

	// Convergence gate: the rejoining node walks pg-manager's
	// deposed-primary path (demote, possibly wipe + basebackup), so
	// the cluster shape settles on a variable timeline. OBSERVE
	// convergence — exactly one stable primary with both standbys
	// streaming — rather than forcing a shape with a switchover;
	// issuing control-plane mutations mid-convergence races the
	// rejoin machinery. Leadership stays wherever the failover put it
	// (this file runs last, so no later test depends on the position).
	if _, err := waitForConvergence(ctx, deadline); err != nil {
		t.Errorf("restore %s: %v", name, err)
		return
	}
	t.Logf("restore %s: node rejoined; cluster converged", name)
}

// waitForConvergence and localPsql moved to lcm_helpers_test.go —
// the upgrade suite's quiescence barrier shares them now.
