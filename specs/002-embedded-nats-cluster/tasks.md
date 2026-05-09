---

description: "Tasks for feature 002 — Embedded NATS Cluster for pgman-proxy Coordination"
---

# Tasks: Embedded NATS Cluster for pgman-proxy Coordination

**Input**: Design documents from `/specs/002-embedded-nats-cluster/`
**Prerequisites**: plan.md (✓), spec.md (✓), research.md (✓; see RD-001a re: cluster-credential pivot), data-model.md (✓), contracts/ (✓ — config, observability, lifecycle, **cluster-credentials** (was nkey-credentials, renamed during /speckit-implement Phase 2), constitution-amendment), quickstart.md (✓)

**Tests**: Included. Constitution VI is NON-NEGOTIABLE for this project; every coordination surface ships with a real-PG + real-NATS integration test in CI.

**Organization**: Tasks are grouped by user story so each can be implemented and tested independently. Both US1 and US2 are P1 — together they form the MVP.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies on incomplete tasks)
- **[Story]**: Which user story this task belongs to (US1, US2, US3)
- File paths in descriptions are absolute repository-relative

## Path Conventions

Single Go module at repository root:
- New code: `internal/embedded/`, `cmd/pgman-proxy/cluster_secret_subcmd.go` (was nkey_subcmd.go pre-RD-001a)
- Edited code: `internal/{config,cluster,runtime,obs,control}/`, `cmd/pgman-proxy/main.go`, `tests/integration/`, `tests/smoke/`, `deploy/`
- Documentation: `README.md`, `specs/002-embedded-nats-cluster/spec.md` (back-port two SC numbers)

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Apply the constitution amendment first, pull in the new dependency, and strip the external-NATS scaffolding from test/deploy harnesses.

- [X] T001 Apply the constitution amendment v1.1.0 → v1.2.0 by editing `.specify/memory/constitution.md` per `specs/002-embedded-nats-cluster/contracts/constitution-amendment.md`: prepend the new Sync Impact Report entry, replace the Architecture Overview paragraph, replace the *Topology & Dependencies* subsection, and bump the footer line. Commit as a standalone change so no later commit on this branch is silently un-constitutional. **MUST land before any code changes in this branch.**
- [X] T002 Add `github.com/nats-io/nats-server/v2` to `go.mod` and promote `github.com/nats-io/nkeys` from indirect to direct in `go.mod`. Run `go mod tidy` so `go.sum` is regenerated. **Done**: nats-server/v2 v2.14.0 + transitive deps (jwt/v2, highwayhash, go-tpm, time, crypto/sys/text upgrades) added; will materialize as direct when `internal/embedded/server.go` lands in T016.
- [X] T003 [P] Drop the external `nats` service from `tests/integration/docker-compose.test.yml`; expose `6222/tcp` on each `pgman-proxy` peer container so cluster routes can mesh.
- [X] T004 [P] Drop the `nats-server` install step from `tests/integration/Dockerfile` (currently shown modified in `git status`); replace with the build of the `pgman-proxy` binary that already embeds NATS.
- [X] T005 [P] Update `deploy/compose/docker-compose.yml` to drop the external `nats` service and expose `6222/tcp` per peer.
- [X] T006 [P] Add `ExecReload=/bin/kill -HUP $MAINPID` to `deploy/systemd/pgman-proxy.service` so `systemctl reload pgman-proxy` triggers FR-014a hot-reload.

**Checkpoint**: Constitution is on v1.2.0; module declares the embedded NATS dependency; no external NATS process referenced in deploy or test harnesses.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Resolve the upstream `pg-manager` prerequisite, replace `NATSConfig` with `ClusterConfig`, and stand up the `internal/embedded/` package skeleton. **No user-story work can begin until this phase is complete.**

- [ ] T007 Resolve RD-002 (pg-manager `WithReplicas` upstream prerequisite): file an issue against `github.com/f1bonacc1/pg-manager` requesting a `WithReplicas(int)` option on `adapters/nats` constructors that threads into `js.CreateKeyValue(jetstream.KeyValueConfig{... Replicas: n})` in `adapters/nats/bucket.go`. Open a paired PR if bandwidth allows. If the upstream change does not land before T018, switch T018 to the documented fallback (pre-create the bucket from this repo) and log the Constitution-IV exception in `plan.md` Complexity Tracking.
- [X] T008 In `internal/config/config.go`: extend the existing `ClusterConfig` (currently just `ID`) with the new fields per `specs/002-embedded-nats-cluster/data-model.md` and `contracts/config.md`: `Name`, `DeclaredSize`, `ClientListen` (Endpoint), `RoutesListen` (Endpoint), `RoutePeers []string`, `TLS` (TLSConfig), `Username`, `Password` (SecretRef), `JetStreamDir`, `ReplicationFactorOverride`, `ConnectTimeout`, `ReconnectWait`. Replace `NATSConfig` with this expanded `ClusterConfig`. Reuse `SecretRef` from 001. **Note (RD-001a)**: drop the per-peer `nkey_seed_source` + `authorized_keys` fields planned at /speckit-clarify time; cluster-route auth is shared username/password instead.
- [X] T009 In `internal/config/validate.go`: implement the validation rules from `contracts/config.md` validation matrix (legacy `nats.*` rejection, multi-peer-with-empty-peers, non-loopback-without-credentials, non-loopback-without-TLS-without-ack, declared_size≥2-without-jetstream_dir, jetstream_dir-unwritable, self-loop-warn-and-exclude, replica-override-audit, password-shorter-than-16-bytes, password-inline-plaintext-rejection).
- [X] T010 In `internal/config/defaults.go`: set `client_listen=127.0.0.1:4222`, `routes_listen=0.0.0.0:6222` in HA, `routes_listen.enabled=false` in single-peer, `tls.plaintext_explicit_ack=false`, `connect_timeout=5s`, `reconnect_wait=2s`.
- [X] T011 In `internal/config/loader.go`: add `cluster.*` flag/env mappings; remove `nats` flag binding; explicit reject of `nats.*` keys with the migration error from `contracts/config.md`.
- [X] T012 Update `internal/config/config_test.go` to cover the new schema and the validation matrix from `contracts/config.md` (one Go subtest per row).
- [X] T013 [P] Create `internal/embedded/options.go`: a pure function `BuildOptions(cfg ClusterConfig, cred ClusterCredential) (*server.Options, error)` that translates `ClusterConfig` + a resolved `ClusterCredential` into NATS `server.Options` (server-name = node_id, cluster.Name = cluster_name, cluster.Username/Password = the resolved credential, cluster.Routes = peer URLs, cluster.TLSConfig from TLSConfig, JetStream StoreDir from JetStreamDir). No I/O; pure assembly.
- [X] T014 [P] Create `internal/embedded/replicas.go`: implement `DeriveReplicas(declaredSize int) (replicas int, warning string)` per RD-004 with the four branches (panic on ≤0, R=1 for size=1, R=2+warning for size=2, R=3 for size≥3). Add `replication_factor_override` handling that emits a warning when set. **Done**: file created with `DeriveReplicas` plus `DecideReplicas`/`ReplicaDecision` for the override-audit path. Compiles clean.
- [X] T015 [P] **(SUPERSEDED by RD-001a — implemented as `credentials.go`)** ~~Create `internal/embedded/nkey.go`~~ → `internal/embedded/credentials.go` already created during /speckit-implement Phase 2: loads `ClusterCredential` (shared username + password) via `SecretRef`, validates ≥ 16-byte password, exposes `Redact()` for safe-for-logging form, and `GenerateClusterPassword()` for the operator-facing subcommand. Replaces the per-peer NKey machinery the original task assumed.
- [X] T016 Create `internal/embedded/server.go`: `EmbeddedServer` struct (per `data-model.md`), `Start(ctx, opts) error` calling `server.NewServer` + `go s.Start()` + blocking on `ReadyForConnections(timeout)`; `Shutdown(ctx)` blocking up to a deadline; `Reload(newOpts)` shim. Emit lifecycle events (`server_started`, `server_ready`, `server_stopped`) via the existing `internal/obs` logger using the schema from `contracts/observability.md`.
- [X] T017 Create `internal/embedded/bucket.go`: pre-create the cluster KV bucket with `Replicas: <derived>` if T007's upstream `WithReplicas` did NOT land in time; if it did, this file is a no-op forwarder. The bucket name MUST match `bucketName(clusterID)` from `pg-manager/adapters/nats/bucket.go` (re-state the format here as a contract — RD-002 fallback caveat).
- [X] T018 Update `internal/cluster/cluster.go`: change `Connect(...)` to dial `nats://<cfg.Cluster.ClientListen.Host>:<port>` instead of `cfg.NATS.URL`. Drop creds-file / token-env wiring (no longer in config). Carry forward all other adapter wiring unchanged (FR-015).
- [X] T019 Update `internal/runtime/start.go` per `contracts/lifecycle.md` startup sequence: validate config → build embedded options → pre-create bucket if applicable → start embedded server → wait ready → start pg-manager adapters → start data-plane → start control-plane → mark ready. Map exit codes 78/75/73 to the failure modes specified.
- [X] T020 Update `internal/runtime/exit.go` per `contracts/lifecycle.md` shutdown sequence: drain data-plane → stop control-plane → release pg-manager leadership → `s.Shutdown()` on embedded server → flush logs → exit 0.
- [X] T021 [P] Update `internal/obs/metrics.go` to register the eight new `pgman_proxy_embedded_nats_*` metrics from `contracts/observability.md` (`up`, `routes_meshed`, `replicas_factor` with `overridden` label, `storage_bytes`, `storage_degraded` with `kind` label, `lifecycle_events_total`, `route_auth_failures_total`, `sighup_reload_outcomes_total`).
- [X] T022 [P] Add unit tests in `internal/embedded/embedded_test.go` covering: `BuildOptions` golden output for one-peer / three-peer / TLS-on / credentialed cases (T013); `DeriveReplicas` table tests covering sizes 1/2/3/5 plus override (T014); `LoadClusterCredential` validation paths and `Redact()` output (T015 / RD-001a).

**Checkpoint**: A peer can start with the new config schema, the embedded NATS server boots, and pg-manager adapters dial loopback. Single-peer 1-of-1 leader election works (sets up US2). Multi-peer mesh untested at this point.

---

## Phase 3: User Story 1 — 3-peer cluster with no external NATS (Priority: P1) 🎯 MVP-A

**Goal**: Three `pgman-proxy` peers stand up a meshed embedded-NATS cluster, elect one leader, and route writes from any peer to that leader — with zero `nats-server` process anywhere on any host.

**Independent Test**: Run `tests/integration/embedded_cluster_test.go`: bring up three peers with the quickstart-style config, assert `routes_meshed=2` on each, run `psql` writes through each peer, assert all writes commit. Re-run the test suite from feature 001 unchanged against this topology — every 001 integration test must pass (SC-006).

### Implementation for User Story 1

- [X] T023 [US1] Add `cmd/pgman-proxy/cluster_secret_subcmd.go` (was nkey_subcmd.go pre-RD-001a): `pgman-proxy cluster-secret-gen` subcommand calling `embedded.GenerateClusterPassword()` and printing the resulting base32-lowercase password to stdout per `contracts/cluster-credentials.md`.
- [X] T024 [US1] Wire shared cluster credential + cluster-name + TLS configuration into the `BuildOptions` call site in `internal/embedded/server.go`: server-name = node_id, cluster.Name = cluster_name, cluster.Username/Password from the resolved `ClusterCredential`, cluster.TLSConfig = nil OR a `*tls.Config` built from `TLSConfig`. Emit `cluster_route.auth_failed` events on inbound auth rejection. Per RD-001a, this is upstream NATS's only supported cluster-route auth shape.
- [X] T025 [US1] In `internal/embedded/server.go`, surface route-up / route-down events with `peer_node_id` (from the sibling's NATS server-name) + `password_prefix` (8-char redacted password fingerprint, per RD-001a's amended FR-010a) per `contracts/observability.md`. NATS server emits route events through its event-loop hooks; subscribe and translate.
- [X] T026 [US1] Update `internal/control/handlers_read.go` to add the `cluster.embedded_nats` sub-block to `Status` responses per `contracts/observability.md` (server_name, ready, listen addrs, tls_enabled, routes_meshed, replicas_factor, replicas_overridden, jetstream_storage_bytes, storage_degraded, last_route_up_at, last_route_down_at, last_reload_at).

### Tests for User Story 1

- [X] T027 [P] [US1] Create `tests/integration/embedded_cluster_test.go`: 3-peer mesh formation; assert `routes_meshed=2` on each peer; assert exactly one leader after `connect_timeout`; assert `psql` writes through each peer commit; assert no `nats-server` process exists in any container (SC-001 + SC-006 verifications).
- [X] T028 [P] [US1] Create `tests/integration/cluster_tls_test.go`: (a) non-loopback bind without TLS material → fail-closed at startup with exit code 78; (b) `plaintext_explicit_ack=true` + non-loopback bind succeeds with audit-logged warning at every startup; (c) cert-mismatch peer is rejected by sibling with `cluster_route.auth_failed{kind="tls_error"}`.
- [X] T029 [P] [US1] Create `tests/integration/replica_factor_test.go`: start clusters of sizes 1, 2, 3, and 5; query the embedded NATS server's stream info for the cluster KV bucket; assert `Replicas` field equals 1, 2, 3, 3 respectively; assert override path sets `Replicas` to the override value AND the `replicas_overridden` audit-log line is present at startup.

**Checkpoint**: US1 is fully functional and testable independently. The MVP-A increment is shippable: a 3-peer cluster with no external NATS, leader-aware write routing, and the same observability surface as 001.

---

## Phase 4: User Story 2 — Single-peer with no external dependencies (Priority: P1) 🎯 MVP-B

**Goal**: One `pgman-proxy` peer runs as a self-contained binary — no external NATS, no peers — exercising the **same** coordination code paths as the HA topology so single-peer is not a separate code path.

**Independent Test**: Run `tests/integration/single_peer_test.go`: start one peer with `declared_size=1` and empty peers; assert it reaches ready in under 5 s; assert `psql` connects through the proxy; assert `routes_meshed=0` and `replicas_factor=1`; assert clean SIGTERM shutdown leaves no orphan files.

### Implementation for User Story 2

- [X] T030 [US2] In `internal/config/validate.go`: implement the warn-and-ignore rule for `declared_size=1` + non-empty `peers` (per `data-model.md` rule "1+non-empty → Warn + ignore peers"). Emit a structured warning at startup; do NOT pass the peer URLs into `BuildOptions`.
- [X] T031 [US2] In `internal/config/defaults.go`: when `declared_size=1`, allow `jetstream_dir` to be empty → in-memory JetStream storage in `BuildOptions` (single-peer ergonomics, FR-011).
- [X] T032 [US2] In `internal/embedded/options.go`: when `declared_size=1`, default `routes_listen.enabled=false` (no peer needs to dial in) and skip the cluster-route options entirely. Same `EmbeddedServer.Start` code path; just a thinner options struct.
- [X] T033 [US2] Update `tests/smoke/standalone_test.go`: drop the external `nats` container expectation; assert no `nats-server` process exists; assert SC-002 (single-peer ready in under 5 s on a developer-laptop-class host).

### Tests for User Story 2

- [X] T034 [P] [US2] Create `tests/integration/single_peer_test.go`: assert ready under 5 s p99 (SC-002); assert `routes_meshed=0` and `replicas_factor=1`; assert `psql` writes through the loopback proxy; assert SIGTERM cleanup leaves no orphan files (FR-012).

**Checkpoint**: MVP complete. Both P1 stories deliver value: 3-peer HA (US1) and single-peer dev/standalone (US2) on one binary, no separate code paths.

---

## Phase 5: User Story 3 — Survive partition / restart / upgrade with no operator NATS knowledge (Priority: P2)

**Goal**: Operators run day-2 operations on the embedded cluster — peer addition, cluster-credential rotation (single-step SIGHUP per RD-001a), rolling upgrade, partition recovery — using only `pgman-proxy`'s own surface (config + signals + control-plane). NATS CLI tools never enter the runbook.

**Independent Test**: Run the three integration scenarios named in the spec's US3 Independent Test: (a) two-minute network partition with a peer; (b) sequential `kill -9` of every peer; (c) rolling restart for a binary upgrade. After each, recovery is verifiable via metrics + structured logs + control-plane Status only — zero `nats`/`nats-server`/`nsc` invocations.

### Implementation for User Story 3

- [X] T035 [US3] Create `internal/embedded/reload.go`: implement `ComputeDiff(oldCfg, newCfg ClusterConfig) ReloadDiff` per `data-model.md` (routes_added/removed, keys_added/removed, skipped_keys for any non-allow-listed change), plus `Apply(srv *server.Server, diff ReloadDiff) error` calling `srv.ReloadOptions(...)`. Emit `embedded_nats.reload_applied` with redacted key prefixes per `contracts/observability.md`.
- [X] T036 [US3] Create `internal/runtime/reload.go`: a SIGHUP signal handler that re-reads config from the same source(s) as startup, computes a `ReloadDiff`, and calls `embedded.Apply`. Bump `pgman_proxy_embedded_nats_sighup_reload_outcomes_total{result=...}`. Per-reload latency target: 1 s p99.
- [X] T037 [US3] In `internal/embedded/server.go`: implement storage-degraded detection (watch JetStream advisories or a periodic disk-stat check on `jetstream_dir`); on degradation emit `storage_degraded` event AND mark the peer's leadership-state to fenced via the existing pg-manager adapter handle, so writes stop locally per Constitution III. Bump `storage_degraded` gauge with `kind` label.

### Tests for User Story 3

- [X] T038 [P] [US3] Add unit tests in `internal/embedded/embedded_test.go` for `ComputeDiff`: empty diff, routes-only change, keys-only change, mixed change, ineligible-key change, ineligible-key-only change.
- [X] T039 [P] [US3] Create `tests/integration/sighup_reload_test.go`: start a 3-peer cluster, add a fourth peer's address to peer-a's `peers` list, send `SIGHUP` to peer-a, assert `routes_meshed` advances to 3 within 1 s p99; in a second sub-test, change a non-allow-listed key (e.g. `client_listen.port`), send `SIGHUP`, assert `reload_applied{skipped_keys=[...]}` event with the skipped key listed and no actual change applied.
- [X] T040 [P] [US3] Create `tests/integration/credential_rotation_test.go` (was nkey_rotation_test.go pre-RD-001a): execute the FR-010a single-step rotation per `contracts/cluster-credentials.md` against a 3-peer cluster — update every peer's `cluster.password` SecretRef and SIGHUP each. Assert `routes_meshed=2` on every peer at every step (cluster never lost quorum); assert `route_up{password_prefix=<new>}` after the SIGHUP fan-out; assert the prior password prefix is absent in subsequent route-accept events.
- [X] T041 [P] [US3] Update `tests/smoke/microservice_test.go` and `tests/smoke/sidecar_test.go`: drop external `nats` container; assert no `nats-server` process; in the sidecar test specifically, assert the colocated peer's loopback `client_listen` is reachable from the local PostgreSQL container.

**Checkpoint**: All three user stories satisfied. Day-2 operations runbook is sustainable without NATS-specific operator knowledge.

---

## Phase 6: Polish & Cross-Cutting Concerns

- [X] T042 [P] Update `README.md` to cite constitution v1.2.0, remove the "operator-provisioned NATS" sentence, add the embedded-NATS topology paragraph and a pointer to `quickstart.md` for the operator workflow.
- [X] T043 [P] Back-port the SC-005 cap (60 MB RSS / <0.5% CPU) and the SC-008 cap (60 s for 3-peer rolling restart) from `plan.md` into `specs/002-embedded-nats-cluster/spec.md` Success Criteria, replacing the "TBD in plan phase" stretch baseline language.
- [X] T044 Add `tests/integration/legacy_config_test.go`: SC-009 automated regression — a config containing `nats.url` is rejected at validation with the migration message; assert exit code 78.
- [X] T045 Add a CI grep gate to the project's lint target (`Makefile` + `.github/workflows/...`): SC-007 — no `nats-server`-as-external-process references in `deploy/`, `quickstart.md`, `README.md`, or any non-test file; mirrors 001's SC-006 pattern.
- [ ] T046 Run `quickstart.md` end-to-end on a clean three-host harness; correct any step that has drifted from the real binary's behaviour; confirm SC-001 (15-minute deploy budget).
- [X] T047 [P] Run `go vet`, `staticcheck`, `gofmt -l` on the diff. Resolve every finding (Constitution Additional Constraints).

---

## Dependencies & Execution Order

### Phase Dependencies

- **Phase 1 (Setup)**: T001 is a hard blocker for every later task (constitution amendment must land first). T002 blocks T013–T022 (need the new dependency in `go.mod`). T003–T006 are independent of code tasks and can run in parallel with Phase 2.
- **Phase 2 (Foundational)**: Blocks all user-story phases. T007 (upstream pg-manager prerequisite) gates the bucket-creation strategy in T017 — if T007 doesn't land in time, T017 falls back to the documented pre-create path with a Constitution-IV exception logged in `plan.md`.
- **Phase 3 (US1)**, **Phase 4 (US2)**, **Phase 5 (US3)**: All start after Phase 2 completes. US1 and US2 can be developed in parallel (different test files; T033 in US2 touches `tests/smoke/standalone_test.go` which is not edited by US1). US3 should start after US1 because T035–T037 build on the lifecycle/options machinery US1 exercises.
- **Phase 6 (Polish)**: After all desired user stories complete.

### Within-Phase Dependencies

**Phase 2**:
- T009–T012 all touch `internal/config/`; sequential.
- T013 (options.go) ← independent of T009–T012 once `ClusterConfig` type exists from T008.
- T013, T014, T015 are different files in `internal/embedded/` and parallel.
- T016 (server.go) depends on T013 + T015.
- T017 (bucket.go) depends on T007 outcome AND T016.
- T018 (cluster.go edit) depends on T008 + T016 (server must be running to dial).
- T019 (runtime/start.go) depends on T009–T012 + T016 + T017 + T018.
- T020 (runtime/exit.go) depends on T016.
- T021 (metrics) is independent of code logic; parallel with T019/T020.
- T022 (unit tests) depends on T013/T014/T015.

**Phase 3 (US1)**:
- T023 (`cluster-secret-gen` subcommand) is independent of the others; parallelisable.
- T024 + T025 both touch `internal/embedded/server.go` indirectly; sequence them.
- T026 (control handler edit) is independent.
- T027/T028/T029 are different test files; parallel after T024–T026 land.

**Phase 4 (US2)**:
- T030/T031/T032 sequence within shared files; T033/T034 parallel after them.

**Phase 5 (US3)**:
- T035 then T036 (handler depends on diff computation).
- T037 independent of T035/T036; parallel.
- T038–T041 all parallel after T035–T037 land.

### Parallel Opportunities

- Phase 1: T003–T006 in parallel.
- Phase 2: T013/T014/T015 in parallel; T021/T022 in parallel with later Phase-2 tasks.
- Phase 3: T027/T028/T029 in parallel; T023 parallel with the implementation tasks.
- Phase 5: T038/T039/T040/T041 in parallel.
- Phase 6: T042/T043/T047 in parallel.

---

## Parallel Example: User Story 1 tests

```bash
# After T024–T026 land, run all US1 integration tests in parallel:
Task: "Create tests/integration/embedded_cluster_test.go (T027) — 3-peer mesh + leader + writes"
Task: "Create tests/integration/cluster_tls_test.go (T028) — TLS gating + plaintext_ack opt-in"
Task: "Create tests/integration/replica_factor_test.go (T029) — replicas-derivation table"
```

---

## Implementation Strategy

### MVP First (US1 + US2)

Both P1 stories ship together as the MVP — they live in the same binary, share the same code paths, and each de-risks the other.

1. Phase 1 (Setup) — constitution + dependency + harness cleanup
2. Phase 2 (Foundational) — config schema + embedded skeleton (CRITICAL; blocks both stories)
3. Phase 3 (US1) — 3-peer cluster
4. Phase 4 (US2) — single-peer
5. **STOP and VALIDATE**: run `tests/integration/embedded_cluster_test.go`, `single_peer_test.go`, plus the entire 001 test suite (SC-006) — every pre-existing test must pass without modification
6. Demo / merge

### Incremental Delivery

1. Phase 1+2 → Foundation
2. + US1 → MVP-A: 3-peer HA delivered
3. + US2 → MVP-B: single-peer dev mode delivered
4. + US3 → operational depth: rotation, hot-reload, storage-degraded handling
5. + Polish → README, docs, CI gates, quickstart end-to-end re-validation

Each phase preserves the previous phase's tests; nothing regresses.

### Parallel Team Strategy

If two developers are on this branch:

- After Phase 2 completes, Developer A picks up Phase 3 (US1), Developer B picks up Phase 4 (US2). They edit largely-disjoint files; the integration points (`internal/embedded/server.go`, `internal/embedded/options.go`) are stable by the end of Phase 2.
- Phase 5 (US3) is best held until US1 lands so the SIGHUP plumbing has a real cluster to test against.

---

## Notes

- **Constitution amendment first**: T001 is non-negotiable. It MUST be the first commit on this branch so no later commit is silently un-constitutional.
- **Upstream prerequisite (T007)**: If `pg-manager` adds `WithReplicas` upstream, T017 becomes a no-op forwarder. If not, T017 is the documented fallback and `plan.md` Complexity Tracking gains a Constitution-IV exception.
- **Tests are mandatory** in this project (Constitution VI is NON-NEGOTIABLE). Every coordination surface ships with a real-PG + real-NATS integration test.
- **Two SC numbers** are committed in plan but live in spec as TBD — T043 back-ports them so the spec's contract matches the plan's commitment.
- **No new operator persona** (FR-014) — every new operator surface (`cluster-secret-gen`, SIGHUP reload, `Status.cluster.embedded_nats`) is on the proxy itself, never on a NATS-ecosystem CLI.
- Each `[P]` task is in a different file from concurrent `[P]` siblings; verify with `git diff --stat` before parallelising.
- Commit after each task or coherent group; the branch's commit history MUST allow a reviewer to follow the constitution amendment → setup → foundation → US1 → US2 → US3 → polish progression.
