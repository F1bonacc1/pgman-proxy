# STAB-DESIGN-SWE — pgman-proxy v1 stability fixes (code-side design)

**Audience**: SW Engineer (implements), Architect (reviews).
**Status**: Design only. No code lands from this document.
**Inputs**: `TEST-RESULTS-STABILITY.md` (SOAK-01 + REPL-03 + REC-04
findings), `COVERAGE-REQUIREMENTS.md §4` (GO/NO-GO bar — REQ-DL-01,
REQ-HEAL-01), `FIXES.md` (prior round — reuses FIX-id idiom).
**Pinned references** (HEAD pgman-proxy=42984f7; pg-manager=d19a660):
re-grep if line numbers drift.

Each entry traces the defect to source lines, proposes the minimal
patch, names blast radius and rollback risk, and identifies the
regression-locking test.

---

## STAB-01 — Stale-standby promotion (P0, REQ-DL-01 violation)

### §A — Symptom + trace

**Symptom** (`TEST-RESULTS-STABILITY.md §4`, SOAK-01 t+302 s → t+314 s):
the primary is killed; a stale standby (node-a — slot `wal_status=lost`,
pending rebootstrap, PGDATA at LSN `~0/10110A28`) wins the lease vs.
the up-to-date standby (node-c). 28,185 ack'd commits forked onto a
discarded timeline-1 segment, fixed-forever as `data_loss_total=28185`.

**Code path**:

1. **Lease acquisition is content-blind.** `pg-manager/adapters/nats/leadership.go:243-278` —
   `tickOnce` races for the leader key via `bucket.Create(...self)`
   whenever the key is absent or a stale-leader eviction succeeds
   (`leadership.go:301-342`). The winning candidate is **the first
   Create that lands**; no input from local PG state, replication
   status, or topology peers is consulted.
2. **The quorum gate is the *only* fence on promotion.** `pg-manager/reconciler/reconciler.go:388`
   calls `applyQuorumGate(ctx, o, curState, curRole)`, which delegates
   to `consultQuorumSnapshot` (`reconciler/quorum_gate.go:55-86`).
   The gate consults a `FailoverQuorumSnapshot` previously *published
   by the dying primary*, comparing only `candidate ∈ StandbyNames`.
3. **`StandbyNames` is built from `LivePeers`, not WAL position.**
   `reconciler/quorum_snapshot.go:253-288` (`buildStandbyNames`) and
   `reconciler/observe.go:561-583` (`observeLivePeers`) define
   "live peer" as appearing in `pg_stat_replication` with state
   `streaming|catchup|startup|backup`. **A peer whose slot is `wal_status=lost`
   but whose walreceiver is still emitting `startup`/`streaming`
   (handshake-attempting) is included as a live peer.** The publisher
   does not filter on `wal_status` or on `restart_lsn` lag.
4. **AutoRebootstrap-pending state does not gate promotion.** Even
   though node-a's reconciler is sitting in `auto_rebootstrap.refused
   reason=cooldown_active`, neither the lease nor the quorum gate
   consults `RebootstrapHistory`, `IdentityFence`, `DivergenceFence`,
   `StaleWALCondition`, nor the per-peer slot state. The gate's
   self-veto fields (operator/identity/divergence fences) are
   evaluated at the **candidate-self** level inside the reducer
   downstream of the gate (`reconciler/rebootstrap.go:137-174`,
   `auto_demote.go:203-273`), but they do not feed back into the
   gate's allow/refuse decision.

So at t+314 s: node-a's lease-loop wins the CAS race because (i) the
old leader (node-b) is dead, (ii) the previous-tick quorum snapshot
*from node-b* included node-a in `StandbyNames` (its walreceiver was
in a retry loop trying to reconnect, indistinguishable from "healthy
standby" by `pg_stat_replication.state`), and (iii) no code anywhere
asks "is this candidate currently in a known-stale or pending-rebootstrap
state on its own report?".

### §B — Patch design

**Two changes, both upstream in pg-manager. Wrapper-side change zero.**

**Part 1 — narrow `buildStandbyNames` to WAL-current peers.**
`pg-manager/reconciler/quorum_snapshot.go:253-288`. Extend the live
filter to drop peers whose slot is invalidated. Two viable signals on
the publisher side:

- `pg_replication_slots.wal_status` for the peer's slot:
  `reserved` / `extended` are OK; `unreserved` / `lost` MUST drop
  the peer. Add a per-tick SQL probe alongside `observeLivePeers`
  (`reconciler/observe.go:561-583`) that joins
  `pg_stat_replication.application_name` against
  `pg_replication_slots.wal_status` and returns the set of "live AND
  not-WAL-stale" peers.
- *Or* impose a max `pg_wal_lsn_diff(pg_current_wal_lsn(),
  restart_lsn)` threshold (e.g., a new `Policy.MaxStandbyLagBytesForElection`
  knob). The DBA may want this knob to express "lag tolerance in
  bytes" rather than slot status — let the architect pick.

Code shape (`buildStandbyNames` signature unchanged; the *populator*
gains a new field):

```
type LivePeerStatus struct { Streaming bool; SlotHealthy bool; LagBytes int64 }
obs.LivePeers map[pgmanager.NodeID]LivePeerStatus  // was map[NodeID]bool
// buildStandbyNames filters n where !LivePeers[n].Streaming || !LivePeers[n].SlotHealthy
```

This is a **state-on-disk wire-format addition**, not a removal. Older
publishers serializing `LivePeers` as the existing bool map keep working
— the consumer just sees a degenerate "Streaming=true, SlotHealthy=true"
for legacy entries. Schema lives at `pg-manager/types.go` near
`FailoverQuorumSnapshot` (line ~378-ish for `AutoRebootstrap`; check
neighbours). The published JSON in `quorum_snapshot.go:77` adds an
optional field.

**Part 2 — candidate self-veto on pending-rebootstrap / lost-slot.**
`pg-manager/reconciler/quorum_gate.go:122-169` (`applyQuorumGate`).
Today the gate only computes "am I in `StandbyNames`?"; extend it to
also refuse when **the candidate itself** observes any of:

- `o.StaleWALCondition == replication.ConditionStaleWAL`
  (already populated by `observeStaleWAL` at `observe.go:358-375`),
- `o.IdentityFence != nil` (already populated),
- `o.DivergenceFence != nil` (already populated),
- `o.RebootstrapHistory.LastCompletedAt` < some "min uptime since
  rebootstrap" knob (i.e., a freshly-rebootstrapped standby may not
  be eligible to lead until it has streamed for ≥ N seconds — closes
  STAB-01's specific 28k-row repro window).

Add a new refusal reason `FailoverRefusalCandidateNotWALCurrent` to
`observability/refusal_reasons.go` (parallel to existing
`FailoverRefusalNoQuorumCandidate`). Emit through the existing
`publishFailoverRefusedEvent` plumbing. Candidate veto fires
**before** the `consultQuorumSnapshot` check so a stale candidate
that doesn't appear in its own old snapshot doesn't conflate two
reasons.

**Wrapper config**: optional new knob in
`internal/config/loader.go:131` group:
`PGMAN_PROXY_POLICY_MAX_STANDBY_LAG_BYTES_FOR_ELECTION` (durSet/intSet).
Production default 0 (= slot-status-only gating). Chaos rig sets to
e.g. 1MB.

**Backwards compat**: legacy `LivePeers map[NodeID]bool` serialization
in StateStore stays readable (decode falls back to "all live = SlotHealthy");
new field is optional. No on-wire break.

**Effort**: **M** (0.5–1.5 days). Touches: 1 new observer probe
(`observeLivePeerSlots`), 1 schema field on `FailoverQuorumSnapshot`,
1 widened filter in `buildStandbyNames`, 1 refusal-reason constant,
1 self-veto branch in `applyQuorumGate`, 1 new
`FailoverRefusedEvent.Reason` case, ~15 LOC wrapper config + env
plumbing. Total ~250 LOC across 6 files in pg-manager + 2 files in
pgman-proxy.

### §C — Blast radius

- `pg-manager/types.go` `FailoverQuorumSnapshot` JSON gains a field
  → readers across milestones consume it; protect with `omitempty` +
  zero-value-means-legacy semantics.
- `pg-manager/reconciler/quorum_snapshot_test.go`,
  `quorum_gate_test.go`, `quorum_snapshot_integration_test.go` need
  new cases for the WAL-stale candidate matrix.
- `pg-manager/examples/three_node_nats/integration_test.go` exercises
  the snapshot publish path → may need updating if its assertions
  pin the legacy shape.
- pgman-proxy `tests/integration/cluster_topology_test.go` (if it
  asserts on `StandbyNames`) — re-baseline.
- The **AutoDemote** + **AutoRebootstrap** paths already use the
  fields the self-veto reads; no risk of double-firing them.

### §D — Test plan

Required pass after fix:

- **SOAK-01** (`validation/scripts/SOAK-01.sh`) — primary kill at
  t+302 s MUST NOT result in stale-standby promotion when a
  current-WAL standby exists.
  Verdict: `data_loss_delta == 0`, `extra_rows_delta == 0`,
  post-settle `streaming = 2`.
- **REC-04** (`validation/scripts/REC-04.sh`) — unchanged; rebootstrap
  byte-equality remains.
- **NET-04** — unchanged; existing AutoDemote happy path must not
  regress.

New scenario (propose to DBA agent for inclusion):

- **DI-05 "stale-standby-not-promoted"** — induce `wal_status=lost`
  on node-a's slot (the REPL-03 setup), then kill primary node-b
  while node-c is current. Assert new primary == node-c (NOT node-a),
  and assert exactly one `FailoverRefusedEvent{Reason="candidate_not_wal_current"}`
  on node-a within the failover budget.

### §E — Upstream vs wrapper

**Upstream pg-manager.** The gate logic, the snapshot schema, and the
WAL-currency observation all live in pg-manager (Constitution
§REQ-CON-02 — the proxy doesn't run replication-slot SQL).
Wrapper-side change is config plumbing only (one env var → one
`Policy.*` field).

### DBA convergence

(File `STAB-DESIGN-DBA.md` not present at write-time — note pending.)
The DBA proposal likely lands on the same axes: slot-status as the
authoritative "WAL-current" signal, plus restart_lsn lag tolerance.
The code-side design here is invariant-on-axis-shape: if the DBA
specifies "use restart_lsn lag" vs "use wal_status", the same patch
shape works — one swap in the probe SQL.

---

## STAB-02 — Divergent ex-primary wedged: PG won't start, AutoDemote never fires (P0)

### §A — Symptom + trace

**Symptom** (`TEST-RESULTS-STABILITY.md §4`, SOAK-01 post-t+302 s):
node-b's `pg_control` checkpoint at `3/907A7D28` on timeline 1; new
cluster timeline 2 forked at `0/10110A28`. PG correctly refuses to
start (`requested timeline 2 is not a child of this server's history`).
pg-manager retries `pg_ctl start` indefinitely; AutoDemote never
fires; node-b is permanently 1/3 capacity loss until manual wipe.

**Code path**:

1. `pg-manager/lifecycle/lifecycle.go:86-97` (`Service.Start`) calls
   `exec.Start` → `pg-manager/internal/pgproto/pgexec.go:121-157`,
   which runs `pg_ctl -D <dir> -w start` and surfaces a non-zero
   exit as an error.
2. The reducer's start dispatcher is `reconciler/act.go:188-217`
   (`runEnsurePostmaster`). On any `Start` error other than
   `ErrPostmasterAlreadyRunning`, it fires
   `state.EventPostgresCrashed{}` → reducer
   (`state/transitions.go:163-166`) transitions to
   `StateFailed`.
3. `StateFailed` is **operator-sticky**:
   `pg-manager/state/transitions.go:46` "Fenced and Failed are
   operator-sticky: only EventOperatorUnfence". No auto-recovery
   path; the only event that leaves `StateFailed` is operator action.
4. **AutoDemote requires PG up.**
   - `reconciler/auto_demote.go:51-90` (`observeDivergence`): line 58
     `if !o.LocalRecoveryObserved { return NoDivergence }`.
   - `LocalRecoveryObserved` is populated by
     `reconciler/observe.go:275-294` (`observeRecoveryState`), gated
     at line 281 by `if o.executor == nil || !obs.PostgresUp { return }`.
   - `PostgresUp` (`observe.go:227`) is `true` only when
     `pg_ctl status` reports running.
   - Net: `pg_ctl start` fails → `PostgresUp=false` → `LocalRecoveryObserved=false`
     → `observeDivergence` fails safe to `NoDivergence`
     → AutoDemote never enters its gate. **The "divergent + can't-start"
     path is structurally unreachable** by the existing AutoDemote
     pipeline.
5. **The wrapper's `ensureStandbySignalIfInitialized`
   (`pgman-proxy/internal/runtime/start.go:354-408`) does not help
   here.** It writes `standby.signal` before manager.Start, which
   makes PG come up as a *standby*. But the standby code path *still*
   reads `pg_control` and *still* refuses with the timeline-mismatch
   FATAL when `pg_control.checkpoint > new_timeline.fork_lsn`. The
   pre-emptive `standby.signal` closes the "PG comes up as
   primary→split-brain" window (its design rationale) but does not
   close the "PG can't recover at all" window.

### §B — Patch design

**Three options, ordered by surgery depth. Recommend Option C, but
all three are valid — architect picks based on appetite for upstream
churn.**

**Option A (wrapper-side, smallest) — pre-Start divergence preflight.**

In `pgman-proxy/internal/runtime/start.go`, alongside
`ensureStandbySignalIfInitialized` (the existing pre-Start hook),
add `preStartDivergencePreflight(dataDir, clusterPrimaryTimeline)`
that runs `pg_controldata` on the existing PGDATA, parses
`"Latest checkpoint location"` and `"Latest checkpoint's TimeLineID"`,
compares against the cluster's published `primary_timeline`
(`pg-manager` writes this to NATS KV at `pgmgr/<cluster>/primary_timeline`
per `reconciler/observe.go:411-419` `primaryTimelineRecord`). If the
local checkpoint is on an old timeline AND past the cluster's known
fork point, **write a sentinel file** (e.g.,
`<DataDir>/pgman.divergence.detected`) before manager.Start so the
pg-manager reducer can synthesize a divergence signal without
needing PG up.

This is **observability-only** in the wrapper. The actual destructive
recovery still belongs to pg-manager.

**Option B (upstream, scoped) — `EventPostgresStartFailedDivergence`.**

In `pg-manager/reconciler/act.go:188-217` (`runEnsurePostmaster`),
**before** firing `EventPostgresCrashed`, parse the start failure's
stderr (or run `pg_controldata` + compare to the cluster's
`primaryTimelineRecord`). On a recognized timeline-mismatch failure
(stderr substring `"requested timeline %d is not a child"`, or
controldata-vs-cluster-timeline divergence), fire a NEW reducer
event `state.EventPGRefusedStartDueToDivergence{}` that lands the
machine in a **non-sticky** state (new: `StateDivergentBeforeStart`)
from which AutoDemote IS eligible to fire.

Schema additions:
- `pg-manager/state/states.go` — new state `StateDivergentBeforeStart`.
- `pg-manager/state/transitions.go` — wire the new event;
  permit `StateDivergentBeforeStart → StatePromoting/StateRunning`
  via standard AutoDemote completion path.
- `pg-manager/reconciler/auto_demote.go:51-90` — extend
  `observeDivergence` to allow `DivergentExPrimary` when
  `curState == StateDivergentBeforeStart` even if
  `!LocalRecoveryObserved`. This is the critical "force-divergence
  detection without PG up" relaxation.

**Option C (upstream, deepest) — controldata-based divergence even
when PG is up.**

The cleanest fix: the divergence-observer should not depend on
`pg_is_in_recovery()` at all. It can be answered by comparing
`pg_controldata`'s `TimeLineID` + `Latest checkpoint location` against
the cluster's `primaryTimelineRecord`. PG-up is irrelevant —
controldata is on disk.

Concretely, add a new `PostgresExecutor.ControlData(ctx)
(ControlData, error)` method
(`pg-manager/interfaces.go` near `ReplicationStatus` at line 173),
wire it into the reducer's `observeTimeline`/`observeDivergence` so
"my on-disk timeline diverges from the published cluster timeline" is
a divergence signal **regardless of whether PG is up**. AutoDemote
gates that fire on the controldata-derived divergence then trigger
the wipe-and-basebackup pipeline through the standard path; the
wipe step in `pg-manager/lifecycle/wipe.go` is a filesystem op that
needs no live PG.

**Recommendation**: Option C. Most surgical for the symptom (no new
state, no event renaming), most general (covers other "PG won't
start due to disk divergence" classes), and removes a long-standing
fragility (PG-must-be-up to detect divergence is itself a B-003
flavor). Effort larger but the code path is well-bounded.

**Effort**: A=**S** (≤0.5 day); B=**M** (1–2 days); C=**L** (2–3 days).
**Backwards compat**: A is pure additive (extra sentinel file); B
adds new state + event but legacy paths unchanged; C adds an
interface method (in-mem fake stub trivial). No on-wire-state changes
in any option.

### §C — Blast radius

- Option A: only `pgman-proxy/internal/runtime/start.go` plus a
  small pg-manager change to *read* the sentinel (otherwise it's
  observability noise). Marginal new surface, but the sentinel-and-
  reader split is awkward.
- Option B: introduces a new state + event; all `state/transitions_test.go`
  + `state/property_test.go` cases need a row for the new state's
  allowed transitions. The auto-demote dispatch in
  `reconciler/auto_demote.go:575-720` (the long sequential
  destructive-recovery routine) needs a new entry point or a
  precondition check at line 722 (`if curState != pgmanager.StateRunning`
  — needs broadening).
- Option C: every observer that consults `LocalIsInRecovery` /
  `LocalRecoveryObserved` may now have two trustable signals (PG +
  controldata) — verify each call site (`observe.go:280-294`,
  `425-490`, `auto_demote.go:51-90`) doesn't conflict.
- pgman-proxy `tests/integration` exercises `runEnsurePostmaster`
  indirectly; add an integration test covering "divergent PGDATA →
  AutoDemote fires without PG ever coming up" (currently impossible).

### §D — Test plan

Required pass after fix:

- **SOAK-01** post-primary-kill: node-b MUST self-recover via
  AutoDemote+rebootstrap within the AutoDemote budget; settle
  window shows `streaming = 2`, no orphan zombies.
- New scenario **REPL-05 "force-divergent-checkpoint-then-restart"**:
  set up a primary, take a checkpoint at LSN X on timeline 1,
  promote another peer at LSN Y < X (creating timeline 2 forked at
  Y), restart the original primary. With the fix, AutoDemote MUST
  fire within budget regardless of PG ever starting; PGDATA wiped;
  peer re-streams against the new timeline.

### §E — Upstream vs wrapper

**Upstream pg-manager** (Option B or C). Option A would put a
PG-correctness signal (controldata vs cluster timeline) on the
wrapper side, which the Constitution `REQ-CON-02` thin-scaffold
gate would reject (`pg_controldata` is a PG binary invocation). The
wrapper may invoke `pg_controldata` indirectly via a pg-manager
helper, but the *logic* belongs upstream.

### DBA convergence

The DBA's invariant is "an ex-primary whose `pg_control` is ahead
of the new timeline's fork point MUST be wiped, not started." The
SWE design above implements that invariant entry on the upstream
side. Option C's controldata-driven divergence is the natural code-
side encoding of the DBA's stated invariant.

---

## STAB-03 — `auto_rebootstrap.refused reason=cooldown_active` blocks recovery (P1, possibly P0)

### §A — Symptom + trace

**Symptom** (`TEST-RESULTS-STABILITY.md §4`, SOAK-01 pre-state and
also §3 REPL-03 carryover): after one successful AutoRebootstrap,
node-a entered cooldown. When a *new* stale-WAL condition appeared
during cooldown the refused-events stacked up; recovery was gated
purely by the wall-clock timer, not by the condition. Under chaos
loads that re-induce stale-WAL faster than the cooldown timer, the
cluster degrades faster than it heals.

**Code path**:

1. **Cooldown is a pure wall-clock timer.**
   `pg-manager/rebootstrap.go:38-43`:
   ```
   func (h RebootstrapHistory) CooldownElapsed(now time.Time, cooldown time.Duration) bool {
       if h.LastCompletedAt.IsZero() { return true }
       return now.Sub(h.LastCompletedAt) >= cooldown
   }
   ```
   `LastCompletedAt` is stamped on every successful rebootstrap
   (`reconciler/rebootstrap.go:457-475` `writeRebootstrapHistory`).
   It is **not reset** on any condition — not on "the standby has
   re-streamed successfully", not on "the streaming-restart proved
   the rebootstrap healthy".
2. **Cooldown gate sits before lease-take.**
   `reconciler/rebootstrap.go:163-165`:
   ```
   if !in.History.CooldownElapsed(in.Now, policy.Cooldown) {
       return AutoRebootstrapDecision{Refusal: pgmanager.ReasonCooldownActive}
   }
   ```
   A second stale-WAL condition arriving during cooldown surfaces
   as `auto_rebootstrap.refused reason=cooldown_active` and is silent
   thereafter (edge-triggered via
   `publishAutoRebootstrapRefusedEdge` at line 343-357). No retry
   path exists until the timer elapses.
3. **`EffectiveAutoRebootstrapPolicy` defaults cooldown to 1h**
   (`reconciler/rebootstrap.go:98-100`). Chaos rigs running 5-minute
   scenarios are guaranteed to hit this.
4. **Cooldown env var is NOT yet wrapped.** Only
   `PGMAN_PROXY_POLICY_AUTO_REBOOTSTRAP_ENABLED` lives at
   `internal/config/loader.go:126`. `AutoRebootstrap.Cooldown` and
   `AutoRebootstrap.PersistenceWindow` have no env aliases. Compare
   with `AutoDemote.Cooldown` etc. at
   `loader.go:136-138` which the prior CHANGELOG (`c05e304`) added.

### §B — Patch design

**Two patches; both upstream-light, wrapper-config-heavy.**

**Part 1 (wrapper) — expose the AutoRebootstrap timing knobs.**

In `pgman-proxy/internal/config/loader.go:136-138` (the AutoDemote
group), add parallel entries:

```
"PGMAN_PROXY_POLICY_AUTO_REBOOTSTRAP_COOLDOWN":           durSet(&cfg.Policy.AutoRebootstrap.Cooldown),
"PGMAN_PROXY_POLICY_AUTO_REBOOTSTRAP_PERSISTENCE_WINDOW": durSet(&cfg.Policy.AutoRebootstrap.PersistenceWindow),
```

In `process-compose.yaml` per-peer env block, set chaos-friendly
defaults: `PGMAN_PROXY_POLICY_AUTO_REBOOTSTRAP_COOLDOWN=30s` and
`PGMAN_PROXY_POLICY_AUTO_REBOOTSTRAP_PERSISTENCE_WINDOW=10s` (so the
detected → wipe gate doesn't sit at 5 minutes either — REC-04's
post-hoc finding).

**This part alone closes the chaos-rig finding.** It does not
address the deeper "cooldown is anti-recovery" critique, but it
lets the rig pick a sane value and unblocks SOAK-01-style scenarios.

**Part 2 (upstream, optional but recommended) — make cooldown
condition-aware.**

`pg-manager/reconciler/rebootstrap.go:137-174` (`ShouldAutoRebootstrap`).
Today the cooldown is unconditional. Refine the gate:

- Track *why* the previous rebootstrap was triggered (already in
  `RebootstrapHistory` is `LastHolderTerm` and `Occurrences` — add
  `LastTriggerLSN uint64` or similar, recording the LSN at which the
  prior rebootstrap fired).
- If the current stale-WAL is **a fresh condition** (current
  walreceiver's `restart_lsn` is markedly ahead of
  `LastTriggerLSN`), the cooldown does NOT apply — the prior
  rebootstrap WAS effective (the standby caught up after it), and
  this is a new condition triggering on new circumstances.
- If the current stale-WAL is **the same condition recurring**
  (e.g., a basebackup-then-immediate-WAL-loss flap), cooldown
  continues to apply (it's the runaway-protection it was designed
  for).

Code shape sketch (`rebootstrap.go:38-43` widens):

```
func (h RebootstrapHistory) CooldownElapsed(now time.Time, cooldown time.Duration, currentLSN, lagThreshold uint64) bool {
    if h.LastCompletedAt.IsZero() { return true }
    if now.Sub(h.LastCompletedAt) >= cooldown { return true }
    // Condition-fresh edge case: this stale-WAL is observably distinct
    // from the prior trigger. Bypass cooldown for forward progress.
    if currentLSN > h.LastTriggerLSN + lagThreshold { return true }
    return false
}
```

Caller threads `o.WALPosition` / `restart_lsn` into the gate inputs;
`AutoRebootstrapInputs` (`rebootstrap.go:60-77`) gains `CurrentLSN`
and `LSNFreshnessThreshold`. Wrapper exposes the threshold via
`PGMAN_PROXY_POLICY_AUTO_REBOOTSTRAP_LSN_FRESHNESS_BYTES`.

**Backwards compat**: `RebootstrapHistory` JSON gains
`last_trigger_lsn` with `omitempty`; zero decodes to "never reset"
which preserves legacy behavior. The signature change in
`CooldownElapsed` ripples to `rebootstrap.go:163` and to
`manager/rebootstrap_lease_test.go:228+`. Manageable.

**Effort**: Part 1 = **S** (≤2 hours, all wrapper). Part 2 = **M**
(0.5-1 day upstream, ~70 LOC). Recommend ship Part 1 immediately;
Part 2 in a separate PR.

### §C — Blast radius

- Part 1 wrapper change is configuration-only. Mirrors the
  pattern from CHANGELOG `c05e304`. Zero risk to non-rig posture.
- Part 2 upstream change touches `RebootstrapHistory`'s JSON
  schema (adding a field) and `CooldownElapsed`'s signature. All
  call sites are in `rebootstrap.go` + tests; no caller outside
  pg-manager imports `CooldownElapsed` (it's a method, only the
  reconciler invokes it).
- `pg-manager/coldstart.go:45` has a similar
  `DemoteHistory.CooldownElapsed` — consider extending symmetrically
  in the same PR, with the same condition-fresh idiom.

### §D — Test plan

Required pass after fix:

- **SOAK-01** — node-a's prior cooldown does not block the second
  rebootstrap when WAL conditions change. (Part 1 alone may close
  this if the rig sets cooldown to 30s.)
- **REC-04** — unchanged byte-equality guarantee.

New scenario **REPL-04 "rebootstrap-then-immediate-stale"**:
1. Force WAL invalidation → node-a rebootstraps. Wait for completion.
2. Within `Cooldown / 4` of completion, force a second WAL invalidation.
3. Assert second rebootstrap fires (Part 2 logic) OR refuses with a
   bounded wait (Part 1 alone, if the cooldown is 30s and the test
   tolerates a 30s wait).

### §E — Upstream vs wrapper

Part 1 is **wrapper-only** (config plumbing). Part 2 is **upstream**
(the cooldown semantics live in pg-manager). The wrapper exposes the
threshold knob; the gate logic stays upstream where it belongs.

### DBA convergence

The DBA's likely invariant: "a freshly-rebootstrapped standby that
has demonstrated streaming progress past LSN X should not be
considered cooldown-locked against a NEW stale-WAL condition at
LSN Y > X." Part 2's `LastTriggerLSN` + freshness threshold encodes
exactly that. If the DBA prefers a different freshness signal
(e.g., "successfully streamed for ≥ N seconds since rebootstrap"),
swap the field but keep the gate shape.

---

## §Summary

| STAB-id | Files | Priority | Effort | Upstream/Wrapper |
|---|---|---|---|---|
| **STAB-01** stale-standby election | `pg-manager/reconciler/quorum_snapshot.go`, `quorum_gate.go`, `observe.go`, `types.go`, `observability/refusal_reasons.go`; `pgman-proxy/internal/config/{config,loader}.go` | **P0** (REQ-DL-01 MUST-PASS) | **M** | Upstream (gate logic) + wrapper config plumbing |
| **STAB-02** divergent ex-primary wedged | Option A: `pgman-proxy/internal/runtime/start.go` + small pg-manager read; Option B: `pg-manager/state/{states,transitions}.go`, `reconciler/{act,auto_demote}.go`; Option C: `pg-manager/interfaces.go`, `internal/pgproto/pgexec.go`, `reconciler/{observe,auto_demote}.go`, `lifecycle/wipe.go` (no change but reachable from new state) | **P0** (REQ-HEAL-01 MUST-PASS mixed) | A=**S**, B=**M**, C=**L** (recommended) | **Upstream** for B/C; wrapper-only for A but discouraged |
| **STAB-03** cooldown anti-recovery | Part 1: `pgman-proxy/internal/config/loader.go`, `process-compose.yaml`. Part 2: `pg-manager/rebootstrap.go`, `reconciler/rebootstrap.go`, `coldstart.go` | **P1** rising to P0 under chaos rate | Part 1=**S**, Part 2=**M** | Part 1 wrapper-only; Part 2 upstream |

**Ship order**:
1. STAB-03 Part 1 (wrapper config — unblocks repro of STAB-01/02 fixes under tight chaos cadence).
2. STAB-02 Option C (deepest upstream fix; closes the "PG can't start" class).
3. STAB-01 (the headline REQ-DL-01 violation; the gate hardening that makes 28k-row regressions impossible-by-construction).
4. STAB-03 Part 2 (condition-fresh cooldown; nicer-to-have once Parts 1+2+3 land).

Effort scale: S = ≤ 0.5 day, M = 0.5–2 days, L = > 2 days.

---

## §DBA convergence — read after STAB-DESIGN-DBA.md is published

The DBA design (`STAB-DESIGN-DBA.md`, read at write-time) and this code-
side design agree on the invariants and mostly on the mechanism. Notable
points of convergence and a few divergences worth flagging:

### STAB-01 — STRONG CONVERGENCE on a better mechanism than mine

The DBA recommends **OPT-1: wire pg-manager's existing
`Manager.PublishReplayLSN` / `Manager.RequestPromote` /
`Policy.PromoteLSNTolerance` machinery into the failover path** (the
DBA pins this at `pg-manager/manager/promote.go` and
`pg-manager/replication/promotion.go`; comment at `manager/promote.go:33-36`
explicitly says "Production wiring (periodic publish from the
reconciler) is pending v0.5.0"). This is **strictly better** than my
"add `wal_status` to `buildStandbyNames`" approach in §B Part 1:

- Reuses existing tested machinery (the `lsnKey` /
  `PublishReplayLSN` / `PromotionEligible` path).
- Operates on `pg_last_wal_replay_lsn()` (the authoritative
  durability surface) rather than `wal_status` (a derived
  invalidation signal).
- The 16 MiB `PromoteLSNTolerance` default is the natural Postgres
  segment-aligned quantum.
- Avoids modifying `FailoverQuorumSnapshot` JSON wire format.

**Updated SWE recommendation for STAB-01**: prefer the DBA's OPT-1 over
my §B Part 1. Keep my §B Part 2 (self-veto on
`o.StaleWALCondition == ConditionStaleWAL` / `IdentityFence` /
`DivergenceFence` / `RebootstrapHistory` freshness) as defense-in-depth
under the DBA's "SL-4 self-disqualification" name. Concrete files
become: `pg-manager/manager/promote.go` (wire periodic publish from
reconciler tick), `pg-manager/reconciler/act.go::runPromote` (call
`PromotionEligible` before `lifecycle.Promote`),
`pg-manager/replication/promotion.go` (existing — extend if needed),
plus the same wrapper config plumbing for the (optional) tolerance
override and the new no-eligible-primary gauge.

**Effort revised**: still **M** (the wire format already exists; what's
missing is the periodic publish from the reconciler tick + the
pre-promote gate call site). Possibly **S** if `PromotionEligible` is
truly drop-in.

**One genuine divergence**: when NO candidate is current (asymmetric
stale-LSN, e.g., both standbys were starved and only the dead primary
was current), the DBA says **block election** (cluster stays
leaderless until operator intervention). I had implicitly let any
WAL-current candidate win even if everyone is stale by the same
budget. The DBA is correct on REQ-DL-01-strict — it is better to be
unavailable than to discard ack'd commits. Update SWE plan: add the
"no eligible primary" gauge + blocking semantic to the patch.

### STAB-02 — STRONG CONVERGENCE on Option C-equivalent

The DBA's OPT-1 is **functionally equivalent to my Option C** (`pg_controldata`
divergence probe driving AutoDemote without requiring `PostgresUp`).
The DBA's framing differs in location: the DBA proposes the probe
at the proxy startup hook (`internal/runtime/start.go::ensureStandbySignalIfInitialized`)
writing a substrate key `pgmgr/<cluster>/divergent_ex_primary/<node>`
that pg-manager observes. My Option C runs the probe inside pg-manager
itself via a new `PostgresExecutor.ControlData(ctx)` method, removing
the wrapper-side write-then-read indirection.

**Which is better?** The wrapper-side write-then-read shape (DBA's) is
slightly more decoupled (the proxy doesn't need any new pg-manager
interface) but introduces a new substrate key. The
in-pg-manager-observer shape (mine) is more cohesive (controldata is
part of "what we know about local PG") but adds a pg-manager interface
method. **Either works.** The DBA's preference for the wrapper-side
shape sidesteps `REQ-CON-02` "no PG binary invocations in this repo"
only if `pg_controldata` is classified as a *read-only metadata* call
(it is, IMO — it doesn't touch WAL or DDL); the architect should
confirm.

**Convergent invariant**: both designs lift the `Observation.PostgresUp`
precondition for the divergence-detected arm of the AutoDemote gate.
That edit lives in `pg-manager/reconciler/auto_demote.go` regardless of
where the probe runs.

**One genuine divergence**: the DBA introduces a NEW substrate key
`pgmgr/<cluster>/timeline_fork/<TLI>` written by `runPromote` after a
successful `pg_promote` (records the fork point per timeline). This is
necessary for the controldata-based divergence comparison and IS an
upstream pg-manager patch in both designs (DBA explicit, mine
implicit). I missed this in my §B — adopt the DBA's recommendation
verbatim.

### STAB-03 — STRONG CONVERGENCE on condition-keyed cooldown

The DBA's OPT-1 (condition-keyed cooldown history) is **the better
shape** of my Part 2 (LSN-keyed history). Reasons:

- The condition class (`stale_wal` vs `divergent_ex_primary` vs
  `sysid_mismatch`) is a more natural identity than the LSN tag; it
  matches the existing reasons in the AutoRebootstrap refusal
  enumeration.
- It's symmetric across AutoDemote and AutoRebootstrap (both have
  cooldowns; both should be condition-keyed).
- It naturally surfaces in metrics (`pgman_proxy_rebootstrap_attempts_total{condition=…}`)
  which the DBA proposes for the loop-detection alert.

**Updated SWE recommendation for STAB-03 Part 2**: adopt the DBA's
OPT-1 condition-keyed history. The wire format change in
`RebootstrapHistory` becomes:

```
type RebootstrapHistory struct {
    LastCompletedByCondition map[string]time.Time `json:"last_completed_by_condition,omitempty"`
    // Legacy field preserved for backward decode:
    LastCompletedAt time.Time `json:"last_completed_at,omitempty"`
    LastHolderTerm  uint64    `json:"last_holder_term"`
    Occurrences     uint64    `json:"occurrences"`
}
```

Adopt the DBA's defense-in-depth bypass: `divergent_ex_primary` with
`PG-refuses-to-start` unconditionally bypasses cooldown (a peer
contributing zero capacity gains nothing from waiting). Log the bypass
as `auto_demote.cooldown_overridden{reason="pg_refuses_to_start"}`.

Keep my Part 1 (wrapper env vars for `AutoRebootstrap.Cooldown` and
`PersistenceWindow`) as the prerequisite; the DBA's OPT-1 + bypass
sits on top.

### Net effect on §Summary table

Replace the STAB-01 row's "files" cell with: `pg-manager/manager/promote.go`
(periodic-publish wiring), `pg-manager/reconciler/act.go` (pre-promote
gate call), `pg-manager/replication/promotion.go` (tolerance read);
`pgman-proxy/internal/config/{config,loader}.go` (tolerance override env)
+ new gauge `pgman_proxy_no_eligible_primary` in
`internal/obs/metrics.go`.

Replace the STAB-02 row's "files" cell with: `pg-manager/reconciler/auto_demote.go`
(lift PostgresUp precondition), `pg-manager/reconciler/act.go::runPromote`
(write `timeline_fork/<TLI>` key on successful promote), plus either
(a) `pgman-proxy/internal/runtime/start.go` (DBA's location) OR
(b) `pg-manager/interfaces.go` + `internal/pgproto/pgexec.go` (my Option
C). Architect-pick.

Replace the STAB-03 row's "files" cell with: `pg-manager/rebootstrap.go`
(`RebootstrapHistory` schema), `pg-manager/coldstart.go` (DemoteHistory
parallel), `pg-manager/reconciler/{rebootstrap,auto_demote}.go` (gate +
bypass), `pgman-proxy/internal/config/loader.go` (env knobs).

No effort changes from the DBA convergence.
