# Implementation Plan: Active/Active PostgreSQL HA Proxy + Lifecycle Manager (pgman-proxy v1)

**Branch**: `001-active-active-pg-proxy` | **Date**: 2026-05-09 (amended) | **Spec**: [spec.md](./spec.md)
**Input**: Feature specification from `/specs/001-active-active-pg-proxy/spec.md`
**Amendment 2026-05-09**: Spec extended to cover full HA-cluster
lifecycle management (US4, FR-021..FR-032). All LCM logic remains in
`../pg-manager`; this plan adds an authenticated control-plane surface
that translates HTTP requests into `Manager` method calls, audits each
action, and forwards leader-only operations to the current leader peer.

## Summary

Build a single-binary Go scaffold that wraps `pg-manager` (sibling module
`github.com/f1bonacc1/pg-manager`) and stands up its NATS-backed
leadership / state / event-bus adapters together with the library's
already-existing topology-aware TCP proxy (`pg-manager/proxy`). The new
code in this repository is process lifecycle, configuration loading, NATS
wiring, observability glue, **and an authenticated control-plane HTTP API
for lifecycle operations** — no wire-protocol code, no consensus code,
no Kubernetes/Helm code, no LCM algorithms (all of those live in
`../pg-manager`). The reference assembly already exists in
`../pg-manager/examples/three_node_nats/main.go`; this milestone
productionises it as `cmd/pgman-proxy` and packages it for the three
deployment topologies (standalone process, microservice, sidecar), then
adds a control-plane surface that exposes `Manager.{Status, Diagnose,
Switchover, Failover, Promote, Fence, Unfence, UpdateTopology,
TriggerBackup, PrepareUpgrade, ExecuteUpgrade}` to operators with
authentication, audit, and leader-routing.

## Technical Context

**Language/Version**: Go 1.25+ (matches `pg-manager` go.mod minimum)
**Primary Dependencies**:
  - `github.com/f1bonacc1/pg-manager` (the engine; provides
    `manager.Manager`, `proxy.Proxy`, `pgmanager.{Topology,Policy,ProxyConfig,SwitchPolicy}`)
  - `github.com/f1bonacc1/pg-manager/adapters/nats` (NATS leadership / state
    store / event bus)
  - `github.com/nats-io/nats.go` (NATS client; transitive but pinned here too)
  - `github.com/prometheus/client_golang/prometheus` + `promhttp`
    (metrics endpoint AND the LCM control-plane `/v1/...` HTTP handlers,
    served on a separate listener)
  - `github.com/oklog/ulid/v2` (request IDs for LCM audit records)
  - Standard library: `flag`, `os/signal`, `net/http`, `log/slog` (JSON
    logger), `crypto/subtle` (constant-time bearer-token comparison)
**Storage**: None local-persistent in this repo. State lives in NATS
  (managed by `pg-manager/adapters/nats`); PostgreSQL data lives in
  `pg-manager`'s `PGDATA`. Configuration may optionally be loaded from a
  YAML file path supplied at startup; the binary itself is stateless.
**Testing**:
  - `go test` for unit tests (config parsing, signal handling, observability
    helpers).
  - Integration tests: docker-compose harness reusing `../pg-manager`'s
    fixtures (a 3-node Postgres + 1 NATS topology) to exercise three
    `pgman-proxy` peers in front of it.
  - Smoke tests: one per deployment topology (standalone, microservice
    multi-replica, sidecar colocated with `pg-manager`).
  - CI gates per Constitution VI: real PG + real NATS in every
    coordination test.
**Target Platform**: Linux x86_64 / arm64 server, container-friendly. No
  Windows/macOS production support; macOS dev is best-effort.
**Project Type**: CLI / single-binary network service (Go module). Single
  project, no frontend.
**Performance Goals**:
  - Per-query proxy overhead < 1ms p99 on local-loopback (Constitution
    Performance Baseline).
  - Leader-failover end-to-end < 5s p99 (SC-002).
  - Crash-restart MTTR < 10s under standard supervisor (SC-007).
**Constraints**:
  - No virtual-IP / ARP / keepalived code paths (FR-004).
  - No Kubernetes API client, no Helm, no CRDs, no admission webhooks
    (FR-015, Constitution VII).
  - No re-implementation of anything `pg-manager` already provides
    (FR-020, Constitution IV).
  - Fail-closed startup: NATS unreachable, missing config, TLS material
    missing, listen port busy → exit non-zero (FR-010).
  - Run cleanly under non-root (FR-013).
**Scale/Scope**:
  - Single cluster per peer (1:1). Multi-tenant deferred.
  - Typical deployment: 3–5 peers per cluster.
  - Connection scale target: 10k concurrent client connections per peer
    on a commodity 4-vCPU host (validation in Phase 0 research).

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*
*Re-evaluated 2026-05-09 after the LCM amendment (US4, FR-021..FR-032).*

Constitution: `.specify/memory/constitution.md` v1.1.0.

| Principle                                            | Status | How this plan complies                                                                                                                                                                                                                  |
|------------------------------------------------------|--------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| I. Wire-Protocol Fidelity                            | PASS   | The wire-level work lives in `pg-manager/proxy`, which forwards bytes verbatim and does not terminate TLS. The new LCM control plane is HTTP+JSON on a **separate listener** and never touches the PostgreSQL wire path. Verified by FR-016, FR-020. |
| II. Fail-Closed Safety                               | PASS   | Startup gates enforce NATS reachability, TLS material presence, listen-port availability, control-plane bind, and required-config presence before opening the data-plane listener. LCM mutating ops are refused when the audit pipeline is down (FR-028) and during leadership transitions (FR-029). Verified by FR-010, FR-018, FR-028, FR-029. |
| III. Active/Active Coordination Correctness          | PASS   | Leader election delegated to `pg-manager/adapters/nats.NewLeadership`; every leader-only LCM action is forwarded to the current leader peer (FR-026). The control plane MUST NOT execute leader-only operations on a non-leader. The library re-checks the lease at action time. |
| IV. Thin Scaffold over pg-manager                    | PASS   | New code is strictly bounded to: `cmd/pgman-proxy`, `internal/config`, `internal/cluster` (NATS wiring), `internal/runtime` (signals/drain), `internal/obs` (logging/metrics), and `internal/control` (LCM HTTP shims). Every LCM handler is a thin shim over a `manager.Manager` method (FR-022). SC-013 grep gate enforces "0 LCM logic in this repo". |
| V. Observability by Default                          | PASS   | `slog` JSON logger + Prometheus `/metrics`. LCM extensions: `pgman_proxy_lcm_*` metrics, dual-sink audit (slog + NATS subject `pgman_proxy.<cluster_id>.audit.lcm`), trace context propagated to LCM requests and engine calls. |
| VI. Integration-First Testing (NON-NEGOTIABLE)       | PASS   | All coordination + LCM tests run against real Postgres + real NATS in CI. The LCM amendment adds four new integration tests under `tests/integration/`: bootstrap, switchover, audit-presence, and leader-routing. Mocks remain permitted only for fault injection paired with real-server happy-path coverage. |
| VII. Scope Discipline & Reversibility                | PASS   | The LCM amendment did NOT introduce any Kubernetes/Helm code. Restore/PITR, mTLS for the control plane, per-operation RBAC, and a bundled backup backend are all explicitly out-of-scope-for-v1 with rationale. SC-006 + SC-013 grep gates enforce both "no k8s" and "no LCM logic in this repo". |

**Result: PASS — no violations to track in Complexity Tracking.**

The amendment ratchets requirements upward (audit fail-closed, leader-routing, BackupExecutor delegation) without invalidating the original design — consistent with the MINOR-version semantics of the constitution itself.

## Project Structure

### Documentation (this feature)

```text
specs/001-active-active-pg-proxy/
├── plan.md              # This file
├── spec.md              # Feature specification
├── research.md          # Phase 0 output (this command)
├── data-model.md        # Phase 1 output (this command)
├── quickstart.md        # Phase 1 output (this command)
├── contracts/           # Phase 1 output (this command)
│   ├── config.md           # Configuration-file & env-var schema
│   ├── cli.md              # Command-line surface
│   ├── observability.md    # Logger field schema, metric names, NATS topics
│   ├── lifecycle.md        # Signal handling, exit codes, health/readiness
│   ├── deployment-modes.md # Standalone / microservice / sidecar matrix
│   └── lcm.md              # Authenticated control-plane HTTP API (US4)
├── checklists/
│   └── requirements.md  # Spec-quality checklist (already passing)
└── tasks.md             # Phase 2 output (NOT created by /speckit-plan)
```

### Source Code (repository root)

```text
pgman-proxy/
├── cmd/
│   └── pgman-proxy/
│       └── main.go              # Entry point — flag parsing, run() loop, exit codes
├── internal/
│   ├── config/                  # Env + flag + YAML config loader, validation
│   │   ├── config.go
│   │   └── config_test.go
│   ├── cluster/                 # NATS adapter wiring (leadership, state, eventbus)
│   │   ├── cluster.go
│   │   └── cluster_test.go
│   ├── runtime/                 # Process lifecycle: signals, drain, fail-closed gates
│   │   ├── runtime.go
│   │   └── runtime_test.go
│   ├── obs/                     # slog JSON logger, Prometheus metrics, /healthz, /readyz
│   │   ├── logger.go
│   │   ├── metrics.go
│   │   ├── health.go
│   │   └── obs_test.go
│   └── control/                 # LCM control-plane: HTTP handlers, auth, leader-route, audit
│       ├── server.go               # HTTP listener + middleware stack
│       ├── auth.go                 # bearer-token validation; token re-read per request (FR-031)
│       ├── handlers.go             # one handler per operation; thin shims over manager.Manager
│       ├── route.go                # leader-routing rule (forward via NATS or 307 redirect)
│       ├── audit.go                # structured-log + NATS-subject audit emitter
│       └── control_test.go
├── tests/
│   ├── integration/             # docker-compose harness; multi-peer scenarios
│   │   ├── failover_test.go
│   │   ├── nats_outage_test.go
│   │   ├── startup_failclose_test.go
│   │   ├── lcm_switchover_test.go      # US4 — Switchover / Fence / Unfence
│   │   ├── lcm_bootstrap_test.go       # US4 — fresh-cluster bring-up (FR-023, SC-009)
│   │   ├── lcm_audit_test.go           # US4 — audit-record presence on every request (SC-010)
│   │   ├── lcm_leader_route_test.go    # US4 — non-leader forwards or redirects (SC-011)
│   │   └── docker-compose.test.yml
│   └── smoke/                   # one test per deployment topology
│       ├── standalone_test.go
│       ├── microservice_test.go
│       └── sidecar_test.go
├── deploy/                      # Reference deployment recipes (NO k8s, NO Helm)
│   ├── systemd/
│   │   └── pgman-proxy.service
│   ├── docker/
│   │   └── Dockerfile
│   └── compose/
│       └── docker-compose.yml   # 3-peer reference topology
├── go.mod
├── go.sum
├── Makefile                     # build / test / lint / smoke targets
├── README.md                    # Cites Constitution; reproduces "Out of Scope"
├── CLAUDE.md                    # speckit-managed pointer (already in repo)
└── .specify/                    # speckit harness (already in repo)
```

**Structure Decision**: Single-project Go module layout. `cmd/` carries
the only `main` package; `internal/` keeps everything else
non-importable from outside the module — a hard fence consistent with
Constitution Principle IV (we are not a library; we are a deployable
binary that wraps one). `tests/` separates integration and smoke tests
from unit tests (which live next to the code under test in
`internal/...`). `deploy/` carries reference recipes for the three
deployment modes; it MUST NOT contain Kubernetes, Helm, or
operator-bundle artefacts (Constitution VII; SC-006 grep gate).

## Complexity Tracking

> **Fill ONLY if Constitution Check has violations that must be justified**

No violations. Table intentionally empty.

| Violation | Why Needed | Simpler Alternative Rejected Because |
|-----------|------------|--------------------------------------|
| —         | —          | —                                    |
