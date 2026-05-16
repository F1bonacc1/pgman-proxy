# CR-009 Root-Cause Analysis & Proposed Fixes

Companion to `docs/chaos-experiments.md`. Detailed RCA for the two
FAIL-class bugs surfaced by experiment **CR-009 — SIGKILL postmaster
on primary**.

All file paths in this document are relative to the `pg-manager` repo
(`../pg-manager` from pgman-proxy). Line numbers are from the commit
present at the time of the chaos run (2026-05-16).

---

## Bug #1 — Zombie primary: failover does not fire when postmaster dies

### Observed symptom

After `kill -9` of the postmaster on the primary node:
- Postmaster process gone (`ps -ef` shows only `pgman-proxy` PID 1).
- pgman-proxy process alive; embedded NATS routes meshed; leader-key in
  JetStream KV continues to be renewed.
- All three nodes' `/v1/status` continued to report
  `LeaderNodeID=<dead-primary> PrimaryNodeID=<dead-primary>` for 97+
  seconds. Workload was blocked across all three proxy ports.
- The dead primary's own `/v1/status` reported `PostgresUp=true` for
  itself during this window — i.e., the node believed its own PG was
  healthy.

### Where the code goes wrong

There are **three** distinct issues that compose into the zombie:

**1a. Leadership renewal has no health gate.**
`adapters/nats/leadership.go:270-286`:

```go
func (l *Leadership) tickOnce(ctx context.Context) {
    cur := l.cur.Load()
    if cur != nil && cur.leader == l.self && cur.rev > 0 {
        _, err := l.bucket.Update(ctx, l.leaderKey, []byte(l.self), cur.rev)
        if err == nil {
            // … renewal succeeded …
            return
        }
    }
    // …
}
```

The renewal decision considers only `cur.leader == l.self && cur.rev > 0`
— pure substrate-CAS state. The adapter has no opinion on whether the
process it represents (a local postmaster) is actually alive. As long
as the JetStream `Update` succeeds (i.e., NATS quorum is healthy), the
key is re-stamped. **There is no surface through which the reconciler
can vetoa renewal.**

**1b. The state machine's primary-side crash path requires `o.IsLeader = false`.**
`reconciler/reconciler.go:713-716`:

```go
case pgmanager.RolePrimary:
    if !o.IsLeader && !o.DiskFull {
        return state.EventLostLeader{}
    }
```

For a `RolePrimary` node, the only role-specific exit is
`EventLostLeader`, which is gated on `IsLeader` flipping. But `IsLeader`
is set by the adapter in 1a, which never reports `false` for a
self-renewing leader. So the role-specific arm of the switch is a
no-op until the substrate itself trips.

**1c. The shared `!o.PostgresUp` path SHOULD have caught this, but
didn't fire in CR-009.**

After the role switch, `reconciler/reconciler.go:736-757`:

```go
if !o.PostgresUp && !r.ensuringPostmaster.Load() && !r.bootstrapping.Load() {
    if r.cfg.Executor != nil {
        if running, err := r.cfg.Executor.IsRunning(ctx); err == nil && running {
            return nil
        }
    }
    // …
    return state.EventPostgresCrashed{}
}
```

This block applies to both roles and would have correctly transitioned
the primary to `StateFailed` via `EventPostgresCrashed` (a transition
that *is* valid from `RolePrimary` per
`state/transitions.go:173-180`). The state-machine table is:

```
StateRunning/RolePrimary → EventPostgresCrashed → StateFailed/RolePrimary
```

But during CR-009 the primary's reconciler **never observed
`PostgresUp=false` for itself**, despite `IsRunning` doing the right
thing (signal-0 liveness + `/proc/<pid>/comm` verification, per
`internal/pgproto/pgexec.go:366-394`).

The most plausible mechanism — the reconciler's observe loop itself
hung on a PG query (pgx blocking on a dead unix socket). Several
observation paths in `reconciler/observe.go` call into the SQL
executor:

```go
// observe.go:329, 443, 603, 760
if o.executor == nil || !obs.PostgresUp { return }
```

These short-circuit *if* `PostgresUp` is already false, but **only the
`IsRunning` call at line 265 sets `PostgresUp`**, and that itself runs
before the SQL probes. If the IsRunning probe is being called but its
result is not propagated (race with another observation timing out),
or if the goroutine that fires the ticks is starved waiting on a
broader SQL probe, the loop wedges.

This third issue is harder to nail down without instrumentation, but
it is the proximate cause. Even if 1c were reliable, the other two
issues (1a and 1b) make the design fragile.

### Proposed fix

Layered fix; each layer closes one gap. Defense-in-depth — any one of
them prevents the zombie, but landing all three closes the design
holes.

#### Fix 1A (must-have) — Leadership.Resign() called from reconciler

Add `Resign(ctx) error` to the `pgmanager.LeadershipProvider`
interface (NATS impl deletes the leader-key with CAS on its own
revision; in-mem impl clears its store). Then call it from the
reconciler when transitioning a primary to `StateFailed`:

```go
// reconciler/reconciler.go — in the runHandleFailed path
// (or wherever EventPostgresCrashed lands for a primary):
if curRole == pgmanager.RolePrimary {
    if l, ok := r.cfg.Leadership.(pgmanager.ResignableLeadership); ok {
        if err := l.Resign(ctx); err != nil {
            r.logger.Warn("leadership resign failed",
                pgmanager.Field{Key: "error", Value: err.Error()})
            // Fall through — substrate will time us out eventually.
        }
    }
}
```

This makes the reconciler the source of truth for "should I be
leading?" and gives it explicit agency to release the lease when its
local PG dies. The adapter stays mechanistic (CAS-Update on a tick).

#### Fix 1B (must-have) — Health-gated renewal as last line of defence

Add an optional `HealthCheck func() bool` callback to the Leadership
adapter. The reconciler installs it after construction:

```go
// pgman-proxy startup, after manager.NewSingleton(...)
nats.SetHealthCheck(func() bool { return reconciler.LocalPostgresUp() })
```

And in `tickOnce`:

```go
if cur != nil && cur.leader == l.self && cur.rev > 0 {
    if l.healthCheck != nil && !l.healthCheck() {
        // Local PG is down; do NOT renew. Drop our belief; on the next
        // tick we observe the key, see ourselves vs. the holder, and
        // either reclaim (PG came back) or accept whoever took over.
        l.cur.Store(nil)
        return
    }
    _, err := l.bucket.Update(...)
    // …
}
```

This guarantees that even if Fix 1A's explicit `Resign` is skipped
(e.g., reconciler loop is itself hung), the renewal tick refuses to
re-stamp the key for a dead postmaster. Within `staleThreshold`
non-renewals, peers run the existing `maybeEvictStale` path and a new
leader is elected.

#### Fix 1C (should-have) — Hard timeouts on observe SQL probes

Audit every `executor.Query` / `executor.IsReady` call in
`reconciler/observe.go` and ensure each runs with a
`context.WithTimeout(ctx, 1*time.Second)` (or a configurable
`Policy.ObserveProbeTimeout` defaulting to 1s). The current code
relies on pgx's connect-timeout, but a query against a hung postmaster
unix socket can stall after the connection is established.

This is the lowest-risk fix and would have likely surfaced the
PostgresUp=false signal in CR-009. It's also useful generally —
operations should never wedge on a slow PG.

---

## Bug #2 — Promotion may elect a peer that lacks acked WAL → data loss

### Observed symptom

After forced recovery (`process-compose restart` of the zombie
primary's container):
- `node-c` was elected as new primary.
- `node-a` ended in `state=failed`; the new
  `failover_quorum.StandbyNames` listed only `[node-b]`, excluding
  node-a.
- `data_loss_total` jumped from 51 → 65,574. ~65,500 acked writes
  vanished from the database. `rows_in_db` dropped from
  ~267,000 → 202,019 while `writes_ok` stayed at 267,538.

### Where the code goes wrong

**2a. The `PromoteLSNTolerance` default is 16 MiB — about one Postgres
WAL segment.** From `reconciler/act.go:469-474`:

```go
// defaultPromoteLSNTolerance is one Postgres WAL segment (16 MiB) — the
// natural segment-aligned quantum the walreceiver acks against. Used
// when Policy.PromoteLSNTolerance is zero. Mirrors manager/promote.go's
// defaultPromoteLSNTolerance constant (kept local to avoid the import
// cycle reconciler ↔ manager).
const defaultPromoteLSNTolerance uint64 = 16 << 20
```

A 16 MiB WAL slack contains **tens of thousands of small INSERT
records**. 65,500 chaos-workload rows fit inside one such window. The
"natural segment quantum" reasoning is sound for async replication —
where lag is the price of throughput — but it is **catastrophic for
synchronous replication**, where every acked write is, by sync_commit
guarantee, on at least one peer in the sync standby list. For a sync
cluster the only safe tolerance is 0.

**2b. `Policy.PromoteLSNTolerance = 0` silently means "use default
(16 MiB)".** Same code, `reconciler/act.go:510-513`:

```go
tolerance := r.activePolicy().PromoteLSNTolerance
if tolerance == 0 {
    tolerance = defaultPromoteLSNTolerance
}
```

This is a UX footgun: an operator who carefully reads
`types.go:276-280` and sets the policy to `0` to mean "exact match
required" gets the opposite — the most-permissive setting. There is
no way to express "zero tolerance" without picking a sentinel like
`1`.

**2c. The LSN gate compares only against the observed max LSN, not
against the previous sync standby set.** From `replication/promotion.go:62-110`:

```go
func PromotionEligible(self, lsnByNode, tolerance) PromotionDecision {
    // …
    var maxLSN uint64
    var maxNode pgmanager.NodeID
    for _, n := range nodes {
        lsn := lsnByNode[n]
        if maxNode == "" || lsn > maxLSN { maxLSN = lsn; maxNode = n }
    }
    // …
    if selfLSN >= maxLSN { dec.Eligible = true; return dec }
    dec.Behind = maxLSN - selfLSN
    if dec.Behind <= tolerance { dec.Eligible = true; return dec }
    // …
}
```

There is no inspection of `failover_quorum.StandbyNames` — the gate
does not know which peers were in the sync replication set when the
old primary last acked a write. The only thing that survives across
the failover is the LSN map, which:

- Includes the **last published LSN of the dead primary** (best-effort
  publish-on-tick keeps writing while alive; after crash the value
  freezes).
- May or may not include each surviving peer (`collectClusterLSNs`
  silently excludes peers that haven't published, per
  `act.go:619-639`).

So in the CR-009 scenario:
- `node-b` (dead primary): published its final LSN before kill.
- `node-a` (sync acker, ahead of node-c): may or may not have been
  in the map at promote-time. If node-a's publish lagged or its
  proxy was momentarily unresponsive, it could be missing.
- `node-c` (lagging standby): in the map, within 16 MiB of the
  observed max, **promotion allowed**.

The fundamental invariant that must hold for safe promotion:

> *The promotee's `pg_last_wal_replay_lsn()` must be at least as new
> as the maximum LSN of every other reachable peer that was in the
> previous primary's `synchronous_standby_names` set at the time of
> the last successful sync ack.*

The current gate enforces "at least within tolerance of any observed
peer", which is strictly weaker.

**2d. `quorum_gate.go` checks set membership, not WAL coverage.** From
`reconciler/quorum_gate.go:23-86`:

```go
// Self-publisher: the candidate is the snapshot's claimed primary.
if snap.Primary == candidate { return true, "" }
for _, n := range snap.StandbyNames {
    if n == candidate { return true, "" }
}
return false, observability.FailoverRefusalNoQuorumCandidate
```

`StandbyNames` is the *eligibility* list — peers permitted to be in
the sync set. Under `ANY 1 (a, c)` only one of them actually has the
acked WAL per commit, but both are in `StandbyNames`. The quorum gate
admits either as a candidate. The decision of "which one actually
has the WAL" is delegated to the LSN gate (2c), which mishandles it.

### Proposed fix

#### Fix 2A (must-have) — Tolerance=0 when replication policy is sync

In `reconciler/act.go:510-513`, branch on policy type:

```go
tolerance := r.activePolicy().PromoteLSNTolerance
if tolerance == 0 {
    switch r.activePolicy().Replication.(type) {
    case pgmanager.QuorumSync, pgmanager.AllSync:
        tolerance = 0
    default:
        tolerance = defaultPromoteLSNTolerance
    }
}
```

For sync clusters, the tolerance defaults to 0: only a peer at the
exact frontier may promote. The 16 MiB slack stays available for
async-only deployments.

#### Fix 2B (must-have) — Distinguish "zero tolerance" from "use default"

Replace the sentinel `0` with an explicit pointer (`*uint64`) or a
typed wrapper:

```go
type LSNTolerance struct {
    SetExplicitly bool
    Bytes         uint64
}
```

…and treat `Policy.PromoteLSNTolerance = {SetExplicitly: true, Bytes:
0}` as "must match exactly". This is a minor API break but a clear
operator UX win. Alternatively, accept the more-conservative
interpretation as the new default (Fix 2A) and document that
operators wanting slack must opt-in with a positive value.

#### Fix 2C (must-have) — Cross-check StandbyNames LSNs at promote time

The promotion gate must consult the last published
`failover_quorum.StandbyNames` from the substrate and refuse promotion
if any of the named standbys is *reachable but ahead*. Pseudocode in
`reconciler/act.go::preflightPromote`:

```go
snap := r.cfg.State.GetFailoverQuorumSnapshot(ctx)
if snap != nil && snap.Method != pgmanager.QuorumMethodAsync {
    selfLSN := lsns[r.cfg.Topology.NodeID]
    for _, n := range snap.StandbyNames {
        if n == r.cfg.Topology.NodeID { continue }
        peerLSN, present := lsns[n]
        if !present {
            // Peer was in previous sync set but is now silent.
            // Conservative: refuse, because we don't know if it
            // holds acked WAL we don't.
            r.logger.Warn("promote: refused (sync-peer not observable)",
                pgmanager.Field{Key: "peer", Value: string(n)})
            return false
        }
        if peerLSN > selfLSN {
            r.logger.Warn("promote: refused (sync-peer ahead)",
                pgmanager.Field{Key: "peer", Value: string(n)},
                pgmanager.Field{Key: "peer_lsn", Value: peerLSN},
                pgmanager.Field{Key: "self_lsn", Value: selfLSN})
            return false
        }
    }
}
// fall through to existing PromotionEligible(...) which still
// guards against the unsynced max-LSN case
```

This restores the invariant: a sync-cluster promotion is only allowed
when the promotee has at-or-more WAL than every peer that *might*
have acked a write.

#### Fix 2D (should-have) — Auto-rebootstrap a behind peer pre-promote

Add a tick-or-two delay: if a candidate is behind a reachable
sync-peer, attempt a `pg_rewind` or basebackup from that peer *first*,
catching up before promoting. This trades unavailability latency for
zero data loss. Today the cluster picks "promote behind peer →
data loss" instead, which is the wrong trade.

---

## Cross-cutting issue — chaos-workload log buffer rolling

`process-compose` retains 1000 lines per process by default. With the
chaos-workload emitting one `verify` log every 5 s plus per-failure
warns, the verify-phase history that pins **when** data loss appeared
is gone after ~5 minutes of failures.

This is what kept us from tracing the 51 rows of pre-CR-009 data loss
to a specific experiment. Two cheap fixes:

1. Bump `process-compose.yaml`'s `log_rotation` / `log_lines` for
   `chaos-workload` (~10 000 lines or 50 MiB).
2. Have `chaos-workload` write a separate `data_loss.jsonl` file via
   a dedicated logger, append-only, that captures only verify-phase
   transitions where `data_loss_total` changes. This is the
   highest-signal stream and should never roll.

This is the cheapest concrete improvement in this RCA — recommended
to land before any further chaos to make future investigations
tractable.

---

## Summary of recommended fix order

| Priority | Fix | Closes |
| --- | --- | --- |
| 1 | **2A** — Tolerance=0 for sync policies | Bug #2 (data loss) |
| 1 | **2C** — StandbyNames cross-check at promote | Bug #2 (data loss) |
| 1 | **1A** — Reconciler resigns leadership on PG crash | Bug #1 (zombie) |
| 2 | **1B** — Health-gated leadership renewal | Bug #1 (defense in depth) |
| 2 | **1C** — Hard timeouts on observe SQL probes | Bug #1 (root cause) |
| 2 | **chaos-workload log retention** | Investigability |
| 3 | **2B** — Explicit zero-tolerance sentinel | UX |
| 3 | **2D** — Pre-promote rewind for behind peers | Availability |

All fixes land in `../pg-manager` except the log-retention change and
(optionally) the wiring of the new `HealthCheck` callback in
pgman-proxy's runtime startup.

The two **priority-1, must-have** changes that would have prevented
the CR-009 outcome:

- **2A + 2C** would have refused node-c's promotion (it was behind
  node-a, a peer in the previous sync set) → **no data loss**.
- **1A** would have caused the zombie primary's reconciler to release
  the leader-key the moment its postmaster died → **failover within
  one tick** instead of a 97s wedge.

Both are mechanically simple. The hard part is regression coverage —
neither bug has a test today.
