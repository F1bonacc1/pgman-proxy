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
| `phase` | string | `post_initdb` or `boot` |
| `reloaded` | bool | `true` iff a configuration reload was issued because the postmaster was running |

### `cluster.identity_mismatch` (Error, pass-through from pg-manager ≥ v0.7.0)

Forwarded through the existing coordination-event subscription into logs and the history stream. pgman-proxy adds no fields. The upstream payload (local and leader system identifiers, leader node id) is documented in [upstream-015.md](upstream-015.md).

### Upstream log lines asserted by tests (not emitted by pgman-proxy)

- pg-manager 015 deference (`campaign: empty-node deference armed|released`, fields `peers_with_data`, `peers_unreported`) and initdb refusal (`initdb: refused`, `reason`). `tests/scaleout` S6 and S1 assert them; exact strings are pinned in [upstream-015.md](upstream-015.md).

## Prometheus metrics

| Name | Type | Labels | Description |
|---|---|---|---|
| `pgman_proxy_cluster_identity_mismatch` | Gauge | (implicit `cluster_id`, `node_id`) | 1 iff pg-manager reports that this node's database identity differs from the current leader's; 0 otherwise. Updated by the observability gauge loop (FR-016, SC-008: visible within 30 s). |

## Doctor checks

### `cluster.identity`

| Property | Value |
|---|---|
| Description | This node's database identity matches the current leader's. |
| Evidence schema | `doctor.evidence.cluster-identity/v1`: `{local_system_identifier, leader_system_identifier, leader_node_id, mismatch}` |
| Severity | `fail` on mismatch; `pass` when identities match; `warn` when either identity is unknown (for example, a node still empty or bootstrapping) |
| Suggested fix | Advisory only (`BlastAdvisory`): "Two unrelated databases exist in this cluster. Do not wipe either before identifying which one holds the acknowledged writes; see docs/scale-out-1-to-3/runbook.md." |
| Read-only | Yes: calls `Engine.Status` only (enforced by the existing read-only test) |
