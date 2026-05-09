---

description: "Task list for Active/Active PostgreSQL HA Proxy + Lifecycle Manager (pgman-proxy v1)"
---

# Tasks: Active/Active PostgreSQL HA Proxy + Lifecycle Manager (pgman-proxy v1)

**Input**: Design documents from `/specs/001-active-active-pg-proxy/`
**Prerequisites**: `plan.md` (required), `spec.md` (required for user stories), `research.md`, `data-model.md`, `contracts/`

**Tests**: INCLUDED. The spec mandates integration tests against real PostgreSQL + real NATS for every coordination and LCM surface (Constitution VI, SC-008, SC-010). PRs lacking such coverage are blocked. Tests are written **first** for each user story and MUST fail before implementation.

**Organization**: Tasks are grouped by user story so each story is independently implementable, testable, and demoable. User-story phase order follows priority: US1 (P1) → US2 (P2) → US4 (P2) → US3 (P3).

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies on incomplete tasks).
- **[Story]**: Maps the task to a user story (US1, US2, US3, US4). Setup, Foundational, and Polish phases have no story label.
- Every task description includes the exact file path it touches.

## Path Conventions

- **Single Go module** rooted at the repo (`/home/eugene/projects/go/pgman-proxy/`).
- Application code lives under `cmd/` (`main` packages) and `internal/` (everything else).
- Tests next to code under test for unit tests; `tests/integration/` and `tests/smoke/` for cross-process tests.

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Initialize the Go module, lint config, build harness, and deploy templates so every subsequent phase has a clean home for code.

- [ ] T001 Initialize Go module — write `go.mod` with module path `github.com/f1bonacc1/pgman-proxy`, Go 1.25 minimum, and a `replace github.com/f1bonacc1/pg-manager => ../pg-manager` directive (per research R10) at `/home/eugene/projects/go/pgman-proxy/go.mod`
- [ ] T002 [P] Create directory skeleton (empty `.gitkeep` files OK): `cmd/pgman-proxy/`, `internal/{config,cluster,runtime,obs,control}/`, `tests/{integration,smoke}/`, `deploy/{systemd,docker,compose}/`, `examples/backup-fs/` under `/home/eugene/projects/go/pgman-proxy/`
- [ ] T003 [P] Create `Makefile` with `build`, `test`, `lint`, `smoke`, `integration`, `release` targets at `/home/eugene/projects/go/pgman-proxy/Makefile`
- [ ] T004 [P] Create `.golangci.yml` mirroring `../pg-manager/.golangci.yml` (gofmt, govet, staticcheck, gosec, errcheck, ineffassign) at `/home/eugene/projects/go/pgman-proxy/.golangci.yml`
- [ ] T005 [P] Create `.goreleaser.yaml` for amd64/arm64 static Linux binaries + distroless OCI image (per research R9) at `/home/eugene/projects/go/pgman-proxy/.goreleaser.yaml`
- [ ] T006 [P] Create `deploy/systemd/pgman-proxy.service` unit-file template at `/home/eugene/projects/go/pgman-proxy/deploy/systemd/pgman-proxy.service`
- [ ] T007 [P] Create `deploy/systemd/pgman-proxy-sidecar.service` unit-file template (After= postgresql.service) at `/home/eugene/projects/go/pgman-proxy/deploy/systemd/pgman-proxy-sidecar.service`
- [ ] T008 [P] Create `deploy/docker/Dockerfile` (distroless static base, non-root) at `/home/eugene/projects/go/pgman-proxy/deploy/docker/Dockerfile`
- [ ] T009 [P] Create `deploy/compose/docker-compose.yml` reference 3-peer topology (3 PG + 1 NATS + 3 pgman-proxy) at `/home/eugene/projects/go/pgman-proxy/deploy/compose/docker-compose.yml`
- [ ] T010 [P] Create `README.md` citing `.specify/memory/constitution.md` v1.1.0 and reproducing the spec's "Out of Scope" list (k8s/Helm, VIP, restore/PITR, multi-tenant, ACME) at `/home/eugene/projects/go/pgman-proxy/README.md`

**Checkpoint**: `make build` succeeds (with no Go source yet beyond `cmd/pgman-proxy/main.go` placeholder); `make lint` runs.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Build the core scaffolding that every user story depends on — config, observability primitives, NATS adapter wiring, runtime lifecycle, manager assembly. NO user-story logic here.

**⚠️ CRITICAL**: No user-story work begins until this phase is complete.

- [ ] T011 Define configuration struct types matching `contracts/config.md` and `data-model.md` `ProxyConfig` entity in `/home/eugene/projects/go/pgman-proxy/internal/config/config.go`
- [ ] T012 [P] Implement layered loader (flags > env > YAML > defaults) per `contracts/config.md` in `/home/eugene/projects/go/pgman-proxy/internal/config/loader.go`
- [ ] T013 [P] Implement validator with cross-field rules (FR-002, FR-010, FR-018, FR-033, FR-034, secret-detection, TLS-disable ack, control-plane plaintext-on-non-loopback ack, mutually-exclusive control auth tokens, leader-route-mode enum, leader-route-timeout range) in `/home/eugene/projects/go/pgman-proxy/internal/config/validate.go`
- [ ] T014 [P] Add unit tests covering every validation row in `data-model.md` § Validation rules summary in `/home/eugene/projects/go/pgman-proxy/internal/config/config_test.go`
- [ ] T015 Implement `slog` JSON logger with stable field schema (`cluster_id`, `node_id`, `component`, `trace_id`, `span_id`) per `contracts/observability.md` in `/home/eugene/projects/go/pgman-proxy/internal/obs/logger.go`
- [ ] T016 [P] Implement Prometheus registry + process/Go collectors + the connection/query/coordination/process metric set per `contracts/observability.md` in `/home/eugene/projects/go/pgman-proxy/internal/obs/metrics.go`
- [ ] T017 [P] Implement `/healthz`, `/readyz`, `/metrics` HTTP handlers + readiness state machine per `contracts/lifecycle.md` § Health-endpoint state machine in `/home/eugene/projects/go/pgman-proxy/internal/obs/health.go`
- [ ] T018 [P] Implement OTel tracer skeleton (noop default; OTLP gRPC exporter when `obs.otel.endpoint` set) in `/home/eugene/projects/go/pgman-proxy/internal/obs/tracer.go`
- [ ] T019 [P] Add unit tests for logger schema, health/readiness state machine, and metrics registration in `/home/eugene/projects/go/pgman-proxy/internal/obs/obs_test.go`
- [ ] T020 Implement NATS connection + adapter wiring (`*nats.Conn`, `LeadershipProvider`, `StateStore`, `EventBus` from `pg-manager/adapters/nats`) in `/home/eugene/projects/go/pgman-proxy/internal/cluster/cluster.go`
- [ ] T021 [P] Implement coordination event subscriber (subscribes to `pgmanager.<cluster>.{auto_rebootstrap,auto_demote,divergence,conninfo}.>` per FR-006) in `/home/eugene/projects/go/pgman-proxy/internal/cluster/events.go`
- [ ] T022 [P] Add unit tests for cluster wiring using the in-process NATS test server helper in `/home/eugene/projects/go/pgman-proxy/internal/cluster/cluster_test.go`
- [ ] T023 Implement startup-gate sequence (the 11 steps from `contracts/lifecycle.md`) in `/home/eugene/projects/go/pgman-proxy/internal/runtime/start.go`
- [ ] T024 [P] Implement signal handler + graceful-shutdown flow per `contracts/lifecycle.md` § Graceful shutdown flow in `/home/eugene/projects/go/pgman-proxy/internal/runtime/shutdown.go`
- [ ] T025 [P] Define exit-code constants (`EX_OK`..`EX_CONTROL`) per `contracts/lifecycle.md` § Exit codes in `/home/eugene/projects/go/pgman-proxy/internal/runtime/exit.go`
- [ ] T026 [P] Add unit tests for runtime: signal handling, exit-code mapping, drain-budget timing in `/home/eugene/projects/go/pgman-proxy/internal/runtime/runtime_test.go`
- [ ] T027 Implement `cmd/pgman-proxy/main.go`: parse flags via T012 loader; wire obs (T015–T018), cluster (T020–T021), and runtime (T023–T025) into a `pg-manager` `manager.Manager` plus `Manager.Proxy().Start(ctx)` per the reference assembly in `../pg-manager/examples/three_node_nats/main.go` at `/home/eugene/projects/go/pgman-proxy/cmd/pgman-proxy/main.go`

**Checkpoint**: `pgman-proxy --version` runs; `pgman-proxy --print-config --config <example>` round-trips a parsed config; the binary starts, opens `/healthz`, and reports `not ready` until cluster wiring is exercised by US1.

---

## Phase 3: User Story 1 — Stand up a 3-peer proxy cluster (Priority: P1) 🎯 MVP

**Goal**: Three `pgman-proxy` peers in front of an existing pg-manager cluster route writes to the current leader regardless of which peer the client connects to. No VIP. Failover is observable within 5s.

**Independent Test**: Deploy three peers against a `three_node_nats`-style PostgreSQL cluster, run a `BEGIN; INSERT ...; COMMIT;` workload through each peer in turn, and verify all writes reach the leader. Kill the leader, reconnect through any peer, and verify writes continue to land on the new leader within the failover budget.

### Tests for User Story 1 (write FIRST; ensure FAIL before implementation)

- [ ] T028 [P] [US1] Build the integration-test docker-compose harness (3 PG + 1 NATS + 3 pgman-proxy) at `/home/eugene/projects/go/pgman-proxy/tests/integration/docker-compose.test.yml`
- [ ] T029 [P] [US1] Integration test for happy-path proxy traffic (simple-query roundtrip through every peer) at `/home/eugene/projects/go/pgman-proxy/tests/integration/happy_path_test.go`
- [ ] T030 [P] [US1] Integration test for forced failover within 5s p99 (SC-002): kill leader, verify new leader reachable through any peer at `/home/eugene/projects/go/pgman-proxy/tests/integration/failover_test.go`
- [ ] T031 [P] [US1] Integration test for NATS outage on one peer: `/readyz` flips to 503 and writes refused locally per FR-011 at `/home/eugene/projects/go/pgman-proxy/tests/integration/nats_outage_test.go`
- [ ] T032 [P] [US1] Integration test for fail-closed startup (NATS unreachable, missing TLS material, listen-port busy → exit codes per `contracts/lifecycle.md`) at `/home/eugene/projects/go/pgman-proxy/tests/integration/startup_failclose_test.go`
- [ ] T033 [P] [US1] Performance benchmark for the <1ms p99 overhead baseline (SC-003) at `/home/eugene/projects/go/pgman-proxy/tests/integration/perf_test.go`

### Implementation for User Story 1

- [ ] T034 [US1] Wire `pg-manager.ProxyConfig{ListenAddr, DialTimeout, OnSwitchPolicy}` into `manager.Config` in `/home/eugene/projects/go/pgman-proxy/internal/runtime/start.go` (extends T023)
- [ ] T035 [US1] Wire `Manager.Proxy().Start(ctx)` on its own goroutine and route its error into `runtime.shutdown` in `/home/eugene/projects/go/pgman-proxy/internal/runtime/start.go`
- [ ] T036 [US1] Plumb the coordination-event subscriber (T021) into the obs metrics + log emitters per FR-006 in `/home/eugene/projects/go/pgman-proxy/internal/cluster/events.go`
- [ ] T037 [US1] Implement readiness-state transitions (`/readyz` 200 ⇄ 503 on NATS-up + listener-up + manager-past-singleton) in `/home/eugene/projects/go/pgman-proxy/internal/obs/health.go` (extends T017)
- [ ] T038 [US1] Add the `pgman-proxy` service entry to the existing `pg-manager`-style compose file referenced by T028 so the test harness brings the whole topology up together at `/home/eugene/projects/go/pgman-proxy/tests/integration/docker-compose.test.yml`

**Checkpoint**: All US1 tests pass against `make integration`. `psql` through any peer routes writes to the current leader; failover within 5s p99; fail-closed on NATS unreachable.

---

## Phase 4: User Story 2 — Sidecar deployment mode (Priority: P2)

**Goal**: The same binary works in standalone, microservice, and sidecar deployment modes — distinguished only by configuration. Sidecar defaults bind loopback so only the colocated app reaches the proxy.

**Independent Test**: Run a single PostgreSQL instance with a colocated `pgman-proxy` under the same supervisor; confirm `host=127.0.0.1 port=6432` works for local apps and external clients are refused. Run the same binary on a separate host (microservice) and confirm cross-host clients connect successfully.

### Tests for User Story 2

- [ ] T039 [P] [US2] Smoke test for **standalone** deployment (single peer, healthy `/readyz`, clean SIGTERM exit) at `/home/eugene/projects/go/pgman-proxy/tests/smoke/standalone_test.go`
- [ ] T040 [P] [US2] Smoke test for **microservice** deployment (3 peers, all-interfaces bind, cross-host client) at `/home/eugene/projects/go/pgman-proxy/tests/smoke/microservice_test.go`
- [ ] T041 [P] [US2] Smoke test for **sidecar** deployment (loopback default; off-host client refused; supervisor restart of sibling process recovers) at `/home/eugene/projects/go/pgman-proxy/tests/smoke/sidecar_test.go`

### Implementation for User Story 2

- [ ] T042 [US2] Add mode-aware listener defaults — sidecar=loopback for both data-plane and control-plane; standalone+microservice=all-interfaces — in `/home/eugene/projects/go/pgman-proxy/internal/config/defaults.go`
- [ ] T043 [P] [US2] Document the supervisor-restart and lifecycle expectations for each mode at `/home/eugene/projects/go/pgman-proxy/deploy/systemd/README.md`
- [ ] T044 [P] [US2] Add a sidecar-specific compose snippet (PG + pgman-proxy as siblings under one tini PID-1) at `/home/eugene/projects/go/pgman-proxy/deploy/compose/sidecar.yml`

**Checkpoint**: All three smoke tests pass. The same binary runs in all three modes with no code-path divergence; `--print-config` shows the mode-aware defaults applied correctly.

---

## Phase 5: User Story 4 — Full HA-cluster Lifecycle Management (Priority: P2)

**Goal**: An authenticated control-plane HTTP API exposes every `pg-manager` `Manager` LCM method (`Status`, `Diagnose`, `Switchover`, `Failover`, `Promote`, `Fence`, `Unfence`, `UpdateTopology`, `TriggerBackup`, `PrepareUpgrade`, `ExecuteUpgrade`). All LCM logic lives in `pg-manager`; this repo only adds request decoding, auth, leader-routing, audit, and result encoding.

**Independent Test**: From empty data directories, bootstrap a 3-peer cluster using only the control plane (no manual `initdb`). Then drive Status → Switchover → Fence → Unfence → UpdateTopology → TriggerBackup → PrepareUpgrade → ExecuteUpgrade end-to-end and confirm each emits a dual-sink audit record and is observable via metrics.

### Tests for User Story 4

- [ ] T045 [P] [US4] Integration test for fresh-cluster bootstrap from empty `PGDATA` (FR-023, SC-009) at `/home/eugene/projects/go/pgman-proxy/tests/integration/lcm_bootstrap_test.go`
- [ ] T046 [P] [US4] Integration test for `Switchover` (target=peer-b) — completes in <15s p99 (SC-012); audit on slog AND NATS subject; result returned only after new leader confirmed at `/home/eugene/projects/go/pgman-proxy/tests/integration/lcm_switchover_test.go`
- [ ] T047 [P] [US4] Integration test for `Fence` then `Unfence` of a misreplicating peer at `/home/eugene/projects/go/pgman-proxy/tests/integration/lcm_fence_test.go`
- [ ] T048 [P] [US4] Integration test for `TriggerBackup` rejecting with `backup_executor_missing` when no `BackupExecutor` is wired (FR-030) at `/home/eugene/projects/go/pgman-proxy/tests/integration/lcm_backup_missing_test.go`
- [ ] T049 [P] [US4] Integration test for leader-routing in BOTH `forward` and `redirect` modes (SC-011): mutating op submitted to a non-leader peer must reach the leader, never execute locally at `/home/eugene/projects/go/pgman-proxy/tests/integration/lcm_leader_route_test.go`
- [ ] T050 [P] [US4] Integration test for audit fail-closed (FR-028): black-hole the NATS audit subject; assert `Switchover` returns `audit_unavailable` while `Status` still succeeds at `/home/eugene/projects/go/pgman-proxy/tests/integration/lcm_audit_failclose_test.go`
- [ ] T051 [P] [US4] Integration test for token rotation without restart (FR-031): rotate `control.auth.token_file`; new token accepted, old token rejected, no process restart at `/home/eugene/projects/go/pgman-proxy/tests/integration/lcm_token_rotation_test.go`
- [ ] T052 [P] [US4] Integration test for SC-010 audit completeness — every LCM request (accepted/rejected/failed) produces records on BOTH sinks at `/home/eugene/projects/go/pgman-proxy/tests/integration/lcm_audit_complete_test.go`
- [ ] T052a [P] [US4] Integration test for FR-033 control-plane TLS — non-loopback bind without `control.tls.cert_file/key_file` and without `plaintext_explicit_ack` MUST exit `EX_CONFIG`; with TLS configured, an HTTPS client succeeds and an HTTP client is rejected at `/home/eugene/projects/go/pgman-proxy/tests/integration/lcm_tls_required_test.go`
- [ ] T052b [P] [US4] Integration test for FR-033 plaintext ack — `control.tls.plaintext_explicit_ack: true` permits plaintext bind on a non-loopback address AND emits the documented `WARN` startup log line at `/home/eugene/projects/go/pgman-proxy/tests/integration/lcm_tls_ack_test.go`
- [ ] T052c [P] [US4] Integration test for FR-034 leader-route timeout — black-hole the forward-mode reply path; assert `Switchover` returns HTTP 504 with `error.code = "leader_route_timeout"` within `control.leader_route_timeout` ± 1s; the audit record carries `leader_at_request` and `outcome = "failed"` at `/home/eugene/projects/go/pgman-proxy/tests/integration/lcm_leader_route_timeout_test.go`
- [ ] T052d [P] [US4] Integration test for FR-029 bootstrap-and-transition rejection — submit `Switchover` before `/readyz=200` (expect `cluster_bootstrapping`); submit `Switchover` mid-failover (expect `leadership_in_transition`) at `/home/eugene/projects/go/pgman-proxy/tests/integration/lcm_transient_refusal_test.go`

### Implementation for User Story 4

- [ ] T053 [US4] Implement HTTP server with middleware stack (request-ID via `oklog/ulid/v2`, panic recovery, slog access middleware) AND TLS bring-up per FR-033 (cert/key from `control.tls.cert_file`/`key_file`; loopback-only bind permits plaintext; non-loopback plaintext requires `control.tls.plaintext_explicit_ack` and emits the documented `WARN` startup log line) per `contracts/lcm.md` in `/home/eugene/projects/go/pgman-proxy/internal/control/server.go`
- [ ] T054 [P] [US4] Implement bearer-token authentication with hot rotation and constant-time compare (`crypto/subtle`) per FR-025 + FR-031 in `/home/eugene/projects/go/pgman-proxy/internal/control/auth.go`
- [ ] T055 [P] [US4] Implement dual-sink audit emitter (slog + NATS subject `pgman_proxy.<cluster_id>.audit.lcm`) with fail-closed-on-mutating-ops behaviour (FR-028) in `/home/eugene/projects/go/pgman-proxy/internal/control/audit.go`
- [ ] T056 [P] [US4] Implement leader-routing helper — `forward` mode publishes on `pgman_proxy.<cluster>.lcm.request.<op>` and awaits reply with `control.leader_route_timeout` budget (FR-034; on timeout return `leader_route_timeout`/HTTP 504 and audit `leader_at_request`); `redirect` mode returns `307` with `Location` per FR-026 in `/home/eugene/projects/go/pgman-proxy/internal/control/route.go`
- [ ] T057 [US4] Implement read handlers (`Status`, `Diagnose`) — no leader routing — at `/home/eugene/projects/go/pgman-proxy/internal/control/handlers_read.go`
- [ ] T058 [P] [US4] Implement membership-mutation handlers (`Switchover`, `Failover`, `Promote`, `Fence`, `Unfence`) at `/home/eugene/projects/go/pgman-proxy/internal/control/handlers_membership.go`
- [ ] T059 [P] [US4] Implement `UpdateTopology` handler at `/home/eugene/projects/go/pgman-proxy/internal/control/handlers_topology.go`
- [ ] T060 [P] [US4] Implement `TriggerBackup` handler with FR-030 missing-executor rejection at `/home/eugene/projects/go/pgman-proxy/internal/control/handlers_backup.go`
- [ ] T061 [P] [US4] Implement `PrepareUpgrade` and `ExecuteUpgrade` handlers at `/home/eugene/projects/go/pgman-proxy/internal/control/handlers_upgrade.go`
- [ ] T062 [US4] Wire control-plane bind into the startup-gate sequence as step #10 + initial-audit-emit verification as step #11 per `contracts/lifecycle.md` in `/home/eugene/projects/go/pgman-proxy/internal/runtime/start.go`
- [ ] T063 [US4] Wire control-plane stop as the FIRST step of graceful shutdown (before data-plane listener close) per `contracts/lifecycle.md` in `/home/eugene/projects/go/pgman-proxy/internal/runtime/shutdown.go`
- [ ] T064 [US4] Add `pgman_proxy_lcm_*` metrics (counters, histograms, gauge) per `contracts/observability.md` § LCM control-plane metrics in `/home/eugene/projects/go/pgman-proxy/internal/obs/metrics.go`
- [ ] T065 [P] [US4] Add unit tests for control-plane (auth, audit dual-sink, leader-route forward/redirect, error-code mapping, response envelope) in `/home/eugene/projects/go/pgman-proxy/internal/control/control_test.go`
- [ ] T066 [P] [US4] Add the `BackupExecutor`-shaped configuration knob and its loader handling in `/home/eugene/projects/go/pgman-proxy/internal/config/backup.go`
- [ ] T067 [P] [US4] Reference filesystem `BackupExecutor` example (out-of-tree; not a default build dep) at `/home/eugene/projects/go/pgman-proxy/examples/backup-fs/main.go`

**Checkpoint**: All US4 tests pass. Fresh-cluster bootstrap works without manual `initdb`. Switchover audit appears on both sinks. Audit-pipeline outage fails closed. Token rotation works without restart.

---

## Phase 6: User Story 3 — Observe coordination and routing (Priority: P3)

**Goal**: An operator can answer "who is leader right now?", "when was the last failover?", and "is any peer running with a stale lease?" using only metric and structured-log output, in under a minute, without reading source code (SC-004).

**Independent Test**: Connect a Prometheus scraper and a JSON log collector to a running 3-peer cluster, trigger a leader failover, and confirm the operator can answer the three incident-call questions from observability output alone.

### Tests for User Story 3

- [ ] T068 [P] [US3] Integration test asserting an operator can answer the three incident-response questions from metrics+logs only (SC-004) at `/home/eugene/projects/go/pgman-proxy/tests/integration/obs_incident_test.go`
- [ ] T069 [P] [US3] Integration test for W3C trace-context propagation across the proxy hop and into NATS message headers and the `coordination event` log line at `/home/eugene/projects/go/pgman-proxy/tests/integration/obs_trace_test.go`
- [ ] T070 [P] [US3] Integration test asserting every required log-event name from `contracts/observability.md` § Required event names is emitted at the correct level with the documented fields at `/home/eugene/projects/go/pgman-proxy/tests/integration/obs_log_schema_test.go`

### Implementation for User Story 3

- [ ] T071 [US3] Audit and complete the metric set per `contracts/observability.md` (every required metric name, type, label, and bucket present) — fix gaps in `/home/eugene/projects/go/pgman-proxy/internal/obs/metrics.go`
- [ ] T072 [P] [US3] Audit and complete the log-event-name table — every required event emitted at the documented level with required fields — across `/home/eugene/projects/go/pgman-proxy/internal/{config,cluster,runtime,obs,control}/`
- [ ] T073 [P] [US3] Wire trace-context propagation: inbound HTTP `traceparent` header on `/v1/...` and `/healthz`/`/readyz`/`/metrics`; outbound NATS message headers (where the schema permits) in `/home/eugene/projects/go/pgman-proxy/internal/{obs/tracer.go,cluster/events.go,control/server.go}`

**Checkpoint**: All three incident-response questions answerable from metrics+logs alone. Trace-context propagates end-to-end. The full event-name and metric-name tables from `contracts/observability.md` are covered.

---

## Phase 7: Polish & Cross-Cutting Concerns

**Purpose**: Lock in non-negotiable invariants, ship the release surface, and verify the deploy quickstart end-to-end.

- [ ] T074 [P] CI grep gate enforcing SC-006 (no `kubernetes`, `helm`, `crd`, `controller-runtime`, `admission-webhook` references) at `/home/eugene/projects/go/pgman-proxy/.github/workflows/scope-gate.yml`
- [ ] T075 [P] CI grep gate enforcing SC-013 (no `initdb`, `pg_basebackup`, `pg_rewind`, `pg_upgrade`, `pg_ctl promote`, replication-slot DDL in source — engine work belongs to `pg-manager`) at `/home/eugene/projects/go/pgman-proxy/.github/workflows/lcm-discipline-gate.yml`
- [ ] T076 [P] Run the performance benchmark on the local-loopback baseline and update `README.md` with the measured number; flag any deviation > 10% per the constitution's performance baseline at `/home/eugene/projects/go/pgman-proxy/README.md`
- [ ] T077 [P] Build and publish the distroless OCI image via `goreleaser`; verify it runs as non-root (FR-013) at `/home/eugene/projects/go/pgman-proxy/.goreleaser.yaml`
- [ ] T078 [P] Write `CHANGELOG.md` v1.0.0 entry covering the data-plane proxy + LCM control plane scope at `/home/eugene/projects/go/pgman-proxy/CHANGELOG.md`
- [ ] T079 [P] Add `CODEOWNERS` referencing reviewers responsible for wire-fidelity (Principle I), auth (Principle II), and coordination (Principle III) per the constitution's Code review requirements at `/home/eugene/projects/go/pgman-proxy/CODEOWNERS`
- [ ] T080 Run `quickstart.md` end-to-end against a fresh container host: verify SC-001 (deploy in <15min) and SC-009 (bootstrap in <10min); record the timing in the README
- [ ] T081 [P] Final pass over `data-model.md` and every contract under `contracts/` to confirm no field renames, metric renames, or topic renames slipped in undocumented (Constitution V: stable observability schema)

**Checkpoint**: All CI gates green. Distroless image published. README baselines recorded. Quickstart timing recorded.

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: starts immediately.
- **Foundational (Phase 2)**: depends on Setup. **BLOCKS all user stories.**
- **US1 (Phase 3)**: depends on Foundational. The MVP — ship after this passes.
- **US2 (Phase 4)**: depends on Foundational. Independent of US1 functionally; smoke tests assume US1's binary works, so practically run after US1 stabilises.
- **US4 (Phase 5)**: depends on Foundational + US1 (LCM bootstrap test exercises the data plane).
- **US3 (Phase 6)**: depends on Foundational + US1 + US4 (operability story spans both surfaces).
- **Polish (Phase 7)**: depends on every prior phase being green.

### Within-Phase Sequencing

- **Foundational**: T011 → {T012,T013,T014} ; T015 → {T016,T017,T018,T019} ; T020 → {T021,T022} ; T023 → {T024,T025,T026} ; T027 depends on T011 + T015 + T020 + T023.
- **Each user-story phase**: tests (T028+ etc.) MUST be written first and FAIL before implementation begins; models/wiring before handlers; handlers before integration.
- **Within US4**: T053 (server) → {T054,T055,T056} ; T057 + T058–T061 (handlers) depend on T053–T056 ; T062–T063 depend on all handlers being present ; T064 [P] with handlers ; T067 (example) [P] with everything.

### Parallel Opportunities

- All `[P]`-marked tasks within a phase can run concurrently when dependencies are met.
- All US tests within a phase are `[P]`-eligible after the harness (T028) is up.
- T021 (event subscriber) is `[P]` with T020 (cluster wiring) only because they live in different files and the subscriber takes the connection as a parameter; if you collapse them into one file, drop the `[P]`.

### Cross-Story Independence

- US1, US2, and US4 are functionally independent **after Foundational**. With multiple developers:
  - Dev A: US1 (data-plane proxy + failover)
  - Dev B: US2 (mode-aware defaults + smoke tests)
  - Dev C: US4 (control plane)
  - Dev D: Foundational completion + observability primitives that all three need
- US3 depends on US4 because it audits the LCM observability surface; do US3 last among the user-story phases.

---

## Implementation Strategy

### MVP First (US1 Only)

1. Phase 1: Setup
2. Phase 2: Foundational
3. Phase 3: US1 (data-plane proxy + leader-aware routing + failover)
4. **STOP and VALIDATE**: integration tests pass against a real 3-peer compose topology
5. Ship MVP if the only requirement is data-plane

### Incremental Delivery

1. Setup + Foundational → infrastructure ready
2. + US1 → MVP shippable (data-plane proxy)
3. + US2 → all three deployment topologies covered
4. + US4 → full HA-cluster lifecycle management
5. + US3 → operability locked in
6. + Polish → CI gates, release surface, perf baseline recorded

### Parallel Team Strategy

After Foundational completes:

- Pair A picks up US1 (data plane) — owns `tests/integration/{happy_path,failover,nats_outage,startup_failclose,perf}_test.go` + `internal/runtime/start.go` proxy wiring.
- Pair B picks up US2 (sidecar/standalone/microservice smoke) — owns `tests/smoke/*` + `internal/config/defaults.go`.
- Pair C picks up US4 (LCM control plane) — owns `internal/control/*` + `tests/integration/lcm_*_test.go`.
- Reconvene for US3 (observability completeness) and Polish (CI gates, release).

---

## Notes

- **Tests required per Constitution VI**: No coordination or LCM PR merges without an integration test against real Postgres + real NATS in CI.
- **Sequential commits**: Commit after each task or logical group; the checkbox in `tasks.md` is the running checkpoint of work-in-progress.
- **Stop-at-checkpoint**: At each phase checkpoint, run the integration suite for that story; do not proceed past failures.
- **Avoid**: vague tasks, same-file edits in `[P]` tasks (use sequential), cross-story dependencies that break independence (each story should remain demoable on its own).
- **Constitution gates** to confirm at each PR: I (wire fidelity), II (fail-closed), III (lease verified at action), IV (no LCM logic in this repo), V (stable obs schema), VI (real PG + NATS in tests), VII (no k8s/Helm).
