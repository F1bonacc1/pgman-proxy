# Contract — Event & Audit History Stream

**Feature**: `003-pgmctl-cli` · **Phase**: 1 · **Date**: 2026-05-14
**Source clarification**: spec.md § Clarifications 2026-05-14 Q1.

This contract pins the JetStream stream that backs `pgmctl events`,
`pgmctl get audit`, the `/v1/history` query endpoint, and the dump
artifact's events/audit slices.

---

## Stream definition

| Field | Value |
|---|---|
| Name | `PGMAN_PROXY_HISTORY_<CLUSTER_ID>` |
| Subjects | `pgman_proxy.<cluster_id>.history.event.>`<br>`pgman_proxy.<cluster_id>.history.audit.>` |
| Storage | `nats.FileStorage` — durable file-backed; lives under `embedded.JetStreamDir` (002 FR-011) |
| Replicas | Derived per 002 FR-011a (R=1/2/3 by declared cluster size); override path `cluster.replication_factor_override` |
| Retention | `LimitsPolicy` with both `MaxAge = 24h` and `MaxBytes = 256 MiB` (defaults) |
| Discard | `DiscardOld` |
| Ack policy | Producer-side `AckExplicit` |
| Duplicates | Window `0` (no message dedup; we ULID-tag each publish) |
| Max msgs | Unlimited (bytes-and-age governed) |
| Max msg size | `1 MiB` per record |

Configuration knobs:

| Key | Default | Notes |
|---|---|---|
| `history.retention_age` | `24h` | Operator-tunable. `0` disables age-based retention. |
| `history.retention_bytes` | `256 MiB` | Operator-tunable. `0` disables size-based retention. |
| `history.max_msg_size_bytes` | `1048576` (1 MiB) | Hard cap per record. |

Both `history.retention_age` and `history.retention_bytes = 0` is
**REFUSED** at config validation — at least one bound must be set
(infinite retention is a foot-gun on a database-adjacent component).

---

## Subject scheme

Two parallel subject hierarchies under a common stream:

```text
pgman_proxy.<cluster_id>.history.event.state_transition
pgman_proxy.<cluster_id>.history.event.leader_change
pgman_proxy.<cluster_id>.history.event.route_up
pgman_proxy.<cluster_id>.history.event.route_down
pgman_proxy.<cluster_id>.history.event.fence
pgman_proxy.<cluster_id>.history.event.unfence
pgman_proxy.<cluster_id>.history.event.storage_degraded
pgman_proxy.<cluster_id>.history.event.embedded_nats.<lifecycle>
pgman_proxy.<cluster_id>.history.audit.lcm.<operation>
```

`<lifecycle>` mirrors the 002 `embedded_nats.*` log-event names
(`server_started`, `server_ready`, `route_up`, etc.).

`<operation>` mirrors the 001 LCM operation set (`Status`, `Diagnose`,
`Switchover`, `Failover`, `Promote`, `Fence`, `Unfence`,
`UpdateTopology`, `TriggerBackup`, `PrepareUpgrade`, `ExecuteUpgrade`)
plus the new 003 operations (`DoctorRun`, `DoctorFix`, `Restart`).

---

## Record schema

Every record on the stream is a `HistoryEvent` per `data-model.md`:

```jsonc
{
  "apiVersion": "pgman-proxy/v1",
  "kind": "HistoryEvent",
  "id": "01H...XYZ",                          // ULID; resume token
  "time": "2026-05-14T13:42:09.123456789Z",   // RFC3339Nano
  "category": "event" | "audit",
  "type": "state_transition" | "lcm_audit" | "route_up" | ...,
  "cluster_id": "prod-east",
  "node_id": "node-1",
  "details": { /* type-specific */ },
  "trace_id": "...",
  "span_id": "..."
}
```

`details` schemas per `type` live in
`specs/001-active-active-pg-proxy/contracts/observability.md` (already
the wire schema for transitions) and
`specs/002-embedded-nats-cluster/contracts/observability.md` (for the
embedded_nats family). Audit records' `details` matches the audit envelope
from 001 FR-027.

---

## Publisher wiring

Two integration points in pgman-proxy:

1. **Transition / lifecycle events**: every code site that emits a
   structured log line on the `event.*` family ALSO publishes the same
   `HistoryEvent` to the stream. The publish is best-effort relative to
   the local-log emission (the log always wins); a publish failure
   increments `pgman_proxy_history_publish_failures_total` and emits a
   `history.publish_failed` log line but does NOT cascade to caller-
   side errors.

2. **Audit records**: the existing audit pipeline (001 FR-027) gains a
   second sink alongside its current NATS-publish-to-audit-subject path.
   The new sink publishes the same audit payload to
   `pgman_proxy.<cluster_id>.history.audit.lcm.<operation>`. The
   existing `audit_unavailable` fail-closed contract (001 FR-028) is
   triggered when **either** sink is unavailable — both sinks must be
   healthy for mutating operations to be accepted.

Both wirings happen inside the existing `internal/obs` and
`internal/control/audit.go` code paths; no new emission points are
created.

---

## Query semantics

`GET /v1/history` reads from the stream via a fresh
`OrderedConsumer` per request. The handler:

1. Constructs a subject filter from `category` + `type`:
   `pgman_proxy.<cid>.history.event.*` / `pgman_proxy.<cid>.history.audit.*`
   (server expands `type` filters into multi-subject consumers when
   possible; falls back to client-side filter for combinatorial cases).
2. Sets `OptStartTime(now - since)` or `OptStartSeq(cursor)` based on
   the query parameters.
3. Drains messages until `until` is reached, `limit` is hit, or the
   subscription times out (15s server-side).
4. Filters by `node` server-side before emitting.
5. Returns the result envelope with `next_cursor` and `truncated`
   flags.

A `since` older than retention returns `410 Gone` with
`error.code = "history_retention_exceeded"`.

Streaming form (`Accept: text/event-stream`) keeps the consumer open
after backfill and emits live events as they arrive, with the same
15s keepalive cadence as `/v1/watch/*`.

---

## Operability

- **Storage path**: the stream's files live in
  `<JetStreamDir>/jetstream/$G/streams/PGMAN_PROXY_HISTORY_<cluster_id>/`.
- **Disk pressure**: when `JetStreamDir` usage exceeds 90% of its
  declared budget, pgman-proxy emits `embedded_nats.storage_degraded`
  per 002 FR-013. The history stream is the largest expected consumer
  in steady state; operators size the JetStream dir accordingly.
- **Bootstrap**: on cold start, `embedded.EnsureHistoryStream` runs
  after the leadership KV bootstrap (002 FR-011) and uses the same
  retry / wait-for-meta-cluster discipline.
- **Cluster id change**: not supported in v1; the stream name is fixed
  at bootstrap and changing `cluster.id` requires a fresh JetStream
  directory.

---

## Tests

Contract test (`tests/contract/history_query_test.go`):

1. Start a 3-peer fixture cluster.
2. Trigger a known sequence of events (failover, fence, unfence,
   doctor-run).
3. Query `/v1/history?since=10m` — assert every emitted event is
   present, in chronological order, with correct `type` / `node_id`
   / `category`.
4. Query with `cursor=<id>` — assert resumption returns records
   strictly after that id.
5. Query with `since=48h` (older than retention) — assert
   `410 history_retention_exceeded`.
6. Bounce the connected peer; reconnect; assert the stream survives
   peer restart (R≥2) — every event before the bounce is still
   queryable.
7. Bounce ALL peers in sequence; assert the stream still contains
   pre-bounce events (R≥2 on a 3-peer cluster means data survives a
   single-peer outage at any moment).
