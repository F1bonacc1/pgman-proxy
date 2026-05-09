# Phase 1 Data Model: pgman-proxy v1

**Feature**: 001-active-active-pg-proxy
**Date**: 2026-05-09

This project does not own a persistent database. Domain "data" lives in
two foreign systems:

1. **NATS** — owned and modeled by `pg-manager/adapters/nats`. The
   leadership lease, KV state, and event-bus subjects are external
   contracts we consume but do not redefine.
2. **PostgreSQL `PGDATA`** — owned by `pg-manager`. Out of scope here.

The data model below describes only the **in-process entities** that
the new code in this repository constructs, validates, and exposes.

---

## Entity: ProxyPeer

A single running `pgman-proxy` instance.

| Field          | Type                | Source              | Validation                                                  |
|----------------|---------------------|---------------------|-------------------------------------------------------------|
| `NodeID`       | `string` (alias of `pgmanager.NodeID`) | required, from config | non-empty; matches `^[a-z0-9][a-z0-9-]{0,62}$`              |
| `ClusterID`    | `string`            | required, from config | non-empty; same regex as NodeID                              |
| `Peers`        | `[]string`          | required, from config | non-empty; MUST contain `NodeID`; each element matches NodeID regex |
| `StartedAt`    | `time.Time`         | runtime              | set once at process start; immutable                         |
| `LeaseHolder`  | `bool` (derived)    | runtime              | mirrors `manager.Manager`'s view; never authoritative on its own |

**Lifecycle states** (state machine):

```
created  →  configured  →  connecting  →  ready  →  draining  →  stopped
                                ↓                  ↑
                                ↓→→→→→→ failed →→→→ (supervisor restart)
```

- `created`: process started, no config loaded yet.
- `configured`: config loaded and validated.
- `connecting`: NATS dialing, listener binding, manager constructing.
- `ready`: `manager.Start` past singleton-claim; listener accepting;
  `/readyz` returns 200.
- `draining`: SIGINT/SIGTERM received; in-flight queries draining;
  listener closed to new connections.
- `stopped`: clean exit (zero).
- `failed`: any startup gate failed → process exits non-zero with the
  exit code from `contracts/lifecycle.md`.

A peer MUST NOT serve writes outside `ready`. (FR-011, Constitution III.)

---

## Entity: ProxyConfig (in-memory)

The validated, fully-resolved configuration for one peer. Loaded from
flags > env > YAML > defaults (R3). Validated in `internal/config`.

| Field                          | Type            | Required | Default                         | Notes                                       |
|--------------------------------|-----------------|----------|---------------------------------|---------------------------------------------|
| `Cluster.ID`                   | `string`        | yes      | —                               | matches NodeID regex                        |
| `Node.ID`                      | `string`        | yes      | —                               | matches NodeID regex                        |
| `Peers`                        | `[]string`      | yes      | —                               | non-empty                                   |
| `NATS.URL`                     | `string`        | yes      | —                               | scheme `nats://` or `tls://`                 |
| `NATS.ConnectTimeout`          | `time.Duration` | no       | `10s`                           | startup gate                                |
| `NATS.ReconnectWait`           | `time.Duration` | no       | `2s`                            |                                             |
| `NATS.MaxReconnects`           | `int`           | no       | `-1` (infinite)                 |                                             |
| `NATS.CredsFile`               | `string`        | no       | —                               | path; mutually exclusive with `NATS.Token`  |
| `NATS.Token`                   | `string`        | no       | —                               | sourced from env, never inline YAML         |
| `Proxy.ListenAddr`             | `string`        | yes      | —                               | `host:port`; bound at startup               |
| `Proxy.DialTimeout`            | `time.Duration` | no       | `5s`                            |                                             |
| `Proxy.SwitchPolicy`           | `enum`          | no       | `hard_close`                    | `hard_close` \| `drain` \| `pause`          |
| `Postgres.BinDir`              | `string`        | yes      | —                               | absolute path                               |
| `Postgres.DataDir`             | `string`        | yes      | —                               | absolute path                               |
| `Postgres.LocalDSN`            | `string`        | yes      | —                               | secret; sourced from env or file path        |
| `Postgres.PeerDSNs`            | `map[NodeID]string` | no   | derived from `Peers` template   | optional override                           |
| `Postgres.TLSMode`             | `enum`          | no       | `verify-full`                   | `disable` rejected at validate time unless explicit named exception |
| `Topology.Port`                | `int`           | no       | `5432`                          | upstream PG port                            |
| `Policy.FailoverDelay`         | `time.Duration` | no       | `30s`                           | passed through to `pg-manager`              |
| `Policy.SwitchoverDelay`       | `time.Duration` | no       | `30s`                           |                                             |
| `Policy.PromoteTimeout`        | `time.Duration` | no       | `60s`                           |                                             |
| `Policy.LivenessInterval`      | `time.Duration` | no       | `5s`                            |                                             |
| `Policy.LivenessFailures`      | `int`           | no       | `3`                             |                                             |
| `Policy.QuorumSync.MinSync`    | `int`           | no       | `1`                             |                                             |
| `Policy.AutoRebootstrap.Enabled` | `bool`        | no       | `false`                         | opt-in, see `pg-manager` docs               |
| `Policy.AutoDemote.Enabled`    | `bool`          | no       | `false`                         | opt-in                                      |
| `Obs.LogLevel`                 | `enum`          | no       | `info`                          | `debug` \| `info` \| `warn` \| `error`      |
| `Obs.MetricsAddr`              | `string`        | no       | `:9090`                         | Prometheus endpoint                         |
| `Obs.HealthAddr`               | `string`        | no       | `:9090`                         | shares port with metrics by default         |
| `Obs.OTel.Endpoint`            | `string`        | no       | —                               | when empty, OTel exporter = noop            |
| `Shutdown.DrainBudget`         | `time.Duration` | no       | `30s`                           | SIGTERM drain budget                        |

**Cross-field validation** (run after layer-merging):

- `Proxy.ListenAddr` MUST be a syntactically valid `host:port`; binding
  is verified at runtime startup, not at parse time.
- `Postgres.TLSMode == "disable"` MUST also set
  `Postgres.TLSDisableExplicitlyAck = true`; otherwise validation fails
  closed (FR-018).
- `Cluster.ID == Node.ID` is permitted (single-node standalone form);
  in that case `Peers` MUST be `[Node.ID]`.
- `NATS.CredsFile` and `NATS.Token` MUST NOT both be set.

---

## Entity: ClusterHandles

The set of NATS-backed adapters that `pgman-proxy` constructs once at
startup and passes into `manager.Config`. Owned by `internal/cluster`;
never re-constructed at runtime.

| Field         | Type                                                | Constructor                                  |
|---------------|-----------------------------------------------------|----------------------------------------------|
| `Conn`        | `*nats.Conn`                                        | `nats.Connect(...)`                          |
| `Leadership`  | `pgmanager.LeadershipProvider`                      | `natsadapter.NewLeadership(...)`             |
| `State`       | `pgmanager.StateStore`                              | `natsadapter.NewStateStore(...)`             |
| `Bus`         | `pgmanager.EventBus`                                | `natsadapter.NewEventBus(...)`               |

Lifecycle: created at startup; `Conn.Drain()` and the adapters'
`Close()` methods are invoked at shutdown.

---

## Entity: ControlPlane

The authenticated HTTP API surface introduced by the LCM amendment
(US4 / FR-021..FR-032). Owned by `internal/control`. One per peer.

| Field             | Type                                              | Source / Notes                                                          |
|-------------------|---------------------------------------------------|-------------------------------------------------------------------------|
| `ListenAddr`      | `string`                                          | from `control.listen_addr`; default `:9091`                              |
| `AuthTokenSrc`    | `enum {env_var, file}`                            | exactly one of `control.auth.token_env` or `control.auth.token_file`     |
| `AllowUnauthReads`| `bool`                                            | from `control.auth.allow_unauth_reads`; default `false`                  |
| `LeaderRouteMode` | `enum {forward, redirect}`                        | default `forward`                                                        |
| `Manager`         | `*manager.Manager` (handle to the in-process engine) | injected at startup; never created here                              |
| `Audit`           | `*Auditor` (internal)                             | dual-sink: slog + NATS subject `pgman_proxy.<cluster_id>.audit.lcm`      |

`ControlPlane` is **constructed last** at startup (after manager is past
singleton claim) and **stopped first** at shutdown (before manager.Stop)
so no LCM operation can be in flight while the engine is tearing down.

### LCM Operation enum

```text
LCMOperation := Status | Diagnose | Switchover | Failover | Promote
              | Fence | Unfence | UpdateTopology | TriggerBackup
              | PrepareUpgrade | ExecuteUpgrade
```

Each enum value maps 1:1 to a `manager.Manager` method (FR-022). New
enum entries are MINOR-version events for the control-plane wire schema.

### AuditRecord

| Field             | Type                  | Notes                                                                  |
|-------------------|-----------------------|------------------------------------------------------------------------|
| `RequestID`       | string (ULID)         | one per inbound request; reused as trace span id                       |
| `Operation`       | `LCMOperation`        |                                                                         |
| `Target`          | string                | e.g., NodeID for `Switchover`/`Fence`; empty when not applicable       |
| `Actor`           | string                | credential identifier (e.g., `bearer:<sha256-prefix>`); never the secret |
| `SourceAddr`      | string                | client IP:port                                                          |
| `Outcome`         | `enum {accepted, rejected, failed}`            |                                              |
| `EngineLatencyMs` | int                   | time spent inside `pg-manager`                                          |
| `TotalLatencyMs`  | int                   | request received → response written                                     |
| `ErrorCode`       | string \| nil         | one of the codes in `contracts/lcm.md`                                  |
| `EmittedAt`       | RFC3339               | UTC                                                                     |

`AuditRecord` MUST be successfully written to **both** sinks before a
mutating operation is allowed to return `accepted` (FR-028).

---

## Entity: ObsContext

The observability surface attached to one peer.

| Field         | Type                              | Notes                                            |
|---------------|-----------------------------------|--------------------------------------------------|
| `Logger`      | `*slog.Logger`                    | JSON handler; default fields: `cluster_id`, `node_id` |
| `Metrics`     | `*prometheus.Registry`            | with `process_*` and `go_*` collectors           |
| `Health`      | `*healthMux` (internal)            | tracks `nats_up`, `listener_up`, `manager_ready` |
| `Tracer`      | `trace.TracerProvider`            | OTel; defaults to noop                            |

`ObsContext` is constructed before `ClusterHandles` so that NATS
connect errors are observable.

---

## Entity: CoordinationEvent (consumed, not owned)

This repository does not define new NATS subjects. It subscribes to the
existing `pg-manager` topic family and exposes the events through
observability:

- `pgmanager.<cluster_id>.auto_rebootstrap.>`
- `pgmanager.<cluster_id>.auto_demote.>`
- `pgmanager.<cluster_id>.divergence.>`
- `pgmanager.<cluster_id>.conninfo.reconciled`

For each event the proxy:
1. Increments a Prometheus counter labelled by `subject` and (where
   present) `reason`.
2. Emits a structured log line at level `info` (or `warn` for refusals).
3. Attaches the trace context if present in the message header.

Schema for each topic is owned by `pg-manager`. We do not redefine it.

---

## Validation rules summary

| Rule                                                                                     | Source                          | Tested at                           |
|------------------------------------------------------------------------------------------|---------------------------------|-------------------------------------|
| Required keys present                                                                    | FR-002                          | `internal/config` unit tests        |
| `NodeID` ∈ `Peers`                                                                       | logical                         | `internal/config` unit tests        |
| TLS mode `disable` requires explicit ack                                                  | FR-018                          | `internal/config` unit tests        |
| NATS connect succeeds within `NATS.ConnectTimeout`                                        | FR-010                          | `internal/runtime` integration test |
| Listen port bind succeeds                                                                 | FR-010                          | `tests/integration/startup_failclose_test.go` |
| Manager singleton claim succeeds within budget                                            | pg-manager FR-007 (007 milestone) | `tests/integration/failover_test.go` |
| `/readyz` returns 503 on lease loss                                                       | FR-011                          | `tests/integration/nats_outage_test.go` |
| Drain budget honoured on SIGTERM                                                          | FR-014                          | `internal/runtime` unit + integration tests |

Every functional requirement maps to a validation row above (see
`tasks.md` for the test-task inventory after `/speckit-tasks`).
