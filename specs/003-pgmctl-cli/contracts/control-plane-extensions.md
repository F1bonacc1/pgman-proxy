# Contract — Control-plane Extensions (delta from features 001 / 002)

**Feature**: `003-pgmctl-cli` · **Phase**: 1 · **Date**: 2026-05-14
**Status**: Locked at `/speckit-plan` time. Each endpoint below is a
MINOR-version additive change to the 001 contract (Constitution V).

This contract specifies **only** the five new endpoints. Every existing
001 endpoint (`/v1/status`, `/v1/diagnose`, `/v1/failover`, …) is
preserved unchanged (Principle VII reversibility).

All endpoints inherit:
- Bearer-token authentication (001 FR-024) — including the
  `audit_unavailable` fail-closed contract for mutating ops (001 FR-028).
- TLS rules per 001 FR-033 (TLS required on non-loopback binds; explicit
  ack opt-out preserved).
- Audit emission on the existing
  `pgman_proxy.<cluster_id>.audit.lcm` subject for mutating ops.
- The standard response envelope from 001 LCM contract (`operation`,
  `request_id`, `outcome`, `engine_result`, `error`).

---

## 1. Watch SSE endpoints

### Path scheme

| Path | Topic |
|---|---|
| `GET /v1/watch/status` | Status snapshot pushed on change |
| `GET /v1/watch/transitions` | State-transition log (append-only) |
| `GET /v1/watch/events` | History-stream events (append-only) |
| `GET /v1/watch/node/<node-id>` | Per-node state |

### Request

| Header | Required | Notes |
|---|---|---|
| `Authorization: Bearer <token>` | yes (unless `allow_unauth_reads`) | 001 bearer-auth. |
| `Accept: text/event-stream` | yes | Server returns `406` otherwise. |
| `Last-Event-ID: <ulid>` | optional | Resume from the event after this id; bounded by history retention. |

Query parameters (mirror the history filter set for `events` and
per-node topics):

| Param | Type | Notes |
|---|---|---|
| `since` | duration | Replay events newer than `now-since` before live tail begins. |
| `type` | string | Filter to this event type. |
| `node` | NodeID | Filter to this node. |

### Response

`200 OK` with `Content-Type: text/event-stream; charset=utf-8`.

Per-event frame:

```text
event: <name>
id: <ulid>
data: <one-line JSON of HistoryEvent>

```

`<name>` is one of: `status_update`, `state_transition`,
`cluster_event`, `gap_marker`, `keepalive`.

A `:keepalive\n\n` line (comment frame) is emitted every 15s of
silence.

### Error frames

A terminal SSE error (e.g., the underlying NATS subscription died) is
sent as:

```text
event: error
data: {"code":"...","message":"..."}

```

…and the server closes the stream after flushing.

### Observability

- Metric `pgman_proxy_watch_streams_active{topic}` — gauge.
- Metric `pgman_proxy_watch_events_emitted_total{topic,kind}` — counter.
- Metric `pgman_proxy_watch_gaps_total{topic,reason}` — counter.
- Structured log `watch.stream_started`, `watch.stream_closed` with
  `client_id` (hash of bearer-token actor, never the token).

---

## 2. Doctor endpoints

### `GET /v1/doctor/checks`

Returns the server-published check catalogue (RD-008).

Response:

```jsonc
{
  "apiVersion": "pgman-proxy/v1",
  "kind": "DoctorChecks",
  "checks": [
    {
      "name": "replication.lag-acceptable",
      "description": "Standby replication lag is below threshold",
      "suggested_fix": {
        "name": "kick-replication",
        "blast_radius": "single-resource",
        "description": "Restart the standby's replication client to clear a stalled WAL receiver"
      },
      "evidence_schema": "doctor.evidence.replication-lag/v1"
    },
    // ... 17 more in v1
  ]
}
```

### `POST /v1/doctor/run`

Executes one or all checks.

Request:

```jsonc
{ "check": "replication.lag-acceptable" }  // omit to run all
```

Response:

```jsonc
{
  "apiVersion": "pgman-proxy/v1",
  "kind": "DoctorReport",
  "captured_at": "...",
  "summary": { "pass": 14, "info": 1, "warn": 2, "fail": 1, "unknown": 0 },
  "checks": [ /* CheckResult[] per data-model.md */ ]
}
```

`POST` (not `GET`) so the audit pipeline can capture *who* ran it,
even though it does not mutate state. Audit `operation = "DoctorRun"`,
`outcome = "accepted"` on the standard audit subject. No special audit
record for individual checks — the `DoctorReport` body is logged once.

### `POST /v1/doctor/fix`

Applies a named fix.

Request:

```jsonc
{
  "fix": "kick-replication",
  "args": { "node": "node-3" },
  "request_id": "01H...XYZ"  // ULID from pgmctl
}
```

Response: standard 001 LCM envelope (`outcome`, `engine_result`,
`error`).

Authorization: bearer-auth always; leader-routing per
`SuggestedFix.blast_radius`:
- `single-resource`: forwarded to the engine on the receiving peer;
  not leader-only.
- `cluster-affecting`: leader-only with leader-route per 001 FR-026.
- `advisory`: **never accepted** — returns `412 Precondition Failed`
  with `error.code = "advisory_only"` (pgmctl should not have called).

### Read-only invariant

`Check.Run` MUST NOT call any LCM mutator. A CI test asserts this by
running the full check battery against a fixture cluster, snapshotting
state before and after, and diffing.

---

## 3. Restart endpoint

### `POST /v1/restart`

Restarts either the managed PostgreSQL or the pgman-proxy peer process
on the target node.

Request:

```jsonc
{
  "target_node": "node-2",
  "target": "postgres" | "proxy",
  "request_id": "01H...XYZ"
}
```

Response: standard 001 LCM envelope.

Audit `operation = "Restart"`, with `target` and `target_node` in the
audit record.

#### `target=postgres` semantics

- Leader-only (per pg-manager Manager pattern). Leader-routing per 001
  FR-026.
- The receiving leader calls `Manager.RestartPostgres(ctx)`.
- Engine emits the same state-transition events that `Start` + `Stop`
  emit individually; they flow into the history stream.
- If `target_node` is the current primary, pg-manager's lease watcher
  may trigger a failover during the restart window — caller is informed
  in the prompt text (`cli-commands.md`).

#### `target=proxy` semantics

- **Local-only** (the receiving peer restarts itself, regardless of
  whether it is the leader). Mirrors `Promote` from 001 — the operation
  acts on the peer the request lands on.
- `target_node` MUST match the receiving peer's `node_id`; mismatch
  returns `400 invalid_argument` with `error.code = "wrong_peer"`.
  pgmctl resolves the right peer by reading `Status` first and
  redirecting via `307` (or following `--endpoint` directly).
- Pre-flight check: if `SupervisorPresence == "none"`, the handler
  returns `412 Precondition Failed` with `error.code =
  "supervisor_not_detected"` BEFORE any drain / shutdown begins.
- On success: server emits `200 OK` with `outcome = "accepted"` and the
  audit record, THEN initiates the drain. The HTTP response is sent
  before the listener begins closing; the operator's last contact with
  the doomed peer is the success envelope.
- Drain budget: 001 FR-014 shutdown budget. Lifecycle event:
  `proxy.self_restart_initiated` with `reason = "operator_restart"`.

---

## 4. History query endpoint

### `GET /v1/history`

Queries the JetStream history stream
(`pgman_proxy.<cluster_id>.history.{event,audit}.>`).

Query parameters:

| Param | Type | Default | Notes |
|---|---|---|---|
| `since` | duration | `30m` | Relative to server clock. |
| `until` | RFC3339 | `now` | Upper bound. |
| `type` | string (repeatable) | — | Filter to event types. |
| `category` | enum | both | `event` / `audit` / both. |
| `node` | NodeID (repeatable) | — | Filter to emitting / affected node. |
| `limit` | int | `1000` | Max records. `0` = no limit (use with care). |
| `cursor` | ULID | — | Resume from records strictly after this id. |

Response (single shot):

```jsonc
{
  "apiVersion": "pgman-proxy/v1",
  "kind": "HistoryQueryResult",
  "captured_at": "...",
  "next_cursor": "01H..." | null,
  "truncated": false,
  "events": [ /* HistoryEvent[] */ ]
}
```

Streaming form: same path with `Accept: text/event-stream`; emits the
same `HistoryEvent` payload per SSE frame plus a `keepalive` cadence
identical to `/v1/watch/*`.

### Observability

- Metric `pgman_proxy_history_publish_failures_total` — counter; non-
  zero triggers a yellow `health` rollup.
- Metric `pgman_proxy_history_query_records_total{category}` — counter
  per record returned by `/v1/history`.
- Metric `pgman_proxy_history_query_latency_seconds` — histogram.

---

## 5. Inter-peer fan-out — NATS subject set

This contract lives on the embedded NATS mesh, not on the HTTP listener.
The connected peer publishes a request and aggregates the replies.

| Subject pattern | Slice | Args | Reply data shape |
|---|---|---|---|
| `pgman_proxy.<cluster_id>.fanout.status.<target>` | per-peer Status | `{}` | `001 Status` for that peer |
| `pgman_proxy.<cluster_id>.fanout.config.<target>` | per-peer effective config | `{ redact_level }` | YAML/JSON document |
| `pgman_proxy.<cluster_id>.fanout.nats_mesh.<target>` | per-peer embedded-NATS state | `{}` | `002 embedded_nats.*` block |
| `pgman_proxy.<cluster_id>.fanout.doctor.<target>` | per-peer doctor results | `{ check? }` | `DoctorReport` subset |

`<target>` is `*` for broadcast (NATS request-many) or a specific node
id for unicast. Wire envelope: `FanOutSlice` per `data-model.md`.

Per-sibling error encoding: a sibling that times out or fails locally
sends back a reply with `status: failed` and `error.code` set. The
connected peer aggregates these into the HTTP response's `slices[]`
array (e.g., in the dump manifest or a `/v1/status?include_peers=true`
response).

### Observability

- Metric `pgman_proxy_fanout_requests_total{slice,outcome}` — counter;
  `outcome` is one of `ok` / `partial` / `failed`.
- Metric `pgman_proxy_fanout_latency_seconds{slice}` — histogram.
- Structured log `fanout.request_sent`, `fanout.reply_received`,
  `fanout.sibling_unreachable`.

---

## Audit record additions

The existing audit-record schema from 001 FR-027 carries forward
unchanged. New `operation` values introduced by 003:

| `operation` | Source endpoint | Notes |
|---|---|---|
| `DoctorRun` | `POST /v1/doctor/run` | Body of the report is the audit payload. |
| `DoctorFix` | `POST /v1/doctor/fix` | `target` = the fix's `name`; arg-redacted. |
| `Restart` | `POST /v1/restart` | `target` = `target_node`; `extras = { target: postgres\|proxy }`. |

No change to the audit subject (`pgman_proxy.<cluster_id>.audit.lcm`).

---

## Error codes added by 003

| `error.code` | When | HTTP status |
|---|---|---|
| `supervisor_not_detected` | `target=proxy` with `SupervisorPresence == "none"` and no override | 412 |
| `wrong_peer` | `target=proxy` with `target_node != receiving_peer.node_id` | 400 |
| `advisory_only` | `POST /v1/doctor/fix` called with an advisory-blast-radius fix | 412 |
| `set_config_key_disallowed` | `POST /v1/config/set` with a key outside the hot-reload allow-list | 400 |
| `history_retention_exceeded` | `GET /v1/history` with `since` older than retention | 410 (Gone) |
| `sibling_unreachable` | Fan-out reply timeout / connection refused | (inside the `slices[]` entry; HTTP 200) |

---

## Versioning & deprecation policy

- All five endpoints belong to `/v1/` and are additive.
- Field renames in request / response bodies are MINOR-version events.
- Removing a field, changing its type, or repurposing a value is a
  MAJOR-version event (Constitution V).
- pgmctl pins its client to `pgman-proxy/v1`; mismatch handling per
  cli-commands.md § version.
