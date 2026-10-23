# Contract: Observability schema (delta from features 001-004)

**Feature**: `005-scale-out-1-to-3`
**Status**: Locked at `/speckit-plan` time. Renames are MINOR-version events (Constitution V).

Common fields are inherited from 001: `level`, `time`, `msg`, `cluster_id`, `node_id`. Everything in the 001-004 schemas is unchanged.

## Structured-log fields

### `manager started` (existing event, new field)

| Field | Type | Description |
|---|---|---|
| `replication_policy` | string | `async` (one configured peer) or `quorum(min_sync=<N>)` (two or more peers). FR-004. |

## Structured-log events

### `embedded_nats.store_archived` (Warn)

Emitted at boot, before the embedded server starts, when the store layout found on disk does not match the mode the server is about to run in (FR-009, FR-010).

| Field | Type | Description |
|---|---|---|
| `from` | string | Absolute path of the store that was moved (`<jetstream_dir>/jetstream`) |
| `to` | string | Absolute path it was moved to (`<jetstream_dir>/jetstream.<layout>-<UTC ts>[-n]`) |
| `actual_layout` | string | `standalone` or `clustered` |
| `desired_layout` | string | `clustered` or `standalone` |
| `reason` | string | `layout_mismatch` |

This event lands in logs only. The history stream of the archived era stays inside the archived store, and the new history stream does not exist yet at this point in boot.

### `postgres.managed_block_applied` (Info)

Emitted when a managed configuration block is inserted, updated or removed (FR-013..FR-015). It is not emitted when the block is unchanged.

| Field | Type | Description |
|---|---|---|
| `file` | string | `pg_hba.conf` or `postgresql.conf` (base name) |
| `block` | string | `postgres.hba_extras` or `postgres.conf_extras` |
| `action` | string | `inserted`, `updated`, `removed` |
| `lines` | int | Number of configured lines now in the block (0 when removed) |
| `phase` | string | `post_init` or `boot` |
| `reloaded` | bool | `true` iff a configuration reload was issued because the postmaster was running |

### `identity.mismatch` / `identity.mismatch_cleared` (pass-through from pg-manager v0.7.0; subjects `pgmanager.<cluster>.identity.mismatch[_cleared]`)

Forwarded through the existing coordination-event subscription into logs and the history stream. pgman-proxy adds no fields. The upstream payload (local and leader system identifiers, leader node id) is documented in [upstream-015.md](upstream-015.md).

### `timeline.diverged` / `timeline.divergence_cleared` / `timeline.fork_conflict` (pass-through from pg-manager v0.7.3; subjects `pgmanager.<cluster>.timeline.*`; pgman-proxy issue #12)

Forwarded through the coordination-event subscription (family `timeline`) into logs, `pgman_proxy_coordination_events_total` and the history stream. pgman-proxy adds no fields. `timeline.diverged` fires once when a standby's WAL is first seen past the end of its timeline in the cluster history (payload `local_timeline`, `local_end_lsn`, `cluster_timeline`, `switch_lsn`, `evidence` = `source` or `fork_record`, `source_node_id`); `timeline.divergence_cleared` when that ends; `timeline.fork_conflict` on a primary whose own history disagrees with the fork record already published for its timeline.

### Upstream log lines asserted by tests (not emitted by pgman-proxy)

- pg-manager 015 deference (`campaign: empty-node deference armed|released`, fields `peers_with_data`, `peers_unreported`) and initdb refusal (`initdb: refused`, `reason`). `tests/scaleout` S6 and S1 assert them; exact strings are pinned in [upstream-015.md](upstream-015.md).

## Prometheus metrics

| Name | Type | Labels | Description |
|---|---|---|---|
| `pgman_proxy_cluster_identity_mismatch` | Gauge | (implicit `cluster_id`, `node_id`) | 1 iff pg-manager reports that this node's database identity differs from the current leader's; 0 otherwise. Updated by the observability gauge loop (FR-016, SC-008: visible within 30 s). |
| `pgman_proxy_timeline_diverged` | Gauge | (implicit `cluster_id`, `node_id`) | 1 iff pg-manager reports that this standby's WAL runs past the end of its timeline in the cluster history (`Status.TimelineDivergence` non-nil), so it cannot stream; 0 otherwise. Same gauge loop and Status read as the identity gauge (issue #12). |

## Doctor checks

### `cluster.identity`

| Property | Value |
|---|---|
| Description | This node's database identity matches the current leader's. |
| Evidence schema | `doctor.evidence.cluster-identity/v1`: `{mismatch}` plus, on mismatch, `{local_system_identifier, leader_system_identifier, leader_node_id}` |
| Severity | `fail` while pg-manager reports a mismatch (`Status.IdentityMismatch` non-nil); `pass` otherwise. pg-manager reports nothing when identities match or either is unknown, so there is no separate `warn` case. `unknown` when `Status` errors |
| Suggested fix | Advisory only (`BlastAdvisory`): "Two unrelated databases exist in this cluster. Do not wipe either before identifying which one holds the acknowledged writes; see docs/scale-out-1-to-3/runbook.md." |
| Read-only | Yes: calls `Engine.Status` only (enforced by the existing read-only test) |

### `replication.timeline-consistent` (pgman-proxy issue #12)

| Property | Value |
|---|---|
| Description | No node's WAL runs past the end of its timeline in the cluster history. |
| Scope | Cluster-wide: the doctor's engine asks every peer through the SliceStatus fan-out, whose reply carries the peer's own `timeline_divergence` (`local_timeline`, `local_end_lsn`, `cluster_timeline`, `switch_lsn`, `evidence`, `source_node_id`, `since`; omitted when none). Without an aggregator, only the serving node. |
| Evidence schema | `doctor.evidence.timeline-divergence/v1`: `{checked, diverged[]}` plus `unanswered[]` when peers did not reply. Each `diverged` entry: `{node_id, local_timeline, local_end_lsn, cluster_timeline, switch_lsn, evidence, source_node_id, since}`, LSNs as `X/Y` |
| Severity | `fail` while any answering node reports a divergence; `pass` otherwise, also when peers did not answer (`nodes.all-reachable` covers them; the message names them); `unknown` when the fan-out or `Status` errors |
| Suggested fix | Advisory only (`BlastAdvisory`, `reclone-diverged-node`): with auto-rebootstrap the node re-clones itself once its clone source confirms the divergence; otherwise stop it, move its data directory aside and start it. Its unreplicated WAL is discarded. |
| Read-only | Yes: `Engine.Status` and the read-only status fan-out only |
