# Contract: LCM Control Plane

**Feature**: 001-active-active-pg-proxy · **Phase**: 1 · **Date**: 2026-05-09
**Added**: 2026-05-09 amendment (US4, FR-021..FR-032)

Pins the authenticated control-plane surface that operators use to drive
HA-cluster lifecycle operations. Every operation listed here is
implemented by `../pg-manager`'s `Manager` type; this contract describes
**only** the request/response shape, leader-routing rule, audit record,
and authentication policy added by `pgman-proxy`.

---

## Listener

- **Default address**: `:9091`. Configurable as `control.listen_addr`.
- MUST be distinct from `proxy.listen_addr` (data plane) and
  `obs.metrics_addr` / `obs.health_addr` (observability), so each
  surface can be firewalled independently (Constitution II).
- MUST be locked to localhost (`127.0.0.1:9091`) by default in **sidecar**
  deployment recipes; bound to all interfaces only when the operator
  explicitly opens it (Constitution VII: don't expand the trust boundary).

## Authentication

- **Default scheme**: bearer token via `Authorization: Bearer <token>`.
- Token source: `control.auth.token_env` (env-var name) or
  `control.auth.token_file` (file path). Inline tokens in YAML are
  rejected at config validation (consistent with `config.md` § Secrets).
- Tokens are re-read from their source on every request (FR-031); no
  process restart is required to rotate.
- The token-source identifier (16-char SHA-256 prefix of the live
  token, formatted as `bearer:<hex>`) is what appears in audit records'
  `actor` field. The plaintext token MUST NEVER appear in any log line,
  metric, or audit record.
- `Status` and `Diagnose` MAY be configured to allow unauthenticated
  reads (`control.auth.allow_unauth_reads = true`); default is `false`.
- `mTLS` and per-operation RBAC are **out of scope for v1**.

## Transport security (FR-033)

The control-plane listener serves bearer-authenticated requests. To
prevent token leakage on the wire, transport security depends on the
bind address:

| Bind address                         | Default behaviour                                          |
|--------------------------------------|------------------------------------------------------------|
| Loopback (`127.0.0.1` / `::1`)        | Plaintext HTTP permitted; trust boundary is the local kernel. |
| Unix domain socket                    | Plaintext permitted; permission bits gate access.           |
| Any other (routable) address          | TLS REQUIRED. Operator MUST supply `control.tls.cert_file` + `control.tls.key_file` (PEM). Validation rejects a non-loopback bind without both fields. |

The only way to bind plaintext HTTP on a non-loopback address is the
explicit, named, audit-logged opt-in
`control.tls.plaintext_explicit_ack: true`. When set, startup logs a
single `WARN` line (`control plane plaintext bind on non-loopback
acknowledged`) so the choice is auditable. This mirrors FR-018's
`postgres.tls_disable_explicit_ack` rule.

mTLS (client-cert auth) and ACME/cert-rotation tooling remain out of
scope for v1.

## Leader-route timeout (FR-034)

When `leader_route_mode = forward`, the receiving peer publishes the
request on `pgman_proxy.<cluster_id>.lcm.request.<op>` and awaits the
leader's reply on a private inbox. The wait is bounded by
`control.leader_route_timeout` (default 30s; valid range `(0, 5m]`,
enforced at config validation). On timeout the receiving peer:

1. Returns HTTP `504 Gateway Timeout` with `error.code = "leader_route_timeout"`.
2. Emits an audit record with `outcome = "failed"`, `error_code =
   "leader_route_timeout"`, and a `leader_at_request` field naming the
   peer believed to be leader at the moment the forward was published.
3. MUST NOT execute the operation locally; MUST NOT silently retry.

When `leader_route_mode = redirect` no forwarding happens and this
timeout does not apply.

## Operation surface

Each row corresponds to a single `../pg-manager` `Manager` method.
Request/response bodies are JSON. Endpoints are versioned under `/v1/`.

| Operation         | Method | Path                       | Body fields                                              | Leader-only? | Audit |
|-------------------|--------|----------------------------|----------------------------------------------------------|--------------|-------|
| `Status`          | GET    | `/v1/status`               | —                                                        | no           | yes   |
| `Diagnose`        | GET    | `/v1/diagnose`             | —                                                        | no           | yes   |
| `Switchover`      | POST   | `/v1/switchover`           | `target` (NodeID, required)                              | yes          | yes   |
| `Failover`        | POST   | `/v1/failover`             | —                                                        | yes          | yes   |
| `Promote`         | POST   | `/v1/promote`              | —                                                        | local-only   | yes   |
| `Fence`           | POST   | `/v1/fence`                | `target` (NodeID, required)                              | yes          | yes   |
| `Unfence`         | POST   | `/v1/unfence`              | `target` (NodeID, required)                              | yes          | yes   |
| `UpdateTopology`  | POST   | `/v1/topology`             | `topology` (Topology), `policy` (Policy)                 | yes          | yes   |
| `TriggerBackup`   | POST   | `/v1/backup`               | —                                                        | yes          | yes   |
| `PrepareUpgrade`  | POST   | `/v1/upgrade/prepare`      | `plan` (UpgradePlan)                                     | yes          | yes   |
| `ExecuteUpgrade`  | POST   | `/v1/upgrade/execute`      | `plan` (UpgradePlan), `pre_swap` (opaque payload)        | yes          | yes   |

The shape of `Topology`, `Policy`, `UpgradePlan`, `Status`, and
`Diagnosis` payloads is owned by `../pg-manager`. This contract carries
them verbatim; renames in the upstream library propagate as
MINOR-version events here (Constitution V).

## Leader routing rule (FR-026)

A leader-only operation arriving at a non-leader peer MUST be handled
in one of two configurable ways:

| Mode (default `forward`) | Behaviour                                                                                       |
|--------------------------|-------------------------------------------------------------------------------------------------|
| `forward`                | Receiving peer publishes a request on `pgman_proxy.<cluster_id>.lcm.request.<op>` and awaits the leader's reply on a private inbox; full result returned to the caller. |
| `redirect`               | Receiving peer returns `307 Temporary Redirect` with `Location` set to the leader's control-plane address (resolved from the cluster's published peer info). |

`Promote` is **local-only** — it executes on the receiving peer
regardless of leadership state, because its semantics are "promote *this
node* to leader." This matches `../pg-manager`'s `Manager.Promote`.

## Response envelope

All non-redirect responses share an envelope:

```json
{
  "operation": "Switchover",
  "request_id": "01H...XYZ",       // ULID; logged in audit and forwarded as trace span id
  "outcome": "accepted|rejected|failed",
  "engine_result": { /* shape from ../pg-manager method, or omitted */ },
  "error": { "code": "string", "message": "string" }   // present only when outcome != "accepted"
}
```

`outcome` semantics:

- `accepted` — engine returned a non-error result; `engine_result` carries it.
- `rejected` — refused before reaching the engine (auth, leader-routing,
  bootstrap-in-progress, audit-pipeline-down, no `BackupExecutor`).
- `failed` — engine returned an error; `error` carries the engine's message.

## Error codes

| `error.code`                    | When                                                                                       | HTTP status |
|---------------------------------|--------------------------------------------------------------------------------------------|-------------|
| `auth_required`                 | No credential supplied for an authenticated operation                                       | 401         |
| `auth_invalid`                  | Credential present but rejected                                                             | 403         |
| `not_leader`                    | Leader-only op received at a non-leader and `mode=redirect`                                | 307         |
| `cluster_bootstrapping`         | Cluster has not finished initial bring-up (FR-029)                                          | 409         |
| `leadership_in_transition`      | Leader-only op attempted while the lease is in flux (FR-029)                                | 503         |
| `backup_executor_missing`       | `TriggerBackup` with no operator-supplied backend (FR-030)                                  | 412         |
| `audit_unavailable`             | Audit pipeline cannot accept records; mutating ops refused (FR-028)                          | 503         |
| `engine_error`                  | `../pg-manager` returned an error; details in `error.message`                                | 500         |
| `invalid_argument`              | Malformed body, unknown enum value, NodeID not a peer, etc.                                  | 400         |
| `leader_route_timeout`          | Forward-mode request to the leader did not return within `control.leader_route_timeout` (FR-034) | 504         |
| `internal`                      | Unexpected internal failure                                                                  | 500         |

## Audit record (FR-027)

Emitted twice for every request: once as a structured-log line at level
`info` (or `warn` for `rejected`/`failed`) and once as a NATS message on
the subject `pgman_proxy.<cluster_id>.audit.lcm`.

```json
{
  "time": "2026-05-09T12:34:56.789Z",
  "request_id": "01H...XYZ",
  "operation": "Switchover",
  "target": "peer-b",
  "actor": "ops-team@bearer:42a..."     // identifier of the credential, never the secret value
                  || "anonymous",
  "source_addr": "10.0.0.42:51234",
  "outcome": "accepted",
  "engine_latency_ms": 187,
  "total_latency_ms": 213,
  "error_code": null,
  "cluster_id": "prod-east",
  "node_id": "pg-east-a",
  "trace_id": "...",
  "span_id": "..."
}
```

`actor` MUST identify which credential signed the request (e.g.,
"bearer:<sha256-prefix>") **without** disclosing the secret material.

## Concurrency

Mutating LCM operations are serialised inside `../pg-manager`'s engine.
The control plane MUST NOT impose its own queue ordering; it forwards
each request and reports the engine's outcome. When two concurrent
mutating requests collide, one returns `accepted`/`failed` per the
engine; the other returns `engine_error` with a conflict message
(FR-029 edge case "Concurrent LCM requests").

## Observability hooks (cross-ref `observability.md`)

The following metrics MUST be emitted for the control plane (label set
omitted when noted):

- `pgman_proxy_lcm_requests_total{operation, outcome}` — counter.
- `pgman_proxy_lcm_request_latency_seconds{operation, outcome}` — histogram.
- `pgman_proxy_lcm_audit_emit_failures_total{sink}` — counter; non-zero
  triggers fail-closed mutating-request rejection (FR-028).
- `pgman_proxy_lcm_in_flight{operation}` — gauge.

---

## Out-of-scope for v1 (record so they don't leak into implementation)

- mTLS for the control plane.
- Per-operation RBAC (every authenticated caller can invoke any operation).
- Restore / PITR endpoint.
- A bundled backup backend.
- Streaming progress for long-running upgrade operations (clients poll
  `Status`).
- gRPC / Protobuf surface (HTTP+JSON only in v1).
