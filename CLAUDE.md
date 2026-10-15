<!-- SPECKIT START -->
Active feature: `005-scale-out-1-to-3` (branch: `005-scale-out-1-to-3`).

For the in-flight feature, read:

- `specs/005-scale-out-1-to-3/spec.md`
- `specs/005-scale-out-1-to-3/plan.md`
- `specs/005-scale-out-1-to-3/research.md`
- `specs/005-scale-out-1-to-3/data-model.md`
- `specs/005-scale-out-1-to-3/contracts/` (observability, jetstream-store-layout, managed-conf-blocks, scaleout-suite, upstream-015)
- `specs/005-scale-out-1-to-3/quickstart.md`
- `docs/scale-out-1-to-3/plan.md`: the verified facts F1-F16 and the work items (WI-A, WI-B, WI-C) this feature implements

Feature 005 fixes single-node mode and makes 1→3 expansion a
config-and-restart procedure that keeps node-a's data.

**pgman-proxy side**:
- `AsyncOnly` replication for one peer (WI-A, ships first);
- boot-window campaign gate (WI-C1);
- incompatible JetStream store moved aside at boot (WI-C2);
- managed `hba_extras`/`conf_extras` blocks applied on every boot (WI-C3);
- identity-mismatch doctor check and gauge (WI-C4);
- pg-manager bump (WI-C5);
- the docker-driven `tests/scaleout` suite (WI-C6, `make scaleout`);
- runbook, spec 002 amendment and changelog (WI-C7).

**Upstream prerequisite**: pg-manager milestone `015-empty-peer-safety`
(contracts/upstream-015.md), which provides empty-node deference, the
data-presence record and empty-peer exclusion from the sync set.
Scenarios needing it call `requireUpstream015(t)`.

Production code must not name engine commands (lcm-discipline-gate);
reloads go through pg-manager's `PostgresExecutor.Reload`.

Prior features still apply:
- 001 `specs/001-active-active-pg-proxy/`: base proxy + control plane.
- 002 `specs/002-embedded-nats-cluster/`: embedded NATS cluster (US2 scenario 2 amended by 005).
- 003 `specs/003-pgmctl-cli/`: pgmctl operator CLI (client-only).
- 004 `specs/004-upgrade-tests/`: upgrade test coverage; its major-gate sentinel pins `upgrade: major strategies wired but gated on v0.8.0` (pg-manager v0.7.0 is milestone 015; see 005 research R-013).

Non-negotiable principles live in `.specify/memory/constitution.md`
(v1.3.0). The wrapped engine is `../pg-manager`; reference assembly is
`../pg-manager/examples/three_node_nats/main.go`.
<!-- SPECKIT END -->
