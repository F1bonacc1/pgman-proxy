# Research: Upgrade Test Coverage

**Feature**: 004-upgrade-tests | **Date**: 2026-07-06

No NEEDS CLARIFICATION markers existed in the Technical Context; the
research below records the design decisions and the evidence behind
them. All code references were verified against the working tree and
the pinned `pg-manager v0.3.0` module cache on 2026-07-06.

## R1. Test placement: two tiers, matching Constitution VI

**Decision**: Split coverage between a contract tier
(`internal/control/handlers_upgrade_test.go`, `fakeEngine`) and an
integration tier (`tests/integration/lcm_upgrade_test.go`, compose
topology, real engine + real PostgreSQL 17 + real embedded NATS).

**Rationale**: Constitution VI permits mocks *only* for fault
injection real servers cannot reproduce deterministically, and
requires them to be paired with at least one real-server happy-path
test. Swap failure and post-swap version mismatch cannot be forced
through the HTTP surface (v1 hardwires a no-op pre-swap in
`handlers_upgrade.go:62`), so those live in the contract tier via
`fakeEngine.executeUpgradeFn`. Everything the real engine can produce
on its own — plan-validation refusals, the major gate, the minor
happy path — lives in the integration tier.

**Alternatives considered**: (a) All-integration — rejected: swap
failure/version mismatch are unreachable with the no-op pre-swap, and
forwarding-timeout injection needs a fake router. (b) All-unit —
rejected outright by Constitution VI (non-negotiable).

## R2. Real-server minor happy path: same-version plan, no-op swap

**Decision**: The integration happy path submits a minor plan whose
target equals the server's current version *as the engine sees it*
(probe `server_version_num` and decompose it exactly like pg-manager
v0.3.0's `pgproto.Executor.Version`: `major = num/10000`,
`minor = (num/100)%100`). Execution drives the real stop → (no-op)
swap → start → version-probe sequence via `upgrade.RunMinorLocal`
inside the engine.

*Implementation finding (2026-07-06)*: the engine uses the
pre-PostgreSQL-10 decomposition scheme, so for PG ≥ 10 it reports
Minor = 0 and puts the real minor in Patch; a plan targeting the
human-readable minor fails the post-swap confirm ("expected minor 10,
got 0"). Pinned in contracts/upgrade-endpoints.md § Version
semantics; flagged as an upstream pg-manager fix candidate
(Constitution IV: don't work around in proxy production code — tests
encode the current truth).

**Rationale**: v1's HTTP surface deliberately wires only a no-op
pre-swap (binary swaps are a host concern — `handlers_upgrade.go`
comment, Constitution VII trust-boundary note), so a same-version
plan is the *maximal* upgrade the HTTP surface can truly perform. It
still exercises every proxy-owned behavior end-to-end plus the
engine's real lifecycle actions against live PostgreSQL. Probing the
live version first makes the test robust to image bumps
(`postgres:17-bookworm` today).

**Alternatives considered**: (a) Two PostgreSQL versions in the test
image with a real symlink-flip pre-swap — rejected: the HTTP surface
cannot deliver a custom pre-swap by design; testing it would require
an out-of-tree build, out of scope. (b) Skipping real execution and
only dry-running — rejected: violates Constitution VI and spec FR-011.

## R3. Execute target and disruption handling

**Decision**: Both upgrade routes are `leaderOnly=true`
(`server.go:225-226`), so the integration execute test addresses the
current leader (discovered the same way existing LCM tests do) and
accepts that the leader's PostgreSQL restarts during the test. After
the accepted envelope returns, the test polls the data plane
(`Peer.DSN()` query loop with a generous deadline) until the cluster
serves queries again, then asserts the reported version.

**Rationale**: Leader routing is existing behavior, not a choice this
feature makes. The stop window is short (fast shutdown + immediate
start of an idle instance); existing tests already exercise heavier
planned disruptions (`lcm_switchover_test.go`, `failover_test.go`)
with poll-until-recovered patterns worth copying.

**Risk & mitigation**: If the brief primary stop proves flaky under
CI load (e.g., coordination machinery reacts mid-window), bound the
plan with a `PerNodeBudget` and lengthen the recovery poll before
considering any topology-level accommodation. Flakiness observed
during implementation must be fixed in the test, not by weakening the
assertion to "eventually some envelope".

## R4. Major-upgrade gate: pin the exact upstream refusal (sentinel)

**Decision**: The integration tier executes a well-formed major plan
(`UpgradeMajorInPlace`, target major = current + 1) and asserts the
envelope is `failed` with `error.code = "engine_error"` and
`error.message` containing the exact upstream string
`"upgrade: major strategies wired but gated on v0.7.0"`.

**Rationale**: Verified in the pinned module: `pg-manager
v0.3.0/manager/backup_upgrade.go` returns exactly that error for both
major strategies after validation passes. Pinning the full string
makes the test a sentinel: when a future `pg-manager` bump lifts or
rewords the gate, this test fails on the first CI run against the new
engine, forcing a conscious decision (spec US3, FR-008, SC-003).
Execution is safe on the shared 3-peer topology — the gate fires
before any lifecycle action.

**Alternatives considered**: (a) Version-conditional skip when the
engine is ≥ v0.7.0 — rejected: silent drift is precisely what the
spec forbids. (b) Substring-only match ("gated") — rejected: a reword
should also trip the sentinel since the runbook quotes the message.

## R5. Sequence-order assertions: delegate internals, pin the boundary

**Decision**: The proxy does not re-test `RunMinorLocal`'s internal
ordering (stop before swap, no start after failed swap, mismatch
detection) — `pg-manager` owns and already tests that
(`upgrade/upgrade_test.go` upstream). The proxy tiers assert the
boundary instead:

- Contract tier: the decoded `UpgradePlan` reaches the engine
  field-for-field intact (capture via `executeUpgradeFn`), the
  supplied pre-swap is non-nil, and an engine error of each simulated
  class (swap failure, version mismatch, validation refusal) maps to
  outcome `failed` / code `engine_error` with the message preserved
  (`finishMutation`, `handlers_membership.go:111`).
- Integration tier: observable effects of the real sequence — data
  plane drops and recovers, version confirmed, `accepted` envelope.

**Rationale**: Constitution IV (thin scaffold): duplicating engine
ordering tests here would create a second diverging source of truth.
The spec's Assumptions section already scopes pgman-proxy's ownership
to validation/delegation/surfacing/forwarding.

## R6. Bad-path matrix placement

**Decision**:

| Bad path | Tier | Mechanism |
|---|---|---|
| Downgrade plan rejected (FR-005) | Integration | Real engine `PrepareUpgrade` refusal via `/v1/upgrade/prepare`; execute variant also asserted (ExecuteUpgrade re-validates first) |
| Minor plan crossing major boundary (FR-006) | Integration | Real engine refusal via prepare |
| Major plan target not higher (US3-AS2) | Integration | Real engine refusal via prepare |
| Well-formed major plan validates (FR-007) | Integration | Real engine prepare succeeds (`accepted`) |
| Major execute gated (FR-008) | Integration | Real engine gate error, exact-string sentinel |
| Swap-step failure surfaces, no restart (FR-003) | Contract | `executeUpgradeFn` returns the upstream-shaped error; assert `failed`/`engine_error` envelope + message passthrough |
| Version-mismatch failure surfaces (FR-004) | Contract | Same mechanism, mismatch-shaped error |
| Malformed/empty body rejected pre-engine (FR-009) | Contract | Assert `rejected`/`invalid_argument` (HTTP 400) and that no engine hook fired |
| Forwarding relay success + failure (FR-010) | Contract | `fakeLeader{leader:false}` in forward mode; relay of leader envelope verbatim; `ErrLeaderRouteTimeout` → `leader_route_timeout` (HTTP 504) |

**Rationale**: Validation logic is upstream; testing it through the
real engine (integration) tests the truth, while the contract tier
tests the proxy's surfacing of injected failures — the only part the
proxy owns. Envelope mappings verified in source: engine error →
`CodeEngineError`/`failed` (`finishMutation`), decode error →
`CodeInvalidArgument`/`rejected` (`rejectInvalid`,
`httpStatusForCode` → 400), forward timeout →
`CodeLeaderRouteTimeout` (→ 504).

## R7. CI wiring: no changes needed — with one pre-existing gap flagged

**Decision**: No CI workflow changes. Contract-tier tests are picked
up by the existing per-PR job (`go test -race ./internal/...`);
integration-tier tests are picked up by `make integration`
(`go test -tags=integration -timeout=15m ./tests/integration/...`).

**Correction (2026-07-06, verified during implementation)**: ci.yml's
header comment says integration tests "run on a separate workflow",
but **no such workflow existed** in `.github/workflows/` — the
integration suite did not run in CI at all. Related pre-existing
finding: a full local `make integration` run failed independently of
this feature — `TestFailover` SIGKILLed the leader container and
nothing restarted it, so every later test needing that node failed.

**Resolution (2026-07-07, follow-up work executed alongside 004)**:
both gaps are closed. `.github/workflows/nightly-integration.yml` now
runs `make integration` nightly (cron + manual dispatch), checking out
the pinned pg-manager version as the sibling module. The failover
test restores the killed peer via `t.Cleanup` and was renamed
`zz_failover_test.go` so its variable-timeline aftermath runs last.
The suite hardening also surfaced real product fixes (unwired
observability gauges, uninstalled SIGHUP handler, missing
FailoverDelay/LeaseTTL config bindings, and an upstream pg-manager
runRewind DSN bug) and led to relaxing the SC-002 failover budget to
45s p99 (constitution v1.3.0): the p99 is dominated by embedded-NATS
JetStream re-election when the killed primary also hosted the
leadership-KV RAFT leader, plus a first-commit stall while
synchronous replication waits for a standby to re-attach — structural
terms no timer tuning removes.

**Rationale**: Both invocations are package-glob based; new `_test.go`
files in existing directories are collected automatically. The
`lcm-discipline-gate` excludes `tests/` and `*_test.go`, so test
files may name `pg_upgrade` etc. freely — but nothing in this
feature touches production source anyway.

## R8. JSON shape of the request body

**Decision**: Tests marshal plans as
`{"plan": {"Strategy": <int>, "TargetMajor": <int>, "TargetMinor": <int>}}`
using the exported Go field names.

**Rationale**: `pgmanager.UpgradePlan` (v0.3.0 `types.go:977`) has no
JSON tags, so encoding/json uses exported names (decode is
case-insensitive, which is why the existing smoke test's lowercase
`{"strategy":1}` worked). Strategy values verified: `0` =
`UpgradeMinor`, `1` = `UpgradeMajorInPlace`, `2` =
`UpgradeMajorLogicalBridge`. Integration tests build bodies with
typed structs from the module rather than raw strings wherever the
test package already imports it, keeping drift impossible.
