# pgman-proxy

An opinionated active/active PostgreSQL HA proxy and lifecycle manager.
Wraps the [`pg-manager`](https://github.com/f1bonacc1/pg-manager) library
and scaffolds it with **an embedded NATS server** (one per peer; peers
form a NATS cluster) as the message bus and leader-election substrate.
Deployable as a standalone process, microservice, or sidecar.

> Specs: `specs/001-active-active-pg-proxy/` (baseline) and
> `specs/002-embedded-nats-cluster/` (embedded coordination plane —
> reverses 001's external-NATS dependency)
> Constitution: `.specify/memory/constitution.md` (v1.2.0)
> Reference assembly: `../pg-manager/examples/three_node_nats/main.go`

## What this binary is (and is not)

Two surfaces in one binary:

1. **Data-plane proxy** — direct TCP listener that routes PostgreSQL
   wire-protocol traffic to the current leader as identified by the
   NATS-backed leadership lease (NATS embedded in-process per feature
   002 — no external broker). No virtual IP, no keepalived, no
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

## Upgrading PostgreSQL

pgman-proxy does not upgrade PostgreSQL itself — it forwards
`PrepareUpgrade` / `ExecuteUpgrade` to `pg-manager` and leaves binary
swapping and the cross-node loop to the **host**. Minor (patch-level)
rolling upgrades are supported today; major-version strategies are
gated on v0.7.0.

See **[`docs/upgrade-orchestration.md`](docs/upgrade-orchestration.md)**
for the step-by-step host orchestration guide.

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

## Continuous integration

GitHub Actions workflows in `.github/workflows/`:

- `ci.yml` — `go build / vet / test -race / smoke / golangci-lint`.
- `govulncheck.yml` — Go vulnerability DB scan; runs per PR and on a
  weekly cron.
- `scope-gate.yml` — SC-006 grep-gate; rejects Kubernetes / Helm /
  CRD / controller-runtime / admission-webhook tokens (FR-015).
- `lcm-discipline-gate.yml` — SC-013 grep-gate; rejects engine-
  mechanic references (`initdb`, `pg_basebackup`, `pg_rewind`,
  `pg_upgrade`, `pg_ctl promote`, replication-slot DDL) in non-test
  production source (Constitution IV).
- `release-image.yml` — builds and pushes the bundled
  pgman-proxy+pg18 image to `ghcr.io/f1bonacc1/pgman-proxy` on tags
  matching `v*` and on `workflow_dispatch`. Multi-arch
  (linux/amd64, linux/arm64); see "Container images" below.

**Required repo secret**: `PG_MANAGER_TOKEN` — a PAT (or fine-grained
token) with `contents:read` on `f1bonacc1/pg-manager`. `go.mod` pins
that private module to a tagged release, so every build authenticates
the Go toolchain's fetch with this token: the `ci`, `govulncheck`, and
`release` jobs configure a git credential on the runner, and
`release-image` passes it to the bundle build as a BuildKit secret
mount (it never enters an image layer, cache, or provenance). The
default `GITHUB_TOKEN` cannot read other private repos.

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

## pgmctl — operator CLI

`pgmctl` is a kubectl-style operator CLI for a running pgman-proxy
cluster (feature 003). Single statically-linked Go binary, kubectl-
shaped contexts, ANSI-colour output, JSON / YAML for automation.

```bash
make pgmctl                                # → ./bin/pgmctl
./bin/pgmctl --help                        # top-level command tree
./bin/pgmctl status                        # one-glance cluster health
./bin/pgmctl events --since 1h --list-types
./bin/pgmctl watch status                  # live SSE redraw
./bin/pgmctl dump --output dump.tar.gz     # post-mortem capture
./bin/pgmctl doctor                        # health checks + fixes
./bin/pgmctl explain leader-election       # plain-English narrative
./bin/pgmctl restart --target=postgres --cluster pgman-pc node-a
```

Per-subcommand reference: `docs/pgmctl/reference/`.
Man pages: `docs/pgmctl/man/` (`man -l docs/pgmctl/man/pgmctl.1`).
Quickstart: `specs/003-pgmctl-cli/quickstart.md`.
Generated by `make pgmctl-docs`.

`pgmctl` is a **client** — never embeds NATS, never opens a Postgres
connection, never opens cluster routes. It consumes only the
documented `/v1/*` control-plane and the embedded-NATS observability
surface.

## Container images

Two production-shape Dockerfiles, addressing two deployment topologies:

| File | Purpose | Base | Tag (ghcr.io/f1bonacc1/pgman-proxy) |
|------|---------|------|--------------------------------------|
| `deploy/docker/Dockerfile` | distroless static — pgman-proxy binary only. Use as a sidecar to an externally-managed Postgres. | `gcr.io/distroless/static:nonroot` | published by goreleaser on tag |
| `deploy/docker/Dockerfile.bundle` | bundled — pgman-proxy + PostgreSQL 18 in one container. Each peer in the active/active topology runs this. | `postgres:18-bookworm` | `:vX.Y.Z`, `:vX.Y`, `:latest` on tag; `:edge` on `workflow_dispatch` |

The bundled image is built by `.github/workflows/release-image.yml`
(triggers: tag `v*` and manual `workflow_dispatch`). Multi-arch
(linux/amd64, linux/arm64) via QEMU + buildx. Provenance + SBOM
attached.

```bash
# Pull the latest tagged release:
docker pull ghcr.io/f1bonacc1/pgman-proxy:latest

# Run one peer with PG18 colocated. pgman-proxy is PID 1; pg-manager
# (wrapped by pgman-proxy) drives initdb / pg_basebackup / pg_ctl
# against the local PGDATA. See process-compose.yaml for a working
# 3-peer reference.
docker run --rm \
    -e PGMAN_PROXY_CLUSTER_ID=prod-cluster \
    -e PGMAN_PROXY_NODE_ID=node-a \
    -e PGMAN_PROXY_PEERS=node-a,node-b,node-c \
    -e PGMAN_PROXY_CLUSTER_PASSWORD_ENV=PGMAN_CLUSTER_PASSWORD \
    -e PGMAN_CLUSTER_PASSWORD="$(pgman-proxy cluster-secret-gen)" \
    -e PGMAN_PROXY_CONTROL_AUTH_TOKEN_ENV=PGMAN_PROXY_CONTROL_TOKEN \
    -e PGMAN_PROXY_CONTROL_TOKEN=<bearer-token> \
    # …+ the rest of the config (see process-compose.yaml for the full env)
    -p 6432:6432 -p 9090:9090 -p 9091:9091 \
    -v node-a-data:/var/lib/postgresql/data \
    ghcr.io/f1bonacc1/pgman-proxy:latest
```

Both images **MUST run unprivileged**; `--user 0:0` is rejected at
the orchestrator level (FR-013). The bundled image's default user is
`postgres`.

Goreleaser (`.goreleaser.yaml`) publishes the distroless image on tag;
`make release` does a snapshot build.

## License

Apache-2.0 (see `LICENSE`).
