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

// exitMeshNotFormed is the exit code of a node whose route mesh never
// formed (internal/runtime ExitDeps, F15).
const exitMeshNotFormed = 75

// TestSoloInThreeNodeConfig is S4 (spec 005 edge case "original node
// alone in the 3-node config"): node-a waits for a mesh that never
// forms, exits 75, and its database is untouched.
func TestSoloInThreeNodeConfig(t *testing.T) {
	tp := newTopo(t, "s4")
	g := buildGolden(tp, 1000, singleEnv("a"))

	tp.stop("a", 30*time.Second)
	start := time.Now()
	tp.start("a", threeEnv("a"))
	code, err := tp.wait("a", 4*time.Minute)
	took := time.Since(start)
	if err != nil {
		t.Fatalf("waiting for node-a to exit: %v", err)
	}
	t.Logf("node-a exited %d after %s", code, took.Round(time.Second))
	if code != exitMeshNotFormed {
		t.Fatalf("exit code %d, want %d", code, exitMeshNotFormed)
	}
	if took < 110*time.Second || took > 180*time.Second {
		t.Errorf("exited after %s, want the ~120s route-mesh budget", took)
	}

	tp.start("a", singleEnv("a"))
	waitFor(t, time.Minute, time.Second, "node-a answering queries", func() (bool, string) {
		r := tp.role("a")
		return r != "down", "role=" + r
	})
	if id := tp.sysid("a"); id != g.sysid {
		t.Errorf("system identifier %q, want golden %q", id, g.sysid)
	}
	if n := tp.rows("a", "bulk"); n != g.rows {
		t.Errorf("bulk rows %d, want %d", n, g.rows)
	}
}

// TestFreshClusterBootstrapsOnce is S2 (spec 005 US3 AS-1, FR-018):
// the boot-window gate and, with 015, empty-node deference must not
// stop three empty nodes from bootstrapping exactly one database.
func TestFreshClusterBootstrapsOnce(t *testing.T) {
	tp := newTopo(t, "s2")
	for _, n := range []string{"a", "b", "c"} {
		tp.ensureVolumes(n)
	}
	for _, n := range []string{"a", "b", "c"} {
		tp.start(n, threeEnv(n))
	}
	var primary string
	waitFor(t, 4*time.Minute, 2*time.Second, "one primary with two streaming standbys", func() (bool, string) {
		roles := map[string]string{}
		primary = ""
		for _, n := range []string{"a", "b", "c"} {
			roles[n] = tp.role(n)
			if roles[n] == "primary" {
				primary = n
			}
		}
		ok := primary != "" && tp.streaming(primary) == 2
		return ok, fmt.Sprintf("roles=%v", roles)
	})
	tp.waitWritable(primary, 30*time.Second)

	var initialized []string
	ids := map[string]bool{}
	for _, n := range []string{"a", "b", "c"} {
		if strings.Contains(tp.allLogs(n), "initdb: starting") {
			initialized = append(initialized, n)
		}
		ids[tp.sysid(n)] = true
	}
	if len(initialized) != 1 || initialized[0] != primary {
		t.Errorf("nodes that initialized a database: %v, want exactly the primary node-%s", initialized, primary)
	}
	if len(ids) != 1 {
		t.Errorf("system identifiers %v, want one", ids)
	}
}

// TestDataNodeDownEmptyNodesWait is S6 (spec 005 US3 AS-2/AS-3, SC-005).
// With node-a, the only node holding data, down, node-b and node-c
// never reach the engine: the route-mesh gate needs every peer and
// exits 75 (F15). No database is created and nothing accepts writes.
// Bringing node-a back and restarting the empty nodes (as a supervisor
// would) must then complete the expansion, which needs pg-manager 015.
func TestDataNodeDownEmptyNodesWait(t *testing.T) {
	tp := newTopo(t, "s6")
	g := buildGolden(tp, 1000, singleEnv("a"))
	tp.stop("a", 30*time.Second)
	tp.ensureVolumes("b")
	tp.ensureVolumes("c")
	tp.start("b", threeEnv("b"))
	tp.start("c", threeEnv("c"))

	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		for _, n := range []string{"b", "c"} {
			if strings.Contains(tp.allLogs(n), "initdb: starting") {
				t.Fatalf("node-%s initialized a database while node-a was down", n)
			}
			if _, err := tp.exec(n, 5*time.Second, "test", "-e", dataDir+"/PG_VERSION"); err == nil {
				t.Fatalf("node-%s has a data directory while node-a was down", n)
			}
			if tp.write(n) == nil {
				t.Fatalf("node-%s accepted a write while node-a was down", n)
			}
		}
		if exitedBoth(tp) {
			break
		}
		time.Sleep(10 * time.Second)
	}
	for _, n := range []string{"b", "c"} {
		code, err := tp.wait(n, 2*time.Minute)
		if err != nil || code != exitMeshNotFormed {
			t.Fatalf("node-%s exit = %d (%v), want %d (route mesh not formed)", n, code, err, exitMeshNotFormed)
		}
		if _, err := tp.exec(n, 5*time.Second, "true"); err == nil {
			t.Fatalf("node-%s still running", n)
		}
	}

	requireUpstream015(t)
	tp.start("a", threeEnv("a"))
	tp.start("b", threeEnv("b"))
	tp.start("c", threeEnv("c"))
	assertExpanded(tp, g, 4*time.Minute)
}

func exitedBoth(tp *topo) bool {
	for _, n := range []string{"b", "c"} {
		out, err := tp.exec(n, 5*time.Second, "true")
		if err == nil || !strings.Contains(out, "is not running") {
			return false
		}
	}
	return true
}

// TestRollbackToSingle is S3 (spec 005 US4, SC-006): after an
// expansion, stop the new nodes FIRST (research R-011), then revert
// node-a to the single-node config.
func TestRollbackToSingle(t *testing.T) {
	requireUpstream015(t)
	tp := newTopo(t, "s3")
	g := buildGolden(tp, 1000, singleEnv("a"))
	expand(tp, orderALast, threeEnv)
	assertExpanded(tp, g, 4*time.Minute)
	if err := tp.write("a"); err != nil {
		t.Fatalf("write before rollback: %v", err)
	}
	rows := tp.rows("a", "probe")

	tp.stop("b", 30*time.Second)
	tp.stop("c", 30*time.Second)
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
	if logs := tp.logs("a"); !strings.Contains(logs, `"actual_layout":"clustered"`) {
		t.Error("node-a did not move the clustered store aside on rollback")
	}

	// A second boot with the same config moves nothing.
	tp.stop("a", 30*time.Second)
	tp.start("a", singleEnv("a"))
	tp.waitPrimary("a", 30*time.Second)
	if strings.Contains(tp.logs("a"), "embedded_nats.store_archived") {
		t.Error("store archived again on a boot whose layout already matched")
	}
}
