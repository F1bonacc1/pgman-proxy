// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build scaleout

package scaleout

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// joinWindow is node's /v1/status JoinWindow block (pg-manager B-025),
// nil when the cluster is not in a join window or the node is not
// answering.
func (tp *topo) joinWindow(node string) map[string]any {
	res, err := tp.control(node, "GET", "/v1/status", "")
	if err != nil {
		return nil
	}
	if engine, ok := res["engine"].(map[string]any); ok {
		res = engine
	}
	jw, _ := res["JoinWindow"].(map[string]any)
	return jw
}

// joinWindowPeers maps each peer in jw to its published LSN.
func joinWindowPeers(jw map[string]any) map[string]float64 {
	out := map[string]float64{}
	peers, _ := jw["Peers"].([]any)
	for _, p := range peers {
		m, _ := p.(map[string]any)
		id, _ := m["NodeID"].(string)
		lsn, _ := m["LSN"].(float64)
		out[id] = lsn
	}
	return out
}

// pgmctl runs pgmctl inside node against its own control plane.
func (tp *topo) pgmctl(node string, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	base := []string{"exec",
		"-e", "PGMCTL_ENDPOINT=http://127.0.0.1:9091",
		"-e", "PGMCTL_TOKEN=" + commonEnv()["PGMAN_PROXY_CONTROL_TOKEN"],
		tp.container(node), "pgmctl"}
	return docker(ctx, append(base, args...)...)
}

// S11 (pg-manager B-025): the join-window publisher is lost for good
// right after its peers finished cloning, the window nightly run
// 40176451731 (S10b) hit by accident. The survivors must keep deferring
// to it, and `pgmctl promote NODE --accept-data-loss`, run on the
// survivor that is not the target with no leader in the cluster, must
// end the window: the target becomes primary, keeps every write the
// cluster had replicated, loses only what node-a alone acknowledged,
// and node-a is fenced so it cannot promote when it returns.
//
// The cluster forms the way a 1→3 expansion does: node-a holds the
// data as a single node, then restarts into the 3-node config while
// node-b and node-c start empty. To land in the window deterministically,
// node-a's Postgres egress is throttled so b and c are still cloning
// when node-a is cut off NATS (its failover record then cannot change),
// with Postgres left reachable so the clones finish.
func TestPromoteOutOfJoinWindow(t *testing.T) {
	requireUpstream015(t)
	tp := newTopo(t, "s11")
	tp.netAdmin = true
	g := buildGolden(tp, 20000, singleEnv("a"))
	tp.mustPSQL("a", "create table golden(id int primary key)", "insert into golden select generate_series(1, 1000)")

	tp.stop("a", 30*time.Second)
	tp.ensureVolumes("b")
	tp.ensureVolumes("c")
	tp.start("a", threeEnv("a"))
	// Throttle only node-a's Postgres replies (pg_basebackup), not its
	// NATS routes.
	tp.rootExec("a", "tc", "qdisc", "add", "dev", "eth0", "root", "handle", "1:", "prio")
	tp.rootExec("a", "tc", "qdisc", "add", "dev", "eth0", "parent", "1:3", "handle", "30:",
		"tbf", "rate", "8mbit", "burst", "64kbit", "latency", "400ms")
	tp.rootExec("a", "tc", "filter", "add", "dev", "eth0", "protocol", "ip", "parent", "1:0", "prio", "3",
		"u32", "match", "ip", "sport", "5432", "0xffff", "flowid", "1:3")
	tp.start("b", threeEnv("b"))
	tp.start("c", threeEnv("c"))
	waitFor(t, 3*time.Minute, 500*time.Millisecond, "b and c cloning from node-a inside its join window", func() (bool, string) {
		backups, _ := tp.psql("a", "select count(*) from pg_stat_progress_basebackup")
		jw := tp.joinWindow("a")
		return backups == "2" && jw != nil && jw["Publisher"] == "node-a", fmt.Sprintf("basebackups=%s join_window=%v", backups, jw != nil)
	})

	// Cut node-a off NATS. Its Postgres stays reachable, so the clones
	// complete, but its failover record can no longer change.
	tp.rootExec("a", "iptables", "-A", "INPUT", "-i", "eth0", "-p", "tcp", "--dport", "5432", "-j", "ACCEPT")
	tp.rootExec("a", "iptables", "-A", "INPUT", "-i", "eth0", "-j", "DROP")
	tp.rootExec("a", "iptables", "-A", "OUTPUT", "-o", "eth0", "-p", "tcp", "--sport", "5432", "-j", "ACCEPT")
	tp.rootExec("a", "iptables", "-A", "OUTPUT", "-o", "eth0", "-j", "DROP")
	cutAt := time.Now()
	tp.rootExec("a", "tc", "qdisc", "del", "dev", "eth0", "root")
	waitFor(t, 2*time.Minute, time.Second, "b and c cloned (standbys)", func() (bool, string) {
		rb, rc := tp.role("b"), tp.role("c")
		return rb == "standby" && rc == "standby", "roles b=" + rb + " c=" + rc
	})
	if jw := tp.joinWindow("b"); jw == nil || jw["Publisher"] != "node-a" {
		t.Fatalf("node-b must see node-a's join window; got %v", jw)
	}

	// Writes only node-a acknowledges: isolate it entirely, then commit
	// locally (asynchronous: the join-window record names no peer).
	tp.rootExec("a", "iptables", "-I", "INPUT", "1", "-i", "eth0", "-j", "DROP")
	tp.rootExec("a", "iptables", "-I", "OUTPUT", "1", "-o", "eth0", "-j", "DROP")
	abandoned := 0
	for i := 1; i <= 5; i++ {
		if _, err := tp.psql("a", fmt.Sprintf("insert into golden values (%d)", 100000+i)); err == nil {
			abandoned++
		}
	}
	t.Logf("node-a acknowledged %d writes no survivor received", abandoned)
	tp.mustDocker(time.Minute, "kill", tp.container("a"))

	// Too early: node-a may only be partitioned. Right after the cut the
	// control plane can refuse every mutation for a few seconds while the
	// audit stream re-elects (audit_unavailable, FR-028); retry through
	// that, inside the silence window.
	var early error
	for time.Since(cutAt) < 45*time.Second {
		_, early = tp.control("b", "POST", "/v1/promotion-authorization",
			`{"target":"node-c","reason":"S11 early","accept_data_loss":true}`)
		if early == nil || !strings.Contains(early.Error(), "audit_unavailable") {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if early == nil || !strings.Contains(early.Error(), "not silent") {
		t.Fatalf("an authorization inside the silence window must be refused as not silent; got %v", early)
	}

	// The survivors defer for as long as the window stands.
	var jw map[string]any
	waitFor(t, 3*time.Minute, 2*time.Second, "node-a silent for the 60s abandon window", func() (bool, string) {
		if rb, rc := tp.role("b"), tp.role("c"); rb == "primary" || rc == "primary" {
			t.Fatalf("a survivor promoted inside the join window: b=%s c=%s", rb, rc)
		}
		jw = tp.joinWindow("b")
		silent, _ := jw["PublisherSilentFor"].(float64)
		return jw != nil && time.Duration(silent) > 65*time.Second, fmt.Sprintf("silent_for=%s", time.Duration(silent))
	})

	// The most advanced survivor is the target (pg-manager refuses the
	// other, naming it).
	lsn := joinWindowPeers(jw)
	target, other := "b", "c"
	if lsn["node-c"] > lsn["node-b"] {
		target, other = "c", "b"
	}
	out, err := tp.pgmctl(other, 5*time.Minute, "promote", "node-"+target, "--accept-data-loss",
		"--reason", "S11: node-a volume lost", "--force", "--cluster", "pgso")
	t.Logf("pgmctl promote on node-%s:\n%s", other, out)
	if err != nil {
		t.Fatalf("pgmctl promote node-%s: %v", target, err)
	}
	for _, want := range []string{"published by node-a", "request_id=", "node-a is fenced", "node-" + target + " is primary"} {
		if !strings.Contains(out, want) {
			t.Errorf("pgmctl output missing %q", want)
		}
	}

	tp.waitWritable(target, 2*time.Minute)
	if got := tp.rows(target, "bulk"); got != g.rows {
		t.Fatalf("node-%s has %d of node-a's %d bulk rows", target, got, g.rows)
	}
	if got := tp.mustPSQL(target, "select count(*) from golden where id <= 1000"); got != "1000" {
		t.Fatalf("node-%s lost replicated writes: %s of 1000 rows", target, got)
	}
	if got := tp.mustPSQL(target, "select count(*) from golden where id > 1000"); got != "0" {
		t.Fatalf("node-%s has %s rows only node-a acknowledged", target, got)
	}
	waitFor(t, 2*time.Minute, time.Second, "node-"+other+" streams from node-"+target, func() (bool, string) {
		r := tp.walReceiver(other)
		return r == "streaming", "wal_receiver=" + r
	})
	if jw := tp.joinWindow(other); jw != nil {
		t.Errorf("node-%s still reports a join window: %v", other, jw)
	}
	if !strings.Contains(tp.logs(target), "promotion authorization consumed") {
		t.Error("node-" + target + " did not log the authorization's consumption")
	}
	diag, err := tp.control(other, "GET", "/v1/diagnose", "")
	if err != nil || !strings.Contains(fmt.Sprint(diag), "peer fenced: node-a") {
		t.Errorf("node-a must be fenced; diagnose = %v (%v)", diag, err)
	}

	// node-a returns with its abandoned writes. It is fenced: it must
	// never answer as a primary, and must stay fenced (B-025 §5.7).
	// Its rejoin can stall the KV stream for a few seconds, long enough
	// for the new primary's lease to lapse; a survivor must hold the
	// primary role again afterwards. The gap is logged, not hidden.
	tp.start("a", threeEnv("a"))
	var lostAt, backAt time.Time
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if ra := tp.role("a"); ra == "primary" {
			t.Fatalf("the fenced ex-publisher answers as a primary")
		}
		rt, ro := tp.role(target), tp.role(other)
		switch {
		case rt != "primary" && ro != "primary" && lostAt.IsZero():
			lostAt = time.Now()
		case (rt == "primary" || ro == "primary") && !lostAt.IsZero() && backAt.IsZero():
			backAt = time.Now()
		}
		time.Sleep(time.Second)
	}
	if !lostAt.IsZero() {
		if backAt.IsZero() {
			t.Fatalf("no survivor is primary since node-a returned (%s ago)", time.Since(lostAt).Round(time.Second))
		}
		t.Logf("node-a's return cost the cluster its primary for %s", backAt.Sub(lostAt).Round(time.Second))
	}
	primary := target
	if tp.role(target) != "primary" {
		primary = other
	}
	tp.waitWritable(primary, time.Minute)
	diagA, err := tp.control("a", "GET", "/v1/diagnose", "")
	if err != nil || !strings.Contains(fmt.Sprint(diagA), "this node is fenced") {
		t.Errorf("node-a must still be fenced; diagnose = %v (%v)", diagA, err)
	}
	t.Logf("after node-a returned: a=%s %s=%s %s=%s", tp.role("a"), target, tp.role(target), other, tp.role(other))
	for _, line := range strings.Split(tp.logs("a"), "\n") {
		if strings.Contains(line, `"msg":"state transition"`) {
			t.Logf("node-a: %s", line)
		}
	}
}
