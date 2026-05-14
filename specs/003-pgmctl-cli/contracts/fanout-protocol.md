# Contract — Inter-Peer Fan-Out Protocol

**Feature**: `003-pgmctl-cli` · **Phase**: 1 · **Date**: 2026-05-14
**Source clarification**: spec.md § Clarifications 2026-05-14 Q3.

This contract pins how the **connected peer** asks its siblings for
per-peer slices over the embedded NATS request/reply mesh. It exists
because FR-006 binds pgmctl to one peer per invocation while US2's
dump (P1) requires per-peer data.

---

## Subject scheme

```text
pgman_proxy.<cluster_id>.fanout.<slice>.<target_node>
```

| Element | Constraint |
|---|---|
| `<cluster_id>` | The cluster's `cluster.id`. |
| `<slice>` | One of `status`, `config`, `nats_mesh`, `doctor`. Future slices are MINOR-version events. |
| `<target_node>` | A peer `node_id` for unicast, or `*` for broadcast. |

Every peer (including the originator) subscribes to:

```text
pgman_proxy.<cluster_id>.fanout.<slice>.<self_node_id>
pgman_proxy.<cluster_id>.fanout.<slice>.*
```

The wildcard subscription enables broadcast fan-out via NATS
`RequestMany`; the unicast subscription enables targeted queries.

---

## Request envelope

Published by the connected peer:

```jsonc
{
  "version": 1,
  "request_id": "01H...XYZ",        // ULID, propagated from the originating control-plane request
  "operator_actor": "bearer:42a...", // 001 audit identifier; appears in the responder's audit log
  "trace_id": "abc123...",           // W3C
  "deadline_ms": 5000,               // per-slice timeout; siblings honour
  "slice": "status",
  "args": { /* slice-specific */ }
}
```

`args` schemas per slice:

| `slice` | `args` shape | Notes |
|---|---|---|
| `status` | `{}` | Returns the responder's 001 Status. |
| `config` | `{ "redact_level": "normal" \| "strict" }` | Returns redacted YAML. |
| `nats_mesh` | `{}` | Returns the 002 `embedded_nats.*` block for the responder. |
| `doctor` | `{ "check": "<name>?" }` | Empty `check` runs the whole battery. |

---

## Reply envelope

Each responder replies on the inbox carried in the NATS request:

```jsonc
{
  "version": 1,
  "request_id": "01H...XYZ",
  "node_id": "node-2",                // responder
  "status": "ok" | "partial" | "failed",
  "data": { /* slice-specific */ },   // absent when status=failed
  "error": { "code": "...", "message": "..." },  // present when status≠ok
  "responded_at": "2026-05-14T13:42:11.000000123Z"
}
```

Slice-specific `data` shapes mirror the corresponding HTTP responses
(001 `Status`, 002 `embedded_nats`, `DoctorReport` from
`control-plane-extensions.md`), letting the connected peer aggregate
them into HTTP responses without re-shaping.

---

## Per-sibling error codes

Encoded in `error.code` on a `status: "failed"` or `status: "partial"`
reply, OR fabricated by the connected peer when a sibling never replies
within the deadline:

| Code | Cause |
|---|---|
| `sibling_unreachable` | No reply received within `deadline_ms`. Connected peer fabricates this entry. |
| `deadline_exceeded` | Sibling started replying but did not complete in time. Sibling sends. |
| `auth_failed` | Sibling rejected the request (e.g., cluster credential mismatch during rotation). |
| `slice_internal` | Sibling's slice handler returned an error (e.g., JetStream KV miss). |

`sibling_unreachable` is the only code the originator (connected peer)
synthesizes; the others come from the sibling itself.

---

## Aggregation rules

The connected peer aggregates replies into HTTP responses (e.g., the
dump artifact's `peers/<node_id>/*` files, `pgmctl get nodes
--all-peers`, etc.) using these rules:

1. **Successful replies** (`status: "ok"`) populate the slice's payload
   slot.
2. **Failed replies** (`status: "failed"` or fabricated
   `sibling_unreachable`) populate the slice's payload slot with a
   placeholder `{ "_error": { "code": "...", "message": "..." } }` so
   the consumer sees the gap and the reason without parsing two
   different shapes.
3. **Partial replies** (`status: "partial"`) populate normally; the
   `_error` field is added alongside the payload.
4. **The whole request never fails** because one or more siblings
   failed (FR-006a). The HTTP envelope still returns `200`; the slice
   array carries the per-sibling outcomes.

---

## Authorization

- pgmctl authenticates to the connected peer with a bearer token; the
  connected peer extracts `operator_actor` from the token's `actor`
  (001 FR-027).
- The connected peer embeds `operator_actor` in every fan-out request.
- Siblings trust the intra-cluster NATS auth (002 cluster credential
  per RD-001a) as the inter-peer trust anchor; they do NOT re-validate
  a bearer token.
- Siblings log the originating `operator_actor` in their audit record
  for `doctor` slice requests (the only mutation-adjacent fan-out
  slice in v1 is `doctor` with the `check` arg set — still read-only
  per FR-027, but auditable).

---

## Latency budget

| Path | Budget (p99) |
|---|---|
| Connected peer → sibling unicast → reply (steady state) | 100ms |
| Broadcast fan-out (3-peer cluster) | 200ms |
| Aggregated dump's per-peer slice | 5s (default `deadline_ms`) |

Budgets are well inside the per-slice 10s default that FR-032 uses for
the dump (`--per-slice-timeout`).

---

## Observability

- Metric `pgman_proxy_fanout_requests_total{slice,outcome}` — counter;
  `outcome` ∈ `ok` / `partial` / `failed`.
- Metric `pgman_proxy_fanout_replies_total{slice,outcome}` — counter
  (originator-side, per-sibling).
- Metric `pgman_proxy_fanout_latency_seconds{slice}` — histogram
  (originator-side, end-to-end per slice).
- Structured log `fanout.request_sent`, `fanout.reply_received`,
  `fanout.sibling_unreachable`.

---

## Tests

Contract test (`tests/contract/fanout_test.go`):

1. 3-peer fixture cluster.
2. Issue a `status` fan-out from peer A; assert all three responses
   arrive within budget; assert aggregated payload contains entries
   keyed by all three node ids.
3. Drop peer B's network mid-flight; assert peer B's slot in the
   aggregated payload has `status: "failed"` with `code:
   "sibling_unreachable"`; assert the HTTP response is still `200`.
4. Issue a `config` fan-out with `redact_level=strict`; assert all
   replies have host:port pairs replaced.
5. Issue a `doctor` fan-out with `check="cluster.has-leader"`; assert
   every reply contains exactly one `CheckResult`.
6. Auth-failed path: rotate the cluster credential on peer B mid-
   flight; assert subsequent fan-outs from peer A see
   `code: "auth_failed"` in peer B's slot until the rotation
   completes.
