# Specification Quality Checklist: Active/Active PostgreSQL HA Proxy + Lifecycle Manager (pgman-proxy v1)

**Purpose**: Validate specification completeness and quality before proceeding to planning
**Created**: 2026-05-09
**Last re-validated**: 2026-05-09 (after the LCM amendment that added US4, FR-021..FR-032, edge cases, SC-009..SC-013, and LCM key entities/assumptions)
**Feature**: [spec.md](../spec.md)

## Content Quality

- [x] No implementation details (languages, frameworks, APIs)
      *Note: spec mentions PostgreSQL wire protocol, NATS, and `pg-manager` because those are the named domain dependencies in the user input — they are scope, not implementation choices the spec is making. No Go-specific, library-version, or code-structure decisions appear in the spec.*
- [x] Focused on user value and business needs
- [x] Written for non-technical stakeholders
      *Note: technical terms (NATS, leader lease, TLS) are unavoidable for a database-proxy spec; each is introduced with the operator/user value it delivers.*
- [x] All mandatory sections completed (User Scenarios & Testing, Requirements, Success Criteria)

## Requirement Completeness

- [x] No [NEEDS CLARIFICATION] markers remain
- [x] Requirements are testable and unambiguous
- [x] Success criteria are measurable (time budgets, p99 latencies, percentages, grep-verifiable counts)
- [x] Success criteria are technology-agnostic (described as user-facing or operator-facing outcomes; PostgreSQL/NATS/Prometheus appear as named dependencies, not implementation choices)
- [x] All acceptance scenarios are defined (Given/When/Then for every user story)
- [x] Edge cases are identified (NATS unreachable, stale lease, TLS failure, port conflict, leader-transition race, etc.)
- [x] Scope is clearly bounded (in-scope: leader-aware routing across three deployment modes **plus** full HA-cluster LCM via control-plane API; out-of-scope explicitly: VIP, k8s/Helm/operators, read/write split, multi-tenant, ACME, restore/PITR for v1, bundled backup backend)
- [x] Dependencies and assumptions identified (pg-manager engine, operator-provisioned NATS, operator-supplied TLS, operator-supplied BackupExecutor, single-cluster scope, mode-as-configuration)

## Feature Readiness

- [x] All functional requirements have clear acceptance criteria — FR-001..FR-020 (proxy data-plane) map to US1–US3 and SC-001..SC-008; FR-021..FR-032 (LCM control-plane) map to US4 acceptance scenarios and SC-009..SC-013
- [x] User scenarios cover primary flows (deploy, sidecar, observe, **manage lifecycle**)
- [x] Feature meets measurable outcomes defined in Success Criteria
- [x] No implementation details leak into specification (no file paths, struct names, Go module decisions, or code-layout statements; the control-plane shape is described as "authenticated HTTP API on a dedicated listener" — a stated default in Assumptions, not a frozen implementation choice)

## Notes

- Re-validation after the LCM amendment: 18/18 PASS. No `[NEEDS CLARIFICATION]` markers introduced.
- The spec leans on the project Constitution v1.1.0 (`.specify/memory/constitution.md`); plan-phase Constitution Check should map FR-015 + SC-006 to Principle VII (Scope Discipline) and FR-020 + FR-022 + SC-013 to Principle IV (Thin Scaffold over pg-manager) explicitly.
- The reference implementation `../pg-manager/examples/three_node_nats/main.go` is the starting wiring sketch for US1–US3; the LCM surface (US4) builds on `../pg-manager`'s `Manager.Switchover/Failover/Promote/Fence/Unfence/UpdateTopology/TriggerBackup/PrepareUpgrade/ExecuteUpgrade/Status/Diagnose` methods.
- The amendment introduces a "what's missing" set the user asked to surface; see the `/speckit-specify` reply for the prioritised list. None of those open questions block planning, but the user MAY choose to resolve them through `/speckit-clarify` before `/speckit-plan` if any of the stated defaults are wrong for them (control-plane shape, backup backend, restore inclusion).
- **Resolved 2026-05-09 (item #9 from the "what's missing" list — control-plane bind default in sidecar)**: User confirmed both defaults: (a) mode-aware bind — sidecar = loopback, standalone+microservice = all-interfaces; (b) bearer token required for mutating ops, reads optionally unauthenticated. The mode-aware bind rule was promoted from a `deployment-modes.md` note to a first-class assumption in `spec.md` so it does not drift later. No FR additions; no SC additions; FR-025 unchanged.
- **Resolved 2026-05-09 (post-`/speckit-analyze` remediation, top-3 HIGH issues)**:
  - **C1**: `contracts/config.md` now has the full `control.*` block (YAML schema + env-var mapping + 6 new validation outcomes).
  - **C2**: New **FR-033** — control-plane TLS required on non-loopback bind (with `control.tls.plaintext_explicit_ack` named opt-in mirroring FR-018). New edge case + integration tests T052a/T052b.
  - **C3**: New **FR-034** — forward-mode leader-route bounded by `control.leader_route_timeout` (default 30s, range `(0, 5m]`). New error code `leader_route_timeout` (HTTP 504) in `lcm.md`. New integration test T052c.
  - Bonus: New integration test T052d covers FR-029 bootstrap-and-transition refusal (closes finding C4 as well).
  - Total: 32 → **34 FRs**, 81 → **85 tasks**. Constitution Check still PASS on all 7 principles.
- Items marked incomplete would require spec updates before `/speckit-clarify` or `/speckit-plan`. None are incomplete.
