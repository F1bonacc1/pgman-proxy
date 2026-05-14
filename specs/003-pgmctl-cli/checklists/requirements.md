# Specification Quality Checklist: `pgmctl` Operator CLI

**Purpose**: Validate specification completeness and quality before proceeding to planning
**Created**: 2026-05-14
**Feature**: [spec.md](../spec.md)

## Content Quality

- [x] No implementation details (languages, frameworks, APIs)
- [x] Focused on user value and business needs
- [x] Written for non-technical stakeholders
- [x] All mandatory sections completed

*Notes*: Some unavoidable technical anchors carry forward from features 001/002
that the spec amends/builds on (HTTPS control plane, bearer tokens, server-
sent events as a wire-level family) — these are surfaces, not implementations,
and they pin the integration contract rather than the build. Cobra (the user-
requested framework) appears only in the user's quoted Input string; no FR
mentions a Go library, framework, or package.

## Requirement Completeness

- [x] No [NEEDS CLARIFICATION] markers remain
- [x] Requirements are testable and unambiguous
- [x] Success criteria are measurable
- [x] Success criteria are technology-agnostic (no implementation details)
- [x] All acceptance scenarios are defined
- [x] Edge cases are identified
- [x] Scope is clearly bounded
- [x] Dependencies and assumptions identified

*Notes*: Six user stories, prioritised P1/P1/P1/P2/P2/P2, each with an
independent test. 40 functional requirements organised into 8 themed buckets.
10 success criteria, each with a quantitative bar. 11 edge cases. Assumptions
section documents every reasonable default chosen in lieu of a clarification
marker (auth scheme, transport, config-file location, multi-context model,
`restart`/`delete`/`set-config` semantics, color contract, no-plug-in scope).

## Feature Readiness

- [x] All functional requirements have clear acceptance criteria
- [x] User scenarios cover primary flows
- [x] Feature meets measurable outcomes defined in Success Criteria
- [x] No implementation details leak into specification

*Notes*: Every FR has a corresponding acceptance scenario in at least one
user story OR a directly-mapped success criterion. The mutating-operations
matrix (FR-028..FR-031) is fully covered by US6's six acceptance scenarios.
Doctor (FR-022..FR-027) is fully covered by US3's seven acceptance scenarios.

## Notes

- Items marked incomplete require spec updates before `/speckit-clarify` or
  `/speckit-plan`. All items currently pass.
- The spec calls out five additive, MINOR-version expansions to the 001
  control-plane contract: (1) SSE watch-stream endpoints,
  (2) doctor-check / doctor-fix discovery and execution endpoint,
  (3) managed-PostgreSQL restart + peer self-terminate endpoints,
  (4) JetStream-backed durable history stream for events/audit, and
  (5) inter-peer fan-out request/reply on the embedded NATS mesh. These
  are intentional scope-boundary acknowledgements, surfaced now rather
  than discovered in `/speckit-plan`.
- A logs-tail endpoint is **not** in the expansion set: `pgmctl logs`
  was removed from v1 per the 2026-05-14 clarification (Q5). Operators
  consume structured logs through the host's log sink.
- The doctor check battery is server-driven (FR-026) so the spec does not
  pin a binary catalogue; checks evolve with the server.
- 16 successful checklist items, 0 failing. Spec has completed
  `/speckit-clarify` (5 questions asked, 5 answered, integrated). Ready
  for `/speckit-plan`.

## Clarifications applied (Session 2026-05-14)

1. Event/audit history retention → JetStream-backed durable stream
   (FR-016a); R derived per 002 FR-011a; retention defaults `24h` / `256 MiB`.
2. Mutating-op idempotency → no server dedup; pgmctl never retries
   mutating ops; operator reconciles via `request_id` lookup (FR-039
   tightened).
3. Per-peer slice fan-out → single connection, server-side fan-out via
   embedded NATS mesh (FR-006 / FR-006a). pgmctl never opens multiple
   control-plane connections per invocation.
4. `pgmctl restart` scope → both targets via `--target=postgres|proxy`
   (default postgres); proxy target uses privileged self-terminate +
   supervisor respawn (FR-031a/b/c).
5. Structured-log retrieval → **removed from v1**; FR-002 / FR-017 / US2
   / US5 / Dump entity / server-side surface list all purged of `pgmctl
   logs` references.
