# Contract — Observability schema (delta from feature 001)

**Feature**: `002-embedded-nats-cluster`
**Phase**: 1
**Status**: Locked at `/speckit-plan` time. Field renames are MINOR-version
events per Constitution V.

This contract specifies the new structured-log events and Prometheus
metrics introduced by feature 002. Everything in 001's observability
schema (`internal/obs/...`) is preserved unchanged (FR-016).

## Structured-log events

All events flow through the existing `internal/obs/logger.go` `slog` JSON
sink. Common fields (inherited from 001's schema):

- `level`, `time`, `msg`, `cluster_id`, `node_id`, `trace_id?`,
  `span_id?`.

### `embedded_nats.server_started`

| Field | Type | Description |
|---|---|---|
| `client_listen_addr` | string | Address the embedded NATS client listener bound to |
| `routes_listen_addr` | string | Address the embedded NATS cluster-routes listener bound to (or empty if disabled) |
| `jetstream_dir` | string | Storage path; empty = in-memory |
| `replicas` | int | Result of RD-004 derivation |
| `replicas_overridden` | bool | `true` iff `cluster.replication_factor_override` is set |
| `tls_enabled` | bool | True iff routes listener uses TLS |
| `plaintext_explicit_ack` | bool | True iff non-loopback plaintext was opted into |
| `declared_size` | int | From config |

### `embedded_nats.server_ready`

| Field | Type | Description |
|---|---|---|
| `wait_ms` | int | Time from `server_started` to `ReadyForConnections` returning true |

### `embedded_nats.route_up`

| Field | Type | Description |
|---|---|---|
| `peer_route_url` | string | URL we connected to or accepted from |
| `peer_node_id` | string | Sibling's `cluster.node_id` (from NATS server-name advertisement; the per-peer audit-identity surface per RD-001a) |
| `password_prefix` | string | First 8 chars of the cluster password's base32 encoding, identifying which credential the route was accepted under (FR-010a redaction; RD-001a) |
| `direction` | string | `"outbound"` or `"inbound"` |

### `embedded_nats.route_down`

| Field | Type | Description |
|---|---|---|
| `peer_route_url` | string | URL of the lost route |
| `peer_node_id` | string | Sibling's node ID, if known |
| `reason` | string | One of: `peer_disconnect`, `auth_failed`, `tls_error`, `local_shutdown`, `unknown` |
| `error` | string? | Optional underlying error message (already redacted of secrets) |

### `embedded_nats.server_stopped`

| Field | Type | Description |
|---|---|---|
| `reason` | string | One of: `clean_shutdown`, `storage_degraded`, `panic`, `startup_timeout` |
| `uptime_ms` | int | Wall-clock uptime |
| `error` | string? | Optional |

### `embedded_nats.storage_degraded`

| Field | Type | Description |
|---|---|---|
| `kind` | string | One of: `disk_full`, `path_unwritable`, `js_corruption`, `quota_exceeded` |
| `path` | string | The affected JetStream storage path |
| `error` | string | Underlying error (redacted) |

Emission of this event triggers the peer's self-fencing (Constitution III).

### `embedded_nats.reload_applied`

| Field | Type | Description |
|---|---|---|
| `routes_added` | []string | Peer URLs added to the routes list |
| `routes_removed` | []string | Peer URLs removed from the routes list |
| `password_rotated` | bool | True iff the resolved cluster password changed during this reload (RD-001a) |
| `password_old_prefix` | string? | 8-character base32 prefix of the retired password — present only when `password_rotated == true` |
| `password_new_prefix` | string? | 8-character base32 prefix of the new password — present only when `password_rotated == true` |
| `skipped_keys` | []string | Config keys the operator changed that are NOT in the hot-reload allow-list |
| `skipped_reason` | string? | Set when `skipped_keys` is non-empty: short explanatory string |

### `cluster_route.auth_failed`

| Field | Type | Description |
|---|---|---|
| `peer_addr` | string | Remote address that failed auth |
| `peer_server_name` | string? | Server-name the remote claimed (its `cluster.node_id`); empty if the route was rejected before INFO exchange |
| `kind` | string | One of: `invalid_credential`, `cluster_name_mismatch`, `protocol_error` (RD-001a — wire-level cred is shared username/password, not NKey) |

## Prometheus metrics

All metrics live in the `pgman_proxy_embedded_nats_*` namespace. Existing
metrics from feature 001 are unchanged. Labels are minimal — no
high-cardinality fields.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `pgman_proxy_embedded_nats_up` | Gauge | — | 1 iff server is in `ready` state, 0 otherwise |
| `pgman_proxy_embedded_nats_routes_meshed` | Gauge | — | Currently-meshed peer count (excluding self) |
| `pgman_proxy_embedded_nats_replicas_factor` | Gauge | `overridden` ("true"/"false") | The R value in effect |
| `pgman_proxy_embedded_nats_storage_bytes` | Gauge | — | Bytes used by the JetStream storage dir, or 0 if in-memory |
| `pgman_proxy_embedded_nats_storage_degraded` | Gauge | `kind` | 1 iff in degraded state, with kind label |
| `pgman_proxy_embedded_nats_lifecycle_events_total` | Counter | `event` (one of `server_started`,`server_ready`,`route_up`,`route_down`,`server_stopped`,`storage_degraded`,`reload_applied`) | Tally of lifecycle events |
| `pgman_proxy_embedded_nats_route_auth_failures_total` | Counter | `kind` (one of `invalid_credential`,`cluster_name_mismatch`,`protocol_error`) | Count of inbound route auth rejections (RD-001a) |
| `pgman_proxy_embedded_nats_sighup_reload_outcomes_total` | Counter | `result` (one of `applied`,`partial_skipped`,`error`) | Tally of SIGHUP reload outcomes |

Cardinality budget: the new label values are bounded by the small enums
above; no peer-id, no addr, no key prefix appears in label space.

## Status response (control-plane)

001's `Status` LCM operation gains a sub-block. Field schema is stable:
renames are MINOR-version events.

```jsonc
{
  "cluster": {
    // ... 001 fields unchanged ...
    "embedded_nats": {
      "server_name": "peer-a",
      "ready": true,
      "client_listen_addr": "127.0.0.1:4222",
      "routes_listen_addr": "0.0.0.0:6222",
      "tls_enabled": true,
      "routes_meshed": 2,
      "replicas_factor": 3,
      "replicas_overridden": false,
      "jetstream_storage_bytes": 12345678,
      "storage_degraded": null,
      "last_route_up_at":   "2026-05-09T15:32:11.000Z",
      "last_route_down_at": null,
      "last_reload_at":     null
    }
  }
}
```

## Audit log

The existing `pgman_proxy.<cluster_id>.audit.lcm` audit subject (from
001 FR-027) is **unchanged**. Embedded-NATS lifecycle events are NOT
audit records — they are operational events on the structured-log sink
only. The audit log gains nothing in feature 002.

The exception is route-accept events: when the embedded server accepts
an inbound route, the `route_up` event includes `peer_node_id` (sibling
identity) and `password_prefix` (which credential was accepted) per
FR-010a / RD-001a so a credential rotation is forensically
reconstructable from the structured log alone.
