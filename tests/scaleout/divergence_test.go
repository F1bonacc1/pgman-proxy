// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build scaleout

package scaleout

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// mustPSQL is psql that fails the test on error.
func (tp *topo) mustPSQL(node string, stmts ...string) string {
	tp.t.Helper()
	out, err := tp.psql(node, stmts...)
	if err != nil {
		tp.t.Fatalf("psql on node-%s: %v", node, err)
	}
	return out
}

// waitReplayed waits until every standby has replayed primary's WAL up
// to where it is now.
func (tp *topo) waitReplayed(primary string, standbys []string) {
	tp.t.Helper()
	lsn := tp.mustPSQL(primary, "select pg_current_wal_lsn()")
	waitFor(tp.t, time.Minute, 500*time.Millisecond, "standbys replayed to "+lsn, func() (bool, string) {
		got := map[string]string{}
		ok := true
		for _, s := range standbys {
			out, err := tp.psql(s, "select pg_last_wal_replay_lsn() >= '"+lsn+"'::pg_lsn")
			got[s] = out
			ok = ok && err == nil && out == "t"
		}
		return ok, fmt.Sprintf("caught up=%v", got)
	})
}

// forkPoint reads the last line of primary's current timeline history
// file: the parent timeline, the LSN where it ended, and the current
// timeline.
func (tp *topo) forkPoint(primary string) (parent int, switchLSN string, current int) {
	tp.t.Helper()
	tliHex := tp.mustPSQL(primary, "select substr(pg_walfile_name(pg_current_wal_lsn()), 1, 8)")
	cur, err := strconv.ParseInt(tliHex, 16, 64)
	if err != nil {
		tp.t.Fatalf("node-%s timeline %q: %v", primary, tliHex, err)
	}
	hist := tp.mustPSQL(primary, "select pg_read_file('pg_wal/"+tliHex+".history')")
	lines := strings.Split(strings.TrimSpace(hist), "\n")
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 2 {
		tp.t.Fatalf("node-%s history file %s.history: %q", primary, tliHex, hist)
	}
	if parent, err = strconv.Atoi(f[0]); err != nil {
		tp.t.Fatalf("node-%s history line %q: %v", primary, lines[len(lines)-1], err)
	}
	return parent, f[1], int(cur)
}

// walRecordAt runs pg_waldump against node's stopped data volume and
// returns the first record on timeline tli at or after lsn, or "" with
// pg_waldump's complaint when there is none.
func (tp *topo) walRecordAt(node string, tli int, lsn string) (string, string) {
	tp.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := docker(ctx, "run", "--rm", "--entrypoint", binDir+"/pg_waldump",
		"-v", tp.volume(node, "data")+":"+dataDir, image,
		"-p", dataDir+"/pg_wal", "-t", strconv.Itoa(tli), "-s", lsn, "-n", "1")
	if err != nil {
		return "", strings.TrimSpace(out)
	}
	return strings.TrimSpace(out), ""
}

// parseLSN turns PostgreSQL's "X/Y" notation into a byte position.
func parseLSN(t *testing.T, s string) uint64 {
	t.Helper()
	hi, lo, ok := strings.Cut(s, "/")
	h, err1 := strconv.ParseUint(hi, 16, 32)
	l, err2 := strconv.ParseUint(lo, 16, 32)
	if !ok || err1 != nil || err2 != nil {
		t.Fatalf("parse LSN %q", s)
	}
	return h<<32 | l
}

// control calls node's control plane over loopback and returns the
// envelope's engine_result.
func (tp *topo) control(node, method, path, body string) (map[string]any, error) {
	args := []string{"curl", "-s", "-X", method,
		"-H", "Authorization: Bearer " + commonEnv()["PGMAN_PROXY_CONTROL_TOKEN"],
		"-H", "Content-Type: application/json"}
	if body != "" {
		args = append(args, "-d", body)
	}
	out, err := tp.exec(node, 30*time.Second, append(args, "http://127.0.0.1:9091"+path)...)
	if err != nil {
		return nil, err
	}
	var env struct {
		Outcome      string         `json:"outcome"`
		EngineResult map[string]any `json:"engine_result"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", method, path, err, firstLine(out))
	}
	if env.EngineResult == nil {
		return nil, fmt.Errorf("%s %s: outcome %q: %s", method, path, env.Outcome, firstLine(out))
	}
	return env.EngineResult, nil
}

// timelineDivergence is node's /v1/status TimelineDivergence (nil when
// it reports none).
func (tp *topo) timelineDivergence(node string) (map[string]any, error) {
	res, err := tp.control(node, "GET", "/v1/status", "")
	if err != nil {
		return nil, err
	}
	engine, _ := res["engine"].(map[string]any)
	if engine == nil {
		return nil, fmt.Errorf("/v1/status without an engine block: %v", res)
	}
	div, _ := engine["TimelineDivergence"].(map[string]any)
	return div, nil
}

// timelineCheck runs the replication.timeline-consistent doctor check
// on node and returns its status and message.
func (tp *topo) timelineCheck(node string) (string, string) {
	tp.t.Helper()
	res, err := tp.control(node, "POST", "/v1/doctor/run", `{"check":"replication.timeline-consistent"}`)
	if err != nil {
		tp.t.Fatalf("doctor on node-%s: %v", node, err)
	}
	checks, _ := res["checks"].([]any)
	if len(checks) != 1 {
		tp.t.Fatalf("doctor on node-%s returned %v", node, res)
	}
	c, _ := checks[0].(map[string]any)
	status, _ := c["status"].(string)
	msg, _ := c["message"].(string)
	return status, msg
}

// waitGauge waits until node's gauge name reads want.
func (tp *topo) waitGauge(node, name, want string) {
	tp.t.Helper()
	waitFor(tp.t, 30*time.Second, time.Second, fmt.Sprintf("node-%s %s = %s", node, name, want), func() (bool, string) {
		v, ok := tp.gauge(node, name)
		return ok && v == want, fmt.Sprintf("value=%q found=%v", v, ok)
	})
}

// diverged is what divergeOldPrimary leaves behind.
type diverged struct {
	old, promoted         string
	standbys              []string
	parentTLI, clusterTLI int
	switchLSN             string
}

// divergeOldPrimary forms a 3-node cluster and leaves its primary
// diverged, as in pgman-proxy issue #12, with its container stopped.
// pg-manager v0.7.2 never noticed this state: it compared the old
// primary's last checkpoint with the promoted standby's last
// restartpoint, and here both are the same checkpoint.
//
// The divergence is built deterministically: one shared checkpoint,
// more replicated writes, then the primary is cut off the network,
// commits 100 rows locally, and is SIGKILLed. pg_waldump proves its
// WAL runs past the new timeline's switch point.
func divergeOldPrimary(tp *topo) diverged {
	t := tp.t
	t.Helper()
	old, standbys := formThree(tp)
	tp.mustPSQL(old, "create table s10(id bigint primary key)",
		"insert into s10 select generate_series(1, 100)", "checkpoint")
	tp.waitReplayed(old, standbys)
	for _, s := range standbys {
		tp.mustPSQL(s, "checkpoint")
	}
	tp.mustPSQL(old, "insert into s10 select generate_series(101, 200)")
	tp.waitReplayed(old, standbys)
	ckpt := map[string]string{}
	for _, n := range append([]string{old}, standbys...) {
		ckpt[n] = tp.mustPSQL(n, "select checkpoint_lsn from pg_control_checkpoint()")
	}
	for _, s := range standbys {
		if ckpt[s] != ckpt[old] {
			t.Fatalf("precondition: every node's last checkpoint must be the same one, got %v", ckpt)
		}
	}
	t.Logf("shared last checkpoint %s", ckpt[old])

	tp.mustDocker(time.Minute, "network", "disconnect", tp.net, tp.container(old))
	if _, err := tp.psql(old, "set synchronous_commit = local",
		"insert into s10 select generate_series(1001, 1100)"); err != nil {
		t.Fatalf("local commit on the cut-off node-%s: %v", old, err)
	}
	localEnd := tp.mustPSQL(old, "select pg_current_wal_lsn()")
	tp.mustDocker(time.Minute, "kill", "-s", "KILL", tp.container(old))

	d := diverged{old: old, standbys: standbys}
	d.promoted = tp.waitPromoted(standbys, 2*time.Minute)
	tp.waitWritable(d.promoted, 30*time.Second)
	d.parentTLI, d.switchLSN, d.clusterTLI = tp.forkPoint(d.promoted)
	t.Logf("node-%s promoted to timeline %d; timeline %d ended at %s; node-%s wrote to %s",
		d.promoted, d.clusterTLI, d.parentTLI, d.switchLSN, old, localEnd)
	rec, complaint := tp.walRecordAt(old, d.parentTLI, d.switchLSN)
	if rec == "" {
		t.Fatalf("precondition: node-%s has no WAL past the switch point %s: %s", old, d.switchLSN, complaint)
	}
	t.Logf("node-%s WAL past the switch point: %s", old, firstLine(rec))
	return d
}

// TestDivergedExPrimaryReported is S10a (pgman-proxy issue #12): with
// auto-rebootstrap off, the diverged ex-primary is reported everywhere
// an operator looks, from the new primary's own timeline history, and
// left alone.
//
// The first report can come from the fork record in the coordination
// store: at boot the node does not know the leader yet. Once it does,
// the report must rest on the leader's history (evidence "source").
func TestDivergedExPrimaryReported(t *testing.T) {
	tp := newTopo(t, "s10a")
	d := divergeOldPrimary(tp)
	old, promoted := d.old, d.promoted

	tp.start(old, threeEnv(old))
	var div map[string]any
	elapsed := waitFor(t, 2*time.Minute, 2*time.Second, "node-"+old+" reporting a timeline divergence from node-"+promoted+"'s history",
		func() (bool, string) {
			got, err := tp.timelineDivergence(old)
			if err != nil {
				return false, firstLine(err.Error())
			}
			div = got
			return got["Evidence"] == "source", fmt.Sprintf("TimelineDivergence=%v wal_receiver=%s", got, tp.walReceiver(old))
		})
	t.Logf("node-%s TimelineDivergence after %s: %v", old, elapsed.Round(time.Second), div)
	if div["SourceNodeID"] != "node-"+promoted {
		t.Errorf("divergence source = %v, want node-%s", div["SourceNodeID"], promoted)
	}
	num := func(k string) uint64 { f, _ := div[k].(float64); return uint64(f) }
	if num("LocalTimeline") != uint64(d.parentTLI) || num("ClusterTimeline") != uint64(d.clusterTLI) {
		t.Errorf("timelines %d -> %d, want %d -> %d", num("LocalTimeline"), num("ClusterTimeline"), d.parentTLI, d.clusterTLI)
	}
	if got, want := num("SwitchLSN"), parseLSN(t, d.switchLSN); got != want {
		t.Errorf("SwitchLSN = %X, want %X from the history file", got, want)
	}
	if num("LocalEndLSN") <= num("SwitchLSN") {
		t.Errorf("LocalEndLSN %X is not past SwitchLSN %X", num("LocalEndLSN"), num("SwitchLSN"))
	}

	tp.waitGauge(old, "pgman_proxy_timeline_diverged", "1")
	for _, n := range d.standbys {
		if v, ok := tp.gauge(n, "pgman_proxy_timeline_diverged"); !ok || v != "0" {
			t.Errorf("node-%s pgman_proxy_timeline_diverged = %q (found %v), want 0", n, v, ok)
		}
	}
	if status, msg := tp.timelineCheck(promoted); status != "FAIL" || !strings.Contains(msg, "node-"+old) {
		t.Errorf("doctor on node-%s: %s %q, want FAIL naming node-%s", promoted, status, msg, old)
	} else {
		t.Logf("doctor on node-%s: %s", promoted, msg)
	}
	if !strings.Contains(tp.logs(promoted), "pgmanager.pgso.timeline.diverged") {
		t.Errorf("node-%s logged no timeline.diverged event", promoted)
	}
	if wr := tp.walReceiver(old); wr == "streaming" {
		t.Errorf("node-%s WAL receiver streams while diverged", old)
	}
	if strings.Contains(tp.logs(old), "bootstrap: starting basebackup") {
		t.Errorf("node-%s was re-cloned with auto-rebootstrap off", old)
	}
}

// TestDivergedExPrimaryRecloned is S10b (pgman-proxy issue #12), the
// issue's own setup: auto-rebootstrap is on when the diverged
// ex-primary comes back. Postgres starts, but its WAL receiver cannot
// follow the new timeline. pg-manager must re-clone it through the
// stale-WAL regime (detection_source local_lag_persisted), whose wipe
// needs node-<promoted> to confirm the divergence from its own
// history, and not wait for some later failure to start Postgres
// (local_pg_start_failed). The node then streams without the commits
// only it held, and every signal clears.
func TestDivergedExPrimaryRecloned(t *testing.T) {
	tp := newTopo(t, "s10b")
	d := divergeOldPrimary(tp)
	old, promoted := d.old, d.promoted

	env := threeEnv(old)
	env["PGMAN_PROXY_POLICY_AUTO_REBOOTSTRAP_ENABLED"] = "true"
	env["PGMAN_PROXY_POLICY_AUTO_REBOOTSTRAP_PERSISTENCE_WINDOW"] = "15s"
	env["PGMAN_PROXY_POLICY_AUTO_REBOOTSTRAP_COOLDOWN"] = "60s"
	tp.start(old, env)
	elapsed := waitFor(t, 4*time.Minute, 2*time.Second, "node-"+old+" re-cloned and streaming", func() (bool, string) {
		r, wr, n := tp.role(old), tp.walReceiver(old), tp.streaming(promoted)
		return r == "standby" && wr == "streaming" && n == 2,
			fmt.Sprintf("node-%s role=%s wal_receiver=%s, streaming(%s)=%d", old, r, wr, promoted, n)
	})
	t.Logf("node-%s streaming %s after its restart", old, elapsed.Round(time.Second))

	logs := tp.logs(old)
	for _, marker := range []string{"pgmanager.pgso.timeline.diverged", "bootstrap: starting basebackup"} {
		if !strings.Contains(logs, marker) {
			t.Errorf("node-%s log lacks %q", old, marker)
		}
	}
	decided := ""
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "pgmanager.pgso.auto_rebootstrap.decided") {
			decided = line
		}
	}
	if !strings.Contains(decided, `"detection_source":"local_lag_persisted"`) {
		t.Errorf("node-%s was not re-cloned through the stale-WAL regime; decided event: %q", old, decided)
	}
	if n := strings.Count(logs, `"reason":"divergence_unconfirmed"`); n > 0 {
		t.Logf("node-%s refused %d time(s) with divergence_unconfirmed before the leader confirmed", old, n)
	}

	tp.waitGauge(old, "pgman_proxy_timeline_diverged", "0")
	if div, err := tp.timelineDivergence(old); err != nil || div != nil {
		t.Errorf("node-%s TimelineDivergence after the re-clone = %v (%v), want none", old, div, err)
	}
	if status, msg := tp.timelineCheck(promoted); status != "PASS" {
		t.Errorf("doctor on node-%s after the re-clone: %s %q, want PASS", promoted, status, msg)
	}
	for _, n := range []string{promoted, old} {
		if got := tp.mustPSQL(n, "select count(*) filter (where id <= 200) || '/' || count(*) filter (where id > 1000) from s10"); got != "200/0" {
			t.Errorf("node-%s s10 rows replicated/unreplicated = %s, want 200/0", n, got)
		}
	}
}
