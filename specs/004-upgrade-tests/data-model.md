# Data Model: Upgrade Test Coverage

**Feature**: 004-upgrade-tests | **Date**: 2026-07-06

Test-only feature: no new persistent entities. The entities below are
the *existing* shapes the tests construct and assert against, pinned
here so the tasks phase can generate assertions without re-deriving
them.

## UpgradePlan (existing — `pg-manager v0.3.0`)

The operator's declaration of an upgrade, decoded from the request
body and passed to the engine verbatim.

| Field | Type | JSON name | Notes |
|---|---|---|---|
| Strategy | UpgradeStrategy (int) | `Strategy` | `0` minor, `1` major-in-place, `2` major-logical-bridge (no json tags; decode is case-insensitive) |
| TargetMajor | int | `TargetMajor` | |
| TargetMinor | int | `TargetMinor` | |
| NodeOrder | []NodeID | `NodeOrder` | empty ⇒ topology peer order |
| PerNodeBudget | time.Duration (ns int) | `PerNodeBudget` | bound per-node work |

**Validation rules (owned by the engine; asserted through it):**
- Minor strategy: `TargetMajor` MUST equal the running major;
  `TargetMinor` MUST be ≥ the running minor (downgrade rejected).
- Major strategies: `TargetMajor` MUST be strictly greater than the
  running major.
- Unknown strategy value: execution refused (`unknown strategy`).

## upgradeReq (existing — `internal/control/handlers_upgrade.go:28`)

Request body for both endpoints:
`{"plan": <UpgradePlan>, "request_id": "<optional, accepted-but-ignored>"}`.

## Response envelope (existing — `internal/control` / mirrored by `lcmResponse`)

| Field | Values asserted by tests |
|---|---|
| `operation` | `PrepareUpgrade` \| `ExecuteUpgrade` |
| `request_id` | non-empty; echoed in `X-Request-Id` |
| `outcome` | `accepted` \| `rejected` \| `failed` |
| `error.code` | see state mapping below |
| `error.message` | engine/message text preserved verbatim |

### Outcome state mapping (the contract under test)

| Stimulus | Outcome | error.code | HTTP |
|---|---|---|---|
| Engine returns nil | `accepted` | — | 200 |
| Engine returns any error (validation refusal, swap failure, version mismatch, gate) | `failed` | `engine_error` | 500 |
| Body fails to decode | `rejected` | `invalid_argument` | 400 |
| Missing/invalid bearer token | `rejected` | `auth_required` / `auth_invalid` | 401 / 403 |
| Forward to leader times out | `failed` | `leader_route_timeout` | 504 |
| Forward to a dead leader (no NATS responders) | `failed` | `engine_error` | 500 |
| Non-leader in redirect mode | `rejected` | `not_leader` | (redirect semantics) |
| Forwarded successfully (any inner outcome) | outer `accepted`; **effective outcome is the leader's envelope nested in `engine_result`** | inner code | outer 200 |

## Gate Sentinel (pinned constant)

The exact upstream refusal for major-strategy execution, from
`pg-manager v0.3.0 manager/backup_upgrade.go:102`:

```
upgrade: major strategies wired but gated on v0.7.0
```

Tests assert `error.message` contains this full string. A pg-manager
bump that lifts or rewords the gate breaks the assertion — that
failure is the sentinel firing, not a regression in pgman-proxy.

## Test Matrix (requirement → tier → new file)

| Req | Scenario | Tier | File |
|---|---|---|---|
| FR-001 | Minor plan accepted by prepare | Integration | `tests/integration/lcm_upgrade_test.go` |
| FR-002, FR-011 | Minor execute happy path vs real PG 17 (same-version plan) | Integration | `tests/integration/lcm_upgrade_test.go` |
| FR-003 | Swap failure surfaced (injected) | Contract | `internal/control/handlers_upgrade_test.go` |
| FR-004 | Version mismatch surfaced (injected) | Contract | `internal/control/handlers_upgrade_test.go` |
| FR-005 | Downgrade rejected (prepare + execute) | Integration | `tests/integration/lcm_upgrade_test.go` |
| FR-006 | Minor crossing major rejected | Integration | `tests/integration/lcm_upgrade_test.go` |
| FR-007 | Major plan validates via prepare | Integration | `tests/integration/lcm_upgrade_test.go` |
| FR-008 | Major execute gated — exact-string sentinel | Integration | `tests/integration/lcm_upgrade_test.go` |
| FR-009 | Malformed body → `rejected`, engine never called | Contract | `internal/control/handlers_upgrade_test.go` |
| FR-010 | Forward relay (success + failure) & timeout | Contract | `internal/control/handlers_upgrade_test.go` |
| FR-013 | All bad paths asserted via envelope fields | Both | both files |
| — | Plan pass-through fidelity + non-nil pre-swap | Contract | `internal/control/handlers_upgrade_test.go` |

No state transitions beyond the envelope outcomes above; no new
entities are persisted by this feature.
