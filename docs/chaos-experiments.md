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

- **Verdict (original CR-009 run):** FAIL (data safety) —
  **STOP-THE-WORLD CLASS.** Two distinct bugs:

  1. **Failover does not trigger on PG-only failure.** Leader-key
     renewal must be gated on local-PG health; today it is not.
     A primary whose PG has crashed (OOM, SEGV, operator
     mistake) holds the leader-key indefinitely.
  2. **Promotion may select a replica behind the freshest peer.**
     The selector that picks who promotes during failover does
     not (or did not in this run) require the new primary's WAL
     to dominate every other reachable standby's WAL — leading
     to silent data loss.

- **CURRENT STATUS: CLEARED** (as of CR-009c, 2026-05-16). The
  combined fix (milestone-012 in `../pg-manager` + fix 1D in
  `internal/pgproto/pgexec.go`) closes both bugs. The fix landed in
  three stages:

  | Stage | Run | Outcome |
  | --- | --- | --- |
  | Original | CR-009 | FAIL — two bugs surfaced, 65k-row data loss |
  | First attempt | CR-009b | NOT CLEARED — milestone-012 unreachable because of zombie-blind `IsRunning` |
  | After 1D | **CR-009c** | **CLEARED** — failover in 2–5 s, `data_loss_total = 0` |

- **Fix:** milestone-012 (`../pg-manager` branch
  `012-cr009-failover-safety`) provides
  `LeadershipProvider.Resign(ctx)` + `Reconciler.resignOnPostgresCrash`
  called from the `EventPostgresCrashed` arm
  (`reconciler/reconciler.go:842-844`); `WithHealthCheck` callback on
  the NATS leadership adapter (`adapters/nats/leadership.go:319`);
  sync-aware `pgmanager.ResolveTolerance` replaces the `0 → 16 MiB`
  silent override. The additional **fix 1D**
  (`internal/pgproto/pgexec.go:394-407`) reads `/proc/<pid>/stat`
  field 3 and returns `false` from `IsRunning` when the postmaster
  PID is a zombie — closing the precondition gap that made
  milestone-012 unreachable in CR-009b.
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

### CR-009b — Re-test under milestone-012 (`012-cr009-failover-safety`)

- **Date:** 2026-05-16 (T0 = `19:37:43+03:00`)
- **Description:** Identical procedure to CR-009 against the rig
  rebuilt against pg-manager branch `012-cr009-failover-safety`. The
  Docker image `pgman-proxy:dev` was rebuilt at `19:35:06+03:00`
  (mtime of `/usr/local/bin/pgman-proxy` inside the image:
  `2026-05-16 16:34 UTC`), which is *after* the resign / health-gate /
  ResolveTolerance file modifications (`reconciler/resign.go` 17:51,
  `adapters/nats/leadership.go` 17:52, `reconciler/reconciler.go`
  19:03). chaos-workload was restarted fresh — baseline
  `writes_ok=2899, writes_failed=0, data_loss_total=0, extra_rows=0`.

- **Expected:** Bug #1 cleared — failover triggers within a few
  ticks (10 s budget) after the postmaster dies. Bug #2 cleared —
  `data_loss_total` stays at 0 through the failover. The reconciler
  emits `resigned leadership: local postgres unreachable` on the
  isolated primary.

- **Actual — Bug #1 NOT CLEARED, Bug #2 UNTESTABLE in this run:**

  - **At T+0 through T+121 s**, all three nodes' `/v1/status`
    reported `LeaderNodeID=node-b PrimaryNodeID=node-b` with node-b's
    own row showing `State=running, PostgresUp=true`.
  - **Container logs** (`docker logs pgman-pc-node-b`) show node-b
    continuing to publish `failover_quorum_published` events with
    itself as primary for the entire window, e.g. at T+4 min:
    ```
    {"subject":"pgmanager.pgman-pc.failover_quorum_published",
     "snapshot":{"method":"ANY","primary":"node-b",
                 "standby_names":["node-a","node-c"]}}
    ```
  - **No `resigned leadership` log line ever appears** — the resign
    code path at `reconciler/reconciler.go:842-844` is never reached.
  - chaos-workload starts failing the same way as in CR-009: all
    three proxy ports return `unexpected EOF` because every proxy
    routes writes to the (dead) primary's PG.
  - Recovery required the same `process-compose process restart
    node-b` intervention as CR-009.

  **Mechanism (verified):** the zombie postmaster process survives
  in the kernel process table because pgman-proxy (PID 1, its
  parent) has not reaped it. Probing inside the container at T+5 min:

  ```
  postmaster.pid: 32
  PID 32 is ALIVE          ← kill -0 32 returned nil
  /proc/32/comm: postgres  ← still readable on a zombie
  ```

  Yet `ps -ef` shows `postgres   32   1   [postgres] <defunct>` —
  classic zombie. The `IsRunning` probe at
  `../pg-manager/internal/pgproto/pgexec.go:366-394` uses signal-0
  liveness + `/proc/<pid>/comm` content; **both succeed on a
  zombie**, so `IsRunning` returns `(true, nil)`,
  `obs.PostgresUp = true`, the
  `if !o.PostgresUp` block at `reconciler/reconciler.go:814` is never
  entered, and `resignOnPostgresCrash` is never called.

  Bug #2 (data loss on promotion) cannot be tested here because
  failover never fires.

- **Verdict:** Bug #1 NOT CLEARED. Bug #2 untestable. The
  milestone-012 fix in pg-manager (resign-on-PG-crash + health-gated
  renewal + sync-aware tolerance) is *correct in design* but
  *unreachable in practice*: the precondition (`o.PostgresUp == false`)
  is never observed when the parent of the dying postmaster is the
  pgman-proxy process itself, because the parent does not reap the
  child.

- **New root cause:** `IsRunning` is zombie-blind. Detection needs
  one of:

  1. **Read `/proc/<pid>/stat` field 3** — a `Z` character means
     zombie. Cheap, Linux-only, and would cleanly handle the
     pgman-proxy-as-postmaster-parent case without any process-tree
     changes. Suggested fix:

     ```go
     // pgproto/pgexec.go::IsRunning, after the kill(pid,0) check
     if stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
         // /proc/<pid>/stat fields: 1=pid 2=comm(in-parens) 3=state
         // 'Z' = zombie. Comm may contain spaces inside the parens
         // so split on the LAST ')' before parsing.
         if i := bytes.LastIndexByte(stat, ')'); i >= 0 && len(stat) > i+2 {
             if stat[i+2] == 'Z' { return false, nil }
         }
     }
     ```

  2. **Have pgman-proxy reap its children** — call `wait4(WNOHANG)`
     periodically on a SIGCHLD-aware handler, so the dead postmaster
     leaves the process table and the next `kill(pid, 0)` returns
     ESRCH. More invasive but a general hygiene improvement (PID 1
     in container/init contexts should always reap).

  Option 1 is the minimal fix and lives in the pgproto package.

- **Test exists:** No. CR-009b confirms there's a missing unit test
  for `IsRunning` against a zombie process — straightforward to
  construct on Linux via a fork+exit+pause-parent harness.

- **Auto-test feasibility:** Yes. A unit test in
  `internal/pgproto/pgexec_test.go` that:
  1. Forks a child that `exit(0)`s immediately.
  2. Does NOT call `wait4` on the parent side (so the child is a
     zombie).
  3. Writes the child PID to a fake `postmaster.pid` in a temp data
     dir.
  4. Calls `IsRunning(ctx)` and asserts it returns `false`.
  This is portable to Linux CI; macOS has a different `/proc`
  semantics but the production target is Linux.

### CR-009c — Re-test with fix 1D added (zombie-aware `IsRunning`)

- **Date:** 2026-05-16 (T0 = `23:34:44+03:00`)
- **Description:** Same procedure as CR-009 / CR-009b. Image rebuilt
  at `23:32:33+03:00` (binary mtime `20:32 UTC`) against pg-manager
  with fix 1D applied at `internal/pgproto/pgexec.go:394-407` —
  `IsRunning` now reads `/proc/<pid>/stat` field 3 after the existing
  `kill(pid,0)` and `/proc/<pid>/comm` checks and returns
  `(false, nil)` when the state byte is `'Z'`.

  Fresh chaos-workload baseline at injection time:
  `writes_ok=1899  writes_failed=0  data_loss_total=0  extra_rows=0`.

- **Expected:** Bug #1 — failover triggers within a couple of ticks
  (≤10 s budget); `resigned leadership: local postgres unreachable`
  appears in the killed primary's log. Bug #2 —
  `data_loss_total` stays at 0 through the failover; the new
  primary's `synchronous_standby_names` reconfigures to include only
  reachable peers without losing acked WAL.

- **Actual — BOTH BUGS CLEARED.**

  **Failover timeline (observed in the docker logs of node-b, the
  former primary):**

  | Δt from kill | Event |
  | --- | --- |
  | ~2 s | `resigned leadership: local postgres unreachable` |
  | ~2 s | `state transition: running → failed  role=primary  reason=postgres_crashed` |
  | ~5 s | `state_transition` event from node-a: `from=3 to=4 reason=became_leader` |
  | ~6 s | `peer_slots_ensured` on new primary: slots created for `pgmgr_node_b`, `pgmgr_node_c` |
  | ~7 s | `pg_config_change` on new primary: `synchronous_standby_names = ANY 1 ("node-b","node-c")` (and shortly after `ANY 1 ("node-c")` once node-b's failed state propagated) |

  All-node `/v1/status` snapshot at T+3 s:
  ```
  :19191 [node-a] L=node-b P=node-b   node-b: state=8 up=False
  :19192 [node-b] L=-      P=node-b   node-b: state=8 up=False
  :19193 [node-c] L=node-b P=node-b   node-b: state=8 up=False
  ```
  Within four more seconds the leader/primary fields had converged
  on node-a across all three peers.

  **Workload numbers (final, ~7 min after kill):**

  | Metric | Baseline | After |
  | --- | --- | --- |
  | writes_ok | 1,899 | 10,850 (+8,951 during the chaos run) |
  | writes_failed | 0 | **143** (in-flight during the ~5 s failover window) |
  | **data_loss_total** | 0 | **0** ✓ |
  | **extra_rows** | 0 | **0** ✓ |
  | rows_in_db | 1,899 | 10,850 (matches writes_ok) |

- **Verdict:** **CLEARED.** Both CR-009 bugs are now closed.
  - Bug #1 fixed: ~2 s detection vs. the original 97+ s zombie
    window. The `resigned leadership: local postgres unreachable`
    log line is the smoking-gun confirmation that the
    milestone-012 path (`resignOnPostgresCrash`) is actually
    reached now that fix 1D unblocks the precondition.
  - Bug #2 fixed: `data_loss_total = 0` and `extra_rows = 0`
    across the entire failover. The new primary's
    `synchronous_standby_names` was reconfigured cleanly to the
    reachable sync-eligible peers without acked writes being
    lost. The sync-aware tolerance (`pgmanager.ResolveTolerance`
    returning 0 for `QuorumSync` policies) and the existing LSN
    gate together ensured the right standby was promoted.

- **Test exists:** Two regression-test surfaces now apply.

  1. Unit test for fix 1D belongs in
     `internal/pgproto/pgexec_test.go` — see CR-009b's auto-test
     description (fork-exit-don't-wait, build a zombie, assert
     `IsRunning == false`). This is the highest-signal test and
     should be required for the fix to land.
  2. End-to-end chaos coverage (kill postmaster, assert failover +
     no data loss) remains a candidate for testcontainers — same
     constraints as CR-001/CR-002.

- **Auto-test feasibility:** The unit test in (1) above is portable
  Linux CI. The full chaos scenario requires container test harness
  with CAP_NET_ADMIN, same constraints as the rest of the
  network-partition family.

- **Follow-up observation (not a CR-009 regression):** Seven minutes
  after CR-009c, node-b is still in `state=failed role=primary` and
  has not auto-rebootstrapped back into the cluster as a standby.
  Peers (node-a, node-c) are *publishing* `auto_rebootstrap.detected`
  events identifying node-b as needing recovery, but node-b's own
  reconciler has not transitioned out of `StateFailed`. This is the
  inverse of the milestone-011 "fast-fail auto-rebootstrap from
  StateFailed" path. Recorded as **CR-016** in the planning backlog
  below for separate investigation — it does not affect CR-009's
  data-safety verdict (no data loss occurred and the cluster has a
  healthy primary), but it does mean operators must manually
  `process-compose restart` a node whose PG was SIGKILLed in order
  to fully restore 3/3 capacity.

---

### CR-016 — SIGKILL primary under load: cluster wedges 3-of-3 StateFailed

**Initial verdict: FAIL (CRITICAL). After Fix B1: PARTIAL — Bug B fixed
(no destructive wipe), Bug A and Bug C still open (cluster outage on
the unlucky timing path, but data integrity preserved).**

| Stage   | Result                                                                                                                                                              |
| ---     | ---                                                                                                                                                                  |
| CR-016a | First repro: 3-of-3 StateFailed, two standbys with **wiped PGDATA**, cluster wedged 10+ min until manual restart. `data_loss_total=0` but availability=0.            |
| CR-016b | After Fix B1 (source-primary liveness gate). 3 repros: 2× clean failover (~7s), 1× **half-wedge** — B1 refused dispatches with `source_primary_unreachable`, both standbys intact, but no new primary elected. `data_loss_total=0` in all 3 runs.|

The remainder of this entry describes the original CR-016a repro
(the wedge). Fix B1 details are in the "Fix follow-up" section below.

This experiment was scoped to confirm the original CR-016 hypothesis
(failed ex-primary stranded in `StateFailed/RolePrimary`). It instead
surfaced a **much more severe** failure mode in the same path: under
sustained write load, SIGKILL of the primary postmaster can leave the
entire 3-node cluster wedged in `StateFailed`, all PG instances down,
with no automated recovery. The original CR-016 (stranded ex-primary)
is one of three failures; the new ones are healthy standbys
*self-destroying* on a stale snapshot.

The CR-009c "failover in ~2s" verification did not reproduce this
wedge — the failover-vs-rebootstrap race is timing-sensitive and
CR-009c happened to land on the favorable side. The unfavorable side
is what every operator will eventually hit in production.

- **Scenario:** Rig at steady state (8/8 doctor PASS, node-a primary).
  `chaos-workload` driving 20 writes/sec through the multi-host libpq
  DSN. `docker exec pgman-pc-node-a kill -9 <postmaster_pid>` at
  T0=20:58:34.869Z.

- **What we expected (going in):**
  1. node-a state_transition Running→Failed, role stays Primary, PG
     down — same as CR-009c.
  2. Standby (node-b or node-c) takes the leadership lease via
     pg-manager's KV-lease race and promotes — same as CR-009c.
  3. node-a strands at `StateFailed/RolePrimary` (the original CR-016
     hypothesis) and we observe how long it takes the fast-fail
     rebootstrap to *not* kick in.

- **What actually happened:**

  | T+ (s) | Node   | Event                                                                                                   |
  | ---    | ---    | ---                                                                                                     |
  | +1.2   | node-a | `resigned leadership: local postgres unreachable`; state_transition `Running→Failed`, role `Primary`    |
  | +1.4   | node-b | `auto_rebootstrap.detected consecutive_ticks=1 detection_source=local_lag_persisted`                    |
  | +1.5   | node-c | `auto_rebootstrap.detected consecutive_ticks=1 detection_source=local_lag_persisted`                   |
  | +3..+11| node-b | detected ticks 2..6 (every 2 s)                                                                         |
  | +13.4  | node-b | state_transition `Running→Bootstrapping`, `auto_rebootstrap.decided source_primary_id="node-a"` ⚠️       |
  | +13.5  | node-b | `auto_rebootstrap.wipe.completed duration=38.9ms` — **PGDATA wiped**                                    |
  | +13.5  | node-b | `auto_rebootstrap.basebackup.started source_primary_id="node-a"` — connection refused × 11 attempts     |
  | +33.5  | node-c | `auto_rebootstrap.decided source_primary_id="node-a"`; wipe + basebackup against dead node-a (× 12)     |
  | +9 min | all    | `pgmctl get peers`: node-a primary/failed/down, node-b standby/failed/down, node-c standby/failed/down. **No recovery.** |

  Workload final tally on shutdown 10 min after T0:
  `writes_ok=532, writes_failed=12054, data_loss_total=0, extra_rows=0`.
  All 532 successful writes were the pre-T0 baseline; the cluster
  served zero writes for ten minutes and remained wedged.

- **Root causes (interacting).** Three separate code-path bugs amplify
  into a wedge.

  1. **No new leader is elected during the 10-second
     `AUTO_REBOOTSTRAP_PERSISTENCE_WINDOW`.** node-a explicitly resigns
     leadership at T+1.2s, but the standbys do not race to claim the
     vacated lease aggressively enough — at T+13s neither has been
     elected, and both `o.LeaderNodeID` snapshots still resolve to
     `node-a`. (The KV-lease release vs. the standbys' next observation
     tick is the race.)
  2. **Slow-degradation rebootstrap dispatches with a stale
     source-primary.** `maybeAutoRebootstrap` (lag-persisted regime,
     milestone 004) reads `o.LeaderNodeID` at decision time without a
     liveness probe against the chosen source. node-a is `StateFailed`
     and unreachable on TCP/5432, yet the decided event captures
     `source_primary_id="node-a"`. node-b wipes its own PGDATA before
     even attempting the first basebackup TCP connection — i.e. the
     destructive step happens before evidence of source liveness. node-c
     does the same thing 20 s later against the same dead source.
  3. **Failed ex-primary stays operator-sticky** (the original
     CR-016 hypothesis, confirmed). `reconciler/reconciler.go:872`
     admits `maybeAutoRebootstrapFromFailed` only when
     `curRole ∈ {Standby, Unknown}`. A failed ex-primary keeps
     `curRole == Primary` until rebootstrap drives it back through
     `Standby`, so the fast-fail recovery path is permanently gated
     out. Today only `process-compose process restart` recovers it.

  The wedge requires only #1 + #2: even without #3, two
  freshly-wiped standbys cannot basebackup from each other (neither is
  a primary), and the dead ex-primary cannot serve them either.

- **Impact:** A single SIGKILL (or postmaster crash / OOM / SIGSEGV)
  of the primary under load can take the entire active-active cluster
  offline indefinitely, with one of the standbys having destroyed its
  own PGDATA and another mid-flight on the same destruction. This is
  a **data-availability FAIL**. The data-safety verdict is technically
  PASS (`data_loss_total=0`, since the standbys never got ACKs they
  could lose) but the user-visible behavior is "primary died, cluster
  is gone." It bypasses the entire active-active value proposition.

- **Fix sketch (NOT YET IMPLEMENTED — design discussion needed).**
  Two complementary changes:

  - **Source-primary liveness gate.** Before dispatching
    auto-rebootstrap in *either* regime (slow-degradation or fast-fail),
    require `o.LeaderNodeID`'s peer-state row to report
    `State == Running && PostgresUp` (same predicate as the doctor
    `cluster.has-primary` fix). Refuse with a new
    `ReasonAwaitingPrimaryReady` (or reuse `ReasonAwaitingClusterLease`)
    otherwise. This makes #2 impossible: a healthy standby cannot
    wipe itself based on a dead source.
  - **Demote-on-resign-for-PG-death.** Extend the CR-009 fix-1A path
    in `reconciler.go:842-844` so the `EventPostgresCrashed`-while-
    Primary transition produces `RolePrimary→RoleStandby` alongside
    the lease resignation. The next reconciler tick then sees a
    failed *standby* and the fast-fail gate at line 872 admits it.
    Requires a state-machine transition addition
    (`EventPostgresCrashed` from `Running/Primary` → `Failed/Standby`).

  Both changes are upstream in `../pg-manager`. Neither alone is
  sufficient: source-primary liveness gate fixes the wedge but leaves
  CR-016a (the stranded ex-primary) intact; demote-on-resign fixes the
  stranded ex-primary but leaves a race window where standbys still
  pick the not-yet-failed ex-primary as a basebackup source.

- **Workload invariant outcome:** `data_loss_total=0` (PASS),
  `writes_ok=532 / writes_failed=12054` over the experiment window
  (cluster was unavailable for 10 minutes — FAIL on the implicit
  availability invariant; the workload doesn't have a separate
  metric for this but the throughput collapse is unambiguous).

- **Test exists:** None. The current tests cover individual gates
  (fast-fail eligibility, slow-degradation persistence-window,
  state-transition guards) but no test exercises the
  "primary SIGKILL'd; standbys race auto-rebootstrap vs. promotion"
  scenario. A reproduction needs: real KV substrate (race timing
  matters), a sigkill on a running postmaster (not a graceful stop),
  and the slow-degradation timer set short enough to fire (real-rig
  `PERSISTENCE_WINDOW=10s` does it). This is testcontainer territory.

- **Auto-test feasibility:** Feasible with the existing
  `chaos-workload` + `pgman-pc` rig harness; a CI driver would need
  to (a) bring the rig up, (b) wait for steady state, (c)
  SIGKILL primary postmaster under load, (d) wait 30 s,
  (e) assert at least one peer is `StateRunning/RolePrimary` and
  `data_loss_total==0`. Currently a manual operation.

- **Reproduction artifacts** (this run): full per-node docker logs +
  workload log captured under `/tmp/cr016/` while the wedge was live;
  the rig was left in the wedged state at the end of the session for
  forensic inspection.

#### CR-016b — Fix B1 follow-up: source-primary liveness gate landed

After CR-016a, Fix B1 from `docs/cr-016-rca.md` was implemented in
`../pg-manager`:

- New refusal reason `ReasonSourcePrimaryUnreachable`
  (`interfaces.go:583`).
- New helper `sourcePrimaryReachable(ctx, o)` in
  `reconciler/rebootstrap.go` — bounded `SELECT 1` probe against
  the leader's peer DSN, returns false on any error / missing DSN /
  self-as-leader.
- Slow-degradation gate (`maybeAutoRebootstrap`) — refuses dispatch
  with the new reason before taking the rebootstrap lease.
- Fast-fail-from-failed gate (`maybeAutoRebootstrapFromFailed`) —
  same gate as Gate (c'), refuses before lease acquisition.
- 5 regression tests in `reconciler/rebootstrap_b1_test.go` lock the
  contract: slow-degradation refuse, fast-fail refuse, plus three
  helper-level sanity tests.

Re-tested CR-016 against `pgman-proxy:dev` rebuilt from the patched
pg-manager. Three repro runs:

| Run | Failover outcome | writes_ok / writes_failed / data_loss | Recovery |
| --- | ---              | ---                                   | ---      |
| 1   | node-a→node-b in ~6.5s | 3013 / 132 / 0                       | auto, ~7s |
| 2   | node-b→node-a in ~6.7s | 1337 / 119 / 0                       | auto, ~7s |
| 3   | **No new primary elected; B1 refused 2 dispatches with `source_primary_unreachable` (T+10.3s node-b, T+13.2s node-c)** | 202 / 4493 / 0                       | manual: required `process-compose restart node-a` |

**Run 3 is the load-bearing one.** In the original CR-016a wedge, this
same race produced 2 standbys with wiped PGDATA and an unrecoverable
3-of-3 StateFailed. With B1 in place, both standbys' refused dispatches
are visible on the bus (`reason: source_primary_unreachable`), no
`wipe.started`/`basebackup.started` events fired, and both standbys
stayed `StateRunning/RoleStandby/postgres_up`. The cluster ended in a
*reversible* outage — node-a stranded `StateFailed/RolePrimary` (Bug C
unchanged), no new primary elected (Bug A unchanged), but **data
integrity preserved**. A single `process-compose restart node-a`
restored 3-of-3 healthy in ~25s.

Differences vs. CR-016a:

| Symptom                                    | CR-016a (no fix)              | CR-016b (B1)                    |
| ---                                        | ---                           | ---                             |
| `data_loss_total`                          | 0                             | 0                               |
| Standby PGDATA preserved?                  | NO (both wiped)               | YES                             |
| Cluster recoverable without operator?      | NO (PGDATA-destroying restart needed) | YES (any node restart) |
| Time to manual recovery                    | Multi-step (volume wipe + restart) | One `process-compose restart` |
| Failover-success rate (lucky path)         | ~50%                          | ~67% (2 of 3 in this session)   |

**Verdict on Fix B1: PARTIAL but VALUABLE.** B1 closes the
destructive-wipe door (Bug B) — the most severe of the three CR-016
sub-bugs. Bug A (no spontaneous leader re-election after primary
resign) and Bug C (stranded ex-primary) remain open and will need
follow-up fixes:

- **Fix A1 (TODO)** — accelerate leadership re-election after a
  primary resigns. Currently the standbys' cached `o.LeaderNodeID`
  lags the KV-delete fan-out, and there's no explicit "leader vacant"
  signal that triggers an immediate CAS race. Without A1, every
  unlucky-timing primary failure produces an outage even though
  data is safe.
- **Fix C1 (TODO)** — demote the failed ex-primary's `Role` to
  Standby on the `EventPostgresCrashed` transition so the milestone-011
  fast-fail-from-failed gate (which requires `Role ∈ {Standby,
  Unknown}` per `reconciler.go:872`) admits it. With C1 + A1, an
  ex-primary auto-rebootstraps into the cluster as a standby once a
  new primary is elected.

- **Test exists:** Three new regression tests in
  `reconciler/rebootstrap_b1_test.go`:
  1. `TestB1_SlowDegradation_RefusesWhenSourceUnreachable` — locks
     the slow-degradation gate behavior.
  2. `TestB1_FastFail_RefusesWhenSourceUnreachable` — locks the
     fast-fail gate behavior.
  3. Three helper sanity tests for `sourcePrimaryReachable`
     (happy path, empty leader, self as leader).

  All 5 pass; existing 200+ reconciler tests still pass.

- **Reproduction artifacts** (Fix B1 retest): per-run captures at
  `/tmp/cr016b1/{,run2/,run3/}*.log`. Run 3's `node-b.full.log` shows
  the two `auto_rebootstrap.refused` events with
  `reason: source_primary_unreachable` — the most concrete proof B1
  is doing its job.

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
_(CR-016 promoted out of backlog — see Experiments section above.)_
