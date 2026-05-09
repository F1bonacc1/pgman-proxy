# Specification Quality Checklist: Embedded NATS Cluster for pgman-proxy Coordination

**Purpose**: Validate specification completeness and quality before proceeding to planning
**Created**: 2026-05-09
**Feature**: [spec.md](../spec.md)

## Content Quality

- [x] No implementation details (languages, frameworks, APIs)
- [x] Focused on user value and business needs
- [x] Written for non-technical stakeholders
- [x] All mandatory sections completed

## Requirement Completeness

- [x] No [NEEDS CLARIFICATION] markers remain
- [x] Requirements are testable and unambiguous
- [x] Success criteria are measurable
- [x] Success criteria are technology-agnostic (no implementation details)
- [x] All acceptance scenarios are defined
- [x] Edge cases are identified
- [x] Scope is clearly bounded
- [x] Dependencies and assumptions identified

## Feature Readiness

- [x] All functional requirements have clear acceptance criteria
- [x] User scenarios cover primary flows
- [x] Feature meets measurable outcomes defined in Success Criteria
- [x] No implementation details leak into specification

## Notes

- **Clarifications resolved on 2026-05-09** (`/speckit-clarify` session, 4 questions):
  - Cluster-route auth → per-peer NKey (Ed25519) with shared cluster name (drove FR-009, FR-010, FR-010a, Key Entities split into Peer NKey seed / Authorized-keys list / Cluster name).
  - Cluster-routes TLS posture → required on non-loopback; loopback plaintext OK; named explicit-ack opt-out (drove FR-010b; replaced the "TBD in plan phase" Assumption).
  - JetStream replication factor → auto-derived from declared cluster size (1/2/3+ → R=1/2/3) with explicit override (drove FR-011a).
  - Hot-reload of peer-set / auth-keys → SIGHUP-scoped reload of those two surfaces only (drove FR-014a, tightened FR-010a, removed the US2 acceptance-scenario hedge).
- This spec **amends feature 001** (`specs/001-active-active-pg-proxy/spec.md`) by
  reversing FR-003 (which forbade embedding NATS) and the "NATS is
  operator-provisioned" assumption. The reversal is captured explicitly in the
  spec's *Context & Relationship to Feature 001* section and in FR-002 / FR-016 of
  this feature.
- A constitution amendment (v1.1.0 → at least v1.2.0) is required to update the
  *Architecture Overview* and the *Topology & Dependencies* subsection of
  `.specify/memory/constitution.md`, which currently describe NATS as externally
  provisioned. This amendment is **in scope for `/speckit-plan`**, not for this
  specification.
- Two areas are intentionally deferred to the plan phase rather than guessed in
  the spec, and are flagged as TBD in success criteria so they cannot be lost:
  - **SC-005** memory/CPU budget for the embedded NATS server at idle.
  - **SC-008** rolling-restart time budget for a 3-peer upgrade.
  These are quantitative caps that need a measurement baseline before they can
  be set without becoming arbitrary; the spec commits to having a number, and
  the plan phase commits to picking it.
- One **technology-named** entity appears in the Key Entities section
  (`JetStream durable-storage path`). It is retained because (a) the
  `pg-manager` NATS adapters' contract already requires JetStream, so naming
  it preserves the contract surface that this feature must keep stable
  (FR-015), and (b) hiding the substrate name would obscure the operator-facing
  storage path the spec commits to. This is the same "named-public-API"
  precedent feature 001 used for `BackupExecutor`.
- Cross-spec consistency check passed: every behavioural commitment in 001 that
  this feature touches (FR-003, the "NATS is operator-provisioned" assumption)
  is explicitly named and amended; every other 001 commitment is explicitly
  preserved by FR-016.
- Items marked incomplete require spec updates before `/speckit-clarify` or
  `/speckit-plan`.
