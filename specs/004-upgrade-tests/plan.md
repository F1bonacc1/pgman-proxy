# Implementation Plan: Upgrade Test Coverage (Minor & Major, Happy & Bad Paths)

**Branch**: `004-upgrade-tests` | **Date**: 2026-07-06 | **Spec**: [spec.md](spec.md)
**Input**: Feature specification from `/specs/004-upgrade-tests/spec.md`

## Summary

Add behavioral test coverage for the operator-facing upgrade surface
(`POST /v1/upgrade/prepare`, `POST /v1/upgrade/execute`) across the
full matrix {minor, major} × {happy path, bad path}. Today only a
route-existence smoke test exists in this repo. The approach is
two-tier, matching the repo's established pattern and Constitution VI:

1. **Contract tier** (`internal/control/handlers_upgrade_test.go`,
   `fakeEngine`): plan pass-through fidelity, envelope semantics for
   engine errors (simulated swap failure, version mismatch, validation
   refusals), malformed-body rejection, leader forwarding relay and
   timeout. Mocks are used only for fault injection real servers can't
   reproduce — exactly the carve-out Constitution VI permits.
2. **Integration tier** (`tests/integration/lcm_upgrade_test.go`,
   `integration` build tag, docker-compose 3-peer topology with real
   PostgreSQL 17 + real embedded NATS): real-engine plan validation
   (downgrade / cross-major / non-higher-major rejections via
   prepare), the pinned major-execution gate error (sentinel), and the
   minor-execute happy path — a same-version plan exercising the real
   stop → no-op swap → start → version-probe sequence against a live
   server.

Test-only feature: zero production code changes.

## Technical Context

**Language/Version**: Go 1.26.4 (repo `go` directive; CI installs from it)
**Primary Dependencies**: `github.com/f1bonacc1/pg-manager v0.3.0`
(pinned engine — contains `UpgradePlan`, `upgrade.PreSwap`, the
validation rules, and the major-strategy gate), stdlib `net/http` +
`encoding/json`, `pgx/v5` (integration DB probes), docker compose
(integration harness)
**Storage**: PostgreSQL 17 (`postgres:17-bookworm` base) inside the
compose test topology; no storage changes
**Testing**: `go test -race ./internal/...` (contract tier, runs in
per-PR CI job), `go test -tags=integration ./tests/integration/...`
via `make integration` (separate longer-timeout workflow), existing
harness helpers `callLCM` / `lcmResponse` / `Peers()`
**Target Platform**: Linux (CI: ubuntu-latest; compose harness)
**Project Type**: Single Go module (proxy + tests) — test-only feature
**Performance Goals**: added integration cases keep suite wall clock
within +20% (SC-005); contract tests are sub-second
**Constraints**: `lcm-discipline-gate` workflow bans engine-mechanic
tokens (`pg_upgrade` etc.) in non-test source under `cmd/`/`internal/`
— test files are exempt but new tests must not push such tokens into
production source; no new config keys; no production behavior changes
**Scale/Scope**: ~12–16 new test functions in 2 new test files; no new
packages

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| # | Principle | Assessment |
|---|-----------|------------|
| I | Wire-Protocol Fidelity | PASS — no wire-protocol changes; tests observe the existing HTTP control surface and PostgreSQL data plane only. |
| II | Fail-Closed Safety | PASS (reinforced) — the bad-path tests pin fail-closed behavior: invalid plans refused, engine failures surfaced as `failed` envelopes, malformed bodies rejected before reaching the engine. No permissive fallback is introduced. |
| III | Active/Active Coordination Correctness | PASS — no coordination changes. The forwarding tests exercise the existing leader-routing path (`leaderOnly=true` on both upgrade routes) and pin its relay/timeout behavior. |
| IV | Thin Scaffold over pg-manager | PASS — no engine logic is duplicated. Sequence-internals (stop/swap/start ordering) remain tested upstream in `pg-manager/upgrade`; this feature tests only what the proxy owns: request decode, delegation, envelope surfacing, forwarding. The gate sentinel pins the upstream contract instead of re-implementing it. |
| V | Observability by Default | PASS — no new failure modes are introduced (test-only). Tests assert through the operator-visible envelope (`operation`, `outcome`, `error.code`), which also exercises the audit/metrics wrap path already in place. |
| VI | Integration-First Testing (NON-NEGOTIABLE) | PASS — this feature exists to satisfy it. Real-server happy path: minor execute against real PostgreSQL 17 + real embedded NATS in the compose topology. Mocks (`fakeEngine`) are confined to fault injection a real server cannot deterministically produce (swap failure, version mismatch, engine error surfacing), each paired with the real-server happy path per the VI carve-out rule. |
| VII | Scope Discipline & Reversibility | PASS — smallest correct change: two test files, no production diffs, no config keys, trivially reversible. No Kubernetes/Helm surface. |

**Initial gate: PASS** (no violations; Complexity Tracking empty).
**Post-Phase-1 re-check: PASS** — design artifacts introduce no
production changes and no new interfaces; contracts/ documents the
*existing* endpoint behavior that tests will pin, not new surface.

## Project Structure

### Documentation (this feature)

```text
specs/004-upgrade-tests/
├── plan.md              # This file (/speckit-plan command output)
├── research.md          # Phase 0 output (/speckit-plan command)
├── data-model.md        # Phase 1 output (/speckit-plan command)
├── quickstart.md        # Phase 1 output (/speckit-plan command)
├── contracts/
│   └── upgrade-endpoints.md  # Pinned endpoint contract the tests assert
└── tasks.md             # Phase 2 output (/speckit-tasks command - NOT created by /speckit-plan)
```

### Source Code (repository root)

```text
internal/control/
├── handlers_upgrade.go            # existing — NOT modified
├── handlers_upgrade_test.go       # NEW — contract tier (fakeEngine)
├── control_test.go                # existing — fakeEngine/fakeLeader reused;
│                                  #   upgrade hooks already present
│                                  #   (prepareUpgradeFn / executeUpgradeFn)
└── types.go                       # existing — Engine iface, error codes

tests/integration/
├── lcm_upgrade_test.go            # NEW — integration tier (real engine,
│                                  #   real PG 17, real embedded NATS)
├── lcm_helpers_test.go            # existing — callLCM / lcmResponse reused
├── harness_test.go                # existing — Peers()/DSN() reused
└── docker-compose.test.yml        # existing — NOT modified
```

**Structure Decision**: Both new files slot into existing test
packages; no new directories, packages, or harness changes. The
contract tier reuses `fakeEngine` (which already has
`prepareUpgradeFn`/`executeUpgradeFn` hooks) and `newTestServer` from
`control_test.go`. The integration tier reuses the compose harness,
`callLCM`, and the `lcmResponse` envelope mirror from
`lcm_helpers_test.go`.

## Complexity Tracking

> No Constitution Check violations — table intentionally empty.
