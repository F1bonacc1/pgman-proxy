# Feature Specification: Upgrade Test Coverage (Minor & Major, Happy & Bad Paths)

**Feature Branch**: `004-upgrade-tests`
**Created**: 2026-07-06
**Status**: Draft
**Input**: User description: "upgrade tests major, minor, happy and bad path in pgman-proxy"

## Overview

pgman-proxy exposes an operator-facing upgrade surface (a dry-run
"prepare" check and an "execute" action) that drives PostgreSQL
version upgrades on a cluster node. Today that surface has only a
route-existence smoke test in this repository: no test verifies that a
valid minor upgrade actually succeeds end-to-end, that dangerous plans
(downgrades, wrong-version targets) are refused, or that the
currently-gated major-upgrade path fails safely with the documented
message. The published operator runbook
(`docs/upgrade-orchestration.md`) makes promises that nothing in CI
enforces.

This feature adds automated test coverage for the upgrade surface in
pgman-proxy across four quadrants: minor-upgrade happy path,
minor-upgrade bad paths, major-upgrade happy path (plan validation),
and major-upgrade bad paths (including the upstream execution gate).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Minor upgrade happy path is proven (Priority: P1)

A maintainer merging changes to pgman-proxy needs confidence that an
operator following the minor rolling-upgrade runbook will succeed: a
valid minor-upgrade plan passes the dry-run check, and executing it
performs the documented sequence — stop the database, apply the binary
swap step, restart, and confirm the running version — reporting
success to the operator.

**Why this priority**: The minor rolling upgrade is the only upgrade
flow operators are told they can perform today. It is the core promise
of the runbook, and it currently has zero behavioral coverage in this
repository. If it silently regresses, an operator discovers it
mid-maintenance-window on a production cluster.

**Independent Test**: Can be fully tested by submitting a valid
minor-upgrade plan to the dry-run and execute endpoints of a running
node and observing the sequence and the success response. Delivers
value alone: it pins the single most important operator flow.

**Acceptance Scenarios**:

1. **Given** a healthy node and a well-formed minor-upgrade plan
   (same major version, equal-or-higher minor), **When** the operator
   runs the dry-run check, **Then** the plan is accepted with no
   rejection reasons.
2. **Given** the same plan, **When** the operator executes it,
   **Then** the node performs stop → binary-swap step → start →
   version confirmation in that order and returns success.
3. **Given** the executed upgrade completed, **When** the operator
   queries node status, **Then** the database is running and reports
   the expected version.
4. **Given** a real database server (not a simulated engine),
   **When** the happy-path execution runs in CI, **Then** it passes
   against that real server (Constitution VI: integration-first).

---

### User Story 2 - Dangerous or broken minor upgrades fail safely (Priority: P2)

A maintainer needs proof that the upgrade surface refuses plans that
would damage a cluster, and that mid-flight failures stop the sequence
rather than plowing ahead: downgrades are rejected, a minor plan that
crosses a major-version boundary is rejected, a failed binary-swap
step prevents the database from being restarted on wrong binaries, and
a post-restart version mismatch is reported as a failure.

**Why this priority**: Bad-path behavior is the safety net for
operators under pressure. These refusals are documented but untested
in this repository; a regression here turns an operator typo into an
outage or data-compatibility incident.

**Independent Test**: Can be tested independently by submitting each
invalid plan or injecting each failure and asserting the specific
refusal or abort behavior, without any happy-path test existing.

**Acceptance Scenarios**:

1. **Given** a plan whose target version is lower than the current
   version, **When** the operator runs the dry-run check or executes
   it, **Then** the plan is rejected with a reason naming the
   downgrade.
2. **Given** a minor-strategy plan whose target crosses a
   major-version boundary, **When** it is checked or executed,
   **Then** it is rejected.
3. **Given** a binary-swap step that fails, **When** the operator
   executes a minor plan, **Then** the sequence aborts, the database
   is not restarted, and the failure is reported.
4. **Given** a completed swap that results in an unexpected running
   version, **When** the post-restart confirmation runs, **Then** the
   mismatch is reported as a failure to the operator.
5. **Given** a malformed or empty request, **When** it is submitted
   to either upgrade endpoint, **Then** it is rejected as invalid
   with a clear message and no upgrade action is attempted.
6. **Given** an upgrade request addressed to a different peer,
   **When** it arrives at a node, **Then** it is forwarded to the
   addressed peer and that peer's outcome (success or failure) is
   relayed back unchanged.

---

### User Story 3 - Major upgrade behavior is pinned: planning works, execution is gated (Priority: P3)

A maintainer needs the current major-upgrade contract pinned by tests:
a well-formed major-upgrade plan (strictly higher major version)
passes the dry-run validation, but attempting to execute any major
strategy fails with the documented gate message and changes nothing on
the node. When the upstream engine eventually lifts the gate, a test
must fail loudly so the team consciously revisits the surface rather
than silently inheriting new behavior.

**Why this priority**: Major upgrades are explicitly out of operator
reach until the upstream engine's hardening release. The risk today is
not a broken feature but an unnoticed contract change — the gate
lifting, or its message drifting, without pgman-proxy noticing.

**Independent Test**: Can be tested independently by validating and
then attempting to execute a major-upgrade plan and asserting the
documented gate refusal.

**Acceptance Scenarios**:

1. **Given** a well-formed major-upgrade plan with a strictly higher
   target major version, **When** the operator runs the dry-run
   check, **Then** validation succeeds (planning is supported today).
2. **Given** a major-upgrade plan with a target major equal to or
   lower than the current major, **When** it is checked, **Then** it
   is rejected.
3. **Given** any well-formed major-upgrade plan, **When** the
   operator executes it, **Then** the request fails with the
   documented gate message and no upgrade steps are performed.
4. **Given** the upstream engine lifts the execution gate in a future
   release, **When** the test suite next runs against that engine,
   **Then** at least one test fails, forcing an explicit decision
   about the newly-unlocked behavior.

---

### Edge Cases

- A dry-run check passes but conditions change before execution (for
  example the database is stopped in between): execution reports the
  engine's failure rather than a false success.
- An execute request arrives while the database is already stopped:
  the outcome is the engine's reported error, surfaced unchanged.
- A forwarded upgrade request targets a peer that is unreachable: the
  operator receives a routing error, not a hang or a silent local
  execution.
- The upstream engine changes the wording of its gate message: the
  pinned test fails, prompting a deliberate re-pin rather than silent
  drift.
- Concurrent upgrade requests to the same node: the second request's
  outcome is whatever the engine reports; the test suite documents
  (not invents) this behavior.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The test suite MUST verify that a well-formed
  minor-upgrade plan is accepted by the dry-run check with no
  rejection reasons.
- **FR-002**: The test suite MUST verify that executing a well-formed
  minor-upgrade plan performs stop → binary-swap step → start →
  version confirmation in that order and reports success.
- **FR-003**: The test suite MUST verify that a failure in the
  binary-swap step aborts the sequence before the database is
  restarted and surfaces the failure to the caller.
- **FR-004**: The test suite MUST verify that a post-restart version
  mismatch is detected and reported as a failure.
- **FR-005**: The test suite MUST verify that plans targeting a lower
  version than currently running (downgrades) are rejected by both
  the dry-run check and execution.
- **FR-006**: The test suite MUST verify that minor-strategy plans
  crossing a major-version boundary are rejected.
- **FR-007**: The test suite MUST verify that a well-formed
  major-upgrade plan (strictly higher target major) passes the
  dry-run validation.
- **FR-008**: The test suite MUST verify that executing any
  major-strategy plan fails with the documented upstream gate message
  and performs no upgrade steps; this pin doubles as a sentinel that
  fails when the upstream gate is lifted or its message changes.
- **FR-009**: The test suite MUST verify that malformed or empty
  upgrade requests are rejected as invalid with a descriptive message
  and never reach the upgrade engine.
- **FR-010**: The test suite MUST verify that upgrade requests
  addressed to another peer are forwarded to that peer and the peer's
  outcome is relayed to the caller for both success and failure
  outcomes.
- **FR-011**: At least one minor-upgrade happy-path scenario MUST run
  against a real database server (not a simulated engine), consistent
  with the project's integration-first testing principle.
- **FR-012**: All upgrade tests MUST run automatically in CI on every
  merge; a failure in any of them MUST block the merge.
- **FR-013**: Bad-path failures MUST be asserted through the same
  operator-visible response surface (status and error envelope) that
  a real operator or the CLI would see, not only through internal
  state.

### Key Entities

- **Upgrade Plan**: The operator's declaration of an upgrade —
  strategy (minor in-place, major in-place, major logical-bridge),
  current version, target version, and node visit order. The subject
  being validated and executed.
- **Dry-Run (Prepare) Outcome**: The full list of reasons a plan
  would be rejected, or an empty list meaning the plan is
  executable. Never changes cluster state.
- **Execution Outcome**: Success, or a failure naming the step that
  failed (validation, stop, swap, start, version confirmation) —
  including the special upstream gate refusal for major strategies.
- **Gate Sentinel**: The pinned expectation of the upstream engine's
  major-execution refusal, whose failure signals an upstream contract
  change.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Every quadrant of the upgrade matrix — {minor, major} ×
  {happy path, bad path} — has at least one automated test, and all
  of them pass in CI.
- **SC-002**: A deliberately introduced regression in the minor
  upgrade sequence (for example, restarting before the swap step, or
  skipping version confirmation) causes at least one test to fail.
- **SC-003**: An upstream change to the major-upgrade gate (lifted or
  reworded) is detected by the first CI run against the changed
  engine, with zero manual monitoring.
- **SC-004**: Every upgrade behavior promised in the operator runbook
  section on minor rolling upgrades and gated major upgrades is
  covered by at least one test assertion.
- **SC-005**: The added tests complete quickly enough to keep the
  existing CI wall-clock budget within 20% of its pre-feature
  duration.

## Assumptions

- Scope is the pgman-proxy repository only. The upstream engine
  (`pg-manager`) already tests its own minor-upgrade execution logic;
  those tests are not duplicated here. What pgman-proxy owns — and
  therefore tests — is its operator-facing surface: request
  validation, delegation to the engine, ordering of the delegated
  sequence, error surfacing, and peer forwarding.
- The binary-swap step exposed through the operator surface is a
  no-op hook in v1 (actual binary swaps are a host concern). The
  real-server happy-path test may therefore use a same-version
  "no-op swap" plan to exercise the full stop → swap → start →
  confirm sequence against a real database without requiring two
  installed database versions in CI.
- Major-upgrade execution is gated by the upstream engine until its
  v0.7.0 hardening release. For this feature, the major "happy path"
  is defined as plan validation succeeding; the gated execution
  refusal is the expected, pinned behavior — not a defect.
- Bad-path scenarios (swap failure, version mismatch, engine errors)
  may be exercised with simulated engine behavior, since forcing
  those failures on a real server is impractical; the happy path
  additionally runs against a real server per Constitution VI.
- This is a test-only feature: no operator-visible behavior changes.
  Any test hooks added must not alter production runtime behavior.
- The existing CI pipeline (including the discipline gate that
  forbids upgrade-tooling literals in proxy source) remains in force;
  tests must comply with it.
