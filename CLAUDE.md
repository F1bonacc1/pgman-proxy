<!-- SPECKIT START -->
Active feature: `003-pgmctl-cli` (branch: `003-pgmctl-cli`).

For the in-flight feature, read:

- `specs/003-pgmctl-cli/spec.md`
- `specs/003-pgmctl-cli/plan.md`
- `specs/003-pgmctl-cli/research.md`
- `specs/003-pgmctl-cli/data-model.md`
- `specs/003-pgmctl-cli/contracts/`
- `specs/003-pgmctl-cli/quickstart.md`

Feature 003 adds `pgmctl` — a kubectl-style CLI for operators of a
running pgman-proxy cluster. It is a **client** (no embedded NATS, no
PostgreSQL connection, no cluster routes); it consumes the 001 control
plane and the 002 embedded-NATS observability surface. The plan
introduces five additive, MINOR-version expansions of the 001 contract:
SSE watch streams, doctor discovery/execution, managed-PostgreSQL
restart + peer self-terminate, JetStream-backed event/audit history,
and an inter-peer fan-out subject set. A logs-tail endpoint is
**explicitly excluded** (clarified out of scope on 2026-05-14). One
upstream change in `../pg-manager` is required first:
`func (m *Manager) RestartPostgres(ctx context.Context) error`.

Prior features still apply:
- 001 `specs/001-active-active-pg-proxy/` — base proxy + control plane.
- 002 `specs/002-embedded-nats-cluster/` — embedded NATS cluster.

Non-negotiable principles live in `.specify/memory/constitution.md`
(v1.2.0). The wrapped engine is `../pg-manager`; reference assembly is
`../pg-manager/examples/three_node_nats/main.go`.
<!-- SPECKIT END -->
