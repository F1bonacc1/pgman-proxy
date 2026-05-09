# pgman-proxy

An opinionated active/active PostgreSQL HA proxy and lifecycle manager.
Wraps the [`pg-manager`](https://github.com/f1bonacc1/pg-manager) library
and scaffolds it with NATS as the message bus and leader-election
substrate. Deployable as a standalone process, microservice, or sidecar.

> Spec: `specs/001-active-active-pg-proxy/`
> Constitution: `.specify/memory/constitution.md` (v1.1.0)
> Reference assembly: `../pg-manager/examples/three_node_nats/main.go`

## What this binary is (and is not)

Two surfaces in one binary:

1. **Data-plane proxy** — direct TCP listener that routes PostgreSQL
   wire-protocol traffic to the current leader as identified by the
   NATS-backed leadership lease. No virtual IP, no keepalived, no
   gratuitous ARP. Clients connect directly to a peer's `host:port`.
2. **Lifecycle-management control plane** — authenticated HTTP API
   that exposes `pg-manager`'s `Manager` operations (`Status`,
   `Diagnose`, `Switchover`, `Failover`, `Promote`, `Fence`,
   `Unfence`, `UpdateTopology`, `TriggerBackup`, `PrepareUpgrade`,
   `ExecuteUpgrade`). Every LCM action goes through `pg-manager`;
   this repository only adds auth, leader-routing, audit, and
   observability glue.

## Out of Scope

The following are **explicitly not** part of this project; they belong
to a separate downstream project. PRs introducing them will be
rejected at review (`.specify/memory/constitution.md` Principle VII;
spec `FR-015`):

- Kubernetes API client code, controllers, CRDs, admission webhooks.
- Helm charts, Kustomize bases, operator bundles.
- Cluster-bootstrap orchestration of PostgreSQL itself (lives in
  `pg-manager`).
- Virtual-IP, ARP, keepalived, or any layer-3 floating-address
  mechanism (`FR-004`).
- Read/write splitting (v1 routes all client traffic to the leader).
- Multi-tenant proxies (one peer fronts one cluster).
- Restore / point-in-time recovery (deferred to a future spec).
- A bundled backup backend (operators wire their own
  `BackupExecutor` adapter; reference filesystem example under
  `examples/backup-fs/`).
- ACME / cert-rotation tooling (operators supply TLS material).

## Quickstart

See `specs/001-active-active-pg-proxy/quickstart.md` for the full
15-minute deploy walkthrough (SC-001) and 10-minute fresh-cluster
bootstrap (SC-009).

## Build & test

```bash
make build         # static Linux binary at bin/pgman-proxy
make test          # unit tests under internal/...
make lint          # go vet + staticcheck + golangci-lint
make integration   # docker-compose-driven integration tests
make smoke         # one smoke test per deployment topology
```

## Performance baseline (SC-003)

The proxy hop adds <1 ms p99 to a `SELECT 1` round-trip vs. direct PG
on the same host. The benchmark lives at
`tests/integration/perf_test.go` and runs as part of `make integration`.
It records p50 / p99 / sample count via `t.Logf`.

**How to record a baseline:**

```bash
make integration  # full suite
go test -tags=integration -run=TestPerf_ProxyHopOverhead -v ./tests/integration/...
```

Update this section with the measured number whenever you cut a tag.
The constitutional gate (`.specify/memory/constitution.md`) flags any
deviation > 10% from the recorded baseline as a regression.

| Date       | Hardware            | p50    | p99    | Notes |
|------------|---------------------|--------|--------|-------|
| 2026-05-09 | _baseline pending_  | _TBD_  | _TBD_  | Recorded after Phase 3 cluster-bootstrap fix lands. |

## Container image

`deploy/docker/Dockerfile` is a distroless static base running as
non-root (`USER nonroot:nonroot`). Goreleaser (`.goreleaser.yaml`)
publishes the OCI image on tag — `make release` does a snapshot build.
The image MUST run unprivileged; `--user 0:0` is rejected at orchestrator
level (FR-013).

## License

Apache-2.0 (see `LICENSE`).
