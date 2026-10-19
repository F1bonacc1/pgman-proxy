// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build scaleout

package scaleout

import (
	"fmt"
	"time"
)

// goldenRows is the default dataset for expansion scenarios: about
// 50 MB, enough for a real clone without slowing CI (research R-010).
const goldenRows = 50000

// golden describes the single node an expansion starts from.
type golden struct {
	sysid string
	rows  int
}

// buildGolden starts node-a as a single node with env, loads rows rows
// of ~1 KB into table bulk, and leaves it running as a writable
// primary: expansions start from a live single node, so the measured
// write outage covers the operator's restart of node-a.
func buildGolden(tp *topo, rows int, env map[string]string) golden {
	tp.t.Helper()
	tp.ensureVolumes("a")
	tp.start("a", env)
	tp.waitPrimary("a", 90*time.Second)

	// Builds without the single-peer AsyncOnly fix hang every commit
	// (F1). Clear the value for the load and restore it afterwards so
	// the helper still produces a baseline on such builds.
	ssn, err := tp.ssn("a")
	if err != nil {
		tp.t.Fatalf("show synchronous_standby_names: %v", err)
	}
	if ssn != "" {
		tp.t.Logf("golden: synchronous_standby_names=%q on a single node; clearing it for the load", ssn)
		if _, err := tp.psql("a", "alter system set synchronous_standby_names = ''", "select pg_reload_conf()"); err != nil {
			tp.t.Fatalf("clear sync names: %v", err)
		}
		time.Sleep(time.Second)
		defer func() {
			if _, err := tp.psql("a", "alter system reset synchronous_standby_names", "select pg_reload_conf()"); err != nil {
				tp.t.Fatalf("reset sync names: %v", err)
			}
		}()
	}

	// probe is the write-probe table (startWriteProbe, write); it must
	// exist before the expansion so the outage measurement is real. The
	// probe commits as probeRole, which replOnlyHBA admits.
	load := fmt.Sprintf("create table bulk(id bigint primary key, pad text); "+
		"create table if not exists probe(t timestamptz default now(), src text); "+
		"create role %s login; grant insert on probe to %s; "+
		"insert into bulk select g, repeat('x', 900) from generate_series(1, %d) g", probeRole, probeRole, rows)
	out, err := tp.exec("a", 10*time.Minute, "psql", "-h", "/var/run/postgresql", "-U", "postgres",
		"-XAtq", "-v", "ON_ERROR_STOP=1", "-c", load)
	if err != nil {
		tp.t.Fatalf("load golden data: %v\n%s", err, out)
	}
	g := golden{sysid: tp.sysid("a"), rows: tp.rows("a", "bulk")}
	if g.sysid == "" || g.rows != rows {
		tp.t.Fatalf("golden: sysid=%q rows=%d, want rows=%d", g.sysid, g.rows, rows)
	}
	tp.t.Logf("golden: node-a sysid=%s rows=%d", g.sysid, g.rows)
	return g
}
