# pgmctl — operator CLI for pgman-proxy

`pgmctl` is a single statically-linked Go binary that gives a kubectl-style
console over a running pgman-proxy cluster. It is a **client**: it never
embeds NATS, never opens a PostgreSQL connection, never opens cluster
routes.

Feature spec: `specs/003-pgmctl-cli/`.

---

## Install

From repo root, development build:

```bash
make pgmctl                     # → ./bin/pgmctl
./bin/pgmctl version
```

Release binaries for linux/amd64, linux/arm64, darwin/amd64, darwin/arm64
are produced by `make pgmctl-release`.

---

## Configure a context

Configuration lives at `$XDG_CONFIG_HOME/pgmctl/config.yaml`
(falls back to `~/.config/pgmctl/config.yaml`). The file MUST have mode
`0600`; pgmctl refuses to load anything looser.

The simplest way to bootstrap a context:

```bash
# The process-compose dev fixture publishes the control plane per peer
# at http://127.0.0.1:19191 (node-a), 19192 (node-b), 19193 (node-c).
pgmctl config set-context dev \
  --endpoint http://127.0.0.1:19191 \
  --expected-cluster pgman-pc \
  --token-env PGMCTL_DEV_TOKEN

export PGMCTL_DEV_TOKEN="process-compose-dev-token"
pgmctl config view              # secrets are redacted by default
```

Three credential sources are supported — pick exactly one per context:

| Source | Flag | Notes |
|---|---|---|
| Env var name | `--token-env NAME` | Read every request — supports 001 FR-031 rotation without restart. |
| File path | `--token-file /path/...` | Read every request. Supports `~` prefix. |
| Command | `--token-command vault kv get -field=token kv/dev/token` | stdout (trimmed) is the token. |

Plaintext tokens are **never** accepted on the flag list — a deliberate
non-feature to keep secrets out of shell history.

Switching the active context:

```bash
pgmctl config use-context prod-east
pgmctl --context dev status     # per-invocation override
```

---

## Read-only surface (US1, P1 MVP)

| Command | What it does |
|---|---|
| `pgmctl status` | One-screen cluster health: leader, primary, peers, mesh, lag. Green / yellow / red severity. |
| `pgmctl topology` | Cluster topology as an ASCII tree. |
| `pgmctl health` | One-line-per-component rollup for a higher-level monitor. |
| `pgmctl lag --warn 64MB --fail 1GB` | Per-standby replication lag. |
| `pgmctl get nodes` / `peers` / `slots` / `topology` / `version` | Resource-kind verbs (kubectl-style). |
| `pgmctl list <resource>` | Alias of `get` for collection-shaped resources. |
| `pgmctl describe <resource>[/<name>]` | Verbose form of `get`. |
| `pgmctl version` | Client + server versions, with version-skew classification. |
| `pgmctl config view\|use-context\|set-context\|delete-context` | Manage the local kubeconfig-style configuration. |

All commands accept these global flags (see `pgmctl --help` for the full
list): `-o table|json|yaml|wide`, `--no-color`, `-q`, `-v[v[v]]`,
`--timeout`, `--endpoint`, `--context`, `--cluster`, `--insecure-skip-
tls-verify`, `--insecure-skip-version-check`, `--strict`.

`--strict` upgrades WARN-level outcomes to a non-zero exit; default is
to exit 0 on WARN and only flag FAIL.

---

## Exit codes

| Code | Symbol | Meaning |
|---|---|---|
| 0 | `EX_OK` | Clean success. |
| 1 | `EX_WARN_STRICT` | Non-failure warnings present AND `--strict` was supplied. |
| 2 | `EX_UNHEALTHY` | Cluster unhealthy / a doctor check returned `FAIL`. |
| 3 | `EX_PARTIAL` | Partial-reach (some peers unreachable; dump still produced). |
| 64 | `EX_USAGE` | Bad subcommand or flag combination. |
| 65 | `EX_NETWORK` | Connectivity / TLS / auth failure. |
| 67 | `EX_VERSION_SKEW` | Major-version skew without `--insecure-skip-version-check`. |
| 78 | `EX_CONFIG` | Missing endpoint, malformed config file, bad file permissions. |
| 124 | `EX_TIMEOUT` | Overall `--timeout` exceeded. |

---

## What's deferred to later phases

| Feature | Phase | Why |
|---|---|---|
| `pgmctl events`, `pgmctl get audit` | US2 | Needs the JetStream-backed history stream and `GET /v1/history`. |
| `pgmctl dump` | US2 | Builds on the history stream + per-peer fan-out. |
| `pgmctl doctor [--fix]` | US3 | Needs the server-side doctor catalogue + `POST /v1/doctor/{checks,run,fix}`. |
| `pgmctl watch …` | US4 | Needs the server-side SSE endpoints. |
| `pgmctl explain …` | US5 | Composes from US2 / US3 surfaces. |
| `pgmctl fence` / `unfence` / `set-config` / `failover` / `switchover` / `promote` / `restart` / `delete` | US6 | Mutating operations + cross-repo `Manager.RestartPostgres` change in `../pg-manager`. |

Each shipped subcommand is exercised by contract tests under
`tests/contract/pgmctl/`. The `process-compose` dev fixture now
publishes the control plane on `127.0.0.1:1919{1,2,3}` (one port per
peer), so live-fixture integration tests can talk to it directly.

---

## Pointers

- Spec: `specs/003-pgmctl-cli/spec.md`
- Plan: `specs/003-pgmctl-cli/plan.md`
- Tasks: `specs/003-pgmctl-cli/tasks.md`
- Quickstart (executable form of the P1 user stories):
  `specs/003-pgmctl-cli/quickstart.md`
- Command surface contract: `specs/003-pgmctl-cli/contracts/cli-commands.md`
