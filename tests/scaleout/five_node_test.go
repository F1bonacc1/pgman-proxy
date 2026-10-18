// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build scaleout

package scaleout

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// 1→5 scenarios (contracts/scaleout-suite.md S8-S11). Spec 005 verified
// 1→3 only; these apply the same runbook procedure with five peers: push
// the 5-node config, start the four new nodes, restart node-a.

// TestExpandOneToFive is S8: a live single node expanded to five, once
// per start order, with the write outage measured as in S1.
func TestExpandOneToFive(t *testing.T) {
	requireUpstream015(t)
	for i, order := range []string{orderALast, orderAFirst, orderTogether} {
		t.Run(fmt.Sprintf("%d-%s", i+1, order), func(t *testing.T) {
			tp := newTopo(t, "s8")
			g := buildGolden(tp, goldenRows, singleEnv("a"))
			tp.startWriteProbe("a")
			time.Sleep(2 * time.Second) // a baseline of successful commits before the restart
			expandAt := time.Now()
			expandTo(tp, order, fiveNodes[1:], fiveEnv)
			assertExpandedTo(tp, g, fiveNodes, 4*time.Minute)
			var gap time.Duration
			var oks int
			waitFor(t, 30*time.Second, time.Second, "a probe commit after the expansion", func() (bool, string) {
				var lastOK time.Time
				gap, oks, lastOK = tp.longestWriteGap()
				return oks >= 10 && lastOK.After(expandAt), fmt.Sprintf("%d commits, last at %s", oks, lastOK.Format(time.RFC3339Nano))
			})
			t.Logf("longest write gap through node-a: %s (%d commits)", gap.Round(time.Millisecond), oks)
			if gap >= 30*time.Second {
				t.Errorf("write outage %s, want < 30s", gap)
			}
		})
	}
}

// TestFiveDataNodeDownEmptyNodesWait is S9, S6 with five peers. The four
// empty nodes hold four of five coordination votes, yet with node-a, the
// only data holder, down they must never create a database or accept
// writes: the route-mesh gate needs every peer (F15).
func TestFiveDataNodeDownEmptyNodesWait(t *testing.T) {
	tp := newTopo(t, "s9")
	g := buildGolden(tp, 1000, singleEnv("a"))
	tp.stop("a", 30*time.Second)
	newNodes := fiveNodes[1:]
	for _, n := range newNodes {
		tp.ensureVolumes(n)
		tp.start(n, fiveEnv(n))
	}
	assertNoDatabase(t, tp, newNodes, 3*time.Minute)
	for _, n := range newNodes {
		code, err := tp.wait(n, 2*time.Minute)
		if err != nil || code != exitMeshNotFormed {
			t.Fatalf("node-%s exit = %d (%v), want %d (route mesh not formed)", n, code, err, exitMeshNotFormed)
		}
	}

	requireUpstream015(t)
	for _, n := range fiveNodes {
		tp.start(n, fiveEnv(n))
	}
	assertExpandedTo(tp, g, fiveNodes, 4*time.Minute)
}

// TestFiveOneNewNodeMissing is S10: node-a is restarted into the 5-node
// config but one new host (node-e) never starts. No mesh can form, so
// every started node, node-a included, exits 75 and no database is
// created on the new nodes. Stopping the new nodes and reverting node-a
// to its single-node config restores a writable node-a with its data.
func TestFiveOneNewNodeMissing(t *testing.T) {
	tp := newTopo(t, "s10")
	g := buildGolden(tp, 1000, singleEnv("a"))
	started := []string{"b", "c", "d"}
	expandTo(tp, orderALast, started, fiveEnv)
	assertNoDatabase(t, tp, started, time.Minute)
	for _, n := range append([]string{"a"}, started...) {
		code, err := tp.wait(n, 4*time.Minute)
		if err != nil || code != exitMeshNotFormed {
			t.Fatalf("node-%s exit = %d (%v), want %d (route mesh not formed)", n, code, err, exitMeshNotFormed)
		}
	}
	assertNoDatabase(t, tp, started, 0)

	restart := time.Now()
	tp.start("a", singleEnv("a"))
	tp.waitPrimary("a", 30*time.Second)
	tp.waitWritable("a", 30*time.Second-time.Since(restart))
	t.Logf("node-a writable %s after the rollback restart", time.Since(restart).Round(time.Millisecond))
	if id := tp.sysid("a"); id != g.sysid {
		t.Errorf("system identifier %q, want golden %q", id, g.sysid)
	}
	if n := tp.rows("a", "bulk"); n != g.rows {
		t.Errorf("bulk rows %d, want %d", n, g.rows)
	}
}

// TestRollbackFiveToSingle is S11, S3 with five peers: after an
// expansion, stop the four new nodes first, then revert node-a.
func TestRollbackFiveToSingle(t *testing.T) {
	requireUpstream015(t)
	tp := newTopo(t, "s11")
	g := buildGolden(tp, 1000, singleEnv("a"))
	expandTo(tp, orderALast, fiveNodes[1:], fiveEnv)
	assertExpandedTo(tp, g, fiveNodes, 4*time.Minute)
	if err := tp.write("a"); err != nil {
		t.Fatalf("write before rollback: %v", err)
	}
	rows := tp.rows("a", "probe")

	for _, n := range fiveNodes[1:] {
		tp.stop(n, 30*time.Second)
	}
	tp.stop("a", 30*time.Second)
	restart := time.Now()
	tp.start("a", singleEnv("a"))
	tp.waitPrimary("a", 30*time.Second)
	tp.waitWritable("a", 30*time.Second-time.Since(restart))
	t.Logf("writable %s after rollback restart", time.Since(restart).Round(time.Millisecond))

	if ssn, err := tp.ssn("a"); err != nil || ssn != "" {
		t.Errorf("synchronous_standby_names = %q (%v), want empty after rollback", ssn, err)
	}
	if n := tp.rows("a", "bulk"); n != g.rows {
		t.Errorf("bulk rows %d, want %d", n, g.rows)
	}
	if n := tp.rows("a", "probe"); n < rows {
		t.Errorf("probe rows %d, want ≥ %d", n, rows)
	}
}

// assertNoDatabase fails the test if any of nodes initializes a
// database, has a data directory or accepts a write, checking every
// 10 s for d (once when d is 0). A stopped node counts as having none.
func assertNoDatabase(t *testing.T, tp *topo, nodes []string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		for _, n := range nodes {
			if strings.Contains(tp.allLogs(n), "initdb: starting") {
				t.Fatalf("node-%s initialized a database", n)
			}
			if _, err := tp.exec(n, 5*time.Second, "test", "-e", dataDir+"/PG_VERSION"); err == nil {
				t.Fatalf("node-%s has a data directory", n)
			}
			if tp.write(n) == nil {
				t.Fatalf("node-%s accepted a write", n)
			}
		}
		if !time.Now().Before(deadline) {
			return
		}
		time.Sleep(10 * time.Second)
	}
}
