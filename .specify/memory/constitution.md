<!--
SYNC IMPACT REPORT
==================
Version change: 1.0.0 → 1.1.0
Bump rationale: MINOR — added two principles (III. Active/Active Coordination
Correctness, IV. Thin Scaffold over pg-manager) and broadened scope guidance
(VII. Scope Discipline & Reversibility) to reflect the project's actual shape:
an opinionated active/active PostgreSQL proxy that wraps `../pg-manager` and
uses NATS for message bus + leader election, deployable as standalone process,
microservice, or sidecar — explicitly excluding Kubernetes/Helm concerns.
No principles removed; existing principles unchanged in intent. References
to Principle numbering in Quality Gates and Code Review updated to match.

Modified principles:
- I.   Wire-Protocol Fidelity                           (unchanged)
- II.  Fail-Closed Safety                               (unchanged)
- III. Active/Active Coordination Correctness           (NEW)
- IV.  Thin Scaffold over pg-manager                    (NEW)
- V.   Observability by Default                         (was III; renumbered)
- VI.  Integration-First Testing (NON-NEGOTIABLE)       (was IV; renumbered)
- VII. Scope Discipline & Reversibility                 (was V "Simplicity &
       Reversibility"; renamed and broadened to include explicit non-scope:
       Kubernetes/Helm orchestration belong to a separate project)

Added sections:
- Architecture Overview (one-paragraph topological frame for the principles)
- Additional Constraints: new "Topology & Dependencies" subsection (NATS
  required; pg-manager is the wrapped library; three deployment modes).
- Additional Constraints: new "Out of Scope" subsection (no Kubernetes API
  consumption, no Helm chart, no CRDs).

Removed sections: none.

Templates requiring updates:
- ✅ .specify/templates/plan-template.md — Constitution Check section is generic.
- ✅ .specify/templates/spec-template.md — no constitution-specific anchors.
- ✅ .specify/templates/tasks-template.md — sample tasks; no anchors.
- ✅ .specify/templates/checklist-template.md — generic; no anchors.
- ✅ CLAUDE.md — only points readers to the current plan; no principle references.

Follow-up TODOs:
- README.md still does not exist; when added, MUST cite this constitution and
  reproduce the "Out of Scope" subsection so external contributors don't open
  Kubernetes/Helm work against this repo.
-->

# pgman-proxy Constitution

`pgman-proxy` is an opinionated active/active PostgreSQL proxy (Go) that wraps
the `pg-manager` library (sibling repo `../pg-manager`) and scaffolds it with
NATS as the message bus and leader-election substrate. It is deployable as a
standalone process, a microservice, or a sidecar inside a PostgreSQL pod. This
constitution defines non-negotiable engineering rules. All plans, specs, tasks,
and code reviews MUST verify compliance with these principles.

## Architecture Overview

The proxy itself is a thin scaffold. The protocol- and management-level work
lives in `pg-manager`; coordination across active replicas (leader election,
membership, control-plane events) flows through NATS. `pgman-proxy` MUST stay
deployment-mode-neutral: nothing in the codebase may assume Kubernetes, Helm,
service-mesh, or any specific orchestrator.

## Core Principles

### I. Wire-Protocol Fidelity

The proxy MUST be transparent to PostgreSQL clients and servers at the wire
level. Frame parsing, message ordering, parameter passing, error semantics, and
authentication exchanges MUST round-trip byte-accurate against the upstream
server unless an explicit policy intercepts them. Any interception MUST be
documented as a named policy with a regression test that pins the altered bytes.

**Rules:**
- MUST NOT re-encode pass-through traffic; copy bytes when no transform is in play.
- MUST preserve PostgreSQL error codes (SQLSTATE) and severity end-to-end.
- MUST treat any divergence from upstream behavior as a breaking change (see §VII).

**Rationale:** A proxy that silently mutates protocol semantics is undebuggable
and dangerous — clients will see errors that no `psql` reproduction can explain.

### II. Fail-Closed Safety

When the proxy cannot uphold its safety contract — TLS termination, auth
verification, policy decision, connection pool integrity, NATS coordination
liveness — it MUST refuse the operation. It MUST NOT degrade to a permissive
state to "keep traffic flowing."

**Rules:**
- MUST close client connections on policy-engine errors; never bypass the policy.
- MUST reject startup if required config is missing or invalid; no defaults that
  expand the trust boundary (e.g., never default to `sslmode=disable`).
- MUST NOT log or emit secrets, even in error paths.
- On loss of NATS connectivity, the proxy MUST follow the rule of Principle III
  for its current role; it MUST NOT silently re-elect itself or fan out writes
  on its own authority.

**Rationale:** A management proxy sits in the credential and authorization path.
Open-failure modes turn it into an attack vector.

### III. Active/Active Coordination Correctness

`pgman-proxy` runs as a peer cluster. Coordination decisions — leader identity,
write routing, fencing of stale leaders, control-plane fan-out — MUST go
through NATS. The proxy MUST be correct under split-brain, partition, and
NATS-outage conditions.

**Rules:**
- Leader election MUST be performed via NATS primitives (e.g., JetStream
  KV/lease or equivalent); the proxy MUST NOT invent its own consensus.
- Every leader-only action MUST verify lease validity at the moment of action,
  not just at election time. Stale leaders MUST self-fence.
- The proxy MUST NOT serve writes when its NATS lease cannot be confirmed.
- Cluster invariants (only one writer per shard at a time; idempotent control
  events) MUST be expressible as assertions in tests; violations MUST fail CI.
- Configuration MUST allow tuning of lease/heartbeat timeouts, but defaults
  MUST favor safety over availability.

**Rationale:** Active/active that "usually works" is the worst possible
operational mode for a database proxy. Correctness under partition is the
product, not a feature.

### IV. Thin Scaffold over pg-manager

`pgman-proxy` is a scaffold; `pg-manager` is the engine. The proxy MUST NOT
duplicate, fork, or reimplement functionality already provided by
`pg-manager`. New protocol- or management-layer behavior MUST land in
`pg-manager` first, then be exposed by the proxy.

**Rules:**
- The proxy MUST depend on `pg-manager` as an external module; no copy-paste of
  its source into this repo.
- If a needed capability is missing in `pg-manager`, the PR MUST link to the
  upstream (or sibling-repo) change that adds it; do not work around with a
  proxy-local reimplementation.
- The proxy's own code SHOULD be limited to: process lifecycle, configuration,
  NATS wiring, deployment-mode adapters, and observability glue.
- Any logic in this repo that arguably belongs in `pg-manager` MUST be flagged
  in the PR description with a follow-up issue to upstream it.

**Rationale:** A scaffold project that grows its own duplicate engine becomes
two products with diverging behavior. Discipline here keeps the architecture
honest.

### V. Observability by Default

Every connection, query routing decision, policy outcome, NATS coordination
event, and leadership transition MUST be observable without code changes.
Structured logs, metrics, and traces are first-class — not add-ons. Every
surface that can fail MUST emit a structured event with a stable schema.

**Rules:**
- MUST emit structured logs (JSON) with a documented field schema; no `fmt.Println`
  in production code paths.
- MUST expose Prometheus-compatible metrics for connection counts, query latency
  (p50/p95/p99), policy decisions, error rates by SQLSTATE, NATS round-trip
  latency, leadership state, and lease-renewal failures.
- MUST propagate trace context (W3C Trace Context) across the proxy hop and
  attach it to NATS messages where the message schema permits.
- Log/metric/trace field names MUST be stable; renames are MINOR-version events.

**Rationale:** A proxy is only useful if operators can answer "what did it just
do, and why?" in under a minute during an incident — and in active/active that
question often crosses replicas.

### VI. Integration-First Testing (NON-NEGOTIABLE)

Unit tests are insufficient for a wire-protocol component coordinating across
peers. Every behavior that crosses the proxy boundary MUST have an integration
test that runs against a real PostgreSQL server and a real NATS server,
containerized in CI, and a real client driver.

**Rules:**
- MUST run integration tests against at least one supported PostgreSQL major
  version per release; matrix expanded as supported versions grow.
- MUST run a multi-replica test topology (≥ 2 proxy peers + real NATS) for
  every change touching coordination, leader election, or write routing.
- Contract tests MUST cover: startup/auth handshake, simple query, extended
  query (parse/bind/execute), COPY, error propagation, TLS, connection
  termination, leader election, leader fencing, and NATS-outage recovery.
- New protocol surface, policy hook, or coordination message MUST ship with an
  integration test before merge — no "tests coming in a follow-up."
- Mocks of PostgreSQL or NATS are PERMITTED only for fault-injection cases that
  real servers cannot reproduce deterministically; such mocks MUST be paired
  with at least one real-server test of the happy path.

**Rationale:** Unit-level mocks of the wire protocol or of NATS coordination
drift from real server behavior. Integration tests catch the regressions that
matter.

### VII. Scope Discipline & Reversibility

Every feature MUST start as the smallest correct change, MUST stay inside the
project's declared scope, and MUST be reversible without breaking deployed
clients.

**Rules:**
- New configuration keys MUST have a default that preserves prior behavior.
- Breaking protocol or config changes MUST follow MAJOR-version semantics
  (see Governance) and ship with a documented migration path.
- YAGNI applies: no dead code, no "framework" abstractions until two real
  callers exist.
- Out-of-scope concerns (Kubernetes API, Helm charts, CRDs, operators,
  cluster-bootstrap orchestration) MUST be rejected at review time and routed
  to the dedicated downstream project. PRs that introduce them MUST be closed
  with a pointer to that project.
- Complexity that violates this principle MUST be recorded in the plan's
  **Complexity Tracking** table with a justification and the rejected simpler
  alternative.

**Rationale:** Complexity in a credential-path component compounds operational
risk. Scope creep into orchestration would couple this proxy to one specific
deployment platform and break the standalone/microservice/sidecar promise.

## Additional Constraints

**Topology & Dependencies:**
- `pg-manager` (sibling module) is the wrapped engine; the proxy MUST depend on
  it as an external Go module.
- NATS is a hard runtime dependency for clustered operation. The proxy MUST
  document the minimum required NATS feature set (e.g., JetStream KV) in its
  README and pin a tested version range.
- Three supported deployment modes MUST all be exercised in CI smoke tests:
  1. **Standalone process** — single binary, single proxy peer, NATS optional
     for single-node mode but required for any HA mode.
  2. **Microservice** — multi-replica deployment behind a load balancer.
  3. **Sidecar** — colocated with a PostgreSQL instance in the same pod / VM /
     container group.
- The proxy MUST NOT assume any specific service-discovery mechanism beyond
  NATS subjects and explicit configuration.

**Out of Scope (do not build here):**
- Kubernetes API consumption, controller loops, CRDs, admission webhooks.
- Helm charts, Kustomize bases, operator bundles.
- Cluster bootstrap orchestration of PostgreSQL itself (that lives in
  `pg-manager` or downstream tooling).
- A separate project owns those concerns and consumes `pgman-proxy` as a
  black-box binary; cross-cutting requests SHOULD be filed against that
  project, not this one.

**Language & toolchain:**
- Go is the implementation language. Minimum supported version tracks the two
  most recent stable Go releases.
- Dependencies MUST be vendored or pinned via `go.mod` with checksums
  (`go.sum`); ad-hoc `replace` directives require a comment explaining why
  (note: a `replace` for the local `../pg-manager` path during development is
  acceptable, but release builds MUST use a tagged version).
- `go vet`, `staticcheck`, and `gofmt`/`goimports` MUST pass on every commit.

**Security & operational:**
- TLS for upstream connections MUST default to `verify-full`; weakening it
  requires an explicit, named, per-route policy.
- Secrets (database, NATS auth) MUST be sourced from environment or a
  secret-manager interface — never from disk-resident plaintext config files.
- The proxy binary MUST start cleanly under non-root, with a writable directory
  no larger than its log/state needs.

**Performance baseline:**
- Per-query overhead added by the proxy MUST stay under 1ms p99 for simple
  queries on the local-loopback benchmark; regressions of >10% MUST be flagged
  in the PR description.
- Leader-failover MUST complete (new leader confirmed and accepting writes) in
  under 5s p99 in the CI multi-replica topology; regressions MUST be flagged.

## Development Workflow & Quality Gates

**Spec-Kit flow** (this project uses Spec Kit):
1. `/speckit-specify` → spec.md captures WHAT and WHY (user-visible).
2. `/speckit-clarify` → resolves NEEDS CLARIFICATION.
3. `/speckit-plan` → produces plan.md; the **Constitution Check** gate MUST pass
   before Phase 0 research and again after Phase 1 design.
4. `/speckit-tasks` → tasks.md, organized so each user story is independently
   testable.
5. `/speckit-implement` → tasks executed in order; each task ends in a commit.

**Quality gates (each MUST pass before merge):**
- All Core Principles (I–VII) reviewed against the diff. Any violation must
  appear in the plan's **Complexity Tracking** table with justification.
- Out-of-scope check: no new Kubernetes/Helm/CRD code (Principle VII).
- Integration tests cover every new protocol or coordination surface
  (Principle VI), including a multi-replica scenario when applicable.
- Structured logs / metrics / traces emitted for new failure modes
  (Principle V).
- No new secret material in code, logs, fixtures, or tests.
- Performance and failover baselines (above) are unbroken or regression is
  documented.

**Code review requirements:**
- At least one reviewer MUST sign off explicitly on Principles I, II, III,
  and VI for any PR touching wire-protocol code, auth flow, or coordination.
- Doc-only and test-only changes MAY use a lighter review, but still require
  one approver.

## Governance

**Authority.** This constitution supersedes ad-hoc engineering preferences. Where
a Spec-Kit template, command prompt, or runtime guidance file conflicts with this
document, this document wins until the conflict is resolved by amendment.

**Amendments.** Any change to this constitution MUST:
1. Land as a PR that modifies `.specify/memory/constitution.md` and updates the
   Sync Impact Report comment at the top of the file.
2. Bump `CONSTITUTION_VERSION` per the policy below.
3. Update `LAST_AMENDED_DATE` to the merge date (ISO `YYYY-MM-DD`).
4. Propagate changes to dependent templates (`plan-template.md`,
   `spec-template.md`, `tasks-template.md`, `checklist-template.md`) and to any
   runtime guidance docs (`CLAUDE.md`, `README.md`, `docs/`).

**Versioning policy (semantic):**
- **MAJOR** — backward-incompatible governance or principle removal/redefinition,
  or any change that invalidates existing plans or merged code's compliance status.
- **MINOR** — new principle or section added; materially expanded guidance that
  ratchets requirements upward without invalidating prior work.
- **PATCH** — clarifications, wording, typo fixes, non-semantic refinements.

**Compliance review.** Quality gates above are enforced per-PR. In addition,
once per release cycle the maintainer MUST audit a random sample of merged PRs
for principle compliance and file an issue if drift is detected.

**Runtime guidance.** Day-to-day technology, file-layout, and command guidance
lives in `CLAUDE.md` (and `README.md` once the project gains source code).
Those files MUST cite this constitution; they do not redefine it.

**Version**: 1.1.0 | **Ratified**: 2026-05-09 | **Last Amended**: 2026-05-09
