# Contract — Constitution Amendment v1.1.0 → v1.2.0

**Feature**: `002-embedded-nats-cluster`
**Phase**: 1
**Status**: To be applied as the **first** task in `/speckit-tasks`
execution. Without this amendment, merging the feature would put the
codebase in a state where the constitution asserts NATS is
"operator-provisioned" while the binary embeds it — silently
un-constitutional. The amendment lands in the same phase as the feature.

## Type

**MINOR** (1.1.0 → 1.2.0). Per the constitution's own versioning policy:

> MINOR — new principle or section added; materially expanded guidance
> that ratchets requirements upward without invalidating prior work.

No principle is removed or redefined. The architectural framing broadens
to reflect that the coordination plane is now in-process. Feature 001's
plan and code are amended (FR-003 reversal in 002's spec is in plain
sight) — not invalidated.

## Diff text

The amendment edits three locations in `.specify/memory/constitution.md`.

### 1. Sync Impact Report (top-of-file comment)

Add a new entry above the v1.0.0→v1.1.0 entry:

```text
Version change: 1.1.0 → 1.2.0
Bump rationale: MINOR — broadened the Architecture Overview and the
"Topology & Dependencies" subsection of Additional Constraints to
reflect feature 002 (embedded NATS cluster). NATS is now embedded in
every proxy peer; the project ships with a bundled NATS server and MUST
NOT require an external NATS service. No principle removed or redefined;
existing principles unchanged in intent. References to external-NATS
provisioning are removed; the "minimum required NATS feature set"
documentation requirement is preserved and now applies to the bundled
version.

Modified principles: none (text unchanged).

Added sections: none.

Removed sections: none.

Templates requiring updates:
- ✅ .specify/templates/plan-template.md — generic; no NATS anchors.
- ✅ .specify/templates/spec-template.md — no NATS anchors.
- ✅ .specify/templates/tasks-template.md — sample tasks only.
- ✅ .specify/templates/checklist-template.md — generic.
- ✅ CLAUDE.md — points readers to the current plan; no NATS anchors.

Follow-up TODOs: README.md cites this constitution; once 002 ships it
MUST replace the "operator-provisioned NATS" sentence with the bundled
language.
```

### 2. Architecture Overview (text replacement)

Find:

```text
The proxy itself is a thin scaffold. The protocol- and management-level work
lives in `pg-manager`; coordination across active replicas (leader election,
membership, control-plane events) flows through NATS. `pgman-proxy` MUST stay
deployment-mode-neutral: nothing in the codebase may assume Kubernetes, Helm,
service-mesh, or any specific orchestrator.
```

Replace with:

```text
The proxy itself is a thin scaffold. The protocol- and management-level work
lives in `pg-manager`; coordination across active replicas (leader election,
membership, control-plane events) flows through a NATS cluster **embedded
in the proxy peers themselves** — every replica boots its own in-process
NATS server, and the replicas mesh into a single coordination cluster via
NATS routes. The project does NOT depend on, ship with, or require an
external `nats-server` process. `pgman-proxy` MUST stay deployment-mode-
neutral: nothing in the codebase may assume Kubernetes, Helm,
service-mesh, or any specific orchestrator.
```

### 3. Additional Constraints → Topology & Dependencies (text replacement)

Find:

```text
**Topology & Dependencies:**
- `pg-manager` (sibling module) is the wrapped engine; the proxy MUST depend on
  it as an external Go module.
- NATS is a hard runtime dependency for clustered operation. The proxy MUST
  document the minimum required NATS feature set (e.g., JetStream KV) in its
  README and pin a tested version range.
- Three supported deployment modes MUST all be exercised in CI smoke tests:
```

Replace with:

```text
**Topology & Dependencies:**
- `pg-manager` (sibling module) is the wrapped engine; the proxy MUST depend on
  it as an external Go module.
- NATS is **embedded in every proxy peer** via the upstream
  `github.com/nats-io/nats-server/v2` Go module; the binary MUST NOT
  require an external `nats-server` process, container, or service for
  any clustered operation. The proxy MUST document the minimum required
  NATS feature set (e.g., JetStream KV) and pin the bundled NATS version
  in its README; bundled-version bumps that change wire compatibility or
  on-disk format are MAJOR-version events for the proxy.
- Three supported deployment modes MUST all be exercised in CI smoke tests:
```

The bullet describing the three deployment modes (1. Standalone, 2.
Microservice, 3. Sidecar) is preserved unchanged. The text "NATS optional
for single-node mode but required for any HA mode" inside the
"Standalone" bullet is replaced with "embedded NATS handles single-peer
and multi-peer modes uniformly."

### 4. Footer

Update the footer line:

```text
**Version**: 1.1.0 | **Ratified**: 2026-05-09 | **Last Amended**: 2026-05-09
```

Replace with:

```text
**Version**: 1.2.0 | **Ratified**: 2026-05-09 | **Last Amended**: 2026-05-09
```

## Application

Applied as a single commit — `chore(constitution): bump to v1.2.0
(embedded NATS topology)` — by the first task generated in
`/speckit-tasks`. The commit MUST land before any `internal/embedded/`
code so that no merge in this branch is ever in the silently-un-
constitutional state described above.
