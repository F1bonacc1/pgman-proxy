# Orchestrating PostgreSQL upgrades

How a **host** drives a PostgreSQL version upgrade across a pgman-proxy
cluster, step by step.

> **Audience.** Operators and supervisors (systemd units, container
> orchestrators, deployment scripts, or a Go program embedding
> `pg-manager`) who own the binary tree on each node and coordinate the
> cluster-wide upgrade. If you only want the conceptual answer to "does
> pgman-proxy upgrade Postgres?", read [Delegation model](#delegation-model) and stop.

---

## Delegation model

pgman-proxy **does not upgrade PostgreSQL itself.** It is a thin
scaffold over the `pg-manager` engine (Constitution Principle IV): it
decodes a request, routes it to the right peer, and forwards to the
engine. Every PostgreSQL mechanic — `pg_basebackup`, `pg_rewind`,
`initdb`, `pg_upgrade`, `pg_ctl promote`, binary swapping — lives in
`../pg-manager` (or on the host). A CI grep gate rejects those tokens
in proxy source.

Two responsibilities are therefore **always the host's**, never the
proxy's:

1. **Swapping the PostgreSQL binaries on disk** (symlink/systemd unit
   update). The control plane deliberately never accepts binary bytes.
2. **The cross-node orchestration loop** — deciding which node to act
   on next, performing the switchover before the primary's turn, and
   handling per-node timeouts and retries.

The engine provides the per-node steps and the planning helpers; the
host provides the binaries and the conductor.

---

## What's supported in v1

| Strategy (`UpgradeStrategy`) | Identifier | Status |
|---|---|---|
| `UpgradeMinor` | `minor_rolling` | **Implemented** — rolling restart across patch versions, zero data loss, bounded outage. |
| `UpgradeMajorInPlace` | `major_in_place` | **Wired but gated on v0.7.0.** `ExecuteUpgrade` returns `"upgrade: major strategies wired but gated on v0.7.0"`. |
| `UpgradeMajorLogicalBridge` | `major_logical_bridge` | **Wired but gated on v0.7.0.** Same gate. |

This guide focuses on the supported **minor rolling** upgrade. See
[Major-version upgrades](#major-version-upgrades-gated) for the gated
strategies.

---

## Two orchestration surfaces

There are two ways to drive an upgrade. Pick based on how your host is
integrated.

### A. Library-level host (embeds `pg-manager`)

This is the **fully-supported cross-node path**. Your program holds a
`*pgmanager.Manager` per node (or drives them over your own transport)
and uses the `upgrade` package directly:

- `upgrade.Validate(plan, topology, current)` — pre-flight checks.
- `upgrade.EffectiveOrder(plan, topology, currentPrimary)` — the node
  visit sequence (primary moved to the **end**).
- `upgrade.RunMinorLocal(ctx, lifecycle, probe, plan, preSwap)` — the
  four local steps on one node.
- `upgrade.EstimateDuration(plan, topology)` — maintenance-window size.

Reference: `../pg-manager/upgrade/upgrade.go`,
`../pg-manager/upgrade/minor.go`.

### B. Control-plane host (HTTP / `pgmctl`)

You drive the cluster over the `/v1/*` control plane. Note one v1
constraint that shapes the whole flow:

- `POST /v1/upgrade/prepare` and `POST /v1/upgrade/execute` are
  **leader-only**. A request hitting a non-leader is forwarded to (or
  redirected to) the leader, so `ExecuteUpgrade` always runs on the
  **leader's local node**. HTTP-driven fan-out of `ExecuteUpgrade`
  across *every* peer lands in v0.7.0.
- `POST /v1/restart` with `target=postgres` is **not** leader-only. It
  restarts the **receiving peer's** local PostgreSQL. Its `target_node`
  field must match the peer you addressed, or you get `wrong_peer`
  (HTTP 400). This is the per-node primitive the v1 control-plane host
  uses to restart each node in turn.

  ⚠️ `/v1/restart` is a plain stop→start. It does **not** run a
  pre-swap hook or verify the resulting version. The host must swap the
  binary symlink on that node's disk *before* issuing the restart, and
  verify the version *after* (via `/v1/status` / `pgmctl status`).

Source: `internal/control/server.go:225-239`,
`internal/control/handlers_upgrade.go`,
`internal/control/handlers_restart.go`.

---

## The upgrade plan

Both surfaces center on an `UpgradePlan`
(`../pg-manager/types.go`):

```go
type UpgradePlan struct {
    Strategy      UpgradeStrategy // UpgradeMinor for a rolling patch upgrade
    TargetMajor   int             // must be > 0; >= current major (no downgrade)
    TargetMinor   int             // expected post-upgrade minor (0 = don't check)
    NodeOrder     []NodeID        // empty = topology peer order, primary last
    PerNodeBudget time.Duration   // 0 = strategy default (see below)
}
```

Validation rules (`upgrade.Validate`):

- `TargetMajor > 0` and **never less than the current major** (no
  downgrades).
- `UpgradeMinor` requires `TargetMajor == current.Major`.
- Major strategies require `TargetMajor > current.Major`.
- `PerNodeBudget >= 0`; every `NodeOrder` entry must exist in topology.

`PerNodeBudget == 0` falls back to per-strategy defaults used by
`EstimateDuration`: **2 min** (minor), 15 min (major in-place), 30 min
(major logical bridge). Worst-case window = `PerNodeBudget × peer count`.

As JSON over the control plane:

```json
{
  "plan": {
    "Strategy": 0,
    "TargetMajor": 18,
    "TargetMinor": 4,
    "NodeOrder": [],
    "PerNodeBudget": 120000000000
  }
}
```

---

## Step 0 — Pre-flight (always)

1. **Stage the new binaries on every node** (e.g. install the new
   PostgreSQL package to a versioned path). Do **not** flip the
   `current` symlink yet.
2. **Snapshot cluster state.** `GET /v1/status` (or `pgmctl status`)
   and record `LeaderNodeID`, `PrimaryNodeID`, and per-instance
   `Role` / `State` / `LagBytes`. You need the current primary to
   compute the visit order.
3. **Validate the plan (dry run).** `POST /v1/upgrade/prepare` with the
   plan, or call `upgrade.Validate(...)` directly. This surfaces every
   reason the plan would be rejected and commits nothing.
4. **Confirm a green cluster.** All standbys streaming, lag near zero,
   no node fenced, no in-flight failover. Abort if not.
5. **Size the window.** `upgrade.EstimateDuration(plan, topology)` →
   schedule a maintenance window if needed.

---

## Step 1 — Compute the visit order

Call `upgrade.EffectiveOrder(plan, topology, currentPrimary)`.

- If `plan.NodeOrder` is set, it is used verbatim.
- Otherwise: topology peer order with the **current primary moved to
  the end**.

The invariant that matters: **standbys upgrade first, the primary
upgrades last**, so write availability is preserved until the final
switchover. (`../pg-manager/upgrade/upgrade.go:61`.)

---

## Step 2 — Upgrade each standby, in order

For every node in `EffectiveOrder` **except** the current primary:

The four per-node steps are exactly what `upgrade.RunMinorLocal`
performs (`../pg-manager/upgrade/minor.go:53`):

1. **Stop and verify down** — `StopAndVerifyDown` confirms the
   postmaster is no longer accepting connections.
2. **Pre-swap** — replace the binaries on disk *while Postgres is
   stopped* (see [Pre-swap](#pre-swap-the-binary-swap-window)).
3. **Start** — boot Postgres against the new binaries.
4. **Verify version** — probe `SELECT version()`; abort if it doesn't
   match `TargetMajor` / `TargetMinor`.

**Surface A (library):**

```go
order := upgrade.EffectiveOrder(plan, topology, status.PrimaryNodeID)
for _, node := range order {
    if node == status.PrimaryNodeID {
        continue // handled in Step 4
    }
    if err := upgrade.RunMinorLocal(ctx, lifecycleOf(node), probeOf(node), plan, preSwap); err != nil {
        return fmt.Errorf("node %s: %w", node, err) // STOP — do not continue
    }
    waitForReplicationCaughtUp(node) // LagBytes ~ 0 before moving on
}
```

**Surface B (control plane / `pgmctl`):** `/v1/restart` has no pre-swap
hook, so split steps 1–2 manually — swap the symlink on disk first,
then issue the restart (its stop→start picks up the new binary):

```bash
# For each standby node, in EffectiveOrder:
#   1. (on that node's host) flip the binary symlink to the new tree
ssh "$node" 'ln -sfn /usr/lib/postgresql/18.4 /usr/lib/postgresql/current'

#   2. restart that node's Postgres — addressed to that peer's control plane
pgmctl restart --target=postgres "$node"     # requires cluster-name confirmation

#   3. verify version + replication health before the next node
pgmctl status   # confirm role=standby, postgres up, lag ~ 0, version == target
```

> Addressing matters: `/v1/restart` acts on the **receiving** peer. Hit
> that node's own control-plane endpoint (or set `target_node` to it);
> a mismatch returns `wrong_peer` / HTTP 400.

**Wait for each node to rejoin and catch up** (`LagBytes` back to ~0 in
`/v1/status`) before moving to the next. Never have two nodes down at
once.

---

## Step 3 — Switch over to an upgraded node

Once at least one standby is upgraded and caught up, move the primary
role onto it so the *current* primary can be taken down without losing
writes:

```bash
pgmctl switchover --target <an-already-upgraded-node>
# or: POST /v1/switchover  {"target":"<node>"}   (leader-only)
```

This makes the old primary a standby. Confirm the new primary is
writable and the old primary is now streaming.

---

## Step 4 — Upgrade the old primary last

The old primary is now a standby. Run the **same four steps** on it
(Surface A: `RunMinorLocal`; Surface B: symlink swap + `pgmctl restart
--target=postgres <old-primary>`). Verify version and replication.

Optionally switch the primary role back to the original node if your
topology prefers a specific writer.

---

## Step 5 — Post-flight verification

- `GET /v1/status` — every instance `PostgresUp`, expected roles, lag
  ~0, no node fenced.
- Confirm every node reports the target `major.minor` (`pgmctl status`
  / version probe).
- Run application smoke tests against the proxy listener.

---

## Pre-swap: the binary-swap window

`PreSwap` (`../pg-manager/upgrade/minor.go:39`) is the host callback
invoked **while Postgres is stopped**, between `StopAndVerifyDown` and
`Start`:

```go
type PreSwap func(ctx context.Context) error
```

A production host typically:

- confirms the new PostgreSQL binary tree is staged,
- updates the `current` symlink (e.g. `/usr/lib/postgresql/current`),
- updates the systemd unit's binary path,

then returns `nil`.

**Failure semantics:** returning a non-nil error aborts the local
upgrade with that error and **leaves Postgres stopped** — recovery is
the host's responsibility (typically: restore the old symlink and start
Postgres back up).

> The proxy's `POST /v1/upgrade/execute` wires a **no-op pre-swap** in
> v1 — the HTTP surface intentionally does not accept binary bytes
> (Constitution VII). To use a real pre-swap over HTTP you need an
> out-of-tree proxy build, or drive the swap yourself via the
> Surface B symlink-then-restart pattern above.

---

## Failure & recovery

- **Stop on first error.** If any node's steps fail, halt the loop. Do
  not advance to the next node, and do not switch over onto a node that
  failed to upgrade.
- **A failed pre-swap leaves Postgres down** on that node — restore the
  old binary and restart before retrying.
- **Lost a node mid-upgrade?** Operators must explicitly trigger
  recovery (`pgmctl failover`, `pgmctl promote`, or `pgmctl
  fence`/`unfence`). There is **no automated failover during an upgrade
  sequence** (Constitution II — fail-closed, no silent re-election).
- **Per-node budget** bounds how long you wait before declaring a node
  step failed; size it with `PerNodeBudget` / `EstimateDuration`.

---

## Major-version upgrades (gated)

`UpgradeMajorInPlace` (`pg_upgrade` per node) and
`UpgradeMajorLogicalBridge` (logical-replication bridge to a
new-major replica) are **defined, validated, and sequenced** but
**gated on the v0.7.0 release-hardening pass**: `ExecuteUpgrade`
returns `"upgrade: major strategies wired but gated on v0.7.0"`
(`../pg-manager/manager/backup_upgrade.go:101`). Plan and
`EffectiveOrder`/`EstimateDuration` work today; execution does not.
Do not attempt a major upgrade through this surface until that gate
lifts.

---

## Endpoint & helper reference

| Purpose | Control plane | `pg-manager` | Leader-only? |
|---|---|---|---|
| Read cluster state (primary, roles, lag) | `GET /v1/status` | `Manager.Status` | no |
| Validate plan (dry run) | `POST /v1/upgrade/prepare` | `upgrade.Validate` | **yes** |
| Execute local-node steps | `POST /v1/upgrade/execute` | `upgrade.RunMinorLocal` | **yes** (runs on leader) |
| Restart one node's Postgres | `POST /v1/restart` `target=postgres` | `Manager.RestartPostgres` | no (receiving peer) |
| Move primary role | `POST /v1/switchover` | `Manager.Switchover` | **yes** |
| Force a new primary | `POST /v1/failover` | `Manager.Failover` | **yes** |
| Promote this node | `POST /v1/promote` | `Manager.Promote` | local-only |
| Fence / unfence a node | `POST /v1/fence` · `/v1/unfence` | `Manager.Fence` · `Unfence` | **yes** |

Planning helpers (no side effects):
`upgrade.EffectiveOrder`, `upgrade.EstimateDuration`.

---

## Checklist (minor rolling upgrade)

- [ ] New binaries staged on every node (symlink **not** yet flipped)
- [ ] `/v1/status` snapshot taken; current primary identified
- [ ] `/v1/upgrade/prepare` (or `upgrade.Validate`) returns no error
- [ ] Cluster green: standbys streaming, lag ~0, nothing fenced
- [ ] `EffectiveOrder` computed (primary last)
- [ ] Each standby: stop-verify → swap → start → version check → caught up
- [ ] Switchover onto an upgraded standby; new primary writable
- [ ] Old primary upgraded with the same steps
- [ ] Post-flight: all nodes on target version, lag ~0, smoke tests pass

---

*Source of truth:* `internal/control/handlers_upgrade.go`,
`internal/control/handlers_restart.go`,
`internal/control/server.go`, `../pg-manager/upgrade/`,
`../pg-manager/manager/backup_upgrade.go`,
`../pg-manager/manager/operator.go`. Constitution:
`.specify/memory/constitution.md` (Principles II, IV, VII).
