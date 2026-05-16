# Reliability & Chaos Experiments

Operating log for chaos / reliability experiments run against the
pgman-proxy three-node rig. Each entry is one *experiment* with a
stable ID — append new entries here as they are performed.

This file is the authoritative record of what has been tested, what
was learned, and what is automated vs. still manual. It is intended
to drive future experiment planning: a glance should answer "have we
broken X yet?" and "is the fix locked by a regression test?".

## Rig under test

- `process-compose.yaml` orchestrates 3 Docker-in-Docker nodes
  (`pgman-pc-node-{a,b,c}`) on the `pgman-pc-net` bridge.
- Each node runs `pgman-proxy` + embedded NATS + PostgreSQL 18.
- Cluster ID: `pgman-pc`. Declared size: 3. Replicas factor: 3.
- Sync replication: `synchronous_commit=on`,
  `synchronous_standby_names="ANY 1 (peer, peer)"` — survives one
  standby loss without blocking, blocks on two losses.
- `chaos-workload` (`cmd/chaos-workload/main.go`) hammers writes via
  libpq multi-host DSN `host=127.0.0.1,127.0.0.1,127.0.0.1
  port=16432,16433,16434` so it follows failovers transparently.

Reported workload invariants:
- `data_loss_total` — rows the workload acknowledged but later cannot
  read on the current primary. **MUST stay 0.**
- `extra_rows` — rows present in the database with no in-memory
  acknowledgment. Locally committed but never sync-acked. Bounded.
- `writes_ok` / `writes_failed` — coarse throughput / error counters.

## Verdict legend

| Verdict | Meaning |
| --- | --- |
| **PASS** | System behaved as designed; no gap discovered. |
| **FINDING** | System behaved as designed at the *data* level, but a presentation / observability / operator-UX gap was discovered. Linked fix lands either here or in `../pg-manager`. |
| **FAIL** | Data-safety or self-healing failure. Linked bug report. |

## Auto-test classification

Each experiment notes:
- **Test exists** — pointer to the test that locks the *outcome*, or
  "no" with explanation.
- **Auto-test feasibility** — can this *full scenario* (not just its
  observable behavior) be driven by CI?

The bar for "auto-test exists" is intentionally narrow: a contract
test against a fixture that reproduces the failure mode counts; a
unit test that doesn't actually inject the fault does not. The full
chaos scenario (real PG, real NATS, real iptables, real timing)
generally requires testcontainers + CAP_NET_ADMIN and has not been
wired into CI — those are listed as **feasible but not implemented**.

---

## Experiments

### CR-001 — Graceful primary stop

- **Date:** 2026-05-16
- **Description:** `process-compose process stop <current-primary>`
  during steady-state chaos workload. The primary's container exits
  cleanly; pg-manager's signal handler runs the drain path.
- **Expected:** A surviving standby acquires the leader-key, promotes
  to PG primary, replication is restitched. Workload continues with
  bounded transient errors (libpq multi-host failover).
- **Actual:** Failover completed; workload `writes_ok` continued to
  climb; `data_loss_total=0`.
- **Verdict:** PASS
- **Test exists:** No end-to-end test. pg-manager's drain / demote
  paths have unit coverage in `../pg-manager`.
- **Auto-test feasibility:** Feasible via testcontainers + a Go test
  that runs the full 3-node rig and asserts the workload invariants.
  Not yet implemented — heavy CI footprint (3 PG-18 containers +
  embedded NATS), and timing assertions are flake-prone.

### CR-002 — SIGKILL primary

- **Date:** 2026-05-16
- **Description:** `kill -9` (or equivalent SIGKILL) of the
  pgman-proxy process on the primary node. process-compose's restart
  policy brings it back.
- **Expected:** Peer cluster fails over within heartbeat timeout; the
  killed node returns under process-compose, observes it has lost
  the leader-key, and rejoins as a standby via auto-demote +
  rebootstrap.
- **Actual:** As expected. Recent commits `6dfbb46` and `9429dcb`
  hardened exit-code propagation and CAP_NET_ADMIN handling along
  this path.
- **Verdict:** PASS
- **Test exists:** No end-to-end test.
- **Auto-test feasibility:** Same as CR-001.

### CR-003 — Two of three nodes stopped simultaneously (FTT exceeded)

- **Date:** 2026-05-16 (T0 = `13:00:43+03:00`)
- **Description:** `process-compose process stop node-a` and
  `process-compose process stop node-b` issued back-to-back, leaving
  primary `node-c` as sole survivor. Observed for 144 s.
- **Expected:** Cluster correctly becomes *unavailable for writes*
  (CP, FTT=1 on a 3-node cluster: 2-of-3 outage is outside design
  envelope). `synchronous_standby_names = "ANY 1 (node-a, node-b)"`
  blocks commits at the surviving primary. **No split-brain.**
- **Actual:**
  - `node-c` reported `primary=running` continuously for the full
    144 s. PG sync_commit correctly blocked all client writes.
  - chaos-workload: `data_loss_total=0` throughout. `extra_rows=50`
    appeared (locally committed but never sync-acked — not durable
    from the application's point of view).
  - **No way for an operator to tell from `pgmctl status` that the
    cluster had lost write quorum.** Header still read
    `Primary: node-c   Peers: 1/3 healthy`, looking like a
    transient single-peer hiccup.
- **Verdict:** FINDING (data safety PASS; observability gap)
- **Fix:** commit `c778437 feat(pgmctl): surface substrate quorum
  loss in status output`. Adds the **QUORUM LOST** line and a
  derivable `substrate.{required,responding,total,quorate}` block
  in `-o json`. Verdict downgrades to `ExitUnhealthy` (exit 2).
- **Test exists:** Yes.
  `tests/contract/pgmctl/status_test.go`:
  - `TestStatus_SubstrateQuorumLost_JSON`
  - `TestStatus_SubstrateQuorumLost_Text`
  Both run against a `statusQuorumLost()` fixture (no real cluster
  needed).
- **Auto-test feasibility:** The behavior fix is automated. Driving
  the *real* 2-of-3 outage in CI is feasible (process-compose stop
  is scriptable) but not implemented; the fixture-based tests catch
  the regression cheaply.

### CR-004 — Network partition isolating the current primary

- **Date:** 2026-05-16 (first partition: `15:28:25+03:00`; healed
  `15:33:43+03:00`. Re-runs against node-b, then post-fix against
  node-a, all on the same date.)
- **Description:** Inside the primary's container, inject:

  ```
  iptables -A INPUT  -s <peer_a_ip> -j DROP
  iptables -A INPUT  -s <peer_b_ip> -j DROP
  iptables -A OUTPUT -d <peer_a_ip> -j DROP
  iptables -A OUTPUT -d <peer_b_ip> -j DROP
  ```

  This severs *all* inter-container traffic (NATS routes 6222,
  client 4222, PG streaming 5432) while preserving host→container
  control-plane reachability via the docker bridge gateway.

- **Expected:**
  - Surviving 2/3 peers retain JS quorum, elect new leader, new
    primary takes over.
  - Isolated former primary's PG sync_commit blocks all writes
    (`synchronous_standby_names` references peers it can't reach).
  - No data divergence makes it to the application — `data_loss_total`
    stays 0.
- **Actual (chaos run summary):**
  - **Failover sub-second**: at the first sample (T+0 in the
    observation loop) the surviving peers had already converged on
    a new leader.
  - **PG split-brain visible at SQL level**: both old and new primary
    returned `pg_is_in_recovery=false`. WAL diverged on the isolated
    side (old primary's WAL grew from `0/AB40090` → `0/AB540D0`,
    ≈80 KB of failed-write WAL).
  - **Writes on the isolated primary blocked**: a probe
    `INSERT INTO partition_probe` hung for the full 8 s timeout.
  - **Workload data-safety preserved**: `writes_failed` only went
    365 → 369 (+4) over a 5-minute partition; libpq multi-host
    absorbed the rest. `data_loss_total = 0`.
  - **Observability gap**: querying the isolated primary's
    `/v1/status` directly returned `LeaderNodeID=<self>,
    PrimaryNodeID=<self>` — the local snapshot was frozen at the
    moment substrate failed.
  - `/v1/status` response time on the partitioned node degraded
    from <100 ms (healthy) to ~1 s, causing a too-tight `--max-time
    1` probe to mistakenly read it as UNREACHABLE.
- **Verdict:** FINDING (data safety PASS; observability FAIL —
  split-leadership-belief on the isolated side).
- **Fix:** commit `17d9c3b feat(pgmctl): mark Leader/Primary stale
  when substrate is non-quorate`. When `total>0 && !quorate`, pgmctl
  forces SevFail on the Leader/Primary headlines with a `(stale)`
  suffix and exposes `leader_belief_stale` /
  `primary_belief_stale` in the JSON envelope (omitempty so the
  healthy shape is unchanged).
- **Test exists:** Yes.
  - `TestStatus_SubstrateQuorumLost_Text` — asserts the `(stale)`
    suffix appears on `node-c` and that the QUORUM LOST line
    fires.
  - `TestStatus_SubstrateQuorumLost_JSON` — asserts
    `leader_belief_stale=true` and `primary_belief_stale=true`.
  - `TestStatus_HealthyCluster_NoStaleFlags` — negative case;
    healthy snapshot must not leak the flags (omitempty).
- **Auto-test feasibility:** The pgmctl-rendering fix is automated.
  The *underlying* split-leadership condition (real iptables + real
  PG + real JS-quorum loss + real timing) is NOT automated. Doing
  so would require:
  - Container test harness with CAP_NET_ADMIN (already wired for
    chaos-workload — see commit `9429dcb`).
  - A way to drive iptables rules from a Go test without
    privileged-mode escalation. Achievable via `docker exec
    iptables` from the host test, but slow (multi-second container
    spin-up per case) and flakier than fixture tests.
  - End-to-end fault-injection tests are tracked here, not in CI.

### CR-005 — Partition heal: old primary reintegration

- **Date:** 2026-05-16
- **Description:** After CR-004's iptables DROP rules left the old
  primary isolated with divergent WAL, run `iptables -F` to remove
  them. Observe how the cluster reintegrates the former primary.
- **Expected:**
  - Substrate visibility restores; former primary observes it has
    lost the leader-key; demotes itself; throws away divergent WAL;
    rebuilds from new primary; resumes streaming as a standby.
- **Actual:** 14-second end-to-end recovery, captured from
  `node-c`'s logs (12:33:50 → 12:34:04 UTC):

  | Δt | Event |
  | --- | --- |
  | +7 s | `demote: starting` → `demote: complete needs_rewind=false` |
  | +9 s | `auto_demote.detected condition=divergent_ex_primary leader_at_detection=node-b` |
  | +9 s | `auto_demote.refused reason=leadership_not_stable` (5 s stability window) |
  | +13 s | `auto_demote.probe_attempted result=confirmed_primary target=node-b` |
  | +13 s | `state transition: running → bootstrapping reason=auto_demote_triggered` |
  | +13 s | `auto_demote.wipe.{started,completed}` (54 ms) on `/var/lib/postgresql/data` |
  | +14 s | `bootstrap: basebackup` from node-b (668 ms) |
  | +14 s | `auto_demote.conninfo_written target=node-b` |
  | +14 s | `auto_demote.streaming.resumed slot=pgmgr_node_c` |

  Final row counts across 3 nodes within `chaos_events` matched
  modulo intra-second replication delta (227049 / 227051 / 227053).
- **Verdict:** PASS
- **Test exists:** No end-to-end test in this repo. The
  `auto_demote` state machine itself is exercised in
  `../pg-manager`'s unit tests.
- **Auto-test feasibility:** Same constraints as CR-004 — feasible
  via testcontainers, not implemented.

### CR-006 — Partition isolating a STANDBY

- **Date:** 2026-05-16
- **Description:** After CR-005 ended with the former primary
  reintegrated as a standby, the *same node* was re-partitioned —
  this time it was a standby, not the leader, at injection time.
- **Expected:** pgmctl status from the isolated standby should
  correctly retain a *non-self* `LeaderNodeID` (the leader from the
  surviving partition, since that was the truth pre-isolation); the
  QUORUM LOST surface should fire from the substrate-fan-out angle;
  no stale-self-leadership belief.
- **Actual:**
  - `Leader: node-b` (correct; this was the actual leader pre-partition).
  - `Primary: (none)` (engine cleared this when it could no longer
    confirm).
  - **Substrate: QUORUM LOST · 1/3 responding (need 2)** line fired.
  - Verdict downgraded to `ExitUnhealthy` (exit 2).
- **Verdict:** PASS — pgmctl's existing fan-out-derived quorum
  surface already handled this case correctly via `c778437`.
- **Test exists:** Yes — the existing
  `TestStatus_SubstrateQuorumLost_*` fixtures cover this shape.
- **Auto-test feasibility:** Already automated at the fixture level.

### CR-007 — Partition isolating the CURRENT LEADER (reproduce stale-belief)

- **Date:** 2026-05-16
- **Description:** Same iptables injection as CR-004, but targeting
  whichever node *currently holds the leader-key* at injection time.
  The pre-injection cluster had `node-b` as primary.
- **Expected:** pgmctl status against the now-isolated leader should
  warn that its leader belief is stale. Verdict unhealthy.
- **Actual (pre-fix run):**
  - `Leader: node-b   Primary: node-b` — both rendered in GREEN PASS
    color from the isolated side.
  - QUORUM LOST line and `Overall: FAIL` did fire (good).
  - **Headline misleading**: an operator scanning the first line
    would read "node-b is healthy primary".
- **Verdict:** FAIL (operator UX). Drove fix `17d9c3b`.
- **Test exists:** Yes (post-fix) — the same tests listed under
  CR-004 lock the stale-headline behavior. Before the fix, this
  case was a coverage gap with no regression test.
- **Auto-test feasibility:** Yes; achieved via fixture tests.

### CR-008 — Post-fix live validation of CR-007

- **Date:** 2026-05-16
- **Description:** After `17d9c3b` landed, re-ran the
  partition-current-leader scenario against `node-a` (which was the
  primary at that point in the rig). Used real iptables on a real
  cluster — not a fixture — to verify the fix in situ.
- **Expected:** pgmctl renders
  `Leader: node-a (stale)   Primary: node-a (stale)`, both red; JSON
  exposes `leader_belief_stale=true` and `primary_belief_stale=true`;
  exit 2.
- **Actual:** Exactly as expected. After heal, the cluster cleanly
  re-converged with `node-b` as new leader and `node-a` as standby.
- **Verdict:** PASS
- **Test exists:** Same regression tests as CR-007.
- **Auto-test feasibility:** Already at the fixture level. The live
  rig validation in this entry is a manual smoke-test; not run by
  CI.

### CR-009 — SIGKILL the postmaster only (proxy stays up)

- **Date:** 2026-05-16 (T0 = `16:43:40+03:00`)
- **Description:** Inside the primary container (node-b at injection
  time), find the postmaster PID and SIGKILL it. pgman-proxy is PID 1
  and remains the postmaster's parent — killing only the postmaster
  takes PostgreSQL down while leaving the proxy / NATS / leader-key
  lease all alive.

  Command:
  ```
  PM=$(docker exec pgman-pc-node-b sh -c "pgrep -f 'postgres -D /var/lib/postgresql/data' | head -1")
  docker exec pgman-pc-node-b kill -9 $PM
  ```
- **Expected:** pg-manager's local-PG health observer detects the
  dead postmaster within a few seconds. Either:
  (a) the proxy stops renewing its leader-key (since it knows its own
  PG is down) and a peer takes over, OR
  (b) the proxy restarts PG locally and reconfirms its leadership.
  Workload should see a brief gap, then resume on a new (or
  recovered) primary.
- **Actual — TWO CRITICAL BUGS:**

  **Bug #1 — Zombie primary (no failover for 97+ seconds).** For
  the full duration of the observation loop (97 seconds), the
  cluster's `/v1/status` from every node continued to report
  `LeaderNodeID=node-b PrimaryNodeID=node-b`. The proxy on node-b
  was PID 1 (alive); the leader-key lease was still being renewed
  in JetStream. No peer attempted to take over.

  - node-b's own `/v1/status`: `b: role=1 state=3 up=True` — the
    local instance row still claimed PostgresUp=true even though
    the postmaster process was gone.
  - node-c's view: `b: role=0 state=0 up=False` — node-c DID
    observe via fan-out that node-b's PG was down. But this did
    not trigger any cluster-level action.
  - node-a's view: `b: role=1 state=3 up=True` — node-a's view of
    node-b stayed stale even at T+97s.
  - **chaos-workload was blocked on EVERY proxy port** because all
    three proxies route writes to the primary's PG, which was
    dead. Error: `failed to receive message: unexpected EOF` on
    16432, 16433, AND 16434 simultaneously.

  **Bug #2 — 65,519-row data-loss event during recovery.** I forced
  recovery by `process-compose process restart node-b`. Within ~30 s
  the cluster converged on node-c as new primary, node-b rejoined
  as standby, but:

  - Pre-CR-009 baseline: `writes_ok=267,255  data_loss_total=51
    extra_rows=56`. (The 51 is itself a finding — see Notes
    below.)
  - Post-recovery: `writes_ok=267,538  writes_failed=4,175
    data_loss_total=65,574  rows_in_db=202,019  max_seq=271,510`.
  - Delta: **+65,523 rows of data loss**. The workload had
    acknowledged ~267,500 writes; only ~202,000 of those rows
    survived in the new primary's database. 65k acknowledged
    writes vanished.
  - This is a CP-system invariant violation. With `synchronous_commit=on`
    and `synchronous_standby_names="ANY 1 (node-a, node-c)"` at
    node-b, every acked write must have been durable on at least
    one of node-a or node-c. Yet node-c — the promoted survivor —
    is missing 65k of those rows.

  Plausible chain: node-a was the sync ACKer for many writes,
  node-c was lagging; on promotion the selector promoted node-c
  (perhaps because it had a successful probe) without checking WAL
  recency vs. peers. node-a then ended up in state=failed (its
  view was ahead of the new primary), confirming the "promoted the
  wrong replica" hypothesis.

- **Verdict:** FAIL (data safety) — **STOP-THE-WORLD CLASS.** Two
  distinct bugs:

  1. **Failover does not trigger on PG-only failure.** Leader-key
     renewal must be gated on local-PG health; today it is not.
     A primary whose PG has crashed (OOM, SEGV, operator
     mistake) holds the leader-key indefinitely.
  2. **Promotion may select a replica behind the freshest peer.**
     The selector that picks who promotes during failover does
     not (or did not in this run) require the new primary's WAL
     to dominate every other reachable standby's WAL — leading
     to silent data loss.
- **Fix:** NOT YET IMPLEMENTED. Investigation pending.
- **Test exists:** No. Neither bug has regression coverage yet.
- **Auto-test feasibility:** Both bugs are testable.
  - Bug #1: kill the postmaster inside a testcontainers rig, assert
    a peer takes over within N seconds. Requires container access.
  - Bug #2: an integration test that wedges replication on one
    standby (e.g. SIGSTOP) then kills the primary; assert promotion
    refuses or picks the freshest standby. Doable in CI with care.
- **Notes / open items:**
  - The 51 rows of data_loss already present at baseline must
    have leaked in during the CR-004..CR-008 partition cycles
    despite each of those runs verifying `data_loss_total=0` at
    the end. The chaos-workload's log buffer rolled (1000-line
    cap; this is a logging issue worth fixing as well) so we
    cannot timestamp those 51 rows precisely.
  - After recovery, node-a remained in `state=failed`; pg-manager's
    failover_quorum snapshot showed `standby_names=["node-b"]` only,
    excluding node-a. Reattaching node-a is its own work.
  - This experiment is conclusive enough to halt further chaos until
    Bug #1 and Bug #2 are root-caused. Adding new fault surfaces on
    top of a known-corrupt rig produces noise, not signal.

---

## Candidate experiments (planning backlog)

Surfaced during chaos sessions; not yet executed. Append as we run them.

- **CR-010 — Asymmetric partition.** One-direction iptables DROP
  (e.g., primary sends to standby but never receives ACKs). Tests
  whether NATS / pg-manager heartbeats correctly fail closed in the
  presence of half-open connections.
- **CR-011 — Slow link (netem delay).** Inject 500 ms-1 s latency on
  one node's link via `tc qdisc add netem`. Tests whether
  leadership-lease timing margins are robust to "slow but reachable"
  peers.
- **CR-012 — Standby disk full.** `dd` a large file into the standby's
  PG data dir until the volume is at 99 %. Tests whether replication
  blocks gracefully and whether the primary backs off correctly.
- **CR-013 — Double failover in quick succession.** Kill primary,
  wait for failover to complete, kill the *new* primary within 5 s.
  Tests whether the state machine handles back-to-back transitions
  or gets stuck in a stability-window refusal.
- **CR-014 — Recovery race after total cluster stop.** Stop all 3
  nodes, then `process-compose process start` them all at the same
  instant. Tests whether the bootstrap-leader race resolves cleanly.
- **CR-015 — Workload during graceful primary stop.** Verify the
  *graceful* CR-001 case under load (CR-001 has only been verified
  in steady state). Tests whether in-flight transactions on the
  outgoing primary terminate cleanly vs. orphaning.
