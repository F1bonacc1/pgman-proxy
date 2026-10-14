# Scale-out from 1 to 3 peers: implementation plan

- **Date:** 2026-10-14
- **Baseline:** pgman-proxy `main` @ `62292d9`, pg-manager `v0.6.1` (`027fbca`), nats-server `v2.14.0`, PostgreSQL 17
- **Status:** plan only; nothing implemented. Every "Verified" statement below was reproduced on a 3-container docker rig on 2026-10-14 (section 6 explains how to rerun it).
- **Goal:** a single-node pgman-proxy can be expanded to a 3-node cluster by (1) pushing the 3-node config to all three hosts, (2) starting node-b and node-c, (3) restarting node-a. No manual Postgres steps, no data loss, write outage measured in seconds. Losing non-crucial KV state (fences, cooldown history, audit history) is accepted.

---

## 0. TL;DR

1. **Single-node mode is broken today, before any expansion.**
   - **D1:** every commit hangs, because `initdb` writes `synchronous_standby_names = 'ANY 1 ("node-a")'`.
   - **D2:** after any restart the node never becomes writable again, because promote is refused with `missing_quorum_snapshot`.
   - Fix: pass `pgmanager.AsyncOnly{}` when there is one peer (WI-A). A small, independent PR; ship it first.
2. **Expanding with today's code always ends with a brand-new empty primary** (3 of 3 trials). One of two things happens:
   - an empty node wins the lease race and runs `initdb`; or
   - node-a wins, but promote is refused 71 times with `sync_peer_silent` (the empty peers are named in the failover-quorum snapshot) until an empty node wins and runs `initdb`.

   Clients are routed to the empty database, and pgman-proxy logs nothing about it.
3. **The fix is mostly in pg-manager** (milestone 015, WI-B):
   - every node publishes whether it has PGDATA;
   - an empty node may campaign or run `initdb` only if every configured peer is known to be empty;
   - empty peers are left out of the sync set and the failover-quorum snapshot, which becomes async when nothing data-bearing is left.
4. **pgman-proxy then needs** (feature 005, WI-C):
   - the boot-window campaign gate;
   - moving incompatible JetStream stores aside;
   - managed `hba_extras`/`conf_extras` blocks applied on every boot;
   - surfacing identity mismatches;
   - a new `tests/scaleout` suite.
5. **Measured** with the gate emulated (node-a forced to win, async quorum): node-a promoted, node-b and node-c cloned it and streamed, and the write outage was **~8.6 s** with the default 5 s lease (0.37 s stop + 8.2 s start to first write).

---

## 1. Verified facts this plan relies on

| # | Fact | How verified |
|---|---|---|
| F1 | Single node with default policy hangs every commit: `synchronous_standby_names = 'ANY 1 ("node-a")'`, wait event `SyncRep`. Cause: `precomputeReconcilerConf` renders from `Topology.Peers` including self (`pg-manager/manager/manager.go:1136`). | rig `single-writes`; 1 GB load blocked > 6 min |
| F2 | Single node restarted with the same config and a persisted KV stays a read-only standby forever: `failover_quorum: skip publish` then `failover refused: quorum gate reason=missing_quorum_snapshot`. pgman-proxy writes `standby.signal` on every boot with PGDATA (`internal/runtime/start.go:523`). | rig `single-restart` (94 s, never primary) |
| F3 | `AsyncOnly` for a single peer fixes F1 and F2: writes succeed; restart is writable in 5 s. | experiment build (one-function patch, see WI-A) |
| F4 | Fresh KV + empty node wins lease: node runs `initdb` (`reconciler.go:954-957` Unknown→Init, `reconciler.go:883` `maybeLateInitDB`). Clients via node-a's proxy see the new empty DB. | rig `race` (trial 1) |
| F5 | Fresh KV + node-a (with data) wins: becomes leader, but `preflightPromote` → `PromotionEligibleSync` refuses `sync_peer_silent` because `publishFailoverQuorum` names never-published empty peers (`quorum_snapshot.go:36-62`, `promotion_sync.go:134`). After ~95 s an empty node wins and runs `initdb`. | rig `a-wins` (trial 2, 71 refusals) |
| F6 | Node-a with foreign primary: `primary_conninfo` points at the empty primary; `postgres.log` shows `database system identifier differs`; pgman-proxy logs nothing. | trial 1 |
| F7 | `auto_rebootstrap` does **not** wipe node-a in that state: preflight refuses with `cluster_identity_mismatch`. | trial 6 |
| F8 | With empty nodes kept out of the race and an async quorum: node-a promotes (first attempt refused once for missing snapshot, next tick approved), node-b and node-c go `init → bootstrapping` from node-a, `pg_basebackup` 2.4 GB in 2.4 s, all three share system id and rows. Write outage ~8.6 s with a 5 s lease. | experiment build, trials 4 and 5 |
| F9 | Rollback (revert node-a to the single-node config) leaves it a read-only standby forever (same as F2), and `postgresql.auto.conf` keeps the stale `synchronous_standby_names = 'ANY 1 ("node-b","node-c")'`. The runtime re-render returns early when no peers qualify (`act.go:1434`, inside `handleTopologyChange`), so the stale value is never cleared. | rig `rollback` |
| F10 | Runtime sync-name changes go through `ALTER SYSTEM` (`postgresql.auto.conf`); lines appended by `conf_extras` to `postgresql.conf` survive. | trial 2 file inspection |
| F11 | `hba_extras`/`conf_extras` are applied only by the PostInitDB hook (`start.go:845`), i.e. only on the node that ran `initdb`; `pg_basebackup` clones inherit `pg_hba.conf`. | code + clone inspection |
| F12 | Lease campaigning starts inside `NewLeadership` (`pg-manager/adapters/nats/leadership.go:451`), before `manager.New`; `Manager.ShouldCampaign` on a nil Manager returns `true` (`manager/manager.go:692-696`), and pgman-proxy's gate closure calls it through a nil pointer during boot (`start.go:174`). | code |
| F13 | Non-clustered JetStream store has no `<jetstream_dir>/jetstream/$SYS/_js_/_meta_`; a clustered one does. Restarting clustered on an old non-clustered dir does **not** delete it: the history stream was adopted with its old records, the KV came out fresh. Behavior is undefined, so do not rely on it. | trial 1 directory listing + KV dump |
| F14 | A 3-replica stream with 2 of 3 servers up fails `no suitable peers for placement` (err 10005); 2-of-3 servers do form a responsive JetStream cluster within ~3 s. | standalone embedded-server experiment |
| F15 | A node alone in a `declared_size: 3` config exits with code 75 after 120 s (`WaitForRouteMesh`, `start.go:140`). | rig `solo` |
| F16 | pgman-proxy shutdown does not stop Postgres; in a container the postmaster is killed with it (`postmaster.pid` left behind, crash recovery on next start). | trial observations |

Rejected or corrected claims from the design discussion (do not reintroduce):
- "nats-server deletes orphaned streams": wrong (F13).
- "auto_rebootstrap could wipe node-a": wrong (F7).
- "writes hang during basebackup": moot today, because node-a never gets to promote (F5).
- "tens of seconds of downtime": too pessimistic (F8).

---

## 2. Target behavior after all work items

### Operator procedure
1. Provision node-b and node-c hosts (empty PGDATA, empty `jetstream_dir`).
2. Push the 3-node config to all three hosts. Config delta from single-node:

   | Key | 1 node | 3 nodes |
   |---|---|---|
   | `peers` | `[node-a]` | `[node-a, node-b, node-c]` |
   | `cluster.declared_size` | `1` | `3` |
   | `cluster.route_peers` | `[]` | the other two `host:6222` |
   | `cluster.routes_listen` | ignored at size 1 | reachable host, `6222` |
   | `cluster.username`, `cluster.password_env\|password_file` | unused | required, identical everywhere |
   | `cluster.tls.*` or `cluster.tls.plaintext_explicit_ack` | unused | required (non-loopback routes) |
   | `cluster.jetstream_dir` | optional | required, durable |
   | `postgres.replication_addr` | n/a | per node, unless peer ids resolve through DNS |
   | `postgres.hba_extras` | (any) | must allow replication from peer addresses; applied on every boot after WI-C3 |
   | unchanged | `cluster.id`, `cluster.name`, node-a `node.id`, `postgres.data_dir`, `postgres.bin_dir` | |

3. Start node-b and node-c (they wait up to 120 s for the full route mesh).
4. Restart node-a.

### What happens (each step maps to a work item)
1. node-a boot: moves the non-clustered `jetstream/` dir aside (WI-C2), applies the managed hba/conf blocks (WI-C3), starts clustered NATS, waits for both routes, creates the KV and history stream with 3 replicas, writes `standby.signal`.
2. node-b and node-c publish `pgdata/<node> = {has_data:false}`; node-a publishes `{has_data:true, system_identifier}` (WI-B1).
3. Campaign gate: empty nodes do not campaign while any configured peer has data or has not reported (WI-B2, plus WI-C1 for the boot window). node-a wins.
4. node-a's leader snapshot excludes empty peers; with no data-bearing peer left it publishes an async snapshot and renders `synchronous_standby_names = ''` (WI-B4). Promote passes the LSN gate. node-a is primary; writes resume.
5. node-b and node-c observe leader node-a: `init → bootstrapping`, `pg_basebackup` from node-a, publish `has_data:true` (WI-B1), start streaming.
6. node-a re-includes them: `synchronous_standby_names = 'ANY 1 ("node-b","node-c")'`, sync snapshot. Done.

### Rollback
Revert node-a to the single-node config and restart. WI-C2 moves the clustered store aside; WI-A gives `AsyncOnly`; WI-B4 clears the stale `synchronous_standby_names` from `postgresql.auto.conf`; node-a promotes and accepts writes.

---

## 3. Work items

IDs used in PR titles and commits. "Old #" maps to the numbering used in the design conversation.

| ID | Repo | Title | Old # | Depends on |
|---|---|---|---|---|
| WI-A | pgman-proxy | Single-peer clusters use `AsyncOnly` replication | 0 | none |
| WI-B1 | pg-manager | Per-node PGDATA presence record | 1 | none |
| WI-B2 | pg-manager | Empty-node campaign deference | 1 | B1 |
| WI-B3 | pg-manager | `initdb` re-check + resign | 1 | B1 |
| WI-B4 | pg-manager | Exclude empty peers from sync set and failover quorum; render empty sync names instead of skipping | 2 | B1 |
| WI-B5 | pg-manager | B-024: `initdb` conf renders sync names including self | (new) | none |
| WI-B6 | pg-manager | Identity-mismatch detection and event | 6 | B1 |
| WI-B7 | pg-manager | (optional) Publish leader snapshot before the first promote preflight | (new) | none |
| WI-C1 | pgman-proxy | Boot-window campaign gate | 3 | B2 (ship together) |
| WI-C2 | pgman-proxy | Move incompatible JetStream store aside at boot | 4 | none |
| WI-C3 | pgman-proxy | Managed `hba_extras`/`conf_extras` blocks on every boot | 5 | none |
| WI-C4 | pgman-proxy | Surface identity mismatch (doctor, metric, event) | 6 | B6 |
| WI-C5 | pgman-proxy | Bump pg-manager to the 015 release | | B1-B6 tagged |
| WI-C6 | pgman-proxy | `tests/scaleout` end-to-end suite + nightly job | | A, C1-C5 |
| WI-C7 | pgman-proxy | Runbook, spec 002 amendment, CHANGELOG | | C6 |

---

### WI-A: Single-peer clusters use `AsyncOnly` replication (pgman-proxy, ship first)

**Problem.** F1, F2. With one peer, sync replication is impossible; `QuorumSync{MinSync:1, Pool:[self]}` can never be satisfied, and `MinSync < 1` is rejected by `replication.Render` (`pg-manager/replication/sync_standby_names.go:71`). pg-manager already supports `pgmanager.AsyncOnly{}` (`types.go:924`), which renders `''` and publishes an async snapshot (`renderQuorumMethod`, `quorum_snapshot.go:223`), so promotion goes through the legacy LSN gate.

**Change.** `internal/runtime/start.go:648` `policyFromConfig`:
```go
Replication: replicationPolicy(cfg),
...
// replicationPolicy derives the pg-manager replication policy from the
// configured peer set. A single peer cannot have a synchronous standby,
// so it runs AsyncOnly; QuorumSync with a pool of one renders a
// self-referential synchronous_standby_names that hangs every commit.
func replicationPolicy(cfg config.Config) pgmanager.ReplicationPolicy {
	if len(cfg.Peers) == 1 {
		return pgmanager.AsyncOnly{}
	}
	return pgmanager.QuorumSync{MinSync: cfg.Policy.QuorumSync.MinSync, Pool: nodeIDs(cfg.Peers)}
}
```
- Startup log: add `replication_policy` (`async` or `quorum(min_sync=N)`) to the existing startup event; Constitution V.
- No new config key (Constitution VII): there is no working alternative for one peer, so deriving it mirrors how `DeriveReplicas` derives R from `declared_size`.
- Validation (optional, small): warn at startup if `policy.quorum_sync.min_sync > len(peers)-1` for multi-peer configs (would make the quorum unsatisfiable).

**Existing broken deployments.** A single node initialized before this fix has `ANY 1 ("node-a")` in `postgresql.conf` from `initdb`. With `AsyncOnly`, pg-manager's runtime render must write `''` to `postgresql.auto.conf`, but today it returns early when `renderPeers` is empty (F9), so the stale value stays until WI-B4 lands. Until then, WI-A on an existing data dir needs a one-time `ALTER SYSTEM SET synchronous_standby_names = ''` documented in the CHANGELOG. Verify this exact upgrade path in the integration test below (start the old image, then the new one on the same volume).

**Tests.**
- Unit: table test for `replicationPolicy` (1 peer → AsyncOnly; 2 and 3 → QuorumSync with MinSync and Pool).
- Integration (Constitution VI requires real Postgres): new test that runs one container with the single-node config (reuse rig `single-a.env` values), asserts (1) a write commits within 10 s of becoming primary, (2) after `docker stop`/start the node is primary and writable within 30 s, (3) upgrade path from a data dir initialized by the old image. The existing `tests/integration/single_peer_test.go` runs without Postgres, which is why F1/F2 were never caught; do not extend it, add `tests/integration/single_node_pg_test.go` or put it in `tests/scaleout` (WI-C6) if that lands first. Note the main integration `TestMain` brings up the 3-node compose topology once per package, so a single-node container must use its own network and container names.

**Acceptance.** rig `single-writes` prints `W_OK`; rig `single-restart` becomes primary in < 30 s and prints `W_OK`.

---

### WI-B: pg-manager milestone `015-empty-peer-safety` (upstream, Constitution IV)

Next free numbers in pg-manager: milestone **015**, backlog **B-024**. Follow pg-manager's speckit flow (`specs/015-empty-peer-safety/`), with this section as the input description.

#### WI-B1: Per-node PGDATA presence record
- **Key:** `pgmgr/<cluster>/pgdata/<node>` (the NATS adapter maps it to `state__pgmgr__<cluster>__pgdata__<node>`, same as the `lsn` keys).
- **Value (JSON):** `{"has_data": bool, "system_identifier": "<decimal or empty>", "updated_at": RFC3339}`.
- **Writer:** reconciler, every tick, next to `publishCurrentLSN` (`act.go:985`). Unlike the LSN publisher it must run when Postgres is down (an empty node never has Postgres up). Write on change, or when older than the LSN freshness window (`lsnFreshnessWindow`, 2 x LivenessInterval).
- **has_data source:** the existing `PGDataEmpty` observation (`observe.go:879`, `PG_VERSION` stat). Fail-safe: any stat error other than not-exist means "unknown": publish nothing rather than `has_data:false`.
- **system_identifier:** read once when Postgres is up (`replication.ReadSystemIdentifier`, `replication/cluster_id.go:39`) and cache; empty when unknown.
- **Join transition:** in `runBootstrap` (`act.go:1219`), publish `has_data:true` right after `BaseBackup` succeeds and before `Lifecycle.Start` (`act.go:1313`). A failed publish must not block the start (the record catches up next tick; until then the leader simply does not wait for this standby).
- **Reader helper:** `peerDataState(ctx) map[NodeID]peerData` covering `activeTopology().Peers` only (records of non-configured nodes are ignored, so decommissioning = removing the peer from config).
- **Contract doc:** add the key to pg-manager's state-store contract (same place as `DivergenceFenceKey` etc.).

#### WI-B2: Empty-node campaign deference
- In `Reconciler.ShouldCampaign` (`reconciler.go:529`), add a first clause: `if r.emptyDefer.Load() { return false }`.
- Compute `emptyDefer` once per tick next to `updateCampaignDeference` (`reconciler.go:549`, call site `reconciler.go:651`):
  - local `PGDataEmpty == false` → `false` (nodes with data are never gated);
  - else `true` unless **every** configured peer other than self has a **fresh** record with `has_data:false`;
  - any peer with `has_data:true` (fresh or stale) → `true`;
  - KV read error → `true` (fail-closed).
- Initial value before the first evaluation: `true` if PGDATA is empty at construction.
- Log edge transitions at Info (`campaign: empty-node deference armed|released`, with `peers_with_data`, `peers_unreported`), like milestone 013's deference logging.
- Rationale for "every peer must report empty": waiting for a majority would let node-b and node-c `initdb` while node-a (the only node with data) is still booting. In pgman-proxy every peer is guaranteed reachable before campaigning anyway (route-mesh gate, F15), so this costs nothing there.

#### WI-B3: `initdb` re-check + resign
- The campaign gate only gates lease **acquisition** (`leadership.go:178`). A node that already holds the lease (boot-window race, publish delay) must not `initdb` either.
- In `runInitDB` (`act.go:325`) and in both dispatch paths (`act.go:123-129`, `maybeLateInitDB` at `reconciler.go:883`): before `Lifecycle.InitDB`, re-read the peer records (not the cached tick value) and apply the B2 predicate. If not satisfied:
  1. log `initdb: refused` with reason `peer_has_data` or `peers_unreported`;
  2. emit an observability event;
  3. resign the lease (`r.cfg.Leadership.Resign`, pattern in `reconciler/resign.go:120`);
  4. reset `initDBDispatched` and stay in `Init`.

  The node will then see the real leader and take `EventBootstrapNeeded` (`reconciler.go:1089`).
- KV unavailable → refuse (fail-closed).

#### WI-B4: Exclude empty peers from sync set and failover quorum; never skip the empty render
- **Filter:** a peer is excluded only if its record is **fresh** and `has_data:false`. Stale-empty or missing records count as unknown and stay included (fail-closed; preserves today's behavior when records are absent).
- **Failover quorum:** in `publishFailoverQuorum` (`quorum_snapshot.go:36`), apply the filter to `standbyNames` (after `buildStandbyNames` and `dropDeadPeers`). Effective `StandbyNumber = min(policy k, len(filtered))`.
  - If the filtered set is empty **and** every excluded peer was fresh-empty, publish `Method = async`, `StandbyNames = []`, `StandbyNumber = 0` instead of hitting the skip-publish guard (`quorum_snapshot.go:78`).
  - If the set is short because peers are unknown, keep today's skip (fail-closed).
  - `PromotionEligibleSync` (`replication/promotion_sync.go:80`) needs no change: an async snapshot delegates to the legacy LSN gate (step 2).
- **Sync names:** in `handleTopologyChange` (`act.go:1370`), apply the same filter to `renderPeers`. When the result is empty (or the policy is `AsyncOnly`), apply `synchronous_standby_names = ''` through the same `ALTER SYSTEM` path instead of the early `return`. This also fixes F9 (stale value after rollback) and the upgrade path in WI-A.
- **Observability:** an edge-triggered event `replication.sync_set_reduced` with `excluded_empty_peers`, and a gauge for the effective sync set size.
- **Durability statement (put it in the spec):** while every pool peer is empty, the primary runs async. That equals single-node durability, which is what the cluster had before expanding; an empty peer cannot hold an acknowledged commit, so excluding it never drops a durable write. A peer re-enters the set once its record flips to `has_data:true` (after basebackup).

#### WI-B5: B-024 `initdb` conf renders sync names including self
- `precomputeReconcilerConf` (`manager/manager.go:1136`) calls `replication.Render(cfg.Policy.Replication, cfg.Topology.Peers)` with self included, producing `ANY 1 ("node-a")` for one peer and `ANY 1 ("node-a","node-b","node-c")` for three.
- After B3, `initdb` only runs when every peer is empty, so the correct `initdb`-time value is always `''`. Render `''` (async metadata) in the precomputed conf. The runtime render (B4) takes over on the first tick.
- File as `specs/B-024-initdb-sync-names-include-self.md`, with F1 as evidence.

#### WI-B6: Identity-mismatch detection and event
- Each tick on a standby: if the leader's `pgdata` record has a `system_identifier` and it differs from the local one, emit `cluster.identity_mismatch` (edge-triggered) and expose it in `Status` (new field). No automatic action.
- After B2/B3 this should never fire; it is the backstop for F6 (today silent).

#### WI-B7 (optional): Publish the leader snapshot before the first promote preflight
- F8: the first promote attempt after winning is always refused once (`missing_quorum_snapshot`), costing one tick (~2 s at the default LivenessInterval). Reorder the tick (`reconciler.go:679-706`) so a newly elected leader publishes its snapshot before `preflightPromote` runs in the same tick. Skip if it complicates 015; it is pure latency.

#### WI-B tests (pg-manager)
- Unit tables for the B2 predicate, the B4 filter and effective `k`, and the B5 render.
- Reconciler scenarios with `adapters/inmem` (3 nodes, fresh store):
  - (a) one node with data plus two empty: only the data node campaigns, it promotes, the empties bootstrap from it;
  - (b) all empty: exactly one `initdb`, ever;
  - (c) the data node absent plus two empty: no `initdb`, no promote, armed deference logged;
  - (d) an empty node already holding the lease is refused at `initdb` and resigns;
  - (e) rollback: a single peer with a stale sync value is rendered `''`.
- pg-manager's compose integration tier: add scenario (a) with real Postgres.

**Release:** tag `v0.7.0` (new state-store key + behavior change). Remember the pgx coupled bump noted in pgman-proxy `go.mod`.

---

### WI-C1: Boot-window campaign gate (pgman-proxy)
- **Problem:** F12. During boot the closure at `start.go:174` calls `mgrRef.Load().ShouldCampaign()` on a nil Manager, which returns `true`, so an empty node campaigns before B2 can see it.
- **Change:** extract a testable function and use it in the closure:
  ```go
  func campaignGate(m *manager.Manager, dataDir string) bool {
  	if m != nil {
  		return m.ShouldCampaign()
  	}
  	// Boot window (before manager.New): only a node that already has
  	// PGDATA may campaign. An empty node waits for the Manager's
  	// empty-node deference (pg-manager 015) to decide.
  	return pgdataInitialized(dataDir) // PG_VERSION present; any stat error → false
  }
  ```
  Reuse the `PG_VERSION` check from `ensureStandbySignalIfInitialized` (`start.go:523`) by extracting it.
- **Do not** change pg-manager's nil-receiver semantics (other embedders rely on them).
- **Ship with** WI-C5. Alone it only narrows the window.
- **Tests:** unit table (nil manager with and without PG_VERSION, stat error); end to end via WI-C6.

### WI-C2: Move an incompatible JetStream store aside at boot (pgman-proxy)
- **Problem:** F13. Reusing a non-clustered store in clustered mode (or the reverse on rollback) has undefined results.
- **Change:** new `internal/embedded/storage.go` function, called from `bootEmbeddedNATS` (`start.go:730`) before `embedded.NewServer` (`start.go:791`):
  ```go
  // ArchiveIncompatibleStore moves <dir>/jetstream aside when its layout
  // does not match the mode we are about to start in: a clustered start
  // (declared_size > 1) over a store without $SYS/_js_/_meta_, or a
  // single-peer start over a store that has it. Returns the new path, or
  // "" when nothing was moved.
  func ArchiveIncompatibleStore(dir string, clustered bool, now time.Time) (string, error)
  ```
  - Rename `<dir>/jetstream` to `<dir>/jetstream.standalone-<UTC 20060102T150405Z>` (or `.clustered-<ts>`). Rename inside `dir`, never `dir` itself: it is often a volume mount point.
  - Emit `embedded_nats.store_archived` `{from, to, reason}` at Warn.
  - Empty `jetstream_dir` (in-memory, single peer) → no-op.
  - Rename failure → fail closed with a startup error naming the path.
- **Not doing:** carrying KV keys across (the user accepted losing them; see section 7).
- **Tests:** unit with temp-dir fixtures for both layouts, both directions, mount-point safety (dir itself untouched), idempotence (second boot is a no-op). Integration via WI-C6 S1 and S3.
- **Docs:** archived dirs are kept for forensics and should be deleted by the operator after verification.

### WI-C3: Managed `hba_extras`/`conf_extras` blocks on every boot (pgman-proxy)
- **Problem:** F11. An existing single node never gets the replication HBA rules it needs to serve `pg_basebackup` to new peers.
- **Change:** replace `appendLines` (`start.go:873`) with `applyManagedBlock(path, name string, lines []string) (changed bool, err error)`:
  - Markers: `# BEGIN pgman-proxy managed: postgres.hba_extras` / `# END pgman-proxy managed: postgres.hba_extras` (same for `conf_extras` in `postgresql.conf`).
  - Rewrites the block in place, or appends it at the end; removes it when `lines` is empty (so rules can be withdrawn); atomic write (temp file + rename, keep mode 0600).
  - Legacy blocks written by the old PostInitDB hook (`# pgman-proxy: postgres.hba_extras` + lines) are left in place: duplicate HBA lines are harmless (first match wins), and for `postgresql.conf` the managed block at the end wins. Document this.
- **Call sites:**
  1. the PostInitDB hook (`start.go:845`);
  2. every boot after `ensureStandbySignalIfInitialized` (`start.go:272`) when PGDATA is initialized;
  3. if the postmaster is already running (`postmaster.pid` present and a `LOCAL_DSN` connect succeeds), `SELECT pg_reload_conf()` when anything changed.
- Runtime sync-name changes go to `postgresql.auto.conf` (F10), so this does not fight pg-manager.
- **Constitution IV:** HBA management arguably belongs in pg-manager; flag it in the PR with a follow-up issue to upstream it.
- **Tests:** unit (insert, update, remove, idempotent, legacy coexistence, file mode); integration via WI-C6 S1 with a golden node created **without** replication HBA rules and expanded with them configured (proves the existing-PGDATA path).

### WI-C4: Surface identity mismatch (pgman-proxy)
- Consume B6: a doctor check `cluster_identity` in `internal/control/doctor_checks.go` (read-only, per the catalogue rules), a gauge `pgman_proxy_cluster_identity_mismatch` (0/1), and pass the event through the existing history and log pipeline.
- **Tests:** contract test with `fakeEngine` returning the mismatch status (fault injection is allowed under the Constitution VI carve-out); real-server coverage comes from the WI-C6 negative scenario (S6).

### WI-C5: Bump pg-manager to v0.7.0
- `go.mod` require bump; the pgx coupled bump if pg-manager moved pgx (see the `go.mod` comment); `go mod tidy`; image builds (`tests/integration/Dockerfile` uses the sibling checkout via `replace`, so the sibling must be at the tag).
- Run the full `make integration` suite: every run starts a fresh 3-node cluster from empty volumes, which is the regression test that B2 did not break fresh bootstrap.

### WI-C6: `tests/scaleout` end-to-end suite (pgman-proxy)
- **Package:** `tests/scaleout` with build tag `scaleout` and its own `TestMain`. It cannot reuse the `tests/integration` compose `TestMain`, which brings up a fixed 3-node topology for the whole package.
- **Driver:** the docker CLI (network, named volumes, `docker run --env-file`), mirroring `docs/scale-out-1-to-3/rig/lib.sh`; reuse `execDocker`-style helpers. Image: the integration image.
- **CI:** new `make scaleout` target (`-count=1 -timeout=30m -tags=scaleout`) and a step in `.github/workflows/nightly-integration.yml`.
- **Scenarios and assertions:**

  | ID | Scenario | Must hold |
  |---|---|---|
  | S1 | 1→3 expansion, start order randomized (a first / a last / all together), **5 iterations** | one system id on all nodes; all golden rows on all nodes; node-a primary; node-b and node-c streaming; `synchronous_standby_names` names node-b and node-c at the end; no `initdb` on node-b or node-c (log assertion); write outage < 30 s |
  | S2 | fresh 3-node cluster, all empty | exactly one `initdb`; cluster healthy (covered by `make integration`, assert explicitly here too) |
  | S3 | rollback after S1 (single-node config again) | node-a primary and writable < 30 s; `synchronous_standby_names = ''` |
  | S4 | node-a alone in the 3-node config | exit code 75 after ~120 s; PGDATA untouched (same system id and rows) |
  | S5 | single-node restart (WI-A) | writable < 30 s |
  | S6 | node-a's data node kept down: node-b and node-c empty with a 3-node config | no `initdb` anywhere within 3 min; deference armed logged; no proxy serves writes |
  | S7 | golden node without replication HBA rules, expanded with them configured | basebackups succeed (WI-C3) |
- Timing budgets are generous on purpose (shared CI runners); the measured local figure is ~8.6 s.

### WI-C7: Docs (pgman-proxy)
- `docs/scale-out-1-to-3/runbook.md`: the operator procedure and config table from section 2, verification (`pgmctl status`, `pgman_proxy_embedded_nats_replicas_factor == 3`, `routes_meshed == 2`, sync set), rollback, and what is lost (KV fences, cooldown histories, history stream is archived not migrated).
- Amend `specs/002-embedded-nats-cluster/spec.md` User Story 2, acceptance scenario 2: it promises SIGHUP scale-out without restart, which the code cannot do (size 1 has no routes listener; `declared_size` and `routes_listen` are startup-only; NATS cannot cluster a lone server, `raft.go:364`). Replace it with the restart-based procedure. This amendment can land early, independently.
- CHANGELOG: fix (D1, D2), feat (safe 1→3 expansion), behavior change (store archiving, managed conf blocks).

---

## 4. Sequencing and PRs

```
Day 1   PR-1  pgman-proxy WI-A  (+ one-time ALTER SYSTEM note)       independent, ship immediately
        PR-2  pgman-proxy WI-C7 part 1: spec 002 US2 amendment         independent
        PR-3  pgman-proxy WI-C2 (store archiving)                      independent
        PR-4  pgman-proxy WI-C3 (managed conf blocks)                  independent
In parallel, pg-manager:
        015 spec/plan/tasks (speckit) → B1 → B2+B3 → B4 → B5 → B6 → (B7) → tag v0.7.0
Then    PR-5  pgman-proxy WI-C5 + WI-C1 + WI-C4 (bump and wire)
        PR-6  pgman-proxy WI-C6 (tests/scaleout + nightly)             may be developed earlier against the sibling checkout
        PR-7  pgman-proxy WI-C7 part 2 (runbook, CHANGELOG)
```

Use speckit for both repos: `/speckit-specify` in pg-manager for 015 (input: section WI-B) and in pgman-proxy for 005 (input: WI-A, WI-C, with WI-B as an upstream prerequisite). pgman-proxy's CLAUDE.md "Active feature" pointer moves to 005 when that starts.

**Definition of done (whole effort):**
- every rig scenario in section 6 passes on the fixed build (the race and a-wins verdicts are PASS);
- `make scaleout` is green, 5 out of 5 S1 iterations;
- `make integration` is green unchanged;
- the runbook is merged.

---

## 5. Decisions to confirm before coding (recommendation first)

1. **WI-A trigger:** derive `AsyncOnly` from `len(peers) == 1` (recommended; no valid alternative exists for one peer) vs a new config key.
2. **Gate strictness:** every configured peer must report empty before an empty node campaigns or runs `initdb` (recommended; fail-closed, free in pgman-proxy because of the mesh gate) vs a majority (would reintroduce the expansion race when node-a boots last).
3. **Join-window durability:** async while all pool peers are empty (recommended; equals pre-expansion single-node durability) vs blocking writes until a standby streams (today's design intent; for a large database that is a write outage as long as the basebackup).
4. **Store archiving direction:** archive in both directions (recommended; makes rollback a plain config revert) vs refuse to start on a clustered store with `declared_size: 1`.
5. **pg-manager version:** `v0.7.0` (recommended; new state key and behavior) vs `v0.6.2`.
6. **HBA ownership:** implement in pgman-proxy now and file the upstream follow-up (recommended) vs block on pg-manager owning HBA rendering.
7. **Override for "the only data node is gone":** out of scope (the cluster waits, by design). A future `pgmctl bootstrap --authorize <node>` could write an explicit authorization key. Record as a follow-up only.

## 6. Reproduction rig (`docs/scale-out-1-to-3/rig/`)

Throwaway verification tooling used to produce section 1, kept so each fix can be checked against the baseline before `tests/scaleout` exists.

```bash
# from the directory that contains pg-manager/ and pgman-proxy/
docker build -f pgman-proxy/tests/integration/Dockerfile -t pgman-proxy-it:latest .
cd pgman-proxy/docs/scale-out-1-to-3/rig
./scenarios.sh single-writes    # baseline: BLOCKED (SyncRep)        fixed (WI-A): W_OK
./scenarios.sh golden           # builds the 1M-row single-node snapshot (needed below)
./scenarios.sh single-restart   # baseline: primary never            fixed (WI-A): < 30 s
./scenarios.sh race             # baseline: VERDICT FAIL (empty initdb primary)   fixed: PASS
./scenarios.sh a-wins           # baseline: FAIL after many 'promote: refused'    fixed: PASS
./scenarios.sh rollback         # baseline: primary never            fixed (A+B4+C2): writable
./scenarios.sh solo             # exit code 75 after ~120 s (expected, unchanged)
./scenarios.sh clean
```
- The rig runs nodes with plain `docker run` on network `pgv-net`, volumes `pgv-<node>-{data,js}`, env from `common.env` + `single-a.env` / `three-<node>.env` + an optional extra env file. `monitor.sh` samples role, system id, rows, sync names and a write probe every 2 s; per-node logs land next to the scripts as `<trial>-<node>.log`.
- `lease-fast.env` / `lease-slow.env` rig the race so node-a wins; it is a test device, never a production setting.
- The `AsyncOnly` experiment build used during verification was a one-function patch equivalent to WI-A plus an env switch forcing `AsyncOnly` on all nodes (to emulate WI-B4 for the happy path); it is not kept.
- `none.env` is created empty by the script; `*.mon` and `*.log` outputs are scratch and should not be committed.

## 7. Out of scope (decided in the design discussion)
- Live (no-restart) 1→3: impossible at the NATS layer (a lone server cannot run clustered JetStream, `nats-server` `jetstream_cluster.go:946`, `raft.go:364`).
- Core-NATS-first single node with JetStream enabled later: rejected (pg-manager leadership and state store require JetStream KV; it would force the in-memory adapters and a live Manager swap, and the gate is still needed).
- Carrying KV state across the rebuild: dropped; the user accepts losing non-crucial KV.
- SIGHUP-reloadable `peers`/`declared_size` for live 2→3 and 3→5: a good follow-up, not needed here. Note that `POST /v1/topology` is leader-only and in-memory, so it is not a membership mechanism.
- Shrinking 3→1 beyond the rollback path.

## 8. Code anchors (at the baseline commits)

pg-manager (`v0.6.1`):
- `adapters/nats/leadership.go:178` campaign gate gates acquisition only; `:451` eager `startLease`; `:757` create jitter
- `manager/manager.go:692` `ShouldCampaign` (nil → true); `:1136` `precomputeReconcilerConf`
- `reconciler/reconciler.go:529` `ShouldCampaign`; `:549` `updateCampaignDeference`; `:622` `observedHealthyPrimary`; `:651/679/683/706` tick call sites; `:883` `maybeLateInitDB`; `:954` Unknown-state startup / join decision; `:1089` late join from Init
- `reconciler/act.go:123-142` lifecycle dispatch; `:325` `runInitDB`; `:689` `preflightPromote`; `:844` `collectClusterLSNs`; `:899` `dropDeadPeers`; `:985` `publishCurrentLSN`; `:1219` `runBootstrap`; `:1313` post-basebackup `Lifecycle.Start`; `:1370` `handleTopologyChange` (sync-name re-render, early return when no peers)
- `reconciler/quorum_snapshot.go:36` `publishFailoverQuorum`; `:78` skip-publish guard; `:223` `renderQuorumMethod`; `:271` `buildStandbyNames`
- `reconciler/resign.go:120` lease resign
- `replication/promotion_sync.go:80` `PromotionEligibleSync` (`:89` missing snapshot, `:134` silent sync peer)
- `replication/sync_standby_names.go:65` `Render` (`:67` AsyncOnly → `''`, `:71` MinSync ≥ 1)
- `state/transitions.go:84-92` `EventStartup`
- `types.go:924` `AsyncOnly`
- `rebootstrap.go:167` `IdentityFenceKey`; `lifecycle/wipe.go` identity preflight

pgman-proxy (`62292d9`):
- `internal/runtime/start.go:140` route-mesh gate; `:161` `PreCreateClusterKV`; `:174` campaign gate closure; `:272` standby-signal call; `:523` `ensureStandbySignalIfInitialized`; `:616` `peerDSNsForConfig`; `:648` `policyFromConfig`; `:730` `bootEmbeddedNATS`; `:791` `embedded.NewServer`; `:845` `postInitDBHook`; `:873` `appendLines`
- `internal/embedded/bucket.go:98` `PreCreateClusterKV`; `options.go:63` `BuildOptions`; `replicas.go:33` `DeriveReplicas`; `storage.go` (home for WI-C2)
- `internal/config/validate.go:220` size-1 disables routes; `loader.go:256` `parseLines` (env `\n` support)
- `internal/control/doctor_checks.go` (home for WI-C4)
- `tests/integration/compose_main_test.go` (package-wide 3-node `TestMain`); `harness_test.go:248` `execDocker`
