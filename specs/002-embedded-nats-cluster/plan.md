# Implementation Plan: Embedded NATS Cluster for pgman-proxy Coordination

**Branch**: `002-embedded-nats-cluster` | **Date**: 2026-05-09 | **Spec**: [spec.md](./spec.md)
**Input**: Feature specification from `/specs/002-embedded-nats-cluster/spec.md`

## Summary

Replace the external-NATS dependency established by feature 001 with a NATS
server **linked into every `pgman-proxy` process**. Each peer boots its own
embedded NATS server during proxy startup, and the embedded servers mesh into
a single NATS cluster via NATS routes — the same `pg-manager`
leadership/state-store/event-bus adapters consume the in-process server as if
it were external. The four clarifications resolved during `/speckit-clarify`
shape the design:

- Cluster-route auth: **shared username + password** sourced via SecretRef
  (RD-001a amended this from the original Q1 NKey design after `/speckit-
  implement` Phase 2 surfaced that NATS server v2.14 cluster routes only
  support a single shared credential). Per-peer identity in audit logs
  comes from the NATS server-name field (set to the pgman-proxy node ID).
- Cluster-route TLS: **required on any non-loopback bind**; loopback may
  remain plaintext; the only escape hatch is a named, audit-logged
  `cluster.tls.plaintext_explicit_ack` opt-in (mirrors 001 FR-033).
- JetStream replication factor: **auto-derived from declared cluster size**
  (R=1/2/3 for 1/2/3+ peers, capped at 3); explicit override requires a
  named, audit-logged opt-in.
- Hot-reload: **SIGHUP-scoped to peer-routes and the cluster password only**
  (the authorized-keys allow-list from the original Q1 design was dropped
  by RD-001a); every other config key is startup-only in v1.

The change is constitutionally meaningful: 001's FR-003 ("MUST NOT embed its
own NATS server in production builds") is reversed, and the constitution's
*Architecture Overview* + *Topology & Dependencies* subsection (which today
describe NATS as operator-provisioned) need a MINOR amendment to v1.2.0 (no
principle is removed or redefined; the architecture is broadened). The
amendment lands as a Phase 0 deliverable in this plan.

The new code in this repository is bounded to:

- `internal/embedded/` — the embedded NATS server lifecycle (start, ready,
  reload, drain, stop), configuration assembly, cluster-credential + TLS
  wiring (RD-001a), and the pre-create-bucket-with-replicas dance (or:
  consume a new pg-manager `WithReplicas` adapter option, if that lands
  upstream first — see research.md decision RD-002).
- Edits in `internal/config/` to remove the external-NATS URL key, add the
  `cluster.{listen, routes, tls, nkey, authorized_keys, size,
  replication_factor_override}` schema, and gate the hot-reload surface.
- Edits in `internal/runtime/` to add a SIGHUP handler that re-reads only the
  hot-reload-eligible keys and calls into the embedded server's `Reload()`.
- Edits in `internal/cluster/` to dial the in-process NATS at the loopback
  client-listener address rather than an external URL.
- Edits in `internal/obs/` to emit the new lifecycle events (server-start,
  server-ready, route-up, route-down, server-stop, storage-degraded,
  reload-applied) and the new metrics (`pgman_proxy_embedded_nats_*`).

No wire-protocol code, no consensus code, no LCM logic, no Kubernetes/Helm
code. The embedded server is the **upstream `nats-server` Go module linked
in**, configured programmatically — we do not fork, vendor a divergent copy,
or substitute a custom implementation (Constitution IV applies to NATS by
analogy: same scaffold-vs-engine principle).

## Technical Context

**Language/Version**: Go 1.25+ (matches `pg-manager` go.mod minimum, unchanged
from 001).

**Primary Dependencies (new in this feature)**:

- `github.com/nats-io/nats-server/v2` — the **embedded NATS server**. New
  direct dependency; today only the `nats.go` *client* is in `go.mod`.
- `github.com/nats-io/nkeys` — already present transitively from `nats.go`;
  promoted to direct for NKey seed/public-key handling and signing.
- `github.com/nats-io/jwt/v2` — only if Phase 0 research determines we want
  account-scoped NKey configuration (see RD-001). Default plan: skip; raw
  NKey + cluster auth is sufficient for v1.

**Primary Dependencies (carried from 001, unchanged)**:

- `github.com/f1bonacc1/pg-manager` (engine; `Manager`, `Topology`, `Policy`,
  `proxy.Proxy`).
- `github.com/f1bonacc1/pg-manager/adapters/nats` (Leadership, StateStore,
  EventBus). One pg-manager-side change is **expected** as a prerequisite —
  see RD-002 in research.md: the adapter must accept a `Replicas` option on
  KV bucket creation, otherwise the FR-011a replication-factor table cannot
  be honoured.
- `github.com/nats-io/nats.go` — still the client used by the pg-manager
  adapter and by the proxy's coordination subscribers.
- `github.com/prometheus/client_golang` — metrics surface, including new
  `pgman_proxy_embedded_nats_*` series.
- Standard library: `flag`, `os/signal` (now also handling `SIGHUP`),
  `net/http`, `log/slog`, `crypto/tls`.

**Storage**: PER-PEER LOCAL DISK becomes load-bearing. The embedded NATS
server's JetStream KV requires a writable directory (e.g., `/var/lib/pgman-
proxy/jetstream/<cluster_id>/`). Defaults: in-memory in single-peer mode for
dev ergonomics (FR-011); on-disk in any multi-peer shape (FR-011a R≥2 needs
durability). Disk-full → fail-closed health-degraded event (spec edge case).

**Testing**:

- `go test` for unit tests (config delta, hot-reload diff computation, NKey
  seed/public-key parsing, replication-factor derivation table).
- Integration tests: docker-compose harness reusing `../pg-manager`'s
  fixtures, but **with the external `nats` service removed**. The proxy
  container hosts NATS in-process. Three peer containers form the cluster
  via routes published on each container's `:6222` port.
- New integration scenarios under `tests/integration/`:
  - `embedded_cluster_test.go` — 3-peer mesh forms; leader elected; psql
    writes route to leader (mirrors 001 US1 but with no external NATS).
  - `nkey_rotation_test.go` — execute the FR-010a three-step rotation
    procedure; assert quorum maintained at every step.
  - `cluster_tls_test.go` — TLS-required-on-non-loopback enforcement; the
    `plaintext_explicit_ack` opt-in path; misconfig → fail-closed.
  - `replica_factor_test.go` — start clusters of sizes 1, 2, 3, 5; assert
    the KV stream's actual `Replicas` field matches the derivation table.
  - `sighup_reload_test.go` — add a peer to the routes list + reload =
    the cluster meshes the new peer without restart; reload of an
    ineligible key is ignored with a structured warning.
- Smoke tests: the existing `tests/smoke/{standalone,microservice,sidecar}_
  test.go` harness from 001 stays in place; each is updated to drop the
  external `nats` container and assert no `nats-server` process exists on
  the test host (SC-001 verifiability).
- Constitution VI (NON-NEGOTIABLE): every embedded-NATS test runs against a
  real PostgreSQL backend and a real (in-process) NATS server. No mocks of
  NATS in the embedded-cluster paths.

**Target Platform**: Linux x86_64 / arm64 (unchanged from 001). Embedding
NATS does not change platform support.

**Project Type**: CLI / single-binary network service. Single Go module.
Single `cmd/pgman-proxy` entrypoint. No additional binaries shipped (e.g.,
no separate `pgman-nkeygen` CLI in v1 — operators use `nats-io/nkeys`
upstream tooling, or a `pgman-proxy nkey-gen` subcommand if RD-003 lands —
default plan: subcommand approach for ergonomics).

**Performance Goals (delta from 001)**:

- 001 baselines hold: per-query overhead < 1 ms p99; failover < 5 s p99
  (001 SC-002); restart MTTR < 10 s (001 SC-007).
- New: embedded NATS server idle overhead per peer:
  - **Memory**: 60 MB RSS p95 cap at idle (single-peer, no JetStream
    persistence). Justified in research.md from upstream nats-server idle
    benchmarks — fits in the project's tightest sidecar slot (a 256 MB pod
    with PostgreSQL + pgman-proxy already needs ~150 MB headroom for
    PostgreSQL, leaving comfortable margin).
  - **CPU**: < 0.5% of one core p95 at idle.
  - The numbers are derived in research.md (RD-005) and committed to in
    SC-005 of the spec.
- New: 3-peer rolling restart for a binary upgrade end-to-end **under 60 s**
  (SC-008 commitment); per-peer drain + restart + re-mesh + leader
  re-confirm budget is 20 s. Derived in RD-006.
- New: SIGHUP reload latency: route diff applied in **under 1 s p99** on a
  3-peer cluster.

**Constraints (delta from 001)**:

- The previous "NATS unreachable on startup → exit non-zero" rule (001
  FR-010) now means **embedded NATS startup failure → exit non-zero**.
  Listener-port collisions, TLS-material-missing, NKey-seed-missing,
  storage-path-unwritable all map to fail-closed.
- New: cluster-routes listener on a non-loopback address without TLS or
  without NKey configured = fail-closed (FR-010b, FR-009). The named opt-in
  is the only path; absence of opt-in = fail.
- New: configuration containing the legacy `nats.url` key = fail-closed at
  validation with a migration-pointing error (FR-002, SC-009).
- Carried from 001 unchanged: no Kubernetes API client, no Helm, no CRDs,
  no virtual IPs, no LCM logic in this repo, run cleanly under non-root.

**Scale/Scope (delta from 001)**:

- Cluster size: 1–5 peers supported; recommended 3. R=3 cap means 4-peer
  and 5-peer clusters get the same fault tolerance as 3-peer (1 peer loss).
  Rationale in research.md (RD-004).
- Connection scale per peer: 10k concurrent client connections (unchanged
  from 001). The embedded NATS server is on a separate listener used only
  by in-process pg-manager adapters; client traffic does not pass through
  it.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

Constitution: `.specify/memory/constitution.md` v1.1.0 — **with a planned
amendment to v1.2.0 as a Phase 0 deliverable** (research.md RD-008). The
amendment broadens the *Architecture Overview* and the *Topology &
Dependencies* subsection of *Additional Constraints* to reflect the
embedded-NATS topology. No principle is removed or redefined; the
amendment is MINOR semantic-versioning per the constitution's own rules.

| Principle                                            | Status     | How this plan complies                                                                                                                                                                                                                                                                                          |
|------------------------------------------------------|------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| I. Wire-Protocol Fidelity                            | PASS       | The PostgreSQL data-plane is unchanged. The embedded NATS server is an internal coordination plane; it never touches the PostgreSQL wire path. SC-013 grep gate from 001 still applies (no PG DDL/`pg_basebackup`/etc. in this repo).                                                                                                                                                       |
| II. Fail-Closed Safety                               | PASS       | Embedded-server startup failures are fatal (FR-007). Non-loopback bind without TLS material → fail (FR-010b). Non-loopback bind without NKey → fail (FR-009). Multi-peer config with empty routes → fail (FR-008). Legacy `nats.url` present → fail (FR-002). Disk-full at runtime → self-fence (Constitution III still applies).                                                                                                  |
| III. Active/Active Coordination Correctness          | PASS       | The same `pg-manager/adapters/nats.{Leadership,StateStore,EventBus}` are used; the embedded server is invisible to them. The replication-factor derivation (FR-011a) ensures coordination state has the right durability for the cluster size — a stricter ratchet than 001 had, which silently used the adapter's R=1 default. Self-fencing on lease/storage failure unchanged.                                              |
| IV. Thin Scaffold over pg-manager                    | PASS       | One upstream prerequisite is filed against `pg-manager`: `adapters/nats.WithReplicas(int)` option for the KV bucket creation in `bucket.go`. Without it, the proxy would have to pre-create the bucket itself, which arguably duplicates engine-side responsibility. RD-002 documents both paths and selects the upstream-first option as the constitutional default; the fallback path is documented as the second-best.                                                                                                                                                                                                                |
| V. Observability by Default                          | PASS       | New structured events: `embedded_nats.server_started`, `…server_ready`, `…route_up`, `…route_down`, `…server_stopped`, `…storage_degraded`, `…reload_applied`. New metrics: `pgman_proxy_embedded_nats_routes_meshed`, `…replicas_factor`, `…storage_bytes`, `…sighup_reload_outcome_total{result}`, `…lifecycle_events_total{event}`. Audit log captures NKey public-key prefix on route-accept (FR-010a). Field schema in contracts/observability.md. |
| VI. Integration-First Testing (NON-NEGOTIABLE)       | PASS       | Five new integration tests (listed under Testing) plus three updated smoke tests. All run real PostgreSQL + real (in-process) NATS. No mocks introduced for embedded-cluster paths.                                                                                                                                                                                                                                                                                                                            |
| VII. Scope Discipline & Reversibility                | **JUSTIFIED VIOLATION (logged below)** | The feature **removes a configuration knob** (`nats.url` external-NATS path) — a non-additive change. Per Principle VII, "configuration keys MUST have a default that preserves prior behavior." This violation is justified because (a) the project has no production deployments yet (001 is committed but not released), (b) the change is the explicit point of the feature, (c) removal is loud at validation (FR-002, SC-009 — operators get a clear migration error, not silent failure), and (d) the constitution amendment makes the new topology canonical. Tracked in **Complexity Tracking** below. |

**Result: PASS with one tracked violation in Complexity Tracking.** The
violation is the deliberate removal of the `nats.url` external-NATS path
required by 001 FR-003. The constitution amendment to v1.2.0 (Phase 0
deliverable) ratifies the architecture change; SC-009 ensures the breaking
change is loud rather than silent.

## Project Structure

### Documentation (this feature)

```text
specs/002-embedded-nats-cluster/
├── plan.md              # This file
├── spec.md              # Feature specification (clarifications integrated)
├── research.md          # Phase 0 output (this command)
├── data-model.md        # Phase 1 output (this command) — config + lifecycle entities
├── quickstart.md        # Phase 1 output (this command) — operator workflow
├── contracts/           # Phase 1 output (this command)
│   ├── config.md            # Configuration schema delta from 001
│   ├── observability.md     # New events/metrics schema for embedded NATS
│   ├── lifecycle.md         # SIGHUP/SIGTERM/SIGINT semantics, exit codes
│   ├── nkey-credentials.md  # NKey seed format, authorized-keys format, rotation procedure
│   └── constitution-amendment.md  # v1.1.0 → v1.2.0 amendment text
├── checklists/
│   └── requirements.md  # Spec-quality checklist (clarifications resolved)
└── tasks.md             # Phase 2 output (NOT created by /speckit-plan)
```

### Source Code (repository root)

```text
pgman-proxy/
├── cmd/
│   └── pgman-proxy/
│       ├── main.go                  # (existing) wires embedded server before pg-manager adapters
│       └── nkey_subcmd.go           # (NEW) `pgman-proxy nkey-gen` operator helper (RD-003)
├── internal/
│   ├── config/                      # (existing)
│   │   ├── config.go                # (edit) remove NATSConfig.URL; add ClusterConfig {…}
│   │   ├── validate.go              # (edit) reject legacy nats.url; validate NKey + TLS combinations
│   │   ├── loader.go                # (edit) add SIGHUP-scoped reload path
│   │   ├── defaults.go              # (edit) default cluster.client_listen=loopback; routes=disabled in 1-peer
│   │   └── config_test.go           # (edit) coverage for new schema, reload-eligibility table
│   ├── cluster/                     # (existing) — pg-manager adapter wiring
│   │   ├── cluster.go               # (edit) Connect() now dials cluster.client_listen instead of cfg.NATS.URL
│   │   └── events.go                # (existing) — unchanged
│   ├── embedded/                    # (NEW) — embedded NATS server lifecycle
│   │   ├── server.go                # Start/Stop, options assembly from config.ClusterConfig
│   │   ├── options.go               # NATS server.Options builder (cluster, routes, TLS, NKey, JS storage)
│   │   ├── replicas.go              # Replication-factor derivation from declared cluster size
│   │   ├── reload.go                # SIGHUP-scoped diff + s.Reload() + structured event emit
│   │   ├── nkey.go                  # NKey seed loading, authorized-keys parsing, public-key redaction
│   │   ├── bucket.go                # Pre-create KV bucket with desired Replicas (or no-op if pg-manager exposes WithReplicas)
│   │   └── embedded_test.go
│   ├── runtime/                     # (existing)
│   │   ├── start.go                 # (edit) start embedded server BEFORE pg-manager adapters; gate on ready
│   │   ├── exit.go                  # (edit) drain embedded server in shutdown sequence
│   │   ├── reload.go                # (NEW) SIGHUP handler → embedded.Reload(...)
│   │   └── runtime_test.go          # (edit) reload semantics
│   ├── obs/                         # (existing)
│   │   ├── metrics.go               # (edit) add pgman_proxy_embedded_nats_* series
│   │   ├── logger.go                # (existing) — unchanged surface
│   │   └── obs_test.go              # (edit) coverage for new event names + redaction rules
│   └── control/                     # (existing) — unchanged surface; Status response gains cluster.embedded_nats {…} block
├── tests/
│   ├── integration/                 # (existing) + 5 new files listed above
│   │   ├── docker-compose.test.yml  # (edit) drop the `nats` service
│   │   ├── Dockerfile               # (edit) drop the `nats-server` install step
│   │   └── embedded_cluster_test.go # (NEW)
│   │   └── nkey_rotation_test.go    # (NEW)
│   │   └── cluster_tls_test.go      # (NEW)
│   │   └── replica_factor_test.go   # (NEW)
│   │   └── sighup_reload_test.go    # (NEW)
│   └── smoke/                       # (existing) — three files updated
├── deploy/                          # (existing) — recipes adjusted
│   ├── compose/
│   │   └── docker-compose.yml       # (edit) drop the nats service; expose 6222/tcp on each peer
│   └── systemd/
│       └── pgman-proxy.service      # (edit) ExecReload=/bin/kill -HUP $MAINPID
├── go.mod                           # (edit) add github.com/nats-io/nats-server/v2; promote nkeys to direct
├── go.sum                           # (edit) regenerated
├── README.md                        # (edit) cite v1.2.0 constitution; remove "operator-provisioned NATS" sentence
├── CLAUDE.md                        # (edit) point at this plan file (handled in step 3)
└── .specify/                        # (existing) — speckit harness
```

**Structure Decision**: Same single-project Go module layout as 001, with one
new `internal/` package — `internal/embedded/` — that owns the embedded NATS
server's lifecycle, options assembly, NKey wiring, TLS wiring, replication-
factor derivation, and SIGHUP reload diff. Putting it under `internal/`
preserves Constitution IV's hard fence: nothing outside this module can
import from it. The package boundary is the smallest one that keeps the
embedded-server concerns out of the existing `cluster/` (which is purely
pg-manager adapter wiring) and out of `runtime/` (which is process
lifecycle): two existing packages stay focused, and one new package owns
the new responsibility.

## Complexity Tracking

| Violation | Why Needed | Simpler Alternative Rejected Because |
|-----------|------------|-------------------------------------|
| Constitution VII reversibility — removal of the `nats.url` config key is non-additive (no default preserves prior behavior; instead, presence of the old key triggers fail-closed). | This is the entire point of the feature. The user explicitly directed that external NATS is "a mistake" and is to be removed, not toggled (Q1 of `/speckit-clarify` did not even consider the toggle option — it was never on the table). The spec captures this as FR-002 + SC-009: legacy config is rejected at validation with a migration-pointing error so the breaking change is **loud**, not silent. | Toggle/dual-mode (keep `nats.url` as an opt-in fallback): rejected because (a) the user explicitly disclaimed external NATS as a mistake, (b) two coordination paths means double the test surface and a second long-tail of misconfiguration modes, (c) the very feature one is buying — operational simplicity of one-binary-no-broker — is undone by the existence of an external mode. Silent ignore of legacy `nats.url`: rejected because operators must be told their old config is no longer wired (Constitution V observability). |
| Constitution IV (Thin Scaffold over pg-manager) — `internal/embedded/bucket.go` (T017) pre-creates the JetStream KV bucket from this repo with the cluster-size-derived `Replicas` (FR-011a) **before** `pg-manager`'s `adapters/nats.ensureBucket` runs. This duplicates knowledge of pg-manager's bucket-naming convention (`pgmgr_<sanitized-cluster-id>`) inside the proxy. | Per RD-002, the constitutional path is to extend `pg-manager/adapters/nats` with a `WithReplicas(int)` option upstream, then have the proxy pass the derived value through that API. Recorded in T007 of `tasks.md`. **The fallback is taken in this implementation session because the upstream PR cannot be driven from this repo's automation context** — filing the issue/PR against the sibling `pg-manager` module and waiting for it to merge would block the entire feature. The fallback is a workaround, not a design: when upstream `WithReplicas` lands, T017 collapses to a pass-through and this exception is removed. | Wait for upstream: rejected because it serialises feature 002 behind sibling-repo work that has its own review cycle; the user has explicitly chosen forward progress. Hardcode R=1 (skip the table): rejected because it makes FR-011a a lie and undoes the partition-resilience guarantee Q3 clarification was specifically chosen for. |
