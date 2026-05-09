# Contract: Process Lifecycle

**Feature**: 001-active-active-pg-proxy · **Phase**: 1 · **Date**: 2026-05-09

Pins startup gates, signal handling, drain semantics, and exit codes.

---

## Startup sequence

The process MUST proceed through these phases in order. Failure at any
phase exits with the documented code; no partial startup is permitted.

| # | Phase                              | Gate                                                                  | On failure        |
|---|------------------------------------|-----------------------------------------------------------------------|-------------------|
| 1 | Argument parsing                   | known flags only; no positional args                                  | `EX_CONFIG` (78)  |
| 2 | Configuration load + validate      | required keys present; types/regex/enum valid; secrets not inline     | `EX_CONFIG` (78)  |
| 3 | Observability bootstrap            | logger initialised; metrics endpoint bound on `obs.metrics_addr`      | `EX_OBS` (74)     |
| 4 | NATS connect                       | `nats.Connect` returns within `nats.connect_timeout`                  | `EX_DEPS` (75)    |
| 5 | NATS adapters constructed          | leadership, state store, event bus all return non-nil                 | `EX_DEPS` (75)    |
| 6 | Postgres executor constructed      | `manager.RealPostgresExecutor` returns non-nil                        | `EX_DEPS` (75)    |
| 7 | Manager constructed                | `manager.New` returns non-nil                                         | `EX_DEPS` (75)    |
| 8 | Data-plane listener bound          | `proxy.Proxy` opens its listener on `proxy.listen_addr`               | `EX_LISTEN` (76)  |
| 9 | Singleton claim resolved           | `manager.Start` past singleton-claim phase OR returns budget-exhausted | `EX_SINGLETON` (77) for budget-exhaustion |
| 10 | Control-plane listener bound      | LCM HTTP server binds `control.listen_addr`; auth-token source is readable | `EX_CONTROL` (81) |
| 11 | LCM audit pipeline verified       | First audit record (`control_plane started`) writes successfully to BOTH slog and the NATS audit subject (FR-027) | `EX_CONTROL` (81) |
| — | **Ready**                          | `/readyz` returns `200`; structured event `manager started` emitted; LCM endpoint accepting | —                  |

Once Ready, the process serves traffic until a signal is received.

---

## Signal handling

| Signal     | Action                                                                                              |
|------------|-----------------------------------------------------------------------------------------------------|
| `SIGINT`   | Initiate graceful shutdown (same path as `SIGTERM`).                                                |
| `SIGTERM`  | Initiate graceful shutdown.                                                                         |
| `SIGHUP`   | Reserved for future config-reload. Currently logged at INFO and otherwise ignored.                  |
| `SIGUSR1`  | Reserved for diagnostic dumps (e.g., a topology shrink in dev). Not implemented in v1; logged INFO. |
| Other      | Standard Go runtime behaviour (panic, abort).                                                       |

### Graceful shutdown flow

1. `/readyz` flips to `503` immediately. Process supervisors and
   sidecar liveness probes route traffic away.
2. **Control-plane HTTP server stops accepting new LCM requests** (FIRST,
   so no new mutating engine calls can start during teardown). In-flight
   LCM requests are allowed to drain up to `shutdown.drain_budget`
   (default 30s); requests still running at the deadline return
   `audit_unavailable` and the audit pipeline records the truncation.
3. Data-plane listener is closed (no new client connections accepted).
4. The library `proxy.Proxy.Stop()` is called per its switch policy:
   - `hard_close`: existing connections closed immediately.
   - `drain`: existing connections allowed to finish, bounded by
     `shutdown.drain_budget`.
   - `pause`: same as drain for shutdown purposes.
5. `manager.Stop(context.Background())` is invoked.
6. NATS `conn.Drain()` is invoked (best-effort).
7. Process exits `EX_OK` (0) if all six steps succeeded within budget;
   `EX_DRAIN_TIMEOUT` (79) if the drain budget was exceeded; non-zero
   downstream-specific code if `manager.Stop` returned an error.

---

## Exit codes

```text
0   EX_OK              Clean shutdown (signal-driven path completed).
74  EX_OBS             Observability bootstrap failed (metrics port busy, etc.).
75  EX_DEPS            External dependency unavailable (NATS unreachable / adapter init failed / executor init failed / manager init failed).
76  EX_LISTEN          Data-plane proxy listener could not bind.
77  EX_SINGLETON       Singleton-claim retry budget exhausted (FR-007 in pg-manager 007 milestone).
78  EX_CONFIG          Configuration error (parse / validate / unknown flag / inline secret).
79  EX_DRAIN_TIMEOUT   Shutdown drain budget exceeded; some connections were force-closed.
80  EX_INTERNAL        Unexpected internal error (panic recovered at top-level).
81  EX_CONTROL         Control-plane bind failed OR initial LCM-audit emit failed (FR-021, FR-027).
```

`64`–`79` are the conventional `sysexits.h` band; we deliberately stay
inside it where the meaning maps. `EX_CONTROL` (81) is outside the
`sysexits.h` band because no standard mapping fits LCM-pipeline init.
Codes used here MUST stay stable — operators may wire them into
supervisor restart policies.

---

## Restart-in-place semantics

A peer that exits and is restarted by its supervisor MUST be able to
re-join the cluster without operator action (FR-012). This is satisfied
by:
- NATS leadership lease: handled by `pg-manager`'s `LeadershipProvider`;
  a missing previous-leader lease is renewed by election.
- State store: durable in NATS JetStream KV; survives peer restarts.
- Event bus: subscriptions are re-established at startup; replay
  semantics are governed by `pg-manager`.

`pgman-proxy` itself stores **no local state** that must persist across
restarts. The only on-disk artefact a peer may produce is its log
output, written to stdout/stderr by default and captured by the
supervisor's logging pipeline.

---

## Health-endpoint state machine

```text
process start
    │
    ▼  /healthz=200, /readyz=503
[init]──────────────► nats up?
                          │
                          ▼  /healthz=200, /readyz=503
                      [connecting]
                          │
                          ▼  manager.Start past singleton-claim?
                          │
                          ▼  /healthz=200, /readyz=200
                       [READY]──────► nats lease renewals fail?
                          │                      │
                          │                      ▼  /readyz=503
                          │                  [degraded]
                          │                      │
                          ▼                      ▼
                    SIGINT / SIGTERM     (recover or exit)
                          │
                          ▼  /healthz=200, /readyz=503
                      [draining]
                          │
                          ▼  exit EX_OK / EX_DRAIN_TIMEOUT
                       [stopped]
```

`/healthz` reports liveness only — once init is past arg parsing it
returns `200` until the process exits. `/readyz` reports the operator's
go/no-go signal for routing traffic.
