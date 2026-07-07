# Quickstart: Upgrade Test Coverage

**Feature**: 004-upgrade-tests

## Run the contract tier (fast, no Docker)

```bash
go test -race -count=1 -run 'Upgrade' ./internal/control/
```

Covers: plan pass-through fidelity, injected swap-failure and
version-mismatch surfacing, malformed-body rejection, forwarding
relay + timeout. Sub-second; runs in the per-PR CI job automatically
(`go test -race ./internal/...`).

## Run the integration tier (Docker required)

```bash
make integration
# or, narrowed to this feature's tests:
go test -tags=integration -timeout=15m -run 'Upgrade' ./tests/integration/
```

Brings up the 3-peer compose topology (real PostgreSQL 17, real
embedded NATS) once per `go test` invocation. Covers: real-engine
plan validation (downgrade / cross-major / non-higher-major
refusals), major-plan prepare acceptance, the pinned major-execution
gate sentinel, and the minor-execute happy path (same-version plan;
the leader's PostgreSQL genuinely stops and restarts — expect a brief
data-plane blip during the test).

> **Update (2026-07-07)**: the pre-existing full-suite failures noted
> during implementation are fixed — `TestFailover` now restores the
> killed peer and runs last (`zz_failover_test.go`), and
> `.github/workflows/nightly-integration.yml` runs `make integration`
> nightly. Details in research.md R7. The narrowed `-run 'TestUpgrade'`
> invocation above still boots its own topology and is fully green
> (verified 3× consecutive).

## What success looks like

- All `Upgrade*` tests green in both tiers.
- `make grep-gates` still clean (test files are exempt from the
  lcm-discipline gate; production source untouched).
- No production diffs: `git diff --stat main -- cmd internal ':!*_test.go'`
  shows only test files for this feature.

## When the sentinel fires

A future `pg-manager` bump that lifts/rewords the major-upgrade gate
will fail the sentinel test
(`tests/integration/lcm_upgrade_test.go`). That is by design — do not
loosen the assertion in place. Open a contract-revision change that
consciously decides what pgman-proxy exposes for executable major
upgrades (see `contracts/upgrade-endpoints.md`, "Sentinel
obligations").
