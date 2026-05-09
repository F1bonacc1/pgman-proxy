# Data Model — Embedded NATS Cluster

**Phase**: 1 (design)
**Feature**: `002-embedded-nats-cluster`
**Date**: 2026-05-09

This document captures the configuration entities, runtime entities, and
state transitions introduced by feature 002. Every entity is materialised
as either a Go struct in `internal/embedded/` or a section in the proxy's
configuration file. The model is intentionally small — embedded NATS adds
state, not concepts — and tracks the four clarifications from
`/speckit-clarify` plus the **RD-001a amendment applied during
`/speckit-implement` Phase 2** (per-peer NKey-on-routes was invalidated
by upstream NATS protocol; replaced with shared cluster username/password
+ per-peer audit identity from server-name. See `research.md` RD-001a).

## Configuration entities (operator-supplied)

### ClusterConfig

Replaces `NATSConfig` from feature 001. Owned by `internal/config`.

| Field | Type | Source | Validation | Hot-reload? |
|---|---|---|---|---|
| `cluster_id` | `string` | env `PGMAN_PROXY_CLUSTER_ID` / flag / file | non-empty; must match across peers | startup-only |
| `cluster_name` | `string` | config | non-empty; must match across peers (FR-009 cluster-name guard) | startup-only |
| `node_id` | `string` | env / flag / file | non-empty; unique per peer; used as embedded NATS server name | startup-only |
| `declared_size` | `int` | config | ≥ 1; drives RD-004 derivation (FR-011a) | startup-only |
| `client_listen` | `Endpoint` | config | default `127.0.0.1:4222`; must be loopback unless explicit override (FR-018) | startup-only |
| `routes_listen` | `Endpoint` | config | default `0.0.0.0:6222` in HA mode, disabled in 1-peer (FR-019); refuse if HA + empty | startup-only |
| `peers` | `[]string` | config + SIGHUP | host:port list of sibling routes (FR-004); self-loops warned + excluded (FR-020) | **HOT-RELOAD (FR-014a)** |
| `tls` | `TLSConfig` | config | required on non-loopback `routes_listen` unless `plaintext_explicit_ack=true` (FR-010b) | startup-only |
| `username` | `string` | config | non-empty; non-secret cluster identifier (RD-001a) | startup-only |
| `password` | `SecretRef` | config + SIGHUP | env / file / secret-manager (FR-010); ≥ 16 bytes after resolution; never plaintext-config-resident | **HOT-RELOAD (FR-014a)** |
| `jetstream_dir` | `string` | config | absolute path; required for declared_size ≥ 2; in-memory permitted only for declared_size = 1 | startup-only |
| `replication_factor_override` | `*int` | config | when set: emits `cluster.replicas_overridden` warning at startup (FR-011a) | startup-only |
| `connect_timeout` | `duration` | config | default 5 s; > 0 | startup-only |
| `reconnect_wait` | `duration` | config | default 2 s; > 0 | startup-only |

**Removed from 001**: `NATSConfig.URL`, `NATSConfig.CredsFile`,
`NATSConfig.TokenEnv`. Presence of any of these in v2 config is a
fail-closed validation error (FR-002, SC-009).

### Endpoint

| Field | Type | Validation |
|---|---|---|
| `host` | `string` | non-empty; resolvable hostname or IP literal |
| `port` | `int` | 1..65535 |
| `is_loopback` | `bool` (derived) | true iff `host` is `127.0.0.1`, `::1`, or a documented Unix-socket path |

### TLSConfig

| Field | Type | Validation |
|---|---|---|
| `cert_file` | `SecretRef` | required when `routes_listen.is_loopback == false` and `plaintext_explicit_ack == false` |
| `key_file` | `SecretRef` | required when `cert_file` is set |
| `ca_file` | `SecretRef` | required when `cert_file` is set; verifies sibling-presented certs (RD-007) |
| `plaintext_explicit_ack` | `bool` | default `false`; true → audit-logged at every startup |

### SecretRef

A discriminated union of `{env: "ENV_VAR"}`, `{file: "/path/to/secret"}`,
or `{secret_manager: "scheme://path"}`. Identical surface to feature 001
FR-017; reused verbatim for the cluster password (RD-001a) and TLS
material.

## Runtime entities (in-process)

### EmbeddedServer

Owned by `internal/embedded/server.go`. One instance per proxy process.

| Field | Type | Notes |
|---|---|---|
| `name` | `string` | Equals `ClusterConfig.node_id`; presented to siblings on cluster routes |
| `opts` | `*server.Options` | Built once at startup from `ClusterConfig` (RD-001) |
| `srv` | `*server.Server` | Result of `server.NewServer(opts)` |
| `ready` | `<-chan struct{}` | Closed when `srv.ReadyForConnections(timeout)` returns true |
| `js_storage_dir` | `string` | From `ClusterConfig.jetstream_dir`; empty in 1-peer in-memory mode |
| `replicas` | `int` | Derived once at startup via RD-004; immutable for the process lifetime |

Lifecycle states: `init → starting → ready → degraded? → stopping → stopped`.
The `degraded` state is entered on `storage_degraded` events and is the
trigger for self-fencing per Constitution III.

### ReloadDiff

Owned by `internal/embedded/reload.go`. Computed on each SIGHUP.

| Field | Type | Notes |
|---|---|---|
| `routes_added` | `[]string` | Peer URLs newly present in `ClusterConfig.peers` |
| `routes_removed` | `[]string` | Peer URLs newly absent in `ClusterConfig.peers` |
| `password_rotated` | `bool` | True iff the resolved `ClusterConfig.password` SecretRef value changed since last reload (RD-001a) |
| `password_old_prefix` | `string?` | 8-character base32 prefix of the password value being retired (only present when `password_rotated == true`) |
| `password_new_prefix` | `string?` | 8-character base32 prefix of the new password value (only present when `password_rotated == true`) |
| `skipped_keys` | `[]string` | Config keys the operator changed that are NOT in the hot-reload allow-list (FR-014a) — emitted in the structured warning |

The diff is the **input** to `srv.ReloadOptions(newOpts)`. Skipped keys
do not trigger reload; they are loudly logged and the in-memory config
does not advance them.

### LifecycleEvent

Owned by `internal/embedded/`. Emitted via the existing `internal/obs`
logger. Stable schema (Constitution V):

| Field | Type | Notes |
|---|---|---|
| `event` | `string` (enum) | One of: `server_started`, `server_ready`, `route_up`, `route_down`, `server_stopped`, `storage_degraded`, `reload_applied` |
| `node_id` | `string` | This peer's ID |
| `cluster_id` | `string` | This cluster's ID |
| `peer_node_id` | `string?` | Set on `route_up` / `route_down` |
| `password_prefix` | `string?` | First 8 chars of the cluster password's base32 encoding, identifying which credential the route was accepted under (FR-010a / RD-001a redaction); set on `route_up` |
| `error` | `string?` | Set on `server_stopped` / `storage_degraded` failure paths |
| `reload_diff` | `ReloadDiff?` | Set on `reload_applied` |
| `occurred_at` | `time.Time` | RFC3339 ms |
| `trace_id` | `string?` | W3C Trace Context propagation when available |

## State transitions

### Process lifecycle (single peer)

```
                    pgman-proxy boots
                            │
                            ▼
                   ┌─────────────────┐
                   │  config loaded  │  ← validation gates: legacy nats.url? → fail-closed
                   └────────┬────────┘
                            │
                            ▼
                   ┌─────────────────┐
                   │ embedded server │  ← server.NewServer(opts); go s.Start()
                   │   starting      │
                   └────────┬────────┘
                            │  ReadyForConnections(timeout)
                            ▼
                   ┌─────────────────┐
                   │ embedded server │  ← emit "server_ready"
                   │     ready       │
                   └────────┬────────┘
                            │  pg-manager adapters dial loopback NATS
                            ▼
                   ┌─────────────────┐
                   │  proxy running  │  ← data-plane listener accepting clients
                   └────────┬────────┘
                            │
            ┌───────────────┼───────────────┐
            ▼               ▼               ▼
   SIGHUP received    storage degraded   SIGTERM/SIGINT
            │               │               │
            ▼               ▼               ▼
   compute ReloadDiff   self-fence      drain data-plane
   apply via Reload     (per Const. III) │
   emit reload_applied                   ▼
                                  s.Shutdown()
                                  emit server_stopped
                                  exit 0
```

### Cluster mesh formation (3-peer)

NATS handles route convergence; the proxy only emits observability:

1. T0: All three peers complete `server_ready` independently.
2. T0+ε: Each peer's embedded server dials its `peers` list. Each
   sibling's inbound credential check (shared username + password,
   RD-001a) passes (or fails — emit `cluster_route.auth_failed`). On
   success, both ends emit `route_up` with `peer_node_id` (from sibling
   server-name) and `password_prefix`.
3. T0+small-ms: NATS gossip extends the mesh — even if peer A only
   listed B, NATS will discover C via B and open a third route.
4. T0+δ: pg-manager's `Leadership` adapter elects exactly one leader.
   `pg-manager`-owned event: not emitted by this feature.

### Cluster credential rotation (FR-010a, amended by RD-001a) — single step, no peer restart

```
step 1: every peer's password SecretRef target updated to the new value
       SIGHUP every peer (FR-014a) → reload_applied{password_rotated=true};
       NATS server's ReloadOptions accepts cluster.Authorization changes
       and re-handshakes existing routes against the new credential —
       cluster remains quorate throughout. Rotation complete.
```

The original three-step procedure (peer-NKey adds-rolls-removes) was
collapsed to one step by the RD-001a amendment because the shared-
credential model has no "intermediate trust window" — every peer's
credential is identical, so a single coordinated swap suffices.

## Validation rules summary (cross-references)

| Rule | Source FR(s) | Enforced where |
|---|---|---|
| Legacy `nats.url` present → fail-closed | FR-002, SC-009 | `internal/config/validate.go` |
| Multi-peer + empty `peers` → fail-closed | FR-008 | `internal/config/validate.go` |
| Non-loopback `routes_listen` + no `username`/`password` → fail-closed | FR-009 (RD-001a) | `internal/config/validate.go` |
| Non-loopback `routes_listen` + no TLS + no plaintext_explicit_ack → fail-closed | FR-010b | `internal/config/validate.go` |
| `jetstream_dir` empty + declared_size ≥ 2 → fail-closed | FR-011 | `internal/config/validate.go` |
| `jetstream_dir` unwritable → fail-closed | spec edge case | `internal/embedded/server.go` startup |
| Self-loop in `peers` → warn + exclude | FR-020 | `internal/embedded/options.go` |
| SIGHUP changes a non-allow-listed key → ignore + warn | FR-014a | `internal/embedded/reload.go` |
| Replication-factor derivation table | FR-011a, RD-004 | `internal/embedded/replicas.go` |
| Override of derivation requires explicit opt-in + audit log | FR-011a | `internal/embedded/replicas.go` |
| Cluster password sourced via SecretRef only (≥ 16 bytes after resolution) | FR-010 (RD-001a) | `internal/embedded/credentials.go` + `internal/config/validate.go` |
| Cluster username MAY be in plaintext config (non-secret) | FR-010 (RD-001a) | `internal/config/validate.go` |
