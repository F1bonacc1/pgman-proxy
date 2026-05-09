<!-- SPECKIT START -->
Active feature: `002-embedded-nats-cluster` (branch: `002-embedded-nats-cluster`).

For the in-flight feature, read:

- `specs/002-embedded-nats-cluster/spec.md`
- `specs/002-embedded-nats-cluster/plan.md`
- `specs/002-embedded-nats-cluster/research.md`
- `specs/002-embedded-nats-cluster/data-model.md`
- `specs/002-embedded-nats-cluster/contracts/`
- `specs/002-embedded-nats-cluster/quickstart.md`

Feature 002 amends feature 001 (`specs/001-active-active-pg-proxy/`) by
removing the external-NATS dependency: every proxy peer embeds a NATS
server in-process, and the peers form a NATS cluster. 001's FR-003 is
reversed; the constitution amendment to v1.2.0 is in
`specs/002-embedded-nats-cluster/contracts/constitution-amendment.md`
and lands as the first task at `/speckit-tasks` time.

Non-negotiable principles live in `.specify/memory/constitution.md`
(v1.1.0; amendment to v1.2.0 pending). The wrapped engine is
`../pg-manager`; reference assembly is
`../pg-manager/examples/three_node_nats/main.go`.
<!-- SPECKIT END -->
