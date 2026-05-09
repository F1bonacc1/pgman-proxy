# Phase 0 Research: Active/Active PostgreSQL Proxy (pgman-proxy v1)

**Feature**: 001-active-active-pg-proxy
**Date**: 2026-05-09
**Status**: Complete — no NEEDS CLARIFICATION items remain

The spec entered the plan phase with **0** `[NEEDS CLARIFICATION]` markers
(see `checklists/requirements.md`). The research below resolves the
implementation-level unknowns derived from the Technical Context and
records the resulting decisions, rationales, and rejected alternatives.

---

## R1 — Wire-protocol execution path

**Decision**: Use `pg-manager`'s built-in `proxy.Proxy` (constructed via
`manager.Manager.Proxy()` after passing `pgmanager.ProxyConfig` in
`manager.Config`) as the byte-forwarding engine. Run it on a goroutine
inside `pgman-proxy`'s `run()` function. `pgman-proxy` owns lifecycle
(start, drain, stop), not bytes.

**Rationale**:
- The library proxy already forwards every byte verbatim, does not
  terminate TLS, and re-checks the upstream target on each new
  connection. That matches Constitution I (Wire-Protocol Fidelity) and
  Constitution IV (Thin Scaffold over pg-manager).
- Re-implementing forwarding here would duplicate ~500 LOC of tested
  code and would inevitably drift from the library's switch policy
  semantics (`SwitchHardClose` / `SwitchDrain` / `SwitchPause`).

**Alternatives considered**:
- *Build our own pgproto3-based proxy.* Rejected: violates Constitution
  IV; doubles the wire-fidelity surface to maintain.
- *Use pgbouncer + custom routing layer.* Rejected: introduces a
  second daemon, breaks the single-binary promise (FR-001), and pgbouncer
  has no native mechanism to consume pg-manager's NATS leadership state.

---

## R2 — NATS adapters & connection lifecycle

**Decision**: Use `pg-manager/adapters/nats` for `Leadership`, `StateStore`,
and `EventBus`. Open one shared `*nats.Conn` per peer with explicit
`Name`, `Timeout`, `ReconnectWait`, and a `MaxReconnects` of `-1`
(infinite) so transient disconnects do not poison the process. On
startup, refuse to proceed unless `nats.Connect` succeeds within a
bounded budget (default 10s, configurable).

**Rationale**:
- The reference example wires exactly these adapters.
- Infinite reconnect with bounded startup connect time gives us
  fail-closed behaviour at startup (FR-010) and graceful tolerance of
  transient network blips at runtime — the latter is required because
  Constitution III says the **library** self-fences on lease loss; we
  must not aggressively kill the process and lose the supervisor's
  back-off budget.

**Alternatives considered**:
- *Embed a NATS server.* Rejected: production peers must connect to an
  operator-managed NATS cluster (FR-003); embedding adds operational
  complexity and confusion about who owns what.
- *Use raw NATS subjects without the adapter package.* Rejected:
  Constitution IV — pg-manager's adapter package already encodes the
  subject layout, lease semantics, and watch behaviour.

---

## R3 — Configuration shape

**Decision**: Three-source layered config — flags > env vars > YAML
file > defaults — implemented in `internal/config`. Required keys:
`cluster_id`, `node_id`, `peers`, `nats.url`, `proxy.listen_addr`,
`postgres.bin_dir`, `postgres.data_dir`, `postgres.local_dsn`. Optional
keys cover policy timeouts, observability endpoints, and the auto-recovery
opt-ins. Validation MUST fail at startup if any required key is missing
(FR-010); secrets MUST come from env, file paths, or a secret-manager
URI — never inline (FR-017).

**Rationale**:
- Matches the env-var conventions already used by
  `three_node_nats/main.go` (`NATS_URL`, `CLUSTER_ID`, `NODE_ID`,
  `PGDATA`, `PG_BINDIR`, `LOCAL_DSN`, `PROXY_LISTEN`) so operators
  switching from the example to the productionised binary feel no
  surface change.
- YAML file is optional; flags are present for one-shot deployments and
  CI; env-vars are the default for sidecar/microservice modes (12-factor).

**Alternatives considered**:
- *TOML.* Rejected: Go ecosystem leans YAML for ops config; YAML is
  what the deploy recipes already use.
- *Hard-coded env-vars only.* Rejected: file form is needed for sidecar
  templating in non-k8s schedulers (Nomad, systemd unit drop-ins).

---

## R4 — Observability stack

**Decision**:
- **Logger**: `log/slog` with a JSON handler. Required fields per record:
  `time`, `level`, `msg`, `cluster_id`, `node_id`, `component`,
  `trace_id`, `span_id` (omitted when no trace context is in scope).
- **Metrics**: `prometheus/client_golang` registered on a dedicated HTTP
  server (default `:9090`, configurable). Reuses the standard process
  collector and Go runtime collector. Domain metrics defined in
  `contracts/observability.md`.
- **Traces**: `go.opentelemetry.io/otel` SDK with a default `noop`
  tracer; OTLP gRPC exporter activates when `otel.exporter.endpoint` is
  configured. Trace context is propagated W3C-style on inbound HTTP
  endpoints and attached to NATS message headers when the schema permits.

**Rationale**:
- `slog` is in the standard library (Go 1.21+) — no new dep, stable API.
- Prometheus is the de-facto metrics standard in this ecosystem and is
  what `pg-manager`'s downstream consumers already scrape.
- OTel is opt-in to keep the binary footprint small for operators who
  don't run a tracing backend.

**Alternatives considered**:
- *zap*. Rejected: extra dep with no advantage over `slog` for our
  field-schema needs.
- *No traces at all*. Rejected: Constitution V mandates trace-context
  propagation across the proxy hop.

---

## R5 — Health and readiness semantics

**Decision**: Two HTTP endpoints on the observability port:
`/healthz` (liveness, always returns 200 OK once the process is past
init) and `/readyz` (readiness, returns 200 only when (a) NATS connection
is up, (b) the proxy listener is accepting, and (c) `pg-manager`'s
`Manager.Start` has returned past its singleton-claim phase). On any
fail-closed condition (NATS lease lost, listener closed),
`/readyz` flips to 503 immediately so process supervisors and sidecar
liveness probes can react.

**Rationale**:
- Distinct liveness/readiness matches the convention every supervisor
  and probe runner already understands (systemd `Restart=`, Docker
  `HEALTHCHECK`, container-orchestrator probes — even though we don't
  ship k8s manifests, sidecar consumers will set probes themselves).
- The split prevents thrashing: the process is alive (don't restart it)
  but not ready to serve (don't route traffic to it).

**Alternatives considered**:
- *Single `/health` endpoint.* Rejected: muddles two distinct decisions.
- *Unix socket health probe.* Rejected: closes off remote scraping
  for the microservice topology.

---

## R6 — Switch policy default

**Decision**: Default `OnSwitchPolicy = SwitchHardClose`. Expose it as a
config key `proxy.switch_policy = hard_close|drain|pause` so operators
can opt into the alternatives without code changes.

**Rationale**:
- `SwitchHardClose` is the safest, simplest semantic: clients see a
  reset and reconnect, the library re-points all new connections at the
  new leader. The reference example uses it. Constitution II (fail-
  closed) prefers hard-close over best-effort drain.
- Opting into `Drain` or `Pause` is a deployment-tier decision; making
  it a flag, not a build-tag, satisfies "mode is configuration, not
  code" (Assumption in spec).

**Alternatives considered**:
- *Default Drain.* Rejected: holds the listener accept loop, so a
  flapping leader produces visible client-side stalls instead of a clean
  fast-fail.

---

## R7 — Test infrastructure & multi-peer harness

**Decision**: Reuse `pg-manager`'s docker-compose patterns. Add a
`tests/integration/docker-compose.test.yml` that brings up:
- 1 NATS server (single node, JetStream enabled);
- 3 PostgreSQL hosts with `pg-manager` running as a sibling (mirror of
  `three_node_nats`);
- 3 `pgman-proxy` peers each pointing at the same NATS URL and cluster ID,
  fronting the corresponding PostgreSQL host.

Tests drive the cluster via `psql` + the proxy listener. CI uses the
same compose file; `go test ./tests/integration/...` orchestrates
compose-up / compose-down per scenario via Go testcontainers or
`exec.Command("docker", "compose", ...)`.

**Rationale**:
- Constitution VI requires real PG + real NATS in coordination tests.
- Compose-based harness is what `pg-manager` already uses; reusing
  that idiom means the operator who reads our integration tests
  recognises the shape immediately.

**Alternatives considered**:
- *In-process embedded NATS server (`natsserver.Run`) for tests.*
  Acceptable for unit-level config or message-shape tests. Not
  acceptable for end-to-end coordination (Constitution VI: real-server
  happy paths required).
- *Kubernetes-based test harness.* Rejected: violates Constitution VII
  and SC-006.

---

## R8 — Deployment-mode adapters

**Decision**: All three deployment modes share the same binary and
config schema. Differentiation is purely topological:
- **Standalone**: 1 peer pointing at 1 PG host; NATS still required if
  HA is desired across reboots; a `--single-node` config shortcut sets
  `peers = [node_id]` and tightens timeouts.
- **Microservice**: N peers behind operator-managed L4/L7 load
  balancing or DNS round-robin (operator's choice); each peer
  configured identically except for `node_id` and (optionally)
  `proxy.listen_addr`.
- **Sidecar**: 1 peer per PG host, colocated under the same supervisor
  (systemd, s6, tini, or any other PID-1). Listener defaults to
  `127.0.0.1:6432` so only the local app can reach it; remote sidecars
  are reachable via the host's external interface if explicitly opened.

**Rationale**:
- Code paths must NOT diverge per mode (FR-001, US2 acceptance #3).
  Treating mode as a deployment topology — verified by smoke tests —
  keeps the binary surface honest.

**Alternatives considered**:
- *Build-tag-gated mode-specific code.* Rejected: doubles the test
  matrix, multiplies the surface to audit, breaks the single-binary
  promise.

---

## R9 — Build & release surface

**Decision**: Single Go module at the repo root, `cmd/pgman-proxy` as
the only `main` package. Release pipeline produces:
- `pgman-proxy` static Linux binaries (amd64, arm64) via `goreleaser`;
- a single OCI image (`Dockerfile` under `deploy/docker/`) built from
  the static binary on `gcr.io/distroless/static`.
No Helm chart. No k8s manifests. The OCI image is generic and works in
any container runtime; sidecar consumers reference it by digest from
their own deployment tooling.

**Rationale**:
- Distroless avoids shipping a shell or libc — smaller surface, fewer
  CVEs, no accidental privilege expansion. Aligns with FR-013 (non-root)
  and Constitution II (Fail-Closed Safety).
- `goreleaser` is the de-facto Go release tool; pg-manager already uses
  it (`.goreleaser.yaml` present in that repo).

**Alternatives considered**:
- *Alpine base image.* Rejected: larger surface, less auditable than
  distroless.
- *Per-mode container images (one per topology).* Rejected: violates
  "mode is configuration" (R8).

---

## R10 — Module path & module dependency to pg-manager

**Decision**: Module path `github.com/f1bonacc1/pgman-proxy` (matches
the GitHub user/org of `pg-manager`). During development, allow a
`replace github.com/f1bonacc1/pg-manager => ../pg-manager` entry in
`go.mod` for fast iteration; release builds MUST replace this with a
tagged version pinned in `go.sum`. CI gates the release branch on
"no `replace` directives" (Constitution: Additional Constraints).

**Rationale**:
- Same GitHub user simplifies cross-repo navigation, issue cross-linking,
  and CODEOWNERS.
- The `replace`-during-dev pattern is documented in `pg-manager`'s
  example repos and is the lowest-friction path for the initial spike.

**Alternatives considered**:
- *Vendored pg-manager source tree in this repo.* Rejected:
  Constitution IV — would invite drift; defeats the "thin scaffold"
  premise.

---

## R11 — LCM control-plane shape *(added 2026-05-09 amendment)*

**Decision**: Authenticated HTTP+JSON API on a dedicated listener
(default `:9091`, mode-aware: loopback in sidecar). Bearer-token auth
sourced from env-var or file path; tokens re-read on every request to
support hot rotation. Endpoints under `/v1/` map 1:1 to
`manager.Manager` methods. Leader-only operations are forwarded via NATS
request/reply (default mode `forward`) or returned as `307 Temporary
Redirect` (mode `redirect`). Full surface is in `contracts/lcm.md`.

**Rationale**:
- HTTP+JSON is the lowest-friction surface for the operator surface area
  we need: every CI runner, every cloud, every shell already speaks it.
- Distinct listener (separate from data-plane and obs) lets each surface
  be firewalled independently — required by Constitution II to keep the
  blast radius of a leaked control-plane token from including the
  PostgreSQL wire path.
- Tokens-per-request (rather than tokens-cached-at-process-start)
  satisfies FR-031 (rotation without restart) while keeping the simple
  bearer-token model.
- Forward-via-NATS by default keeps client tooling oblivious to leader
  identity, mirroring how the data-plane proxy already abstracts that
  away. `redirect` is offered as an opt-in for tooling that prefers to
  observe leadership state.

**Alternatives considered**:
- *gRPC + Protobuf control plane.* Rejected for v1: heavier client
  surface, harder to inspect ad-hoc. Reasonable if v2 needs streaming
  upgrade progress.
- *NATS-only RPC (no HTTP).* Rejected: would force every operator to
  install NATS client tooling for routine ops; unhelpful for
  break-glass scenarios where NATS itself is the suspect.
- *mTLS-only auth.* Deferred to a future spec; cert lifecycle is
  meaningful work and v1 prefers shipping a working surface.

---

## R12 — Audit fail-closed strictness *(added 2026-05-09 amendment)*

**Decision**: Mutating LCM operations are refused when **either** audit
sink (slog or NATS subject) cannot accept a record. Read-only
operations (`Status`, `Diagnose`) are NOT subject to this gate so
operators can still inspect a degraded cluster. The decision is implemented
in `internal/control/audit.go` and exercised by an integration test
that black-holes the NATS audit subject and asserts a `Switchover`
returns `audit_unavailable`.

**Rationale**:
- An LCM action without an audit record is effectively unaccountable.
  For a credential-path component, "fail open on audit" creates a
  silent compromise vector — exactly the failure mode Constitution II
  exists to prevent.
- Allowing read-only ops to succeed on audit failure preserves
  operator visibility during incidents (you can still ask "what's the
  state?") while refusing actions that would mutate state.

**Alternatives considered**:
- *Best-effort audit (log only, never block).* Rejected: violates
  Constitution II for the most security-sensitive surface in the
  binary.
- *Single-sink audit (slog only).* Rejected: a peer with a clogged
  log pipeline (disk-full, fluentd backlog) would silently lose audit
  records.

---

## R13 — Operator-supplied BackupExecutor *(added 2026-05-09 amendment)*

**Decision**: Ship **no** built-in backup backend in v1. The binary
exposes a configuration knob that names an operator-supplied
`BackupExecutor` adapter (filesystem, S3, custom). When no executor is
wired, `TriggerBackup` requests are rejected with
`backup_executor_missing`. A small reference filesystem-backend example
is included under `examples/backup-fs/` (out-of-tree, not a default
build dependency).

**Rationale**:
- Bundling S3 (or any cloud-specific backend) in the core binary would
  expand the dependency surface, force every operator to vendor that
  backend's transitive deps, and violate Constitution IV (Thin Scaffold
  over pg-manager) — backup backends belong outside the proxy core.
- The reference filesystem example demonstrates the wiring pattern so
  operators have a concrete starting point without committing the core
  to a specific adapter shape.

**Alternatives considered**:
- *Bundle a filesystem backend by default.* Rejected: filesystem
  backups are useful for dev, not for production HA; shipping them as
  the default would invite operators to use them in production.
- *Defer backups entirely from v1.* Rejected: the user explicitly
  asked for full LCM, and backup is the highest-value daily operation
  pg-manager already implements.

---

## Open follow-ups (NOT blocking implementation)

- **Cert rotation / ACME**: deferred per Assumptions; will need a
  separate spec when the project has a real production user.
- **Read/write splitting**: deferred per Assumptions; routing model
  remains leader-only for v1.
- **Multi-tenant proxy**: deferred per Assumptions.
- **OTel exporter selection** (gRPC vs HTTP, Tempo vs Jaeger): deferred
  to deployment-time configuration; the binary supports both via
  standard OTel env-var conventions.

All resolved unknowns above are reflected in `data-model.md`,
`contracts/*`, and `quickstart.md`.
