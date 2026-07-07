# Tasks: Upgrade Test Coverage (Minor & Major, Happy & Bad Paths)

**Input**: Design documents from `/specs/004-upgrade-tests/`
**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/upgrade-endpoints.md, quickstart.md

**Tests**: This feature IS tests — every "implementation" task below writes test code. The two
new files are `internal/control/handlers_upgrade_test.go` (contract tier, `fakeEngine`) and
`tests/integration/lcm_upgrade_test.go` (integration tier, `//go:build integration`, compose
harness). Zero production source changes are permitted (verified in Polish phase).

**Organization**: Tasks are grouped by user story. Tasks touching the same file are sequential
within a phase; `[P]` marks tasks that can proceed in parallel because they live in different
files with no pending dependencies.

## Format: `[ID] [P?] [Story] Description`

## Path Conventions

Single Go module at repo root. Contract tier: `internal/control/` (no build tag, runs in per-PR
CI). Integration tier: `tests/integration/` (`integration` build tag, runs via `make
integration` against the docker-compose 3-peer topology).

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Green baseline + file skeletons so every later task has a compilable home.

- [X] T001 Record the green baseline: run `go test -race -count=1 ./internal/control/` and `make integration`; both must pass before any new test lands, and note the integration suite's wall-clock time in the PR description (baseline for SC-005's +20% budget check in T021)
- [X] T002 [P] Create contract-tier skeleton `internal/control/handlers_upgrade_test.go`: `package control`, license header, doc comment naming feature 004 and the Constitution VI fault-injection carve-out; compiles empty against existing `fakeEngine`/`newTestServer` from `control_test.go`
- [X] T003 [P] Create integration-tier skeleton `tests/integration/lcm_upgrade_test.go`: license header, `//go:build integration`, `package integration`, doc comment naming feature 004 and pointing at `specs/004-upgrade-tests/contracts/upgrade-endpoints.md`; compiles empty against the harness (`Peers()`, `callLCM`, `lcmResponse` from `harness_test.go`/`lcm_helpers_test.go`)

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Shared helpers both bad-path and happy-path tests need. MUST complete before any user story phase.

- [X] T004 In `tests/integration/lcm_upgrade_test.go` add shared helpers: (a) `currentServerVersion(t)` — connect via `Peer.DSN()`, parse `SHOW server_version` into (major, minor) ints; (b) `upgradePlanBody(strategy, targetMajor, targetMinor)` — marshal `{"plan": pgmanager.UpgradePlan{...}}` using the typed struct (research R8: no json tags, exported names); (c) leader discovery — reuse the existing pattern the LCM tests use to find the current leader peer (e.g. status probe via `callLCM`); extract, don't duplicate, if an existing helper already does this
- [X] T005 [P] In `internal/control/handlers_upgrade_test.go` add contract-tier fixtures: a `postUpgrade(t, srv, path, body)` helper wrapping the existing `newTestServer` + `httptest` request pattern from `control_test.go`, decoding the response envelope (operation, outcome, error.code, error.message) for assertion; include an engine-call recorder struct capturing the `UpgradePlan` and `PreSwap` passed to `prepareUpgradeFn`/`executeUpgradeFn`

**Checkpoint**: Both skeletons compile with helpers; `go vet ./...` clean.

---

## Phase 3: User Story 1 — Minor upgrade happy path is proven (Priority: P1) 🎯 MVP

**Goal**: Pin the operator's core promise: a valid minor plan passes the dry-run, and executing
it drives the real stop → (no-op) swap → start → version-confirm sequence against live
PostgreSQL 17, returning `accepted` (spec US1; FR-001, FR-002, FR-011).

**Independent Test**: `go test -tags=integration -run 'TestUpgrade_Minor' ./tests/integration/`
passes on its own against a freshly booted compose topology.

- [X] T006 [US1] In `tests/integration/lcm_upgrade_test.go` write `TestUpgradePrepare_MinorAccepted` (FR-001 / US1-AS1): probe current version (T004), build minor plan (same major, same-or-current minor), POST `/v1/upgrade/prepare` to the leader via `callLCM`, assert HTTP 200 + envelope `operation:"PrepareUpgrade"`, `outcome:"accepted"`, no error
- [X] T007 [US1] In `tests/integration/lcm_upgrade_test.go` write `TestUpgradeExecute_MinorHappyPath` (FR-002, FR-011 / US1-AS2..AS4): POST `/v1/upgrade/execute` with the same-version minor plan (research R2) bounded by a `PerNodeBudget`, assert `accepted`; then poll the data plane (`Peer.DSN()` query loop, generous deadline per research R3) until queries succeed again; finally assert `SHOW server_version` equals the pre-upgrade version and the cluster is healthy; use `t.Cleanup` to leave the topology usable for later tests
- [X] T008 [US1] Flake-proof the happy path (research R3 risk): run `go test -tags=integration -count=3 -run 'TestUpgrade(Prepare_Minor|Execute_MinorHappyPath)' ./tests/integration/` — 3/3 green required; if flaky, fix within the test (longer recovery poll, `PerNodeBudget` tuning) — never by weakening assertions

**Checkpoint**: US1 delivers standalone value — the runbook's core minor-upgrade promise is pinned against a real server. MVP complete.

---

## Phase 4: User Story 2 — Dangerous or broken minor upgrades fail safely (Priority: P2)

**Goal**: Pin every documented refusal and failure-surfacing behavior: downgrades and
cross-major minor plans refused (real engine), injected swap-failure / version-mismatch
surfaced with message intact (fakeEngine — Constitution VI carve-out), malformed bodies
rejected pre-engine, forwarding relays outcomes verbatim (spec US2; FR-003..006, FR-009,
FR-010, FR-013).

**Independent Test**: `go test -race -run 'TestUpgrade' ./internal/control/` plus
`go test -tags=integration -run 'TestUpgradePrepare_Rejects' ./tests/integration/` pass without
any US1 test existing.

Contract tier — all in `internal/control/handlers_upgrade_test.go`, sequential (same file):

- [X] T009 [P] [US2] Write `TestUpgradeExecute_PlanPassthrough` (FR-013 boundary, research R5): POST a fully-populated minor plan (Strategy, TargetMajor, TargetMinor, NodeOrder, PerNodeBudget) to `/v1/upgrade/execute`; via the T005 recorder assert the engine received it field-for-field intact and the supplied `PreSwap` is non-nil; repeat for `/v1/upgrade/prepare` (plan only)
- [X] T010 [US2] Write `TestUpgradeExecute_SwapFailureSurfaced` (FR-003 / US2-AS3): `executeUpgradeFn` returns an upstream-shaped pre-swap error; assert HTTP 500, `outcome:"failed"`, `error.code:"engine_error"`, `error.message` contains the injected text verbatim (data-model outcome mapping)
- [X] T011 [US2] Write `TestUpgradeExecute_VersionMismatchSurfaced` (FR-004 / US2-AS4): same mechanism with a version-mismatch-shaped error; same envelope assertions
- [X] T012 [US2] Write `TestUpgrade_MalformedBodyRejected` (FR-009 / US2-AS5): for BOTH `/v1/upgrade/prepare` and `/v1/upgrade/execute`, send undecodable JSON and an empty body; assert HTTP 400, `outcome:"rejected"`, `error.code:"invalid_argument"`, AND that neither engine hook fired (recorder untouched)
- [X] T013 [US2] Write `TestUpgrade_ForwardedToLeader` (FR-010 / US2-AS6): with `fakeLeader{leader:false}` in forward mode (model on the existing forwarding tests in `control_test.go`), assert (a) a success envelope from the leader is relayed verbatim, (b) a failure envelope is relayed verbatim, (c) `ErrLeaderRouteTimeout` maps to HTTP 504 / `error.code:"leader_route_timeout"` (edge case: unreachable peer)

Integration tier — in `tests/integration/lcm_upgrade_test.go`:

- [X] T014 [P] [US2] Write `TestUpgradePrepare_RejectsDowngrade` (FR-005 / US2-AS1): build a minor plan targeting a version lower than the live one; assert prepare returns `failed`/`engine_error` with the engine's downgrade reason; then assert `/v1/upgrade/execute` with the same plan also refuses (engine re-validates) and the data plane never blips (no restart occurred)
- [X] T015 [US2] Write `TestUpgradePrepare_RejectsCrossMajorMinorAndUnknownStrategy` (FR-006 / US2-AS2 + data-model validation rules): (a) minor-strategy plan with `TargetMajor = current+1` → refused; (b) plan with an unknown strategy value (e.g. 9) → refused; assert envelope `failed`/`engine_error` in both cases
- [X] T016 [US2] Checkpoint run: `go test -race -run 'TestUpgrade' ./internal/control/` and `go test -tags=integration -run 'TestUpgradePrepare_Rejects' ./tests/integration/` — all green

**Checkpoint**: All minor-path refusals and failure surfacing pinned; US1 + US2 independently green.

---

## Phase 5: User Story 3 — Major upgrade behavior is pinned: planning works, execution is gated (Priority: P3)

**Goal**: Pin the major-upgrade contract exactly as it exists today: well-formed major plans
validate, non-higher targets are refused, execution of either major strategy fails with the
exact upstream gate string — the sentinel that fires when pg-manager v0.7.0 lifts the gate
(spec US3; FR-007, FR-008).

**Independent Test**: `go test -tags=integration -run 'TestUpgradeMajor' ./tests/integration/`
passes on its own; deliberately editing the expected gate string makes it fail.

All in `tests/integration/lcm_upgrade_test.go`, sequential (same file):

- [X] T017 [US3] Write `TestUpgradeMajorPrepare_Accepted` (FR-007 / US3-AS1): major-in-place plan with `TargetMajor = current+1`; assert prepare returns 200 `accepted` (planning works today)
- [X] T018 [US3] Write `TestUpgradeMajorPrepare_RejectsNonHigherMajor` (US3-AS2): major-in-place plans with `TargetMajor = current` and `TargetMajor = current-1`; assert both refused (`failed`/`engine_error`)
- [X] T019 [US3] Write `TestUpgradeMajorExecute_GateSentinel` (FR-008 / US3-AS3..AS4, research R4): execute a well-formed major-in-place plan AND a major-logical-bridge plan; for both assert HTTP 500, `outcome:"failed"`, `error.code:"engine_error"`, `error.message` contains the EXACT string `upgrade: major strategies wired but gated on v0.7.0`; then assert nothing changed (data plane serving, version unchanged); comment in-test that this is the sentinel — on failure after a pg-manager bump, follow `contracts/upgrade-endpoints.md` "Sentinel obligations", do not loosen in place
- [X] T020 [US3] Checkpoint run: `go test -tags=integration -run 'TestUpgradeMajor' ./tests/integration/` — green; temporarily mutate the expected sentinel string and confirm the test FAILS (proves the sentinel can fire), then restore

**Checkpoint**: All three stories independently green; the full upgrade matrix {minor, major} × {happy, bad} is covered (SC-001).

---

## Phase 6: Polish & Cross-Cutting Concerns

**Purpose**: Prove the suite-level success criteria and the test-only constraint.

- [X] T021 Full-suite verification: `go test -race -count=1 ./internal/...` and `make integration` green; compare integration wall clock against the T001 baseline — must be within +20% (SC-005); record both numbers in the PR description
- [X] T022 [P] Test-only guarantee: `git diff --stat main -- cmd internal ':!*_test.go'` shows zero production changes; `make grep-gates` clean (lcm-discipline-gate: test files exempt, production untouched)
- [X] T023 [P] Traceability sweep: verify every FR-001..FR-013 maps to at least one passing test exactly as the data-model.md Test Matrix claims (update the matrix if names drifted); spot-check SC-002 by temporarily breaking plan pass-through (e.g. zero the plan in a scratch build) and confirming a test fails, then restore; confirm the runbook behaviors in `docs/upgrade-orchestration.md` §"Major-version upgrades (gated)" and the minor checklist are each covered by an assertion (SC-004)
- [X] T024 Run `specs/004-upgrade-tests/quickstart.md` end-to-end as written (both tiers, narrowed `-run` patterns work, sentinel guidance accurate); fix the doc if any command drifts

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: no dependencies
- **Foundational (Phase 2)**: needs T002/T003 skeletons; BLOCKS all user stories
- **US1 (Phase 3)**: needs T004; independent of US2/US3
- **US2 (Phase 4)**: needs T004 + T005; independent of US1/US3
- **US3 (Phase 5)**: needs T004; independent of US1/US2
- **Polish (Phase 6)**: needs all desired stories complete

### User Story Dependencies

None between stories — each is a self-contained set of test functions. They share the two
files, so same-file tasks serialize, but any story can be delivered (and its tests merged)
without the others existing.

### Parallel Opportunities

- T002 ∥ T003 (different files); T004 ∥ T005 (different files)
- Contract-tier chain (T009→T013) ∥ integration-tier chain (T014→T015) — different files
- After Foundational: US1, US2-contract, US2-integration, US3 are four independent work streams
  (US1, US2-integration, US3 serialize with each other only if worked in the same file
  simultaneously; distinct test functions make merge conflicts trivial)
- T022 ∥ T023 in Polish

### Parallel Example: after Phase 2 completes

```bash
# Stream A (contract tier):    T009 T010 T011 T012 T013
# Stream B (integration tier): T006 T007 T008, then T014 T015, then T017 T018 T019
# Streams A and B never touch the same file.
```

---

## Implementation Strategy

**MVP first**: Phases 1–3 only (T001–T008) already deliver the highest-value artifact — the
minor happy path pinned against a real server. Stop, validate, ship if needed.

**Incremental**: add US2 (bad-path safety net), then US3 (gate sentinel), then Polish. Each
checkpoint leaves CI green and every merged test meaningful on its own.

**Total**: 24 tasks — Setup 3, Foundational 2, US1 3, US2 8, US3 4, Polish 4.
