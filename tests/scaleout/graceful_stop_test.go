// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build scaleout

package scaleout

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// gracefulStopTimeout is the docker stop grace for S9: above the 30 s
// default drain budget, as the runbook requires.
const gracefulStopTimeout = 45 * time.Second

// controlData runs pg_controldata against node's data volume in a
// one-off container and returns its "key: value" lines. node's own
// container must be stopped.
func (tp *topo) controlData(node string) map[string]string {
	tp.t.Helper()
	out := tp.mustDocker(time.Minute, "run", "--rm", "--entrypoint", binDir+"/pg_controldata",
		"-v", tp.volume(node, "data")+":"+dataDir, image, "-D", dataDir)
	cd := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok {
			cd[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return cd
}

// clusterState is pg_controldata's "Database cluster state" for node.
func (tp *topo) clusterState(node string) string {
	return tp.controlData(node)["Database cluster state"]
}

// walReceiver is node's WAL receiver status ("" when it has none).
func (tp *topo) walReceiver(node string) string {
	out, err := tp.psql(node, "select status from pg_stat_wal_receiver")
	if err != nil {
		return "error"
	}
	return out
}

// formThree starts three empty nodes and waits for one primary with
// two streaming standbys. It returns the primary and the standbys.
func formThree(tp *topo) (string, []string) {
	tp.t.Helper()
	nodes := []string{"a", "b", "c"}
	for _, n := range nodes {
		tp.ensureVolumes(n)
	}
	for _, n := range nodes {
		tp.start(n, threeEnv(n))
	}
	var primary string
	waitFor(tp.t, 4*time.Minute, 2*time.Second, "one primary with two streaming standbys", func() (bool, string) {
		roles := map[string]string{}
		primary = ""
		for _, n := range nodes {
			roles[n] = tp.role(n)
			if roles[n] == "primary" {
				primary = n
			}
		}
		return primary != "" && tp.streaming(primary) == 2, fmt.Sprintf("roles=%v", roles)
	})
	tp.waitWritable(primary, 30*time.Second)
	var standbys []string
	for _, n := range nodes {
		if n != primary {
			standbys = append(standbys, n)
		}
	}
	return primary, standbys
}

// assertStoppedPostgres checks node's log for the drain's postgres step
// in fast mode.
func assertStoppedPostgres(tp *topo, node string) {
	tp.t.Helper()
	logs := tp.logs(node)
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, `"msg":"postgres stopped"`) {
			tp.t.Logf("node-%s drain: %s", node, strings.TrimSpace(line))
			if !strings.Contains(line, `"mode":"fast"`) {
				tp.t.Errorf("node-%s stopped Postgres in a degraded mode: %s", node, line)
			}
			return
		}
	}
	tp.t.Errorf("node-%s logged no 'postgres stopped' during its drain", node)
}

// TestGracefulStopStandby is S9a (pgman-proxy issue #11): stopping a
// standby's container shuts its Postgres down cleanly. Before the fix
// the drain left Postgres running and the runtime SIGKILLed it, so the
// control file read "in archive recovery".
func TestGracefulStopStandby(t *testing.T) {
	tp := newTopo(t, "s9a")
	primary, standbys := formThree(tp)
	s := standbys[0]
	if st := tp.walReceiver(s); st != "streaming" {
		t.Fatalf("node-%s WAL receiver = %q before the stop, want streaming", s, st)
	}

	tp.stop(s, gracefulStopTimeout)
	if st := tp.clusterState(s); st != "shut down in recovery" {
		t.Fatalf("node-%s cluster state = %q after docker stop, want %q", s, st, "shut down in recovery")
	}
	assertStoppedPostgres(tp, s)

	tp.start(s, threeEnv(s))
	waitFor(t, 2*time.Minute, 2*time.Second, "node-"+s+" streaming again", func() (bool, string) {
		n := tp.streaming(primary)
		return n == 2, fmt.Sprintf("streaming(%s)=%d", primary, n)
	})
}

// TestGracefulStopPrimaryUnderWrites is S9b (pgman-proxy issue #11):
// stopping the primary's container while it takes writes. The drain
// must stop Postgres in fast mode while the node still holds the
// lease, so the walsenders ship every record, the shutdown checkpoint
// included, before any standby promotes. Then:
//   - the old primary's control file reads "shut down";
//   - every commit the writer saw acknowledged is on the new primary;
//   - the old primary rejoins as a streaming standby with no clone and
//     no rewind.
//
// Before the fix the postmaster was SIGKILLed with WAL past the new
// timeline's fork point, and the old primary sat in WAL receiver state
// "waiting" (issue #12).
func TestGracefulStopPrimaryUnderWrites(t *testing.T) {
	tp := newTopo(t, "s9b")
	old, standbys := formThree(tp)
	if _, err := tp.psql(old, "create table gs(id bigint primary key)"); err != nil {
		t.Fatalf("create table: %v", err)
	}

	// The writer bypasses the proxy, as in the issue, so commits keep
	// arriving until the postmaster stops accepting them. It prints the
	// id of every acknowledged commit.
	writer := tp.prefix + "-writer"
	script := `i=0; while true; do i=$((i+1))
  if timeout 5 psql "host=node-` + old + ` port=5432 user=postgres dbname=postgres connect_timeout=2" -XAtqc "insert into gs(id) values ($i)" >/dev/null 2>&1
  then echo "ok $i"; else echo "fail $i"; fi
  sleep 0.05
done`
	tp.mustDocker(time.Minute, "run", "-d", "--name", writer, "--network", tp.net,
		"--entrypoint", "bash", image, "-c", script)
	waitFor(t, 30*time.Second, 500*time.Millisecond, "writer committing", func() (bool, string) {
		n := len(ackedIDs(tp, writer))
		return n >= 20, fmt.Sprintf("acked=%d", n)
	})

	tp.stop(old, gracefulStopTimeout)
	tp.mustDocker(time.Minute, "stop", "-t", "1", writer)
	acked := ackedIDs(tp, writer)
	t.Logf("writer saw %d acknowledged commits before the stop", len(acked))

	if st := tp.clusterState(old); st != "shut down" {
		t.Fatalf("node-%s cluster state = %q after docker stop, want %q", old, st, "shut down")
	}
	assertStoppedPostgres(tp, old)

	var promoted string
	waitFor(t, 2*time.Minute, time.Second, "a standby promoted", func() (bool, string) {
		roles := map[string]string{}
		for _, n := range standbys {
			roles[n] = tp.role(n)
			if roles[n] == "primary" {
				promoted = n
				return true, ""
			}
		}
		return false, fmt.Sprintf("roles=%v", roles)
	})
	tp.waitWritable(promoted, 30*time.Second)
	if missing := missingIDs(tp, promoted, acked); len(missing) > 0 {
		t.Errorf("node-%s lacks %d acknowledged commits, e.g. ids %v", promoted, len(missing), missing[:min(len(missing), 10)])
	}

	tp.start(old, threeEnv(old))
	waitFor(t, 2*time.Minute, 2*time.Second, "node-"+old+" streaming from node-"+promoted, func() (bool, string) {
		r, wr, n := tp.role(old), tp.walReceiver(old), tp.streaming(promoted)
		return r == "standby" && wr == "streaming" && n == 2,
			fmt.Sprintf("node-%s role=%s wal_receiver=%s, streaming(%s)=%d", old, r, wr, promoted, n)
	})
	logs := tp.logs(old)
	for _, marker := range []string{"bootstrap: starting basebackup", `"to":"rewinding"`} {
		if strings.Contains(logs, marker) {
			t.Errorf("node-%s needed a repair to rejoin (log has %q); a clean stop must not", old, marker)
		}
	}
}

// TestGracefulOperatorRestart is S9c (pgman-proxy issue #11): POST
// /v1/restart target=proxy must drain like a stop signal. Before the
// fix the self-terminator called os.Exit(0) right after the response,
// skipping the drain, so Postgres died with the container.
func TestGracefulOperatorRestart(t *testing.T) {
	tp := newTopo(t, "s9c")
	primary, standbys := formThree(tp)
	s := standbys[0]

	// The restart pre-flight refuses without a detectable supervisor.
	// The test container has none, and assume_supervised has no env
	// override, so mimic process-compose, which detection reads from
	// PC_PROC_NAME.
	tp.stop(s, gracefulStopTimeout)
	env := threeEnv(s)
	env["PC_PROC_NAME"] = "scaleout"
	tp.start(s, env)
	waitFor(t, 2*time.Minute, 2*time.Second, "node-"+s+" streaming", func() (bool, string) {
		n := tp.streaming(primary)
		return n == 2, fmt.Sprintf("streaming(%s)=%d", primary, n)
	})

	code, err := tp.exec(s, 30*time.Second, "curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
		"-X", "POST", "-H", "Authorization: Bearer "+commonEnv()["PGMAN_PROXY_CONTROL_TOKEN"],
		"-H", "Content-Type: application/json",
		"-d", `{"target":"proxy","target_node":"node-`+s+`"}`,
		"http://127.0.0.1:9091/v1/restart")
	if err != nil || strings.TrimSpace(code) != "200" {
		t.Fatalf("POST /v1/restart = %q (%v), want 200", code, err)
	}
	exit, err := tp.wait(s, gracefulStopTimeout)
	if err != nil {
		t.Fatalf("waiting for node-%s to exit: %v", s, err)
	}
	if exit != 0 {
		t.Errorf("node-%s exited %d after an operator restart, want 0", s, exit)
	}
	if st := tp.clusterState(s); st != "shut down in recovery" {
		t.Fatalf("node-%s cluster state = %q after an operator restart, want %q", s, st, "shut down in recovery")
	}
	assertStoppedPostgres(tp, s)
}

// ackedIDs parses the writer's "ok <id>" lines.
func ackedIDs(tp *topo, writer string) []int {
	tp.t.Helper()
	out := tp.mustDocker(time.Minute, "logs", writer)
	var ids []int
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || f[0] != "ok" {
			continue
		}
		if id, err := strconv.Atoi(f[1]); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// missingIDs returns the ids in want that node's gs table lacks.
func missingIDs(tp *topo, node string, want []int) []int {
	tp.t.Helper()
	if len(want) == 0 {
		return nil
	}
	list := make([]string, len(want))
	for i, id := range want {
		list[i] = strconv.Itoa(id)
	}
	out, err := tp.psql(node, "select id from unnest(array["+strings.Join(list, ",")+"]::bigint[]) id where id not in (select id from gs) order by id")
	if err != nil {
		tp.t.Fatalf("query acknowledged ids on node-%s: %v", node, err)
	}
	var missing []int
	for _, f := range strings.Fields(out) {
		if id, err := strconv.Atoi(f); err == nil {
			missing = append(missing, id)
		}
	}
	return missing
}
