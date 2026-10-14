// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build scaleout

package scaleout

import (
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Expansion start orders (contracts/scaleout-suite.md S1). node-a is a
// running single node in every case; "restart" is stop + start with
// the 3-node config.
const (
	orderAFirst   = "a-first"  // restart node-a, then start node-b and node-c
	orderALast    = "a-last"   // start node-b and node-c, then restart node-a
	orderTogether = "together" // stop node-a, then start all three back to back
)

// expand pushes the 3-node config: node-b and node-c start empty and
// node-a restarts, in the given order.
func expand(tp *topo, order string) {
	tp.t.Helper()
	tp.ensureVolumes("b")
	tp.ensureVolumes("c")
	restartA := func() {
		tp.stop("a", 30*time.Second)
		tp.start("a", threeEnv("a"))
	}
	startBC := func() {
		tp.start("b", threeEnv("b"))
		tp.start("c", threeEnv("c"))
	}
	switch order {
	case orderAFirst:
		restartA()
		startBC()
	case orderALast:
		startBC()
		time.Sleep(time.Second)
		restartA()
	case orderTogether:
		tp.stop("a", 30*time.Second)
		tp.start("a", threeEnv("a"))
		startBC()
	default:
		tp.t.Fatalf("unknown start order %q", order)
	}
}

// assertExpanded checks the end state of a 1→3 expansion from g
// (spec 005 US2 AS-1/AS-3, SC-002, FR-008): one database identity,
// every golden row everywhere, node-a primary with both new nodes
// streaming and named in its sync set, and no new database created on
// the new nodes.
func assertExpanded(tp *topo, g golden, budget time.Duration) {
	tp.t.Helper()
	waitFor(tp.t, budget, 2*time.Second, "node-a primary with node-b and node-c streaming", func() (bool, string) {
		ra, rb, rc, s := tp.role("a"), tp.role("b"), tp.role("c"), tp.streaming("a")
		obs := fmt.Sprintf("roles a=%s b=%s c=%s, streaming(a)=%d", ra, rb, rc, s)
		return ra == "primary" && rb == "standby" && rc == "standby" && s == 2, obs
	})
	for _, n := range []string{"a", "b", "c"} {
		if id := tp.sysid(n); id != g.sysid {
			tp.t.Errorf("node-%s system identifier = %q, want golden %q (a second database exists)", n, id, g.sysid)
		}
	}
	// A standby replays asynchronously until the sync set settles;
	// poll rather than read once.
	waitFor(tp.t, time.Minute, 2*time.Second, "golden rows on every node", func() (bool, string) {
		ra, rb, rc := tp.rows("a", "bulk"), tp.rows("b", "bulk"), tp.rows("c", "bulk")
		return ra == g.rows && rb == g.rows && rc == g.rows, fmt.Sprintf("bulk rows a=%d b=%d c=%d want %d", ra, rb, rc, g.rows)
	})
	waitFor(tp.t, time.Minute, 2*time.Second, "node-a sync set names node-b and node-c", func() (bool, string) {
		ssn, err := tp.ssn("a")
		return err == nil && strings.Contains(ssn, "node-b") && strings.Contains(ssn, "node-c"), "synchronous_standby_names=" + ssn
	})
	for _, n := range []string{"b", "c"} {
		logs := tp.allLogs(n)
		if strings.Contains(logs, "initdb: starting") {
			tp.t.Errorf("node-%s initialized its own database during the expansion", n)
		}
		if !strings.Contains(logs, "bootstrap: starting basebackup") {
			tp.t.Errorf("node-%s logs show no clone from the primary", n)
		}
	}
	if logs := tp.allLogs("a"); !strings.Contains(logs, "embedded_nats.store_archived") ||
		!strings.Contains(logs, `"actual_layout":"standalone"`) {
		tp.t.Error("node-a did not move its standalone JetStream store aside")
	}
}

// startWriteProbe runs a client container that commits through node's
// proxy port every 250 ms and prints "ok <unix-ms>" or "fail <unix-ms>".
// `timeout` bounds a commit stuck in SyncRep.
func (tp *topo) startWriteProbe(node string) {
	tp.t.Helper()
	script := `while true; do
  if timeout 5 psql "host=node-` + node + ` port=6432 user=postgres dbname=postgres connect_timeout=2" -XAtqc "insert into probe(src) values ('probe')" >/dev/null 2>&1
  then echo "ok $(date +%s%3N)"; else echo "fail $(date +%s%3N)"; fi
  sleep 0.25
done`
	tp.mustDocker(time.Minute, "run", "-d", "--name", tp.prefix+"-probe", "--network", tp.net,
		"--entrypoint", "bash", image, "-c", script)
}

// longestWriteGap parses the probe's output and returns the longest
// interval between consecutive successful commits.
func (tp *topo) longestWriteGap() (time.Duration, int) {
	tp.t.Helper()
	out := tp.mustDocker(time.Minute, "logs", tp.prefix+"-probe")
	var last int64
	var longest time.Duration
	oks := 0
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || f[0] != "ok" {
			continue
		}
		ms, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		if last != 0 {
			if gap := time.Duration(ms-last) * time.Millisecond; gap > longest {
				longest = gap
			}
		}
		last = ms
		oks++
	}
	return longest, oks
}

// TestExpandOneToThree is S1 (spec 005 US2, SC-002, SC-003): five
// expansions of a live single node with randomized start order.
func TestExpandOneToThree(t *testing.T) {
	requireUpstream015(t)
	iterations := 5
	if v, err := strconv.Atoi(os.Getenv("PGMAN_SCALEOUT_ITERATIONS")); err == nil && v > 0 {
		iterations = v
	}
	seed := time.Now().UnixNano()
	t.Logf("start-order seed %d", seed)
	r := rand.New(rand.NewPCG(uint64(seed), 0)) //nolint:gosec // test ordering, not security
	var orders []string
	for len(orders) < iterations {
		round := []string{orderAFirst, orderALast, orderTogether}
		r.Shuffle(len(round), func(i, j int) { round[i], round[j] = round[j], round[i] })
		orders = append(orders, round...)
	}
	for i, order := range orders[:iterations] {
		t.Run(fmt.Sprintf("%d-%s", i+1, order), func(t *testing.T) {
			tp := newTopo(t, "s1")
			g := buildGolden(tp, goldenRows, singleEnv("a"))
			tp.startWriteProbe("a")
			time.Sleep(2 * time.Second) // a baseline of successful commits before the restart
			expand(tp, order)
			assertExpanded(tp, g, 4*time.Minute)
			gap, oks := tp.longestWriteGap()
			t.Logf("longest write gap through node-a: %s (%d commits)", gap.Round(time.Millisecond), oks)
			if gap >= 30*time.Second {
				t.Errorf("write outage %s, want < 30s (SC-003)", gap)
			}
		})
	}
}

// TestExpandWithoutPriorReplicationHBA is S7 (spec 005 US2 AS-4,
// FR-013..FR-015, F11): a single node deployed without replication
// access rules gains them from the 3-node config on its next boot. The
// file and log assertions on node-a hold on today's engine; the clone
// assertions need pg-manager 015.
func TestExpandWithoutPriorReplicationHBA(t *testing.T) {
	tp := newTopo(t, "s7")
	g := buildGolden(tp, 1000, withoutReplHBA(singleEnv("a")))
	hba, err := tp.exec("a", 10*time.Second, "cat", dataDir+"/pg_hba.conf")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(hba, "host replication all 0.0.0.0/0 trust") {
		t.Fatal("golden node already has the replication rule; the scenario needs a node without it")
	}

	expand(tp, orderALast)
	waitFor(t, 3*time.Minute, 2*time.Second, "node-a applied the managed HBA block at boot", func() (bool, string) {
		logs := tp.logs("a")
		return strings.Contains(logs, "postgres.managed_block_applied") && strings.Contains(logs, `"phase":"boot"`),
			"no postgres.managed_block_applied phase=boot yet"
	})
	hba, err = tp.exec("a", 10*time.Second, "cat", dataDir+"/pg_hba.conf")
	if err != nil {
		t.Fatal(err)
	}
	want := "# BEGIN pgman-proxy managed: postgres.hba_extras\nhost replication all 0.0.0.0/0 trust\nhost all all 0.0.0.0/0 trust\n# END pgman-proxy managed: postgres.hba_extras\n"
	if !strings.Contains(hba, want) {
		t.Fatalf("node-a pg_hba.conf lacks the managed block:\n%s", hba)
	}

	if !upstream015() {
		t.Log("pg-manager 015 not present: skipping the clone assertions")
		return
	}
	assertExpanded(tp, g, 4*time.Minute)
}
