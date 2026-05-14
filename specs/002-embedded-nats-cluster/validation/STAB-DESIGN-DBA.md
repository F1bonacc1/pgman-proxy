# STAB-DESIGN-DBA — Postgres-correctness design for STAB-01 / STAB-02 / STAB-03

**Audience**: SW Engineer producing the code-side design in parallel; Architect
signing off the GO/NO-GO bar.
**Authoring**: DBA voice. Postgres invariants only. No Go code. The SW
Engineer picks the implementation from the option space defined here.
**Inputs**: `TEST-RESULTS-STABILITY.md` §4 SOAK-01 root cause;
`COVERAGE-REQUIREMENTS.md` §4 GO/NO-GO bar (`REQ-DL-01` MUST-PASS);
`FIXES.md` (prior round's knob inventory);
`.specify/memory/constitution.md` (III Active/Active Coordination Correctness;
IV Thin Scaffold over pg-manager);
`../pg-manager/manager/promote.go` (existing `lsnKey`/`PublishReplayLSN`/
`RequestPromote` machinery);
`../pg-manager/replication/promotion.go` (existing `PromotionEligible`).

---

## §0 Operating context the design lives inside

Every elected primary in this cluster is configured (per `process-compose.yaml`
and pg-manager's `Policy.OnSyncStandbyLoss = BlockWritesOnSyncLoss`) to run:

```
synchronous_standby_names = 'ANY 1 ("node-a","node-b","node-c")'
synchronous_commit        = on
```

What this gives us, in plain Postgres terms:

- Every `COMMIT` that returned success to a client had its WAL record
  durable in the primary's pg_wal AND flushed on **at least one** of the
  surviving peers' walreceiver disk AND replayed past that record on
  that peer (because `synchronous_commit=on`, not `remote_write`). The
  primary's `pg_stat_replication.flush_lsn` was at or past the commit
  LSN for at least one entry in the `ANY 1 (…)` set when the ack went
  out.
- A peer that is NOT currently in the streaming set ("slot lost" or
  "down") cannot be the synchronous standby for any commit acked
  during that window. The acked WAL therefore lives ONLY on (a) the
  primary's disk and (b) the one peer that WAS the sync standby.

`REQ-DL-01` is the contract that an ack'd commit MUST survive any
single-peer loss. Concretely, that means: **for every commit `C` acked
at LSN `L_C` on timeline `T_C`, the new primary after any single peer
death MUST have `pg_last_wal_replay_lsn() >= L_C` on the SAME timeline
`T_C` (or a child timeline that forked at LSN ≥ `L_C`).**

SOAK-01 violated this. Reconstruction:

1. node-b was primary on timeline 1; chaos pushed forward to LSN
   `3/907A7D28`. node-c was the sync standby and was at flush_lsn
   `3/907A7D28`. node-a had `wal_status='lost'` on its slot — its
   `restart_lsn` was pinned at `~0/10110A28` from the prior
   REPL-03 fallout, so node-a's `pg_last_wal_replay_lsn()` was at most
   `~0/10110A28`.
2. node-b dies. Surviving set = {node-a stale, node-c current}.
3. Election picked **node-a**. node-a ran `pg_promote()` from LSN
   `0/10110A28`. Postgres forked **timeline 2 at `0/10110A28`**, three
   gigabytes of WAL behind where the acked-commit set actually was.
4. Every ack'd commit between `0/10110A28` and `3/907A7D28` was on
   timeline 1, which is now an abandoned fork. The 28,185 rows the
   chaos-workload counted as missing were ack'd commits whose only
   surviving copy was on the now-discarded fork.
5. node-b cannot rejoin: its `pg_controldata` shows "Latest checkpoint
   location: 3/907A7D28, TimeLineID: 1". The new cluster timeline 2
   forked at `0/10110A28` — i.e. behind node-b's checkpoint. Postgres
   correctly refuses to start: `requested timeline 2 is not a child of
   this server's history`. node-b's PG never reaches `pg_is_in_recovery()`,
   so AutoDemote's `Observation.PostgresUp = false` gate is never
   cleared, the divergence-detection path never runs, and AutoDemote
   never triggers the wipe-and-rebasebackup that would recover node-b.

That trace is the entire substrate for the three findings.

---

## STAB-01 — Stale standby was elected primary

### §A Postgres-correctness statement

**Invariant SL (Stale-LSN bar on promotion):** No peer MAY be promoted
to primary unless its `pg_last_wal_replay_lsn()` (or `restart_lsn` of
its inbound walreceiver slot, whichever is operationally accessible) is
within a bounded WAL-byte distance — the **stale-LSN budget** — of the
maximum `pg_last_wal_replay_lsn()` observed across the surviving peer
set at the moment election fires.

Operationally this decomposes into four sub-invariants:

- **SL-1 (Currency publication).** Every peer that holds a streaming
  walreceiver MUST publish its current `pg_last_wal_replay_lsn()` to
  the coordination substrate at a cadence small enough that the staleness
  of the published value is bounded by a single `Policy.LivenessInterval`
  worst case. (Existing wire: pg-manager's
  `pgmgr/<cluster>/lsn/<node>` JetStream KV key, value encoded as
  decimal `uint64`, written by `Manager.PublishReplayLSN`.)
- **SL-2 (Timeline tag on currency).** Each LSN publication MUST be
  accompanied by the peer's current `TimelineID` (already published
  separately as `pgmgr/<cluster>/primary_timeline` for leaders;
  standbys publish their replay-timeline via the same key namespace
  or as a structured value alongside the LSN). LSN comparisons across
  divergent timelines are meaningless; the gate must reject
  cross-timeline comparisons rather than treat them as numeric
  inequality.
- **SL-3 (Pre-promotion gate).** Immediately before
  `lifecycle.Promote(ctx)` (which calls `pg_promote()`), the
  candidate MUST collect the published `(lsn, timeline)` snapshot for
  every reachable peer including self, and MUST refuse to promote if
  any reachable peer is more than the stale-LSN budget ahead on the
  same timeline.
- **SL-4 (Self-disqualification).** A peer with an active
  `AutoRebootstrap` pending state (its own slot is `wal_status='lost'`
  on at least one upstream sample, OR it is in the
  `auto_rebootstrap.detected` consecutive-tick accumulation window)
  MUST refuse promotion regardless of the published LSN map, because
  its on-disk pg_control state is by construction stale relative to the
  WAL frontier the cluster has accepted commits against.

The combined effect: the election may still SELECT a stale peer (via
the existing NATS lease CAS), but the stale peer SELF-REFUSES the
promotion at the lifecycle boundary. The lease key is then released
(or its rev incremented) and another candidate races.

### §B Mechanism options

The wire format choice is the dominant variable. pg-manager already
publishes peer LSNs at `pgmgr/<cluster>/lsn/<node>` and consumes them in
`Manager.RequestPromote → PromotionEligible(self, lsns, tolerance)`
with `Policy.PromoteLSNTolerance` (default 16 MiB = one WAL segment).
The machinery exists; it just isn't wired into the failover path. So
this is mostly an engineering question of where to attach the gate,
not a research question of how to compare LSNs.

Reference points from the wider ecosystem (Postgres clusters that
have shipped this problem):

| Tool      | Currency wire                                                          | Election rule                                                  | Staleness budget                       | Fallback when no candidate is current |
|---        |---                                                                     |---                                                             |---                                     |---                                    |
| Patroni   | DCS keys `/members/<n>` carry `xlog_location` + `state` + `timeline`. Liveness TTL on each member key. | Candidate compares own `xlog_location` against every other member's published value; `master_start_timeout` bounds. | `maximum_lag_on_failover` (bytes; default 1 MiB). | `failover` REST call required; otherwise the leader key stays absent until a candidate qualifies. |
| Stolon    | Sentinels gather `keeper` state in the store: latest `xlogpos`, timeline. | Sentinel picks the keeper with highest xlogpos. Won't promote if best candidate is behind master's last known state by more than the cluster `maxStandbyLag`. | `maxStandbyLag` (bytes; default 1 MiB). | Cluster stays without a master; alert. |
| CNPG (CloudNativePG) | k8s leader CR has `currentPrimary` + `targetPrimary`; instance manager polls `pg_last_wal_receive_lsn()` of each replica. | Operator picks the replica with highest receive LSN; rejects candidates whose received LSN is older than the most-recent successful primary checkpoint. | `replicationSlots.synchronizeReplicas.enabled` + an LSN comparison; effective budget = WAL segment size. | Operator blocks promotion; emits `Cluster:FailoverImpossible` event. |
| Crunchy PGO | Patroni under the hood. | Patroni rules apply.                                           | Same as Patroni.                       | Same as Patroni.                      |

Three concrete design options for STAB-01:

- **OPT-1 (Pre-promotion gate using existing machinery).** Wire the
  existing `Manager.RequestPromote` LSN-quorum gate into the reconciler's
  `StatePromoting` dispatch path, with a `Policy.PromoteLSNTolerance`
  appropriate for the rig (e.g. 16 MiB or one
  `pg_wal_segment_size`). Add periodic `PublishReplayLSN` from the
  reconciler tick (currently missing — pg-manager comment at
  `manager/promote.go:33-36` says "Production wiring (periodic publish
  from the reconciler) is pending v0.5.0"). The candidate calls
  `RequestPromote` instead of (or before) `Promote`; on
  `ErrPromotionRejected` it releases the lease and the next candidate
  races. The substrate already enforces SL-1, SL-2 (timeline already
  published separately at `pgmgr/<cluster>/primary_timeline`), and
  SL-3.
- **OPT-2 (Lease pre-condition with LSN annotation).** Embed the
  candidate's `(lsn, timeline)` directly in the lease-acquisition
  value at the moment of the CAS. Survivors comparing leases reject
  the lease if its embedded LSN is more than the budget behind the
  max LSN they have observed. This is closer to Stolon's sentinel
  shape, but it introduces a new wire format (lease value goes from
  bare NodeID to a struct) and contradicts the constitution's "thin
  scaffold" principle by reimplementing LSN-quorum logic that
  pg-manager already owns.
- **OPT-3 (Sync-standby preference rule).** Read
  `pg_stat_replication` on the previous primary's snapshot (if any) to
  identify which peer was the synchronous standby for the last acked
  commits, and prefer that peer in election. This is closest to what
  Patroni's `synchronous_mode_strict` does but requires the previous
  primary to still be reachable (false in the SOAK-01 scenario; the
  primary is dead). Useful as an OPTIMIZATION on top of OPT-1, not a
  replacement.

### §C Recommended approach

**OPT-1, with the SL-4 self-disqualification added.**

Rationale:

- Reuses pg-manager's existing, tested machinery (`lsnKey`,
  `PublishReplayLSN`, `PromotionEligible`, `PromoteLSNTolerance`).
  No new wire format. No new contract surface. Constitution IV (thin
  scaffold over pg-manager) is honored.
- The existing tolerance value (16 MiB, one WAL segment) is the
  correct Postgres-native quantum: WAL is segmented and the
  walreceiver acks at segment boundaries; a "less than one segment
  behind" candidate is, by the postgres replication contract, current
  enough that the sync-standby quorum cannot have included a peer
  ahead of it. (If sync-standby quorum has been respected, the new
  primary's WAL frontier is at most one segment ahead of the
  most-current standby.)
- The SL-4 self-disqualification is the cheapest correctness gain.
  A peer that knows it has a `wal_status='lost'` slot from the
  primary's side, OR is accumulating `auto_rebootstrap.detected`
  ticks, has by construction a stale `pg_last_wal_replay_lsn()`. The
  observed cluster (the SOAK-01 reproduction) shows the published-LSN
  view of this peer as `~0/10110A28` while the rest of the cluster is
  at `3/907A7D28`; the OPT-1 LSN gate handles it correctly, but the
  SL-4 self-check is faster (no substrate round-trip) and provides
  defense-in-depth.

Specifics (the SW Engineer must encode these — DBA voice):

1. **What each peer publishes**:
   `pgmgr/<cluster>/lsn/<node>` value MUST be the result of
   `SELECT CASE WHEN pg_is_in_recovery() THEN pg_last_wal_replay_lsn()
                                         ELSE pg_current_wal_flush_lsn() END`
   serialized to `uint64`. Primaries publish their flush LSN (the LSN
   they have themselves durably flushed); standbys publish their
   replay LSN (the LSN they have replayed past). The existing
   `Manager.PublishReplayLSN` is mis-named in the primary case but
   the value is correct (sender_pgcurrent_wal_flush_lsn = LSN past
   which the primary itself has flushed = effectively "what the
   cluster has accepted").
2. **Publish cadence**: every reconciler tick (one
   `Policy.LivenessInterval` = 2 s by default). Worst-case publication
   staleness is one tick. The chaos rig's 12 s failover window means
   the published LSN snapshot is at most 2 s stale at election time —
   well below the 16 MiB tolerance budget at realistic write rates.
3. **What election compares**: at the moment `runPromote` is about to
   call `lifecycle.Promote(ctx)`, the reconciler MUST first invoke
   the `PromotionEligible` gate with `(self, snapshotted_LSN_map,
   PromoteLSNTolerance)`. If `Eligible == false`, abort the promote
   path, release the leadership lease, and let the next campaign tick
   race. Emit a structured event
   `promotion_refused{reason="lsn_stale", self_lsn=…,
   leader_lsn=…, behind_bytes=…, tolerance_bytes=…}`.
4. **Staleness budget**: `Policy.PromoteLSNTolerance = 16 MiB`
   (existing default). Lock this as the production posture.
5. **SL-4 self-disqualification**: the candidate also rejects itself
   if `r.staleWALConsecutiveTicks > 0` (already tracked by the
   AutoRebootstrap accumulator at
   `pg-manager/reconciler/rebootstrap.go`). This is independent of
   the LSN snapshot; even if the LSN gate would have passed (e.g.
   between primary's last segment-boundary flush and our last
   replay), accumulating stale-WAL ticks is a sufficient signal that
   we are about to be rebootstrapped, and rebootstrap-pending peers
   cannot be primary by definition.
6. **Fallback when NO candidate is current** (the
   asymmetric-staleness corner case):

   **Block election.** Do NOT force-promote the most-current of a
   stale set. The cluster sits leaderless, `writes_failed` grows on
   the chaos-workload, an alert fires on
   `pgman_proxy_no_eligible_primary` (new gauge). This honors REQ-DL-01
   strictly: it is better to be unavailable than to discard ack'd
   commits. Patroni and Stolon both default to this. Manual operator
   override path: the existing `Manager.Promote` (unchecked) is
   already wired to the `pgmgr/<cluster>/manual_promote/<node>` key;
   that remains the break-glass.

7. **Split-vote and asymmetric staleness call-outs** (the failure
   modes leader-election-by-LSN classically hits):

   - **Split vote on identical-LSN ties**: `PromotionEligible`
     already breaks ties by lexicographically smallest NodeID. This
     is stable. Two peers cannot both campaign successfully against
     the same JetStream lease key (the CAS is linearized within
     the JS replica set); the lexicographic rule is a
     presentation-only tiebreaker for the eligibility check.
   - **Asymmetric staleness during partition reconvergence**: a peer
     returning from a partition has a stale published LSN until its
     first post-reconnect publish tick. The gate sees its OLD LSN.
     This is conservative — the gate may reject a peer that has
     since caught up. The mitigation is the publish cadence (2 s);
     within one tick of reconnection the published LSN is fresh
     again.
   - **All peers stale simultaneously** (the SOAK-01 carry-over
     case): the gate refuses every candidate. The cluster is
     leaderless until either (a) the dead primary's PGDATA is
     reattached and a peer catches up to its LSN, OR (b) the
     operator runs an unchecked `Promote`. This is the correct
     posture per REQ-DL-01. The operator-override path is the only
     way to consciously trade durability for availability.
   - **Lease oscillation under partition + recovery**: if the
     elected lease holder self-refuses on the LSN gate, releases the
     lease, then the next campaign winner ALSO self-refuses, the
     lease can flap. The mitigation is that every self-refusal logs
     `promotion_refused` and the operator gets a clear signal that
     the cluster needs intervention. The flap itself is bounded by
     the lease TTL × stale_threshold (currently 5 s × 3 = 15 s per
     attempt; under FIX-04's proposed 1 s × 2 = 2 s per attempt, the
     flap is rapid but harmless because no promotion actually fires).

### §D Acceptance probes

Extend SOAK-01 (`validation/scripts/SOAK-01.sh`) with these explicit
assertions, runnable on the existing rig:

**Probe SOAK-01-STAB01-a (stale standby self-disqualifies)**.

Precondition: induce the SOAK-01 carry-over state — one standby has
`wal_status='lost'` with stale `restart_lsn`. The harness can do this
in three steps:

```sh
# 1. Identify primary P and a target standby S (not synchronous).
# 2. Stop S (process-compose process stop pgman-pc-node-$S).
# 3. WAL burner on P, 9 iters of pg_switch_wal until the slot for S
#    reports wal_status='lost'. (REPL-03 harness is the reference.)
# 4. Re-start S; do NOT wait the 5 min auto_rebootstrap window.
# 5. Kill the primary P.
```

Expected log on the stale peer S (within 5 s of P's death):

```
slog: promotion_refused
  reason=lsn_stale
  self_lsn=<low>
  leader_lsn=<high>
  behind_bytes=<>16MiB>
  tolerance_bytes=16777216
```

SQL probe on the new primary (whichever non-stale peer won):

```sql
-- run from inside the new-primary container
SELECT pg_last_wal_replay_lsn() AS replay_lsn,
       (SELECT system_identifier FROM pg_control_system()) AS sysid;
-- compare replay_lsn against the chaos_events row at MAX(seq) recorded
-- before the kill. The row's xact_commit_lsn (via xmin → pg_xact_commit_timestamp
-- or via test-rig probe at insert time) must be <= replay_lsn.
SELECT MAX(seq) FROM chaos_events;
```

PASS criteria:
- `data_loss_total(t_end) == data_loss_total(t_start)`.
- New primary is NEVER node S in any iteration. (Stable across N=50
  runs of the harness with random `S ∈ {non-sync standbys}`.)
- Every `promotion_refused{reason="lsn_stale"}` event on a peer is
  followed within 15 s by a successful `leadership.changed` event
  naming a different peer.

**Probe SOAK-01-STAB01-b (no-candidate blocking path)**.

Precondition: starve TWO peers of WAL (REPL-03 the slot twice) so
both are stale, leaving only the primary current. Kill the primary.

Expected: NO promotion. `pgman_proxy_no_eligible_primary{cluster=…}`
gauge = 1 on every survivor. `writes_failed` grows on the
chaos-workload. Cluster stays leaderless until either a stale peer
catches up (impossible without primary) OR an operator runs
`Manager.Promote` (break-glass).

PASS criteria:
- `data_loss_total(t_end) == data_loss_total(t_start)` (no acked
  commit was lost because no fork was created).
- `extra_rows == 0` (no stale-promote write storm).
- Operator-override path remains functional: an explicit
  `pgmgr/<cluster>/manual_promote/<node>` write on a stale node DOES
  promote it (`Manager.Promote` is unchecked) — REQ-CON-02 / break-
  glass.

**Probe SOAK-01-STAB01-c (steady-state LSN publication freshness)**.

Read `pgmgr/<cluster>/lsn/<node>` from the JetStream KV at 1-second
intervals during a 60 s chaos-workload window:

```sh
docker exec pgman-pc-node-a nats kv get pgmgr-<cluster-id> "pgmgr/<cluster>/lsn/node-a"
```

PASS criteria: the published value advances within at most
`2 × Policy.LivenessInterval` seconds (4 s default; 2 s under FIX-04
tuning) of `pg_current_wal_flush_lsn()` observed via SQL on the same
peer.

### §E Risks / open questions

- **R-1 (LSN comparison across timelines).** If two surviving peers
  are on different replay timelines (one was rebootstrapped during
  the partition), comparing their numeric LSNs is incoherent.
  Mitigation: SL-2 requires each publication to carry timeline.
  Promotion gate refuses any candidate whose published timeline ≠
  the most-recent primary timeline observed via
  `pgmgr/<cluster>/primary_timeline`.
- **R-2 (Publication-staleness during write storms).** If
  `Policy.LivenessInterval = 2 s` and the primary is committing 100
  MB/s, a 2 s publication gap means the standby's published LSN can
  lag its actual replay by ~200 MB — wider than the 16 MiB tolerance.
  In that regime even a current standby would be rejected. Mitigation:
  publish on a separate, faster cadence (e.g. 500 ms) decoupled from
  the reconciler tick — OR rely on SL-4 (any current standby cannot
  have a stale-WAL accumulator firing, so it self-passes even when
  the LSN snapshot is racy).
- **R-3 (Quorum loss masquerading as stale-LSN).** During a partition
  the candidate sees only its own LSN; it has no peers to compare
  against. `PromotionEligible` would mark it `Eligible=true` (max LSN
  == self LSN, behind=0). This is the right answer for partition
  scenarios where the candidate IS the only surviving peer holding
  the lease — but it loses the durability guarantee if the candidate
  is on a minority partition. Mitigation: cross-check with the
  failover-quorum snapshot (`Policy.QuorumSnapshotStaleAfter` already
  governs this gate at the reconciler).
- **R-4 (Lease oscillation).** If every candidate self-refuses on the
  LSN gate, the lease churns. The mitigation in §C item 7 (operator
  alert on `pgman_proxy_no_eligible_primary` plus the break-glass
  unchecked-Promote path) is the operationally-acceptable answer.
  Open: should the gate also write a `lsn_gate_block`
  history-tombstone key with a TTL so survivors observing the
  tombstone know "we tried, the LSN gate blocked us" rather than
  re-racing immediately?
- **R-5 (Pre-promotion gate runs on the candidate, not the cluster).**
  The gate is enforced inside `runPromote` on the elected node.
  A buggy candidate that does not call the gate would still
  `pg_promote()` itself. Defense-in-depth idea: the cluster could
  also enforce post-hoc — every promoted leader's first WAL record on
  the new timeline carries an LSN that survivors can compare against
  their own pre-fork LSN; survivors observing a "new primary forked
  behind us" condition can refuse to follow (fence themselves). This
  is a longer-term hardening; not in scope for STAB-01.

---

## STAB-02 — Divergent ex-primary stays wedged when PG refuses to start

### §A Postgres-correctness statement

**Invariant DV (Divergence detection without a running postmaster):**
A peer whose on-disk `pg_controldata` reports a state incompatible
with the cluster's current timeline MUST be recognized as
"divergent_ex_primary" and routed to AutoDemote's destructive
recovery path WITHOUT requiring the local postmaster to come up.

Operationally:

- **DV-1.** At startup, before `pg_ctl start`, the host MUST read
  `pg_controldata $PGDATA` and capture:
  - `Latest checkpoint location` (= local checkpoint LSN, `L_local`)
  - `Latest checkpoint's TimeLineID` (= local timeline, `T_local`)
  - `Database cluster state` (the operational shutdown class —
    `shut down`, `in production`, `shut down in recovery`, etc.)
  - `Database system identifier` (= local sysid)
- **DV-2.** The host MUST read the cluster's published primary
  timeline via the substrate
  (`pgmgr/<cluster>/primary_timeline` key value) and, when present,
  the cluster's most-recent fork-point LSN derived from the new
  timeline's `.history` file (every TLI ≥ 2 has a `<TLI>.history`
  file under `pg_wal` on any peer that has been part of the new
  timeline; the host can also ask any reachable peer's basebackup
  endpoint for the file).
- **DV-3.** The peer is "divergent_ex_primary" iff:
  `T_local != T_cluster` AND `T_local` is NOT an ancestor of
  `T_cluster` (i.e. `T_local` is on a discarded fork), OR
  `T_local == T_cluster` AND `L_local > L_fork_point(T_cluster)` AND
  `L_local` is unreachable from `T_cluster`'s WAL history (i.e. the
  local checkpoint sits in the abandoned future of the previous
  timeline). The SOAK-01 case is precisely this latter shape:
  `T_local = 1, L_local = 3/907A7D28`, `T_cluster = 2,
  L_fork(2) = 0/10110A28`; `L_local > L_fork(2)` and `T_local` (1)
  is the parent of `T_cluster` (2), so the checkpoint sits in the
  discarded portion of timeline 1.
- **DV-4.** A peer satisfying DV-3 MUST NOT start PG. The
  AutoRebootstrap / AutoDemote destructive path MUST be the only path
  out; `pg_ctl start` is forbidden because Postgres will correctly
  refuse with `FATAL: requested timeline X is not a child of this
  server's history` (the exact error observed in SOAK-01 §4 root-cause).
- **DV-5.** The local sysid (`Database system identifier` in
  `pg_controldata`) MUST match the cluster's published sysid (already
  enforced via `pg-manager/replication/cluster_id.go`'s sysid-fence).
  An identity mismatch is a TERMINAL park, not a divergence — handled
  by the existing `ReasonClusterIdentityMismatch` path.

### §B Mechanism options

- **OPT-1 (pre-start `pg_controldata` divergence probe + AutoDemote
  trigger).** Insert a startup gate immediately after
  `ensureStandbySignalIfInitialized` (at
  `internal/runtime/start.go:232`) that runs `pg_controldata $PGDATA`
  via exec, parses the four fields above, fetches
  `pgmgr/<cluster>/primary_timeline` (and the matching `.history`
  file content from a reachable peer), and computes the DV-3
  predicate. If divergent: instead of proceeding to `manager.Start`,
  the host puts the AutoDemote condition into the state store under
  a key that the reconciler observes on its first tick and acts on
  WITHOUT requiring `PostgresUp=true`. (The AutoDemote gate's
  `Observation.PostgresUp` precondition is removed for the
  "divergent_ex_primary at startup" arm; the divergence is already
  fully determined from on-disk state.)
- **OPT-2 (run `pg_rewind` instead of wipe+basebackup).** When the
  ex-primary's checkpoint is on the parent of the new timeline at
  a fork point ≤ local checkpoint (the SOAK-01 case), `pg_rewind`
  is the targeted, lighter-weight repair — it rewinds local WAL to
  the fork point and replays from there. Postgres' own contract.
  Rejected for v1: the constitution (REQ-CON-02) forbids any
  `pg_rewind`/`pg_basebackup` invocation in the proxy repo, and
  pg-manager's reconciler already has the rewind path
  (`reconciler/act.go::runRewind`) for the in-running-PG case. We
  cannot use it here because rewind ALSO requires a running
  postmaster ("connection to server" target) on this side — except
  in `--source-pgdata` mode, which is operationally rare and
  out-of-scope. AutoDemote (wipe + basebackup against the new
  primary) is the simpler, byte-equality-providing path (REQ-HEAL-02
  already PROVEN per TEST-RESULTS §2).
- **OPT-3 (wait for AutoDemote to time out and rebootstrap).** Do
  nothing on the proxy side; let the existing AutoDemote pipeline
  eventually trigger. Rejected: in SOAK-01, AutoDemote NEVER
  triggered for node-b because its precondition gate (`PostgresUp`)
  was never satisfied. The wait is unbounded. Operator intervention
  was required.

### §C Recommended approach

**OPT-1 (pre-start `pg_controldata` divergence probe).**

Specifics (DBA voice — what the comparison must be, not how it's
plumbed):

1. **Where**: the proxy's existing startup-time hook
   `internal/runtime/start.go::ensureStandbySignalIfInitialized` is
   the natural insertion point. The new gate runs RIGHT AFTER it,
   before `manager.New` / `manager.Start`. (SW Engineer: note this
   call site; the rest of this section is DBA-side semantics.)
2. **The probe**: `pg_controldata $PGDATA` is a no-server
   invocation; it reads the on-disk `global/pg_control` file
   directly. It works whether or not PG is running. The four fields
   listed in DV-1 are extracted by line-prefix match
   (`/^Latest checkpoint location:/`,
   `/^Latest checkpoint's TimeLineID:/`,
   `/^Database cluster state:/`,
   `/^Database system identifier:/`). LSN is the
   `HEX/HEX` form; convert to uint64 via the standard PG
   `lsn_to_uint64` arithmetic (`(hi << 32) | lo`).
3. **The cluster-side comparison**: read three substrate keys:
   - `pgmgr/<cluster>/primary_timeline` (already published; value is
     a JSON record with `TimelineID`, written by
     `publishPrimaryTimeline` at `reconciler/act.go:395`).
   - `pgmgr/<cluster>/lsn/<leader>` (current cluster-frontier LSN).
   - One peer's `<TLI>.history` file content. (The host can
     short-circuit the `.history` retrieval by trusting the
     published_timeline + `pgmgr/<cluster>/timeline_fork/<TLI>`
     [NEW key] = `{parent_tli, fork_lsn}`. The new primary publishes
     this once at promote time; cost is one extra KV write per
     successful promote.)
4. **The decision**:
   - If no `primary_timeline` is published (fresh cluster, no prior
     leader): skip the gate. Normal startup.
   - If `T_local == T_cluster`: not divergent. Normal startup.
   - If `T_local < T_cluster` AND `L_local <= L_fork(T_cluster)`:
     not divergent. Normal startup (this peer is just behind on the
     parent timeline; standby replay will catch up via the WAL
     stream).
   - If `T_local < T_cluster` AND `L_local > L_fork(T_cluster)`:
     **divergent_ex_primary**. Trigger the recovery path; do NOT
     start PG.
   - If `T_local > T_cluster`: also divergent (we forked off after
     the cluster's current leader; should never happen given lease
     CAS but the gate handles it).
5. **The recovery trigger**: write a key
   `pgmgr/<cluster>/divergent_ex_primary/<node>` with the
   pg_controldata snapshot as the value (sysid, local_tli,
   local_lsn, cluster_tli, fork_lsn, detected_at). The reconciler
   on its first tick observes this key and routes through the
   existing `auto_demote` path with the `divergent_ex_primary`
   condition — bypassing the `Observation.PostgresUp` precondition
   (the divergence is already proven by on-disk state).
6. **Surfacing in logs/metrics**:
   - Structured event:
     `divergent_ex_primary.detected{local_tli=…,
     local_lsn=…, cluster_tli=…, fork_lsn=…, sysid_match=true|false}`
     on the peer at startup.
   - Gauge: `pgman_proxy_divergent_ex_primary{node=…}` = 1.
   - On successful wipe + basebackup: clear the key + gauge; emit
     `divergent_ex_primary.resolved`.
7. **Cooldown interaction (cross-link to STAB-03)**: the AutoDemote
   path that this divergence triggers honors the existing
   `Policy.AutoDemote.Cooldown`. If the cooldown is active, the
   peer parks with `DivergenceParkedEvent` and waits — but the
   STAB-03 cooldown-override path applies here too (see §STAB-03 §C
   item "what condition resets the cooldown").

### §D Acceptance probes

**Probe STAB02-a (divergent ex-primary at startup auto-recovers).**

Setup (reproduces the SOAK-01 §4 carry-over):

```sh
# 1. Fresh rig, identify primary P.
# 2. Stop a non-primary peer S; WAL burner on P; S's slot goes wal_status='lost'.
# 3. Kill P (the running primary).
# 4. Wait for election; new primary P' takes over (must NOT be S — STAB-01 guarantees this).
# 5. Start S. (S is on the OLD timeline; its pg_controldata reflects the abandoned fork.)
```

Expected on S:

- `divergent_ex_primary.detected` event within 2 s of S's process
  start.
- PG on S is NOT started (no `postmaster.pid` while gate is active).
- AutoDemote runs against P': wipe completes, basebackup completes
  (current `auto_rebootstrap.basebackup.*` event family).
- S rejoins as standby; `pg_is_in_recovery() = t` and `pg_stat_replication`
  on P' shows S streaming within 60 s of S's process start.

PASS criteria:

- Full S recovery (process-start to streaming-standby) within 90 s
  (no human intervention).
- `data_loss_total` on the chaos-workload still equals baseline
  (since the durable commits are already on P' from the STAB-01
  guarantee).
- `pgman_proxy_divergent_ex_primary{node=S}` gauge goes 1 → 0
  within the recovery window.

**Probe STAB02-b (no false divergence on healthy restart).**

Setup: docker restart any one peer cleanly (process-compose stop /
start). The peer's pg_control is at the cluster's current
`(T_cluster, near-current LSN)`.

Expected: gate evaluates "not divergent". PG starts normally.
`divergent_ex_primary.detected` is NEVER emitted.

PASS: zero false positives across 50 restart iterations.

**Probe STAB02-c (sysid-mismatch is a separate park, not auto-demoted).**

Setup: copy a foreign PGDATA (different system identifier) into a
peer's data dir.

Expected: gate evaluates "sysid mismatch" → existing
`ReasonClusterIdentityMismatch` terminal park. AutoDemote is NOT
triggered (sysid mismatch is a fence, not a divergence —
constitution-mandated operator gate).

PASS: peer parks; structured event
`auto_rebootstrap.refused{reason="cluster_identity_mismatch"}`
emitted; PGDATA is NOT wiped.

### §E Risks / open questions

- **R-1 (`.history` file retrieval).** The cleanest fork-point
  source is the `<TLI>.history` file in `pg_wal` on any peer
  that has been on `T_cluster`. The proxy MUST NOT shell out to
  fetch this (REQ-CON-02 forbids pg-basebackup-class operations in
  this repo); it must come via the substrate. Recommended: new
  KV key `pgmgr/<cluster>/timeline_fork/<TLI>` written by the new
  primary at promote time (one write per timeline change). Open:
  who in pg-manager writes this key? Likely `runPromote` after
  successful `pg_promote` — needs a pg-manager patch (REQ-CON-02
  thin-scaffold honored: timeline-history bookkeeping lives in
  pg-manager).
- **R-2 (`pg_controldata` binary path).** The proxy already knows
  `cfg.Postgres.BinDir`; `$BinDir/pg_controldata` is the canonical
  invocation. Open: validate the binary version matches the on-disk
  PG version (refuse to parse cross-version output).
- **R-3 (Empty PGDATA — first boot).** The gate must be a no-op
  when `PG_VERSION` file is absent. (Same precondition the existing
  `ensureStandbySignalIfInitialized` already handles.)
- **R-4 (Concurrent gates).** If the divergence key is set and
  AutoDemote is running but the cooldown is active, the peer parks
  until the cooldown clears. During parking PG is down. This is
  correct posture (no false-start) but it does mean the peer is
  unavailable. Cross-link to STAB-03: the cooldown override on a
  detected `divergent_ex_primary` condition is a candidate
  recovery-aware override.
- **R-5 (False detection during clean promotion handoff).** During
  a planned switchover, the OLD primary's pg_control briefly
  reports the old timeline before its post-demote restart writes
  the new one. The gate could mis-fire. Mitigation: require the
  divergence condition to be observed for ≥ 1 reconciler tick
  before triggering AutoDemote (the SAME persistence-window idiom
  the existing AutoRebootstrap uses, just with a short window).

---

## STAB-03 — Cooldown blocks recovery when the cluster needs it

### §A Postgres-correctness statement

**Invariant CD (Cooldown is anti-loop, not anti-recovery):** The
AutoRebootstrap / AutoDemote `Cooldown` gate exists to prevent
destructive-recovery LOOPS, not to delay a needed recovery from a
NEW failure. Cooldown MUST be evaluated against a per-condition
identity, not a wall-clock-only "time since last rebootstrap."

Operationally:

- **CD-1.** A cooldown period gates "I just rebootstrapped against
  condition X; if X recurs IMMEDIATELY, refuse — that's a loop."
- **CD-2.** A cooldown MUST NOT gate "I just rebootstrapped against
  condition X; condition Y now applies — refuse." The condition
  identity must travel through the history record.
- **CD-3.** The conditions that count as distinct (Postgres-side):
  - `stale_wal` (slot's `wal_status='lost'`, accumulated past
    PersistenceWindow). Identity: `(condition=stale_wal,
    leader=<L>, stale_since_segment=<S>)`.
  - `divergent_ex_primary` (pg_controldata timeline ≠ cluster
    timeline AND local checkpoint LSN beyond fork). Identity:
    `(condition=divergent, local_tli=<T>, cluster_tli=<T'>)`.
  - `sysid_mismatch` — a fence, not a cooldown-gated recovery.
- **CD-4.** A "fresh" condition since the last
  rebootstrap-completion event resets the cooldown for that
  condition class. Concretely: if the last rebootstrap fixed
  `stale_wal` against leader L, and a NEW `divergent_ex_primary`
  condition appears, the cooldown must NOT block; the two are
  independent recovery actions.

### §B Mechanism options

- **OPT-1 (Condition-keyed cooldown history).** Replace the single
  scalar "last_rebootstrap_completed_at" with a small history
  record keyed by condition class. `CooldownElapsed(now, policy.Cooldown)`
  becomes `CooldownElapsedForCondition(now, condition_class,
  policy.Cooldown)`. A new condition class fires immediately
  regardless of how recent the last rebootstrap was on a different
  condition.
- **OPT-2 (Recovery-aware override).** Keep the scalar history but
  add an override: if the current condition is
  `divergent_ex_primary` AND there is no other path to recovery
  (PG cannot start), bypass cooldown unconditionally. This is
  narrower than OPT-1 (covers only the SOAK-01 shape) but cheaper.
- **OPT-3 (Reduce cooldown to near-zero).** Tune the cooldown
  knob from the rig (FIX-04 already plumbs the Policy knobs);
  set `Policy.AutoRebootstrap.Cooldown = 5 s` for chaos rig.
  Rejected for production posture: long cooldowns exist to bound
  the rebootstrap rate against an upstream-broken cluster (e.g.
  WAL keeps getting recycled because primary is misconfigured).
  We can't ship a near-zero default to production.
- **OPT-4 (Patroni's `master_start_timeout` analogue).** Patroni
  has a wall-clock budget; if no candidate has started a successful
  master within the budget, the cluster lets the next candidate
  try without further gating. Equivalent here: track wall-clock
  "time since cluster had a live primary"; if it exceeds a
  cluster-emergency budget (default 5 × `Policy.LivenessInterval`),
  cooldowns are bypassed cluster-wide. Stolon's `failInterval`
  is a near-identical idiom. Rejected: introduces a new
  cluster-wide gauge to track and synchronize.

### §C Recommended approach

**OPT-1 (Condition-keyed cooldown history) + OPT-2 as defense-in-
depth for the `divergent_ex_primary` arm.**

Specifics:

1. **What changes in pg-manager**: `History.CooldownElapsed(now,
   cooldown)` becomes `History.CooldownElapsed(now,
   condition_class, cooldown)`. The history record becomes
   `map[ConditionClass]time.Time` (or a small slice).
   `ConditionClass` is a string-typed enum:
   `"stale_wal" | "divergent_ex_primary"`. Independent histories.
2. **What changes in the proxy**: nothing structural. The
   `Policy.AutoRebootstrap.Cooldown` and
   `Policy.AutoDemote.Cooldown` knobs keep their semantics
   (per-condition).
3. **The condition that resets the cooldown**: a SUCCESSFUL
   completion of the destructive path for that condition class.
   The history-write at
   `pg-manager/reconciler/auto_demote.go::writeDemoteHistory`
   (and the parallel rebootstrap history write) stamps
   `(condition_class, now)`. Any other condition class is
   untouched. Recovery from `divergent_ex_primary` does NOT
   block a later `stale_wal` rebootstrap (and vice versa).
4. **The failsafe so it doesn't degenerate into a rebootstrap loop**:
   - The PER-CONDITION cooldown is still enforced. A flapping
     `stale_wal` condition is still rate-limited to one
     rebootstrap per cooldown interval (the protection OPT-1
     preserves).
   - A new `pgman_proxy_rebootstrap_attempts_total{condition=…}`
     counter exposes per-condition attempt frequency. An
     operational alert at "> 5 attempts per condition per 1
     hour" surfaces a runaway loop. The existing
     `auto_rebootstrap.refused{reason=…}` events already record
     each refusal; this is a counter on top of that.
   - The cluster-wide `RebootstrapLease` (already implemented;
     at most one rebootstrap in flight cluster-wide) bounds the
     concurrent blast radius regardless of per-node cooldown
     state. That gate stays unchanged.
5. **Defense-in-depth for divergent_ex_primary**: even if the
   condition-keyed cooldown is mis-configured, a
   `divergent_ex_primary` condition where PG cannot start (DV-3
   detected, postmaster refuses) bypasses the cooldown gate. The
   rationale: the peer is contributing zero capacity to the
   cluster anyway; refusing the rebootstrap doesn't preserve any
   useful state, it just delays recovery. (This bypass logs
   `auto_demote.cooldown_overridden{reason="pg_refuses_to_start"}`
   so operators see when it fires.)

### §D Acceptance probes

**Probe STAB03-a (sustained chaos, repeated rebootstrap allowed).**

Setup: 10-minute soak with kills every 60 s. At least one peer is
forced into `stale_wal` once and `divergent_ex_primary` once
during the soak.

Expected: the chronologically-second rebootstrap for the SAME
condition is refused with `reason=cooldown_active` IF it falls
within `Policy.AutoRebootstrap.Cooldown`. A rebootstrap for a
DIFFERENT condition is NOT refused.

PASS criteria:
- The cluster never sits at `streaming < 2` for more than 90 s
  continuously.
- Every refused rebootstrap event names the matching condition
  class.
- No peer is rebootstrapped more than 3 times in a 10-min window
  on the SAME condition (loop-protection holds).

**Probe STAB03-b (divergent-ex-primary cooldown bypass).**

Setup: trigger two consecutive `divergent_ex_primary` events on
the same peer within `Policy.AutoDemote.Cooldown`. (E.g. force
two sequential primary kills + stale-promote loops, with the
target peer being divergent in both rounds.)

Expected with bypass active: BOTH events trigger AutoDemote
within 60 s of detection. Each AutoDemote logs
`auto_demote.cooldown_overridden{reason="pg_refuses_to_start"}`
when bypass fires.

PASS: peer recovers from both divergences within the soak
budget.

**Probe STAB03-c (cooldown DOES still block loops on the same
condition).**

Setup: trigger 5 sequential `stale_wal` rebootstraps on the
same peer within the cooldown window (e.g. by deliberately
recycling its slot before it catches up).

Expected: 1 rebootstrap fires; 4 are refused with
`reason=cooldown_active{condition=stale_wal}`. The loop
protection is intact.

PASS: rebootstrap_attempts_total{condition=stale_wal} increments
by exactly 1; the rest are refusal events.

### §E Risks / open questions

- **R-1 (Conditions are not perfectly orthogonal).** A `stale_wal`
  rebootstrap CAN fix a `divergent_ex_primary` condition (since
  wipe+basebackup brings PG into alignment regardless of which
  condition was named). Resetting only the `stale_wal` history
  could under-count the resolution. Mitigation: on a successful
  destructive recovery, write history entries for ALL conditions
  that were observable on the peer at the time, not just the one
  that triggered.
- **R-2 (Operator-set cooldown is intentional).** If an operator
  has explicitly set `Cooldown = 1 hour`, the per-condition split
  changes the effective rate from "1/hour total" to
  "1/hour per condition". Documented behavior change; needs to
  appear in `contracts/policy.md` and the release notes.
- **R-3 (History key bloat).** Per-condition history adds keys to
  the substrate. With 2 conditions × 3 peers = 6 history entries
  per cluster. Bounded, no bloat concern.
- **R-4 (Cluster-wide rebootstrap budget).** The existing
  `RebootstrapLease` already serializes destructive work
  cluster-wide. The condition-split design preserves that — only
  one wipe at a time across the cluster, regardless of how many
  conditions are concurrently outstanding.
- **R-5 (P0 vs P1 classification of STAB-03).** Once STAB-01
  lands (LSN gate blocking stale-promote) AND STAB-02 lands
  (divergent_ex_primary recovers without operator), STAB-03's
  cooldown frustration is mostly cosmetic: a refused rebootstrap
  with cooldown_active no longer leaves the cluster in a
  ack'd-loss state. STAB-03 elevates from P1 to P0 only if the
  combination of "STAB-01 self-disqualification + STAB-03
  cooldown blocking" leaves the cluster unable to ever recover
  a stale peer within an SLA. In the reference rig that does NOT
  occur (the stale peer is rebootstrapped when its cooldown
  elapses, ≤ 1 hour in production posture). DBA recommendation:
  classify STAB-03 as P1 conditioned on STAB-01 + STAB-02
  landing.

---

## §Summary table

| STAB-id  | Postgres invariant                                                                                                                                       | Recommended mechanism                                                                                                                                                                              | Acceptance probe |
|---       |---                                                                                                                                                       |---                                                                                                                                                                                                 |---               |
| STAB-01  | No peer may be promoted whose `pg_last_wal_replay_lsn()` is more than one WAL segment (16 MiB) behind the max published peer LSN on the same timeline.   | Wire pg-manager's existing `Manager.PublishReplayLSN` / `Manager.RequestPromote` / `PromoteLSNTolerance` into the failover path. Pre-promotion gate refuses stale candidate; SL-4 self-disqualifies peers with `auto_rebootstrap.detected` accumulating ticks. Block election if NO candidate is current — operator break-glass via existing unchecked `Manager.Promote`. | SOAK-01 extension: induce `wal_status='lost'` on one peer (REPL-03 harness), kill primary, assert new primary is NEVER the stale peer; `data_loss_total` settled-equal to baseline; `promotion_refused{reason="lsn_stale"}` emitted on the stale peer. |
| STAB-02  | A peer whose on-disk `pg_controldata` reports `(local_tli, local_lsn)` incompatible with the cluster's current `(cluster_tli, fork_lsn)` MUST NOT start PG; it MUST instead trigger AutoDemote (wipe + basebackup against the elected leader) — without requiring the local postmaster to come up. | Pre-start `pg_controldata` divergence probe right after `ensureStandbySignalIfInitialized` (already the proxy's startup hook). Compare local `(tli, lsn, sysid)` against substrate-published `(cluster_tli, fork_lsn, sysid)`. If divergent, write `pgmgr/<cluster>/divergent_ex_primary/<node>` and short-circuit through AutoDemote with the `PostgresUp` precondition lifted for this arm. | Reproduce SOAK-01 §4 root cause (carry-over): force a peer onto the abandoned fork via stale-promote, restart the peer, assert auto-recovery within 90 s — no operator action — and `pgman_proxy_divergent_ex_primary{node=…}` gauge goes 1 → 0. |
| STAB-03  | Cooldown is anti-loop, not anti-recovery: a successful rebootstrap for condition X MUST NOT block a rebootstrap for a NEW condition Y. The condition identity travels with the cooldown history. | Replace pg-manager's scalar `CooldownElapsed(now, cooldown)` with per-condition-class history keyed by `{stale_wal, divergent_ex_primary}`. Plus a defense-in-depth bypass: `divergent_ex_primary` with PG-refuses-to-start unconditionally bypasses cooldown. Loop protection preserved via the unchanged cluster-wide `RebootstrapLease` and per-condition `Cooldown`. | Sustained-chaos SOAK with two distinct conditions (stale_wal then divergent_ex_primary) on the same peer within one cooldown window — both must recover. Same-condition repeat within cooldown stays refused (loop guard intact). |

---

## Document control

- All STAB ids are stable identifiers.
- DBA scope: this document defines INVARIANTS and ACCEPTANCE
  PROBES. The SW Engineer owns the code-side design (`STAB-DESIGN-SW.md`)
  and the implementation. Any conflict between the two designs is a
  defect in this document unless flagged in the §E open-question
  list above.
- Code-line citations to pg-manager are pinned to the sibling repo
  at `/home/eugene/projects/go/pg-manager` (HEAD at the time of
  authoring). Re-grep against `lsnKey`, `PublishReplayLSN`,
  `PromotionEligible`, `EffectiveAutoRebootstrapPolicy` if line
  numbers drift.
