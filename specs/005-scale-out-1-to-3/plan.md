# Implementation Plan: Safe 1→3 Scale-out and Working Single-Node Mode

**Branch**: `005-scale-out-1-to-3` | **Date**: 2026-10-14 | **Spec**: [spec.md](spec.md)
**Input**: Feature specification from `/specs/005-scale-out-1-to-3/spec.md`
**Technical source**: [docs/scale-out-1-to-3/plan.md](../../docs/scale-out-1-to-3/plan.md). It holds the verified facts F1-F16, work items WI-A and WI-C1..C7, and code anchors (section 8). This plan maps those items onto pgman-proxy and does not repeat their evidence.

## Summary

**Single-node mode (WI-A, US1).** Two code changes:

- derive `pgmanager.AsyncOnly{}` when the config lists exactly one
  peer;
- log the effective replication policy at startup.

This fixes the commit hang (F1) and the read-only-after-restart wedge
(F2). It is independent of upstream and ships first.

**Expansion (US2-US5).** The work is split between pg-manager and
pgman-proxy.

pg-manager milestone `015-empty-peer-safety`, released as v0.7.0, is a
prerequisite outside this repo. It provides:

- the per-node data-presence record;
- empty-node deference for campaigning and for initializing a database;
- excluding empty peers from the sync set and the failover quorum;
- an empty sync-names render;
- identity-mismatch status.

pgman-proxy adds four production changes:

1. **Boot-window campaign gate** (WI-C1). Before `manager.New`, only a
   node with a complete database may campaign.
2. **Incompatible JetStream store moved aside** (WI-C2). This runs at
   boot, in both directions.
3. **Managed `hba_extras`/`conf_extras` blocks** (WI-C3). They are
   applied at PostInitDB and on every boot, followed by a reload through
   pg-manager's executor.
4. **Identity mismatch surfaced** (WI-C4) as a doctor check, a gauge
   and an event.

pgman-proxy also:

- bumps pg-manager to v0.7.0 (WI-C5);
- adds a new docker-driven `tests/scaleout` suite (WI-C6);
- adds the runbook, the spec 002 amendment and the changelog (WI-C7).

## Technical Context

**Language/Version**: Go 1.26.7 (repo `go` directive)

**Primary Dependencies**:
- `github.com/f1bonacc1/pg-manager`: v0.6.1 today, v0.7.0 after WI-C5. During development, a commented `replace => ../pg-manager` is used only for work that needs 015 APIs. Release builds use the tag (Constitution, Language & toolchain).
- `github.com/nats-io/nats-server/v2` v2.14.0, embedded.
- `pgx/v5`, for test probes.
- docker CLI, for the scaleout suite.

**Storage**:
- PostgreSQL 17 data directory (PGDATA).
- Embedded NATS JetStream store under `cluster.jetstream_dir`, laid out as `<dir>/jetstream/...`. A clustered store has `<dir>/jetstream/$SYS/_js_/_meta_` (F13).

**Testing**:
- **Unit**: `go test -race ./internal/...` (`make test`).
- **Integration**: `make integration`, the 3-node compose suite, unchanged. It is the fresh-bootstrap regression test (FR-018).
- **Scaleout**: new `make scaleout`, using `-tags=scaleout ./tests/scaleout/...`. It drives `docker run` against the integration image (real PG17 and real embedded NATS), as Constitution VI requires.

**Target Platform**: Linux (systemd hosts, containers). CI runs on `ubuntu-latest`.

**Project Type**: single Go module (proxy binary, operator CLI, tests).

**Performance Goals**:
- single-node first commit within 10 s of becoming primary;
- restart writable within 30 s;
- expansion write outage under 30 s (about 8.6 s measured, F8).

**Constraints**:
- **`lcm-discipline-gate`**: production Go code under `cmd/` and `internal/` must not name `initdb`, `pg_basebackup`, `pg_rewind`, `pg_upgrade`, `pg_ctl promote` or slot DDL outside comments. Any new code that needs to reload Postgres calls pg-manager's `PostgresExecutor.Reload`; it must not shell out itself.
- **Scope-gate tokens**: also banned outside `specs/`.
- **No new config keys** (Constitution VII).

**Scale/Scope**:
- about 6 production files touched or added;
- one new test package (`tests/scaleout`) with about 7 scenarios;
- one Makefile target and one nightly job;
- docs.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| # | Principle | Assessment |
|---|---|---|
| I | Wire-Protocol Fidelity | **Pass.** No data-plane changes. |
| II | Fail-Closed Safety | **Pass.** Every new decision fails closed. <br>• Boot-window gate: any inspection error → do not campaign (FR-006). <br>• Store move fails → startup refused (FR-011). <br>• Managed block write fails → startup refused (an existing node must not run with stale replication HBA silently). <br>• Upstream deference: KV read error → defer. |
| III | Active/Active Coordination Correctness | **Pass, and the point of the feature.** <br>• Leadership stays on the NATS lease. <br>• The boot-window gate only narrows who may contest the lease. <br>• Invariant "one database identity after expansion" (FR-008) is asserted in `tests/scaleout` S1. <br>• Invariant "no initdb while a data-bearing peer exists" is asserted in S1 and S6. |
| IV | Thin Scaffold over pg-manager | **Pass, with one flagged item.** <br>• Data-presence, deference, sync-set and identity logic all land upstream first (015). <br>• Reload goes through the engine's executor. <br>• **Flagged:** HBA/conf block management (WI-C3) arguably belongs in pg-manager. Per decision 6 it is implemented here, extending the existing `postInitDBHook` scaffolding. The PR must link a follow-up issue to upstream it. Recorded in Complexity Tracking. |
| V | Observability by Default | **Pass.** New stable names, documented in [contracts/observability.md](contracts/observability.md): <br>• log field `replication_policy`; <br>• events `embedded_nats.store_archived`, `postgres.managed_block_applied` and `cluster.identity_mismatch` (passthrough); <br>• gauge `pgman_proxy_cluster_identity_mismatch`; <br>• doctor check `cluster.identity`. |
| VI | Integration-First Testing | **Pass.** <br>• Every behavior gets a real-PG, real-NATS scenario in `tests/scaleout` (S1-S7). <br>• `fakeEngine` is used only for the identity-mismatch fault injection in the contract tier (allowed carve-out), paired with real-server scenario S6/S1 coverage. <br>• Multi-replica topology is covered by S1-S4 and S6. |
| VII | Scope Discipline & Reversibility | **Pass.** <br>• No new config keys; AsyncOnly is derived from `len(peers)`, the same way replicas are derived from `declared_size`. <br>• Behavior changes (stores moved aside, managed blocks) are reversible: the store is kept, and legacy blocks are left untouched. They are documented in the CHANGELOG. <br>• Out-of-scope check: nothing touches orchestration. Cluster bootstrap of PostgreSQL stays in pg-manager. |

**Gate result: PASS** (one Constitution IV flag, justified below).

## Project Structure

### Documentation (this feature)

```text
specs/005-scale-out-1-to-3/
├── plan.md              # This file
├── research.md          # Phase 0: decisions R-001..R-010
├── data-model.md        # Phase 1: entities, layouts, state
├── quickstart.md        # Phase 1: how to run and verify each story
├── contracts/
│   ├── observability.md       # new log fields, events, metric, doctor check
│   ├── jetstream-store-layout.md  # detection + archive naming contract (WI-C2)
│   ├── managed-conf-blocks.md     # marker format and apply semantics (WI-C3)
│   ├── scaleout-suite.md          # scenarios S1-S7, assertions, budgets (WI-C6)
│   └── upstream-015.md            # what pgman-proxy consumes from pg-manager v0.7.0
├── checklists/requirements.md
└── tasks.md             # Phase 2 (/speckit-tasks)
```

### Source Code (repository root)

```text
internal/runtime/
├── start.go                 # WI-A policyFromConfig → replicationPolicy; WI-C1 campaign closure;
│                            # WI-C2 call before embedded.NewServer; WI-C3 boot-time apply + PostInitDB
├── replication_policy.go    # NEW (WI-A): replicationPolicy, policyLabel
├── campaign_gate.go         # NEW (WI-C1): campaignGate, pgdataComplete
├── managed_block.go         # NEW (WI-C3): applyManagedBlock (atomic rewrite), applyManagedConf
└── *_test.go                # unit tables for each

internal/embedded/
├── store_layout.go          # NEW (WI-C2): DetectStoreLayout, ArchiveIncompatibleStore
└── store_layout_test.go

internal/control/
├── doctor_checks.go         # WI-C4: cluster.identity check
└── handlers_doctor_test.go  # contract test with fakeEngine mismatch

internal/obs/metrics.go      # WI-C4: pgman_proxy_cluster_identity_mismatch gauge

tests/scaleout/              # NEW (WI-C6), build tag `scaleout`
├── main_test.go             # TestMain: build image once, sweep leftovers
├── docker_test.go           # docker run/exec/volume/network helpers (ports rig/lib.sh)
├── node_test.go             # node env presets (single, three-{a,b,c}), probes (role, sysid, rows, ssn, write)
├── golden_test.go           # build + snapshot a single node with N rows
├── single_node_test.go      # S5 (+ WI-A stale-ssn upgrade path)
├── expand_test.go           # S1 (5 iterations, randomized order), S7
├── rollback_test.go         # S3
├── solo_test.go             # S4
├── fresh_test.go            # S2
└── data_node_down_test.go   # S6

Makefile                                  # `scaleout` target
.github/workflows/nightly-integration.yml # `scaleout` job
docs/scale-out-1-to-3/runbook.md          # WI-C7
specs/002-embedded-nats-cluster/spec.md   # WI-C7 amendment (US2 scenario 2)
CHANGELOG.md                              # WI-C7
go.mod / go.sum                           # WI-C5 bump (+ coupled pgx bump if needed)
```

**Structure Decision**: The feature extends the existing single-module
layout. The only new package is `tests/scaleout`, because the existing
`tests/integration` `TestMain` holds one fixed 3-node compose topology
for the whole package. It cannot restart a single node into a 3-node
config, or run single-node and 3-node shapes in sequence.

## Delivery slices and upstream dependency

| Slice | Stories / FRs | Needs pg-manager 015? | Ships as |
|---|---|---|---|
| S-1 | US1 (WI-A), FR-001..FR-005; `tests/scaleout` harness + S5 | No | PR-1 (first) |
| S-2 | WI-C7 part 1: spec 002 amendment (FR-020) | No | PR-2 |
| S-3 | WI-C2 store layout, FR-009..FR-012 | No | PR-3 |
| S-4 | WI-C3 managed blocks, FR-013..FR-015 | No | PR-4 |
| S-5 | WI-C1 boot-window gate (FR-006). Code and unit tests can land without 015, but alone it only narrows the race | Effective only with 015 | PR-5 with WI-C5 |
| S-6 | WI-C5 bump, WI-C4 identity surface (FR-007, FR-016), S1-S4, S6, S7 | **Yes** | PR-5 / PR-6 |
| S-7 | WI-C7 part 2: runbook, CHANGELOG (FR-019, FR-021) | Runbook needs final behavior | PR-7 |

While 015 is unreleased, scenarios that need it (S1, S2-assert-once,
S3, S6) are written against the sibling checkout. They are gated by
`t.Skip` until `go list -m` reports pg-manager ≥ v0.7.0, so a merge
before the bump does not leave red nightly jobs. The skip names the
missing prerequisite. See research R-009.

## Complexity Tracking

| Violation | Why Needed | Simpler Alternative Rejected Because |
|---|---|---|
| Constitution IV flag: HBA/conf block management stays in pgman-proxy (WI-C3) | An existing single node must gain replication HBA rules before it can serve `pg_basebackup` to new peers (F11). Today only pgman-proxy's PostInitDB hook writes them. | **Blocking on pg-manager owning HBA rendering**: delays the whole expansion for a scaffolding concern. The existing hook already lives here. Follow-up issue to upstream it is mandatory in the PR. |
| New test package `tests/scaleout` instead of extending `tests/integration` | Scenarios restart nodes into different topologies (1 → 3 → 1) and need per-scenario volumes. | **Extending `tests/integration`**: its package-wide `TestMain` pins one compose topology for every test. Adding per-test containers there would collide with its names and network, and slow the PR-critical suite. |
