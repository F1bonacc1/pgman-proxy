<!-- SPECKIT START -->
Active feature: `004-upgrade-tests` (branch: `004-upgrade-tests`).

For the in-flight feature, read:

- `specs/004-upgrade-tests/spec.md`
- `specs/004-upgrade-tests/plan.md`
- `specs/004-upgrade-tests/research.md`
- `specs/004-upgrade-tests/data-model.md`
- `specs/004-upgrade-tests/contracts/upgrade-endpoints.md`
- `specs/004-upgrade-tests/quickstart.md`

Feature 004 is **test-only**: behavioral coverage for the existing
upgrade surface (`POST /v1/upgrade/prepare|execute`) across
{minor, major} × {happy, bad} paths. Two tiers: contract tests with
`fakeEngine` in `internal/control/handlers_upgrade_test.go`
(fault-injection only, per Constitution VI carve-out) and
integration tests in `tests/integration/lcm_upgrade_test.go`
(real engine, real PostgreSQL 17, real embedded NATS; compose
harness). The minor happy path uses a same-version plan (v1 wires a
no-op pre-swap). Major execution is gated upstream — tests pin the
exact error `"upgrade: major strategies wired but gated on v0.7.0"`
(pg-manager v0.4.1 `manager/backup_upgrade.go:102`) as a sentinel
that must fail loudly when a pg-manager bump lifts the gate. Zero
production code changes.

Prior features still apply:
- 001 `specs/001-active-active-pg-proxy/` — base proxy + control plane.
- 002 `specs/002-embedded-nats-cluster/` — embedded NATS cluster.
- 003 `specs/003-pgmctl-cli/` — pgmctl operator CLI (client-only).

Non-negotiable principles live in `.specify/memory/constitution.md`
(v1.3.0). The wrapped engine is `../pg-manager`; reference assembly is
`../pg-manager/examples/three_node_nats/main.go`.
<!-- SPECKIT END -->
