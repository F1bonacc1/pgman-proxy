// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build scaleout

package scaleout

import (
	"fmt"
	"maps"
	"strconv"
	"strings"
	"testing"
	"time"
)

// replHBA is the HBA extras value every preset uses: replication from
// any peer plus client access, trust-authenticated on the private test
// network. Lines are separated by a literal `\n`, which the env loader
// splits (internal/config loader parseLines).
const replHBA = `host replication all 0.0.0.0/0 trust\nhost all all 0.0.0.0/0 trust`

// commonEnv mirrors docs/scale-out-1-to-3/rig/common.env. The
// credentials are test-only literals, as in the compose topology.
func commonEnv() map[string]string {
	return map[string]string{
		"PGMAN_PROXY_CLUSTER_ID":                         "pgso",
		"PGMAN_PROXY_CLUSTER_USERNAME":                   "pgso",
		"PGMAN_PROXY_CLUSTER_PASSWORD_ENV":               "PGMAN_CLUSTER_PASSWORD",
		"PGMAN_CLUSTER_PASSWORD":                         "scaleout-test-cluster-secret-0123456789",
		"PGMAN_PROXY_CLUSTER_TLS_PLAINTEXT_EXPLICIT_ACK": "true",
		"PGMAN_PROXY_CLUSTER_CLIENT_LISTEN_PORT":         "4222",
		"PGMAN_PROXY_CLUSTER_ROUTES_LISTEN_PORT":         "6222",
		"PGMAN_PROXY_CLUSTER_JETSTREAM_DIR":              storeDir,
		"PGMAN_PROXY_PROXY_LISTEN_ADDR":                  "0.0.0.0:6432",
		"PGMAN_PROXY_OBS_HEALTH_ADDR":                    "0.0.0.0:9090",
		"PGMAN_PROXY_POSTGRES_BIN_DIR":                   binDir,
		"PGMAN_PROXY_POSTGRES_DATA_DIR":                  dataDir,
		"PGMAN_PROXY_POSTGRES_LOCAL_DSN_ENV":             "LOCAL_DSN",
		"LOCAL_DSN":                                      "host=/var/run/postgresql user=postgres sslmode=disable",
		"PGMAN_PROXY_OBS_LOG_LEVEL":                      "info",
		"PGMAN_PROXY_CONTROL_AUTH_TOKEN_ENV":             "PGMAN_PROXY_CONTROL_TOKEN",
		"PGMAN_PROXY_CONTROL_TOKEN":                      "scaleout-test-token",
		"PGMAN_PROXY_CONTROL_LISTEN_ADDR":                "0.0.0.0:9091",
		"PGMAN_PROXY_CONTROL_TLS_PLAINTEXT_EXPLICIT_ACK": "true",
		"PGMAN_PROXY_POSTGRES_HBA_EXTRAS":                replHBA,
		"PGMAN_PROXY_POSTGRES_CONF_EXTRAS":               `wal_keep_size = 128MB\nrestore_command = 'false'`,
	}
}

// singleEnv is node's single-peer config (rig single-a.env).
func singleEnv(node string) map[string]string {
	env := commonEnv()
	maps.Copy(env, map[string]string{
		"PGMAN_PROXY_NODE_ID":                "node-" + node,
		"PGMAN_PROXY_PEERS":                  "node-" + node,
		"PGMAN_PROXY_CLUSTER_DECLARED_SIZE":  "1",
		"PGMAN_PROXY_POSTGRES_LOCAL_PG_ADDR": "node-" + node + ":5432",
	})
	return env
}

// Node letters of the 3- and 5-peer presets. node-a is the original
// single node in every expansion.
var (
	threeNodes = []string{"a", "b", "c"}
	fiveNodes  = []string{"a", "b", "c", "d", "e"}
)

// threeEnv is node's config in the a/b/c 3-peer cluster (rig three-x.env).
func threeEnv(node string) map[string]string {
	return clusterEnv(node, threeNodes)
}

// fiveEnv is node's config in the a..e 5-peer cluster.
func fiveEnv(node string) map[string]string {
	return clusterEnv(node, fiveNodes)
}

// clusterEnv is node's config in the cluster of peers: every peer
// listed, the declared size equal to the peer count, and routes to
// every other peer.
func clusterEnv(node string, peers []string) map[string]string {
	ids := make([]string, 0, len(peers))
	var routes []string
	for _, n := range peers {
		ids = append(ids, "node-"+n)
		if n != node {
			routes = append(routes, "node-"+n+":6222")
		}
	}
	env := commonEnv()
	maps.Copy(env, map[string]string{
		"PGMAN_PROXY_NODE_ID":                "node-" + node,
		"PGMAN_PROXY_PEERS":                  strings.Join(ids, ","),
		"PGMAN_PROXY_CLUSTER_DECLARED_SIZE":  strconv.Itoa(len(peers)),
		"PGMAN_PROXY_CLUSTER_ROUTE_PEERS":    strings.Join(routes, ","),
		"PGMAN_PROXY_POSTGRES_LOCAL_PG_ADDR": "node-" + node + ":5432",
	})
	return env
}

// withoutReplHBA drops the replication rule from env's HBA extras, the
// shape of a single node deployed before anyone planned to expand it.
func withoutReplHBA(env map[string]string) map[string]string {
	out := maps.Clone(env)
	out["PGMAN_PROXY_POSTGRES_HBA_EXTRAS"] = `host all all 0.0.0.0/0 trust`
	return out
}

// psql runs each statement on node over the local socket, one `-c`
// per statement, so statements that refuse transaction blocks (ALTER
// SYSTEM) work. Output is the last statement's rows. client_min_messages
// keeps NOTICEs out of the combined output. The exec timeout is the only
// bound on a commit stuck in SyncRep (F1): PostgreSQL disables
// statement_timeout before COMMIT, so it cannot cut that wait short.
func (tp *topo) psql(node string, stmts ...string) (string, error) {
	args := []string{"psql", "-h", "/var/run/postgresql", "-U", "postgres", "-XAtq", "-v", "ON_ERROR_STOP=1",
		"-c", "set client_min_messages = warning", "-c", "set statement_timeout = '10s'"}
	for _, s := range stmts {
		args = append(args, "-c", s)
	}
	out, err := tp.exec(node, 15*time.Second, args...)
	return strings.TrimSpace(out), err
}

// role is "primary", "standby" or "down" (not answering queries).
func (tp *topo) role(node string) string {
	out, err := tp.psql(node, "select pg_is_in_recovery()")
	switch {
	case err != nil:
		return "down"
	case out == "f":
		return "primary"
	case out == "t":
		return "standby"
	}
	return "down"
}

// sysid is the database system identifier, or "" when not answering.
func (tp *topo) sysid(node string) string {
	out, err := tp.psql(node, "select system_identifier from pg_control_system()")
	if err != nil {
		return ""
	}
	return out
}

// rows counts table's rows on node; -1 when the query fails.
func (tp *topo) rows(node, table string) int {
	out, err := tp.psql(node, "select count(*) from "+table)
	if err != nil {
		return -1
	}
	n, err := strconv.Atoi(out)
	if err != nil {
		return -1
	}
	return n
}

// ssn is the effective synchronous_standby_names.
func (tp *topo) ssn(node string) (string, error) {
	return tp.psql(node, "show synchronous_standby_names")
}

// write commits one row into probe on node (creating the table on
// first use; a standby fails the CREATE, which is the right answer).
func (tp *topo) write(node string) error {
	out, err := tp.psql(node, "create table if not exists probe(t timestamptz default now(), src text)",
		"insert into probe(src) values ('scaleout') returning 'W_OK'")
	if err != nil {
		return err
	}
	if out != "W_OK" {
		return fmt.Errorf("unexpected write result %q", out)
	}
	return nil
}

// streaming counts walsenders in state streaming on node.
func (tp *topo) streaming(node string) int {
	out, err := tp.psql(node, "select count(*) from pg_stat_replication where state = 'streaming'")
	if err != nil {
		return -1
	}
	n, _ := strconv.Atoi(out)
	return n
}

// waitFor polls cond every interval until it holds or budget elapses,
// and returns the elapsed time. On timeout it fails the test with the
// last observation cond reported.
func waitFor(t *testing.T, budget, interval time.Duration, what string, cond func() (bool, string)) time.Duration {
	t.Helper()
	start := time.Now()
	last := ""
	for {
		ok, obs := cond()
		if ok {
			return time.Since(start)
		}
		last = obs
		if time.Since(start) >= budget {
			t.Fatalf("%s: not reached within %s (last: %s)", what, budget, last)
		}
		time.Sleep(interval)
	}
}

// waitPrimary waits until node answers as a primary.
func (tp *topo) waitPrimary(node string, budget time.Duration) time.Duration {
	tp.t.Helper()
	return waitFor(tp.t, budget, time.Second, "node-"+node+" primary", func() (bool, string) {
		r := tp.role(node)
		return r == "primary", "role=" + r
	})
}

// waitWritable waits until a commit on node succeeds.
func (tp *topo) waitWritable(node string, budget time.Duration) time.Duration {
	tp.t.Helper()
	return waitFor(tp.t, budget, time.Second, "node-"+node+" writable", func() (bool, string) {
		err := tp.write(node)
		if err != nil {
			return false, firstLine(err.Error())
		}
		return true, ""
	})
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
