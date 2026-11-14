// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build scaleout

package scaleout

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// refusedOnSilentC matches node-a's LSN-gate refusal against node-c
// while node-c has published nothing (pg-manager B-028).
var refusedOnSilentC = regexp.MustCompile(`"msg":"promote: refused \(LSN gate\)".*"reason":"sync_peer_silent".*"leader_node":"node-c"`)

// TestExpandWithUnreportedPeer is S12 (pg-manager B-028, nightly run
// 43585096410). During a 1→3 expansion node-a's first promotion
// attempt finds node-c named in its synchronous set with no data-state
// record. node-a must keep the lease until node-c reports and then
// promote: no resign, node-a primary.
//
// In the nightly, node-c had no record because its runtime was still
// booting. Here it has none because its data directory cannot be
// classified: PG_VERSION is a symlink loop, so stat fails with ELOOP.
// pg-manager writes no record on a classification error ("never empty
// on an error"), and pgman-proxy's own PG_VERSION check stops node-c's
// runtime before its Manager starts.
//
// Two things keep node-c in node-a's pool for the whole wait, as in the
// nightly:
//   - node-b cannot clone yet. Once a peer streams or runs a base backup
//     from node-a, pg-manager narrows node-a's pool to live peers and
//     drops node-c, which would end the wait early.
//   - node-c stays unreported for 8 s after node-a's first refusal,
//     past B-027's three-tick hold (6 s at the 2 s tick). The nightly's
//     peer reported 6.1 s after it, so only B-028's longer hold keeps
//     node-a's lease here.
//
// node-c is then wiped and rebooted as an empty node, and its first
// record lands the way a late booter's would.
func TestExpandWithUnreportedPeer(t *testing.T) {
	requireUpstream015(t)
	tp := newTopo(t, "s12")
	tp.netAdmin = true
	g := buildGolden(tp, 1000, singleEnv("a"))
	cData := map[string]string{tp.volume("c", "data"): "/d"}
	blockB := []string{"INPUT", "-p", "tcp", "--dport", "5432", "-s", "node-b", "-j", "REJECT"}

	tp.stop("a", 30*time.Second)
	tp.ensureVolumes("b")
	tp.ensureVolumes("c")
	tp.rootRun(cData, "ln -s PG_VERSION /d/PG_VERSION")
	tp.start("b", threeEnv("b"))
	tp.start("c", threeEnv("c"))
	tp.start("a", threeEnv("a"))
	tp.rootExec("a", append([]string{"iptables", "-A"}, blockB...)...)
	waitFor(t, 3*time.Minute, time.Second, "node-a refused on a silent node-c", func() (bool, string) {
		return refusedOnSilentC.MatchString(tp.logs("a")), "not yet"
	})

	refused := time.Now()
	time.Sleep(8 * time.Second)
	tp.stop("c", 30*time.Second)
	tp.rootRun(cData, "find /d -mindepth 1 -delete")
	tp.start("c", threeEnv("c"))
	tp.waitPrimary("a", time.Minute)
	t.Logf("node-a primary %s after its first refusal", time.Since(refused).Round(time.Millisecond))
	tp.rootExec("a", append([]string{"iptables", "-D"}, blockB...)...)

	assertExpanded(tp, g, 4*time.Minute)

	logs := tp.logs("a")
	if n := strings.Count(logs, "resigned leadership: promotion failed"); n != 0 {
		t.Errorf("node-a resigned %d times on a refusal it could wait out (B-028)", n)
	}
	if !strings.Contains(logs, "holding lease for a sync peer that has not published a data record") {
		t.Error("node-a did not log the B-028 lease hold")
	}
}
