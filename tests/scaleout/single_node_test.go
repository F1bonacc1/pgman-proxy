// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build scaleout

package scaleout

import (
	"strings"
	"testing"
	"time"
)

// TestSingleNodeFreshWritesAndRestart is S5 (spec 005 US1, SC-001).
// Before WI-A a fresh single node rendered
// synchronous_standby_names = 'ANY 1 ("node-a")' and hung every commit
// (F1), and a restarted one stayed a read-only standby forever because
// promotion waited for a quorum snapshot that a one-peer pool never
// publishes (F2).
func TestSingleNodeFreshWritesAndRestart(t *testing.T) {
	tp := newTopo(t, "s5")
	tp.ensureVolumes("a")
	tp.start("a", singleEnv("a"))

	tp.waitPrimary("a", 90*time.Second)
	took := tp.waitWritable("a", 10*time.Second)
	t.Logf("first commit %s after primary", took.Round(time.Millisecond))

	if ssn, err := tp.ssn("a"); err != nil || ssn != "" {
		t.Fatalf("synchronous_standby_names = %q (err %v), want empty for a single peer", ssn, err)
	}
	waitFor(t, 30*time.Second, time.Second, "replication_policy=async logged", func() (bool, string) {
		return strings.Contains(tp.logs("a"), `"replication_policy":"async"`), "manager started line not seen yet"
	})

	if _, err := tp.psql("a", "create table kept as select g from generate_series(1, 100) g"); err != nil {
		t.Fatalf("seed rows: %v", err)
	}

	tp.stop("a", 30*time.Second)
	// Issue #11: the drain stops Postgres, so no crash recovery follows.
	if st := tp.clusterState("a"); st != "shut down" {
		t.Errorf("cluster state = %q after docker stop, want %q", st, "shut down")
	}
	restart := time.Now()
	tp.start("a", singleEnv("a"))
	tp.waitPrimary("a", 30*time.Second)
	tp.waitWritable("a", 30*time.Second-time.Since(restart))
	t.Logf("writable %s after restart", time.Since(restart).Round(time.Millisecond))

	if n := tp.rows("a", "kept"); n != 100 {
		t.Fatalf("rows in kept after restart = %d, want 100", n)
	}
}

// TestSingleNodeStaleSyncNamesUpgrade is S5b (spec 005 US1 AS-3,
// FR-005, research R-002). A single node initialized before WI-A
// carries the self-referential value in postgresql.conf. Appending it
// reproduces that data directory without building an old image.
//
// Before pg-manager 015 the runtime render returns early for an empty
// peer set, so the stale value survives and the documented one-time
// ALTER SYSTEM step is required. With 015 it renders an empty value and no step is
// needed; the assertion flips with upstream015().
func TestSingleNodeStaleSyncNamesUpgrade(t *testing.T) {
	tp := newTopo(t, "s5b")
	tp.ensureVolumes("a")
	tp.start("a", singleEnv("a"))
	tp.waitPrimary("a", 90*time.Second)
	tp.waitWritable("a", 10*time.Second)

	tp.stop("a", 30*time.Second)
	tp.rootRun(map[string]string{tp.volume("a", "data"): "/d"},
		`printf '%s\n' "synchronous_standby_names = 'ANY 1 (\"node-a\")'" >> /d/postgresql.conf`)

	restart := time.Now()
	tp.start("a", singleEnv("a"))
	tp.waitPrimary("a", 30*time.Second)

	if upstream015() {
		tp.waitWritable("a", 30*time.Second-time.Since(restart))
		t.Logf("writable %s after restart with no manual step", time.Since(restart).Round(time.Millisecond))
		return
	}

	if err := tp.write("a"); err == nil {
		t.Fatal("write succeeded with the stale self-referential sync value; " +
			"if pg-manager now clears it, run with PGMAN_SCALEOUT_UPSTREAM_015=1 and retire the FR-005 note")
	}
	// The documented one-time step (CHANGELOG, FR-005).
	if _, err := tp.psql("a", "alter system set synchronous_standby_names = ''", "select pg_reload_conf()"); err != nil {
		t.Fatalf("documented upgrade step failed: %v", err)
	}
	tp.waitWritable("a", 30*time.Second)
	if ssn, err := tp.ssn("a"); err != nil || ssn != "" {
		t.Fatalf("synchronous_standby_names = %q (err %v) after the upgrade step, want empty", ssn, err)
	}
}
