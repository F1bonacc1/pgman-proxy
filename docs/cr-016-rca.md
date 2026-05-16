# CR-016 — RCA: cluster wedges 3-of-3 StateFailed after primary SIGKILL under load

**STATUS: Bug B FIXED (B1, 2026-05-16). Bug A + Bug C still open.
Critical wedge converted to a reversible outage; data integrity
preserved across primary SIGKILL.**

This document is the deep technical RCA for CR-016, the chaos
experiment summarized in `docs/chaos-experiments.md`. Read that
section first for the timeline and verdict; this file is the
gate-by-gate code-level analysis.

## Update — Fix B1 landed 2026-05-16

`ReasonSourcePrimaryUnreachable` + `sourcePrimaryReachable` helper +
gate calls in both `maybeAutoRebootstrap` (slow-degradation) and
`maybeAutoRebootstrapFromFailed` (fast-fail), in
`../pg-manager/reconciler/rebootstrap.go`. Verified by 5 new tests
in `reconciler/rebootstrap_b1_test.go` and 3 live retests on the
process-compose rig (see `docs/chaos-experiments.md` CR-016b).

**Re-test outcome:** Bug B (destructive PGDATA wipe via stale
source-primary) no longer reproduces. Standbys refuse dispatch with
`source_primary_unreachable` instead of wiping. Bug A (no
spontaneous leader re-election after primary resign) and Bug C
(stranded ex-primary at `StateFailed/RolePrimary`) still reproduce
on the unlucky-timing path; the cluster outage persists until
operator restarts the failed peer. **Data integrity preserved in
all 3 retest runs** (`data_loss_total=0`).

Remaining work (proposed fixes section below):

- Fix A1 — accelerate leadership re-election after primary resign.
- Fix C1 — demote-on-resign so the ex-primary's fast-fail path
  admits it once a new primary exists.

## Scenario recap

Steady-state rig (node-a primary, node-b/c standby, doctor 8 PASS).
`chaos-workload` driving 20 writes/sec via the multi-host libpq DSN
`host=127.0.0.1,127.0.0.1,127.0.0.1 port=16432,16433,16434`.

At T0 (2026-05-16T20:58:34.869Z) we sent SIGKILL to the primary's
postmaster (`docker exec pgman-pc-node-a kill -9 35`). T+10 minutes
later, the cluster was still 3-of-3 `StateFailed/postgres_down`.
`writes_ok=532 / writes_failed=12054 / data_loss_total=0`.

## Observed sequence

```
T+1.2s   node-a   resigned leadership                                # CR-009 fix 1A
T+1.2s   node-a   state_transition Running→Failed, role=Primary
T+1.4s   node-b   auto_rebootstrap.detected ticks=1 source=local_lag_persisted
T+1.5s   node-c   auto_rebootstrap.detected ticks=1 source=local_lag_persisted
...      [no new leader is elected during the 12 s that follow]
T+13.4s  node-b   state_transition Running→Bootstrapping
                  auto_rebootstrap.decided source_primary_id="node-a"   ← dead
T+13.5s  node-b   wipe.completed: PGDATA wiped
T+13.5s  node-b   basebackup against node-a (dead) → "connection refused"
T+13.5..+33s    node-b   11 retries, all refused, ends StateFailed/Standby
T+33.5s  node-c   auto_rebootstrap.decided source_primary_id="node-a"
T+33.5s  node-c   wipe.completed: PGDATA wiped
T+33.5s..      node-c   12 retries against dead node-a, ends StateFailed/Standby
T+...    all three nodes StateFailed indefinitely
```

## Why this happens — three interacting bugs

### Bug A — No new leader elected within `AUTO_REBOOTSTRAP_PERSISTENCE_WINDOW`

`reconciler/reconciler.go:842-844` (CR-009 fix 1A) calls
`resignOnPostgresCrash` when the local primary's PG dies; this issues
a KV-delete on the leadership key. The expectation is that one of
the standbys will see the empty key on its next observation tick and
claim it via CAS.

**What goes wrong**: the rig is configured with
`PGMAN_PROXY_POLICY_AUTO_REBOOTSTRAP_PERSISTENCE_WINDOW=10s` and
`AUTO_DEMOTE_LEADERSHIP_STABILITY_WINDOW=5s`. The lag-persisted
detection ticker on each standby runs every ~2 s and accumulates
unconditionally — it does not reset when the leadership key changes
or when the snapshot's primary becomes unreachable. Between T+1.2 s
(node-a resigns) and T+13.4 s (node-b dispatches rebootstrap), no
standby was elected, and `o.LeaderNodeID` still resolved to
`"node-a"` on both standbys at decision time.

I have not yet traced *why* the lease isn't claimed in that window —
candidate causes:

- The JetStream KV-delete is queued behind other ops and propagates
  to other nodes more slowly than expected (the
  `replicated_kv_substrate` adapter timing).
- The standbys' `Observe` loop coalesces multiple ticks per KV-watch
  notification and the "leader is empty" view arrives later than the
  10 s persistence window.
- Re-election is conditional on something (replication-lag bound,
  promotion-readiness) that the standbys momentarily fail.

A 5 ms KV-write fan-out is the published target for the cluster;
12 s of "no observed leader change" is suspicious enough to warrant
its own milestone-013 investigation.

### Bug B — Slow-degradation dispatch with stale source-primary

`reconciler/rebootstrap.go` (slow-degradation path) reads
`o.LeaderNodeID` at decision time and emits
`auto_rebootstrap.decided source_primary_id=<that>` without a
liveness check. The dispatched `runAutoRebootstrap` then:

1. Calls `lifecycle.WipePreflight` and wipes `/var/lib/postgresql/data`.
2. Connects to `<source>:5432` for `pg_basebackup`.
3. Retries on transient errors.

**The wipe happens before the first basebackup TCP connect.** Once
PGDATA is gone, the standby cannot return to its prior state on
basebackup failure — it sits in `StateFailed/Standby` waiting for a
healthy basebackup source that no longer exists.

The fast-fail-from-failed path
(`rebootstrap.go:1005-1008 maybeAutoRebootstrapFromFailed`) DOES
include a self-as-leader sanity check ("`LeaderNodeID == ""`
returns `ReasonAwaitingClusterLease`"), but it does not verify the
chosen source's `State == Running && PostgresUp`. The slow-
degradation path lacks even the self-as-leader check.

A peer's reported `State`/`PostgresUp` is observable from the cluster
snapshot (`Status.Instances[<id>]`) — this isn't a missing-data
problem, it's a missing-check problem.

### Bug C — Failed ex-primary stays operator-sticky

`reconciler/reconciler.go:872`:

```go
case pgmanager.StateFailed:
    if curRole == pgmanager.RoleStandby || curRole == pgmanager.RoleUnknown {
        if evt := r.maybeAutoRebootstrapFromFailed(ctx, o, curRole); evt != nil {
            return evt
        }
    }
    return nil
```

A SIGKILL'd primary lands `StateFailed/RolePrimary` and stays there:
the role label persists across `EventPostgresCrashed`, and the
fast-fail-from-failed admission is gated on `Role ∈ {Standby,
Unknown}`. Peers observe the failed ex-primary and emit
`auto_rebootstrap.detected condition=1` events *on its behalf*, but
its own reconciler never enters the dispatch path. Only
`process-compose process restart` recovers the node.

This is the bug originally logged as CR-016 during CR-009c follow-up
and confirmed by this experiment.

## Severity

Critical. A single SIGKILL of the primary under load is sufficient
to wedge the cluster permanently — no operator intervention except
PGDATA-destroying `process-compose restart`s recovers it, and during
the wedge, write availability is 0%. Bug A makes Bug B trigger; Bug
B converts the standbys into stranded peers; Bug C ensures the
ex-primary never rejoins. Removing any one of the three would break
the wedge:

- Without Bug A: a new primary is elected within ~5 s, and Bug B
  would harmlessly basebackup from the new primary.
- Without Bug B: the lag-persisted standbys would refuse to wipe
  (no live source), stay `StateRunning/Standby/postgres_up`, and a
  new primary election would still proceed.
- Without Bug C: the ex-primary would itself rebootstrap as a
  standby once the persistence window elapses, contributing to the
  recovery quorum.

The Bug B fix is the lowest-cost individual change and removes the
wedge by itself. Bug A is a deeper milestone-013 trace. Bug C is
the original CR-016 hypothesis and has a narrow code-level fix in
the state machine.

## Proposed fixes

### Fix B1 — Source-primary liveness gate (LANDED 2026-05-16)

In `reconciler/rebootstrap.go` `maybeAutoRebootstrap` (slow-degradation
regime), AND in `maybeAutoRebootstrapFromFailed` (fast-fail regime),
before dispatching: look up `o.LeaderNodeID` in
`Status.Instances` and require
`inst.State == StateRunning && inst.PostgresUp`. Refuse with
`ReasonAwaitingPrimaryReady` (new) or reuse
`ReasonAwaitingClusterLease` otherwise.

Cost: tiny — ~10 LOC + an observability-event reason constant.
Risk: a flapping observation could refuse a valid dispatch — but
"flap during recovery" is preferred over "destructive wipe based on
stale snapshot."

Test surface: a slow-degradation reconciler test that feeds a
snapshot with `Instances["node-a"].State == StateFailed,
PostgresUp == false` and asserts the dispatch is refused with the
new reason. Already mockable in the existing reconciler test rig
(`reconciler/rebootstrap_test.go`).

### Fix C1 — Demote-on-resign-for-PG-death

Extend the CR-009 fix-1A site (`reconciler.go:842-844`) so the
`EventPostgresCrashed` transition for a former primary is paired
with role demotion. Requires either:

- A new event `EventPostgresCrashedFromPrimary` that the state
  machine reduces as `Running/Primary → Failed/Standby`, or
- Re-using `EventPostgresCrashed` and adding the role transition in
  the reducer for the `Running/Primary` case.

After demotion, the fast-fail gate at `reconciler.go:872` admits the
node, and `maybeAutoRebootstrapFromFailed` runs (subject to Fix B1's
liveness gate on the *new* primary).

Cost: medium — state-machine reducer change in `pgmanager/state`,
plus reducer tests. Need to audit other transitions that read
`curRole` from `StateFailed` (operator-fence handling, FR-010).

### Fix A1 — Faster leadership re-election after resign

This is the deepest fix and the one most likely to break adjacent
invariants. Candidates:

- Reduce `AUTO_REBOOTSTRAP_PERSISTENCE_WINDOW` (10 s today). Too
  small a window encourages premature destructive recovery.
- Add an "explicit leadership-vacancy" signal that fires when a peer
  emits `resigned leadership` and short-circuits the standbys'
  observation tick to immediately race for the key.
- Audit the `replicated_kv_substrate` delete-fanout timing.

Recommended approach for this milestone: **land Fix B1 and Fix C1.**
Defer Fix A1 to a separate milestone-013 investigation; the wedge
goes away without it once B1 is in place.

## Reproduction

Manual; the rig must be at steady state with a workload running.

```bash
# Confirm baseline.
PGMCTL_DEV_TOKEN=process-compose-dev-token ./bin/pgmctl doctor   # expect 8 PASS

# Start workload (10s warm-up before T0).
./bin/chaos-workload -dsn "host=127.0.0.1,127.0.0.1,127.0.0.1 port=16432,16433,16434 user=postgres dbname=postgres sslmode=disable connect_timeout=1" -write-rps 20 &

# Identify primary postmaster PID and SIGKILL it.
PRIMARY=$(./bin/pgmctl get peers -o json | jq -r '.peers[] | select(.role=="primary") | .node_id')
PMPID=$(docker exec pgman-pc-$PRIMARY cat /var/lib/postgresql/data/postmaster.pid | head -1)
docker exec pgman-pc-$PRIMARY kill -9 $PMPID

# Wait 30 s and check.
sleep 30
./bin/pgmctl get peers -o wide
# Expected (the bug): all three peers StateFailed/down.
# Hoped-for (after fixes): one primary/running, two standby/running.
```

The race is timing-sensitive; the failure mode reproduces under
sustained workload load roughly half the time on this rig. CR-009c
was a successful-failover run; this CR-016 run was a wedge run.
Both are observable on the same code path; the difference is which
side of the 10 s persistence-window race the substrate happens to
land on.

## Evidence files

Raw per-node logs from the reproduction run are at `/tmp/cr016/`:

- `node-a.full.log` — primary; T+1.2 s resignation + state transition
- `node-b.full.log` — first standby to dispatch; full 11-attempt
  basebackup-against-dead-node-a sequence
- `node-c.full.log` — second standby to dispatch; full 12-attempt
  basebackup-against-dead-node-a sequence
- `workload.log` — chaos-workload writer journal; final tally
- `T0.log` — exact T0 timestamp of the SIGKILL

Note: these are ephemeral (`/tmp` is cleared on reboot). Re-running
the experiment regenerates them.

## What's done / still pending

Done as of 2026-05-16:

- **Fix B1 implemented** — `ReasonSourcePrimaryUnreachable`,
  `sourcePrimaryReachable` helper, gate calls in both regimes.
- **B1 unit tests** — `reconciler/rebootstrap_b1_test.go` (5 tests,
  all pass).
- **B1 retest** — 3 live runs on the rig confirmed the destructive
  wipe no longer reproduces; `data_loss_total=0` in all runs;
  Run 3 showed the gate firing under the unlucky-timing path.

Still pending:

- **Fix A1** — accelerate leadership re-election after a primary
  resigns. Without it, the cluster sits in a no-primary outage on
  the unlucky-timing path (CR-016b Run 3 observed this — both
  standbys healthy with intact data, but no promotion). Recovery
  required operator restart of the stranded ex-primary.
- **Fix C1** — demote-on-resign-for-PG-death so the ex-primary's
  fast-fail-from-failed gate (`reconciler.go:872`) admits it. Pairs
  with A1: once a new primary is elected, the ex-primary's fast-fail
  path picks the new primary as source and auto-rebootstraps.
- **CR-016b reproduction artifacts** — `/tmp/cr016b1/{,run2,run3}/`
  per-run capture; rig was returned to healthy steady state after
  retest (node-c primary, node-a/node-b standbys).
