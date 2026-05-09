# Contract: Observability Surface

**Feature**: 001-active-active-pg-proxy · **Phase**: 1 · **Date**: 2026-05-09

This contract pins the **stable** observability surface. Field renames,
metric renames, or topic-shape changes are **MINOR-version events**
(Constitution V).

---

## Structured logs

JSON via `log/slog`. Every record carries the following fields:

| Field           | Type      | Always present | Notes                                                |
|-----------------|-----------|----------------|------------------------------------------------------|
| `time`          | RFC3339   | yes            | UTC                                                  |
| `level`         | string    | yes            | `DEBUG` \| `INFO` \| `WARN` \| `ERROR`               |
| `msg`           | string    | yes            | short, lower-case, no trailing punctuation           |
| `cluster_id`    | string    | yes            | from config                                          |
| `node_id`       | string    | yes            | from config                                          |
| `component`     | string    | yes            | one of: `config`, `cluster`, `runtime`, `obs`, `proxy`, `manager` |
| `trace_id`      | string    | when in scope  | W3C trace id                                         |
| `span_id`       | string    | when in scope  | W3C span id                                          |
| `error`         | string    | on errors      | redacted; never contains secrets                     |

### Required event names

Each named event MUST appear at least once over the listed lifecycle
phase. Tests assert presence by `msg` value.

| `msg`                          | Level | Phase                               | Extra fields                                  |
|--------------------------------|-------|-------------------------------------|-----------------------------------------------|
| `config loaded`                | INFO  | startup                             | `source` (`flags`/`env`/`yaml`/`default`); secrets redacted |
| `nats connected`               | INFO  | startup                             | `url`                                         |
| `nats reconnected`             | WARN  | runtime                             | `url`, `last_disconnect_age_ms`               |
| `nats disconnected`            | WARN  | runtime                             | `url`, `reason`                               |
| `proxy listener bound`         | INFO  | startup                             | `addr`                                        |
| `proxy listener closed`        | INFO  | shutdown                            | `addr`                                        |
| `manager started`              | INFO  | startup                             | `singleton_claim_attempts`                    |
| `manager start failed`         | ERROR | startup                             | `error`                                       |
| `leader changed`               | INFO  | runtime                             | `old`, `new`                                  |
| `lease renewal failed`         | WARN  | runtime                             | `attempt`, `error`                            |
| `coordination event`           | INFO  | runtime                             | `subject`, `payload_size_bytes`; `payload` only when `log_level=debug` |
| `control_plane started`        | INFO  | startup                             | `addr`, `auth_source` (`env_var`/`file`)      |
| `control_plane request`        | INFO  | runtime                             | `operation`, `outcome`, `target`, `actor`, `request_id`, `total_latency_ms`, `engine_latency_ms`, `error_code` (when applicable) |
| `lcm audit emit failed`        | ERROR | runtime                             | `sink` (`slog`/`nats`), `error`; triggers fail-closed mutating-op rejection (FR-028) |
| `shutdown signal`              | INFO  | shutdown                            | `signal`, `drain_budget_ms`                   |
| `shutdown drain timeout`       | WARN  | shutdown                            | `in_flight`                                   |
| `shutdown complete`            | INFO  | shutdown                            | `duration_ms`                                 |

`coordination event` MUST NOT log message payload by default; debug-
level logging may include payloads but the field schema for those
payloads is owned by `pg-manager`, not by this repo.

---

## Prometheus metrics

All metrics carry the labels `cluster_id` and `node_id` unless noted.
Names are stable; renames are MINOR-version events.

### Connection metrics

| Metric                                | Type      | Labels                | Notes                                |
|---------------------------------------|-----------|-----------------------|--------------------------------------|
| `pgman_proxy_connections_open`        | Gauge     | —                     | current open client conns            |
| `pgman_proxy_connections_accepted_total` | Counter | —                    | lifetime accepted                    |
| `pgman_proxy_connections_closed_total` | Counter   | `reason`              | `client_close` / `server_close` / `switch_hard_close` / `drain` / `error` |
| `pgman_proxy_connection_duration_seconds` | Histogram | —                  | client connection lifetime           |

### Query / latency metrics

| Metric                                | Type      | Labels                | Notes                                |
|---------------------------------------|-----------|-----------------------|--------------------------------------|
| `pgman_proxy_query_latency_seconds`   | Histogram | —                     | round-trip from client byte to client byte |
| `pgman_proxy_errors_total`            | Counter   | `sqlstate`, `severity`| as observed in PG `ErrorResponse` messages forwarded back to client |

### Coordination metrics

| Metric                                | Type      | Labels                | Notes                                |
|---------------------------------------|-----------|-----------------------|--------------------------------------|
| `pgman_proxy_leadership_state`        | Gauge     | `state`               | `state` ∈ `leader`/`follower`/`none`; gauge=1 only on the active state |
| `pgman_proxy_leader_changes_total`    | Counter   | —                     |                                      |
| `pgman_proxy_lease_renewal_failures_total` | Counter | —                  | non-zero implies stale-lease risk    |
| `pgman_proxy_nats_round_trip_seconds` | Histogram | —                     | server-side ping/pong RTT            |
| `pgman_proxy_nats_disconnects_total`  | Counter   | `reason`              |                                      |
| `pgman_proxy_coordination_events_total` | Counter | `subject`, `outcome`  | `outcome` ∈ `delivered`/`refused`/`failed` |

### LCM control-plane metrics (cross-ref `lcm.md`)

| Metric                                              | Type      | Labels                          | Notes                                |
|-----------------------------------------------------|-----------|---------------------------------|--------------------------------------|
| `pgman_proxy_lcm_requests_total`                    | Counter   | `operation`, `outcome`          | `outcome` ∈ `accepted`/`rejected`/`failed` |
| `pgman_proxy_lcm_request_latency_seconds`           | Histogram | `operation`, `outcome`          | total latency (request received → response written) |
| `pgman_proxy_lcm_engine_latency_seconds`            | Histogram | `operation`, `outcome`          | latency inside the `pg-manager` engine call |
| `pgman_proxy_lcm_in_flight`                         | Gauge     | `operation`                     | gauge of currently-running LCM requests |
| `pgman_proxy_lcm_audit_emit_failures_total`         | Counter   | `sink`                          | `sink` ∈ `slog`/`nats`; non-zero triggers fail-closed mutating-op rejection (FR-028) |
| `pgman_proxy_lcm_leader_route_total`                | Counter   | `operation`, `disposition`      | `disposition` ∈ `local_executed`/`forwarded`/`redirected` |

### Process metrics

Standard `process_*` and `go_*` collectors via
`prometheus.NewProcessCollector` and `prometheus.NewGoCollector`. No
custom additions.

### Histogram buckets

- Latency histograms use:
  `[0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10]` seconds.
- Connection duration uses:
  `[0.1, 1, 10, 60, 600, 3600]` seconds.

---

## Health & readiness HTTP

Served on `obs.health_addr` (defaults to the same listener as metrics).

| Path        | Method | Behaviour                                                                             |
|-------------|--------|---------------------------------------------------------------------------------------|
| `/healthz`  | GET    | `200 OK` once process init is past arg parsing; `500` only on internal panic         |
| `/readyz`   | GET    | `200 OK` only when ALL of: NATS connection up, listener accepting, manager past singleton claim; `503` otherwise with `Retry-After: 1` |
| `/metrics`  | GET    | Prometheus text/exposition format                                                     |
| `/debug/pprof/*` | GET | served only when `obs.log_level == debug`                                            |

---

## Trace context

- Inbound HTTP endpoints (`/healthz`, `/readyz`, `/metrics`): accept
  W3C `traceparent` header; if not present, generate a new trace.
- NATS messages emitted by this repository (we don't emit any in v1 —
  pg-manager owns the publisher side) MUST attach `traceparent` to
  the NATS header when present.
- Inbound coordination events: read `traceparent` from the NATS header
  if present and propagate it to the `coordination event` log line.

---

## NATS topics — consumed

Owned by `pg-manager`; this repo subscribes only.

| Topic glob                                              | Action                                              |
|---------------------------------------------------------|-----------------------------------------------------|
| `pgmanager.<cluster_id>.auto_rebootstrap.>`             | observe → metric + log + trace                      |
| `pgmanager.<cluster_id>.auto_demote.>`                  | observe → metric + log + trace                      |
| `pgmanager.<cluster_id>.divergence.>`                   | observe → metric + log + trace                      |
| `pgmanager.<cluster_id>.conninfo.reconciled`            | observe → metric + log + trace                      |

`pgman-proxy` MUST NOT publish on these subjects (Constitution IV: thin
scaffold; the publisher is `pg-manager`).

## NATS topics — published

Owned by this repository (added by the LCM amendment).

| Subject                                                | Direction         | Schema owner | Notes                                              |
|--------------------------------------------------------|-------------------|--------------|----------------------------------------------------|
| `pgman_proxy.<cluster_id>.audit.lcm`                   | publish (LCM audit) | this repo  | Dual-sink audit record (FR-027); see `lcm.md` § Audit |
| `pgman_proxy.<cluster_id>.lcm.request.<operation>`     | request (NATS req/reply) | this repo | Used in `forward` leader-routing mode (FR-026); request body is the LCM operation payload, reply is the engine result envelope |
| `pgman_proxy.<cluster_id>.lcm.reply.<request_id>`      | reply             | this repo    | NATS reply inbox for the matching request          |

The `pgman_proxy.*` namespace is reserved for this repository to keep
LCM and audit traffic distinct from `pg-manager`'s own coordination
subjects.
