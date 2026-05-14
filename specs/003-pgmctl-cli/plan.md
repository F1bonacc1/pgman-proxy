# Implementation Plan: `pgmctl` — Operator CLI for pgman-proxy

**Branch**: `003-pgmctl-cli` | **Date**: 2026-05-14 | **Spec**: [spec.md](./spec.md)
**Input**: Feature specification at `specs/003-pgmctl-cli/spec.md`

## Summary

`pgmctl` is a single statically-linked Go binary — built from the same module
as `pgman-proxy` so it stays version-locked — that gives an on-call operator
a kubectl-style console over a running `pgman-proxy` cluster. It is **a
client**, not a control-plane peer: it never embeds NATS, never opens a
PostgreSQL connection, never opens cluster-route ports. It consumes the
existing 001 control-plane HTTP+JSON listener (bearer-auth, audit pipeline)
and the 002 embedded-NATS observability surface, and it asks the connected
peer to fan out to siblings over the embedded NATS mesh for per-peer slices
(FR-006a).

To make the feature deliverable, this plan lands **five additive, MINOR-
version expansions** of the 001 control-plane contract:

1. SSE watch-stream endpoints for status / transitions / events / per-node.
2. A doctor-check / doctor-fix discovery and execution endpoint.
3. A managed-PostgreSQL restart endpoint **plus** a peer self-terminate
   endpoint for `pgmctl restart --target=postgres|proxy` (FR-031a/b/c). The
   PostgreSQL-target endpoint depends on a new `Manager.RestartPostgres(ctx)`
   primitive landing in **`../pg-manager`** first (Constitution IV).
4. A JetStream-backed durable history stream for events + audit records
   (FR-016a), queryable with `--since` / `--type` / `--node` filters.
5. An inter-peer request/reply fan-out subject set on the embedded NATS mesh
   (FR-006a) so the connected peer can aggregate per-peer slices.

A logs-tail endpoint is **explicitly excluded** (Q5 clarification — `pgmctl
logs` was removed from v1; operators consume the proxy's structured-log
stream through the host's existing sink).

The technical approach was settled across 5 clarification answers
(2026-05-14) recorded in spec.md `## Clarifications`. The plan inherits
those decisions and is graded against them.

## Technical Context

**Language/Version**: Go 1.25 (matches `go.mod` `go 1.25.0`; tracks the two
most recent stable Go releases per Constitution).

**Primary Dependencies**:
- **CLI framework**: `github.com/spf13/cobra` (user-requested; standard for
  kubectl-style command trees). `github.com/spf13/pflag` for POSIX flags
  comes in as a transitive dependency. No `viper` — config resolution is
  hand-rolled to keep precedence rules and the secrets-sourcing contract
  explicit (FR-006 / FR-007 / FR-008).
- **Color**: `github.com/fatih/color` (small, mature, respects `NO_COLOR`
  and TTY detection out of the box). Watch-mode differential redraw uses
  raw ANSI cursor escapes (no full TUI framework — keeps the dep tree
  honest per Constitution VII).
- **Table rendering**: stdlib `text/tabwriter` for the table format (FR-004);
  `wide` adds extra columns through the same writer. No third-party table
  library required.
- **YAML**: existing `gopkg.in/yaml.v3` (already pinned).
- **JSON**: stdlib `encoding/json`.
- **HTTP client**: stdlib `net/http` + `crypto/tls`.
- **SSE consumer**: stdlib `bufio.Scanner` over `net/http` response body
  (small parser, no extra dep). SSE server side uses `http.Flusher`.
- **ULIDs**: existing `github.com/oklog/ulid/v2` (used by `request_id`).
- **Tar (dump artifact)**: stdlib `archive/tar` + `compress/gzip`.
- **Server-side new packages**: `github.com/nats-io/nats.go` JetStream KV +
  stream API (already vendored for 002's leadership KV).

**Storage**:
- **pgmctl**: ~ephemeral~ no local state beyond the kubeconfig-style
  configuration file at `$XDG_CONFIG_HOME/pgmctl/config.yaml` (FR-007).
- **pgman-proxy server-side additions**: a **JetStream stream** for the
  event/audit history (`pgman_proxy.<cluster_id>.history`) with file-backed
  storage, replication factor derived per 002 FR-011a (R=1/2/3 from
  declared cluster size), retention default `24h` / `256 MiB` (FR-016a).

**Testing**:
- Unit: stdlib `testing` (project convention — no `testify` is currently in
  the module; we don't introduce one).
- Integration: existing `tests/integration/` harness extended with pgmctl
  fixtures (in-process `pgman-proxy` peer + golden-file assertions on
  pgmctl output). Multi-peer cluster topology already exercised by 001/002
  integration tests; pgmctl reuses that scaffolding.
- Contract: `tests/contract/` for the five new HTTP / NATS-subject
  contracts.
- Golden files: `tests/golden/pgmctl/*.txt` + `*.json` + `*.ansi` for color,
  table, JSON, YAML, and wide output across the v1 subcommand surface
  (SC-007, SC-008).

**Target Platform**:
- **Tier 1 (CI-gated)**: `linux/amd64`, `linux/arm64` (FR-001).
- **Tier 2 (cross-compiled, smoke-tested)**: `darwin/amd64`, `darwin/arm64`.
- **Not supported in v1**: Windows.

**Project Type**: CLI binary (`cmd/pgmctl/`) **plus** additive expansions to
the existing pgman-proxy service (`internal/control/`, `internal/history/`,
`internal/fanout/`). Two binaries in one module, two test surfaces. No
third project.

**Performance Goals** (from SC-001 / SC-002 / SC-003 / SC-009):
- `pgmctl status` p95 ≤ 1.5s (warm TLS, 3-peer cluster, dev laptop).
- `pgmctl dump` p95 ≤ 15s on a healthy cluster; ≤ `--timeout` (default
  60s) on a partially-reachable cluster.
- `pgmctl watch status` p95 redraw latency ≤ 1s; idle CPU < 1% of one core.
- Doctor TPR ≥ 90%, FPR ≤ 5% across the documented v1 failure fixtures.
- New server-side endpoints MUST NOT degrade 001's data-plane budgets
  (1ms p99 per-query overhead) or 001/002's leader-failover budget
  (5s p99) — verified by the existing CI baselines.

**Constraints**:
- Single connection per invocation (FR-006); server-side fan-out (FR-006a).
- No client retry on mutating ops; no server dedup (FR-039 tightened by Q2).
- TLS verify-full default (`crypto/tls` `InsecureSkipVerify=false` unless
  `--insecure-skip-tls-verify`); ties into Constitution §Security.
- No plaintext secrets in any output mode (FR-009).
- Five additive MINOR-version server-side contract changes; no breaking
  changes to 001's `/v1/*` surface; existing handlers untouched.

**Scale/Scope**:
- 3-peer cluster typical; tested up to 5-peer.
- ~24h history-stream retention; ~256 MiB on-disk default.
- 21 subcommands (`logs` excluded).
- 18 doctor checks (server-driven catalogue; FR-022).
- ~3,500 LOC pgmctl client code, ~1,200 LOC server-side additions
  (estimate; will be ratified at `/speckit-tasks` time).

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-evaluated after Phase 1
design.*

| # | Principle | Status | Evidence |
|---|---|---|---|
| I | Wire-Protocol Fidelity | **N/A** | pgmctl touches no PostgreSQL wire path; new server endpoints are HTTP/NATS only. |
| II | Fail-Closed Safety | **PASS** | FR-009 (no secret leakage), FR-010 (cluster-id validation), FR-040 (audit-unavailable refusal pass-through), FR-031c (supervisor-absent fail-closed), TLS verify default. |
| III | Active/Active Coordination Correctness | **PASS** | The new fan-out path (FR-006a) and history stream (FR-016a) live on top of NATS primitives 002 already established (JetStream KV/streams; cluster gossip). Per-sibling fan-out failure degrades to per-slice error, never silently substitutes data. No new consensus invented. |
| IV | Thin Scaffold over pg-manager | **CONDITIONAL PASS** | New `Manager.RestartPostgres(ctx)` MUST land upstream in `../pg-manager` first (recorded as Complexity Tracking item below; not a violation, just a sequencing constraint). All other behaviours (Status/Diagnose/Promote/Failover/Switchover/Fence/Unfence) already exist on the Manager. |
| V | Observability by Default | **PASS** | All five new endpoints emit structured logs + Prometheus metrics following 001's schema (RFCs in `contracts/control-plane-extensions.md`). pgmctl client itself writes structured logs to stderr at `-vv+`. |
| VI | Integration-First Testing (NON-NEGOTIABLE) | **PASS** | Every new endpoint ships with a contract test against a real proxy peer; every mutating subcommand ships with the six-assertion integration test set per SC-006; multi-replica topology exercised on every change touching coordination (the fan-out path counts). |
| VII | Scope Discipline & Reversibility | **PASS** | All five server-side additions are MINOR-version additive; no breaking changes; new config keys (`history.retention_*`, `pgmctl_features.*`) default to preserve prior behaviour; no Kubernetes / Helm / CRD code. |

**Out-of-scope check**: pgmctl does not consume the Kubernetes API; does not
ship a Helm chart, a CRD, or an operator bundle; does not orchestrate
PostgreSQL bootstrap. ✅

**Topology / dependencies check**: All five server-side additions live on
top of the embedded NATS cluster from 002 — no new external runtime
dependency. ✅

**Security check**:
- TLS verify-full by default. ✅
- Bearer-token reuse from 001 (no new auth scheme). ✅
- Non-root binary (FR-001 — static binary, no privileged operations
  required). ✅
- New endpoints inherit 001's bearer-auth middleware (`internal/control/
  auth.go`) — including `audit_unavailable` fail-closed semantics. ✅

**Performance baseline**: The new endpoints are out-of-band of the
data plane. Fan-out latency budget on the new NATS subjects: 100ms p99 for
intra-cluster RTT (well inside the spec's per-slice timeout default of
10s). Leader-failover budget untouched (no new code in the failover path).

**No unjustified complexity** detected. The Complexity Tracking table below
records the one cross-repo dependency (the upstream `RestartPostgres`
method) and the one client-side abstraction (kubeconfig-style multi-context),
neither of which is a violation but both of which deserve to be surfaced.

## Project Structure

### Documentation (this feature)

```text
specs/003-pgmctl-cli/
├── plan.md                              # this file
├── spec.md                              # feature spec (locked after /speckit-clarify)
├── research.md                          # Phase 0 output (this command)
├── data-model.md                        # Phase 1 output (this command)
├── quickstart.md                        # Phase 1 output (this command)
├── contracts/                           # Phase 1 output (this command)
│   ├── cli-commands.md                  #   pgmctl command surface, flags, exit codes
│   ├── control-plane-extensions.md      #   five new HTTP endpoints
│   ├── history-stream.md                #   JetStream history stream contract
│   ├── fanout-protocol.md               #   inter-peer NATS request/reply contract
│   └── proxy-self-terminate.md          #   FR-031b/c: supervisor detection + endpoint
├── checklists/
│   └── requirements.md                  # already populated by /speckit-clarify
└── tasks.md                             # produced by /speckit-tasks (NOT here)
```

### Source Code (repository root)

```text
cmd/
├── pgman-proxy/                         # existing: proxy daemon entry
└── pgmctl/                              # NEW: CLI entry (cmd/pgmctl/main.go)

internal/
├── cluster/                             # existing (002)
├── config/                              # existing (001 + 002)
├── control/                             # existing (001) — EXTENDED
│   ├── handlers_read.go                 #   existing (Status, Diagnose)
│   ├── handlers_membership.go           #   existing (Fence, Unfence, Failover, Switchover, Promote)
│   ├── handlers_topology.go             #   existing (UpdateTopology — wired to `pgmctl delete`)
│   ├── handlers_backup.go               #   existing
│   ├── handlers_upgrade.go              #   existing
│   ├── handlers_watch.go                #   NEW: SSE streams (status / transitions / events / per-node)
│   ├── handlers_doctor.go               #   NEW: GET /v1/doctor/checks, POST /v1/doctor/run, POST /v1/doctor/fix
│   ├── handlers_restart.go              #   NEW: POST /v1/restart (target=postgres|proxy)
│   └── handlers_history.go              #   NEW: GET /v1/history (events + audit query)
├── embedded/                            # existing (002)
├── fanout/                              # NEW: inter-peer NATS request/reply adapter
│   ├── client.go                        #   "ask my siblings for X" helper
│   ├── server.go                        #   sibling-side responder; subject = pgman_proxy.<cluster_id>.fanout.<slice>
│   └── types.go                         #   slice envelope + per-sibling error encoding
├── history/                             # NEW: JetStream-backed event/audit history
│   ├── stream.go                        #   JetStream stream config; replicas from 002 FR-011a
│   ├── publisher.go                     #   wired into existing audit + transition emission
│   ├── query.go                         #   --since / --type / --node filter logic
│   └── retention.go                     #   24h / 256 MiB combined retention
├── obs/                                 # existing
├── pgmctl/                              # NEW: pgmctl client logic
│   ├── client/
│   │   ├── http.go                      #   HTTP client + bearer auth + TLS + retry policy for read-only ops only
│   │   ├── sse.go                       #   SSE consumer (reconnect w/ backoff per FR-020)
│   │   └── auth.go                      #   bearer-token source (env / file / command)
│   ├── config/
│   │   ├── kubeconfig.go                #   load/save $XDG_CONFIG_HOME/pgmctl/config.yaml
│   │   ├── context.go                   #   Context entity (FR-007)
│   │   └── resolve.go                   #   precedence: --endpoint > --context > current > PGMCTL_ENDPOINT
│   ├── output/
│   │   ├── table.go                     #   tabwriter renderer (default + wide)
│   │   ├── json.go                      #   schema-versioned: apiVersion: pgmctl/v1 (FR-038)
│   │   ├── yaml.go                      #   schema-versioned
│   │   ├── color.go                     #   green/yellow/red + NO_COLOR + --no-color (FR-036)
│   │   └── severity.go                  #   Severity → color/marker mapping
│   ├── cmd/
│   │   ├── root.go                      #   cobra root, global flags (FR-004)
│   │   ├── status.go                    #   pgmctl status
│   │   ├── get.go, list.go, describe.go #   resource verbs
│   │   ├── topology.go, health.go, lag.go
│   │   ├── events.go                    #   history-stream query (FR-016)
│   │   ├── explain.go                   #   v1 subjects (FR-018)
│   │   ├── doctor.go                    #   --list, --check, --fix, -y (FR-022..FR-027)
│   │   ├── dump.go                      #   full state dump (FR-032..FR-035)
│   │   ├── watch.go                     #   watch status|transitions|events|node
│   │   ├── fence.go, unfence.go         #   single-resource mutations
│   │   ├── set_config.go                #   hot-reload-allow-list-gated
│   │   ├── failover.go, switchover.go, promote.go
│   │   ├── restart.go                   #   --target=postgres|proxy
│   │   ├── delete.go                    #   UpdateTopology decommission
│   │   ├── version.go                   #   client + server version w/ skew warning (Edge Cases)
│   │   ├── config_cmd.go                #   pgmctl config view|use-context|set-context|delete-context
│   │   └── completion.go                #   bash|zsh|fish (FR-003)
│   ├── confirm/
│   │   ├── prompt.go                    #   [y/N] (FR-028) + typed-cluster-name (FR-029)
│   │   └── tty.go                       #   non-TTY refusal (Edge Cases)
│   ├── doctor/
│   │   ├── render.go                    #   server-driven check catalogue → terminal output
│   │   └── fix.go                       #   iterate fixes, blast-radius routing, re-run check
│   ├── dump/
│   │   ├── collector.go                 #   parallel slice fetcher w/ per-slice timeout (FR-032)
│   │   ├── redact.go                    #   normal + strict redaction (FR-033)
│   │   ├── tar.go                       #   single tar.gz stream (FR-034)
│   │   └── manifest.go                  #   slices/durations/versions (FR-035)
│   └── watch/
│       ├── status.go                    #   diff-and-redraw (FR-019)
│       ├── append.go                    #   transitions/events/node append-only (FR-020)
│       └── reconnect.go                 #   exponential backoff + gap markers
└── runtime/                             # existing

pkg/
└── proxyapi/                            # NEW (optional): re-exported types for downstream tools
    └── doc.go

../pg-manager/                           # UPSTREAM REPO — separate PR required
└── manager/manager.go                   # ADD: func (m *Manager) RestartPostgres(ctx context.Context) error

tests/
├── contract/
│   ├── cli_commands_test.go             # NEW: golden output for every subcommand (table/json/yaml)
│   ├── watch_sse_test.go                # NEW
│   ├── doctor_test.go                   # NEW
│   ├── history_query_test.go            # NEW
│   ├── fanout_test.go                   # NEW
│   └── restart_test.go                  # NEW (postgres + proxy targets, supervisor-absent refusal)
├── integration/
│   ├── pgmctl_status_integration_test.go            # NEW
│   ├── pgmctl_doctor_fix_integration_test.go        # NEW
│   ├── pgmctl_dump_integration_test.go              # NEW
│   ├── pgmctl_watch_integration_test.go             # NEW
│   ├── pgmctl_mutating_integration_test.go          # NEW (6-assertion grid per op, SC-006)
│   └── pgmctl_restart_proxy_integration_test.go     # NEW (supervisor harness)
└── golden/
    └── pgmctl/
        ├── status_healthy.{txt,json,yaml,ansi}
        ├── status_warn.{txt,json,yaml,ansi}
        ├── status_fail.{txt,json,yaml,ansi}
        ├── doctor_pass.{txt,json}
        ├── doctor_one_fail.{txt,json}
        └── ...

docs/
├── pgmctl/
│   ├── README.md                        # NEW: install, configure, common workflows
│   ├── man/pgmctl.1                     # NEW: top-level man page
│   └── reference/                       # NEW: per-subcommand reference
└── ...
```

**Structure Decision**:
- The repository remains a single Go module rooted at `github.com/f1bonacc1/pgman-proxy`. pgmctl is a second binary under `cmd/pgmctl/`. Server-side additions live in `internal/control/`, `internal/history/`, and `internal/fanout/`. pgmctl client logic lives in `internal/pgmctl/`.
- `pkg/proxyapi/` is a discretionary v1 add-on for downstream tooling that wants to consume the type definitions without depending on `internal/`. It is OPT-IN; the v1 deliverable is OK without it. Recorded so it surfaces at `/speckit-tasks` time as a deferrable task.
- The upstream `../pg-manager` repo gets exactly one change: adding `Manager.RestartPostgres(ctx)` (Constitution IV). The PR is separate, lands first, and is consumed via a pinned go.mod version.

## Complexity Tracking

| Item | Why Needed | Simpler Alternative Rejected Because |
|---|---|---|
| New `Manager.RestartPostgres(ctx)` in `../pg-manager` | FR-031a requires control-plane-mediated PostgreSQL restart; the Manager today has `Start` + `Stop` but no `Restart`. Constitution IV ("If a needed capability is missing in pg-manager, the PR MUST link to the upstream change") forbids a proxy-local Stop+Start wrapper. | A proxy-local wrapper would duplicate Manager's lifecycle invariants (state-store updates, lease re-acquisition, observability event ordering) and would drift over time. Adding one method upstream is the cheapest correct path. |
| Kubeconfig-style multi-context configuration | FR-007 — operators routinely manage multiple clusters (dev, staging, prod-east, prod-west). Recorded as a v1 requirement after the clarification session. | Single-endpoint + env var only: real operators with N clusters would have to maintain N shell wrappers. Worse ergonomics than a 60-line YAML loader. |
| Five additive control-plane endpoints + one cross-repo change | All five are needed to satisfy the spec (clarified down from "maybe more" to exactly five at /speckit-clarify time). | Punting any one would degrade a P1 user story: dropping SSE = no live watch (US4 P2 → broken); dropping doctor exec = no `--fix` (US3 P1); dropping history = no `events`/`audit`/dump backbone (US2 P1, US5 P2); dropping restart = no `pgmctl restart`; dropping fan-out = no per-peer dump slices. The scope is the floor, not the ceiling. |

No Constitution principle is violated. No item above is a violation; this
table exists per the plan-template convention to surface the cross-repo
sequencing constraint and the one client-side abstraction.

---

## Phase 0 — Outline & Research

*Produced this run. See `research.md` for full content.*

Topics resolved:
1. pg-manager Manager API delta (Restart primitive — upstream PR sequence).
2. JetStream history-stream config (subject, retention, replicas, ack
   policy, query interface).
3. NATS request/reply fan-out subject scheme + payload envelope.
4. SSE wire format on the control-plane listener (`text/event-stream`,
   keepalive cadence, error framing).
5. Bearer-token sourcing layers and TLS trust resolution in pgmctl.
6. Color rendering: `fatih/color` vs `lipgloss` vs raw ANSI; NO_COLOR
   contract.
7. CLI config layout (multi-context yaml; precedence resolution).
8. Doctor check registry pattern (server-side enumeration + execution).
9. Proxy self-terminate supervisor detection (PID-1 / parent-PID heuristic
   across systemd, s6, tini, docker, kubernetes).
10. Watch-mode redraw strategy (differential cells vs full repaint;
    cursor-positioning escape protocol).
11. Dump artifact format (single tar.gz, schema-versioned manifest).
12. Cross-platform binary build matrix + reproducible builds.

## Phase 1 — Design & Contracts

*Produced this run. See `data-model.md`, `contracts/`, `quickstart.md`.*

1. **Entities** — kubeconfig Context, DoctorCheck, SuggestedFix,
   HistoryEvent, FanOutSlice, DumpArtifact, Severity, OutputFormat,
   ConfirmationClass, ExitCode (full schema in `data-model.md`).
2. **Contracts** — one file per surface boundary:
   - `contracts/cli-commands.md` — every pgmctl subcommand: synopsis,
     flags, output schema, exit codes.
   - `contracts/control-plane-extensions.md` — the five new HTTP
     endpoints (paths, request/response envelopes, error codes, audit
     records, observability metrics).
   - `contracts/history-stream.md` — JetStream stream name / subjects /
     replicas / retention / ack / query semantics.
   - `contracts/fanout-protocol.md` — inter-peer request/reply subject
     naming, payload, timeout, per-sibling error encoding.
   - `contracts/proxy-self-terminate.md` — FR-031b/c: supervisor-presence
     heuristic, fail-closed rule, endpoint shape.
3. **Quickstart** — install `pgmctl`, set up the kubeconfig file, run
   `pgmctl status`, `pgmctl doctor`, `pgmctl dump`, `pgmctl watch
   status`; the doc is the executable form of US1/US2/US3 acceptance.
4. **Agent context** — CLAUDE.md updated to point at this plan between
   the `<!-- SPECKIT START -->` / `<!-- SPECKIT END -->` markers.

## Phase 2 — Tasks (NOT produced here)

`/speckit-tasks` will derive tasks from this plan + spec. Expected shape:

- Wave 0 (cross-repo prereq): land `Manager.RestartPostgres` in
  `../pg-manager`; bump pgman-proxy's go.mod pin.
- Wave 1 (server-side, independently testable per story):
  - 1a: history stream + query endpoint (US2 backbone).
  - 1b: doctor registry + execution endpoint (US3 backbone).
  - 1c: SSE watch endpoints (US4 backbone).
  - 1d: fan-out subject + responder (US2 multi-peer dump backbone).
  - 1e: restart endpoint (postgres + proxy self-terminate) (US6).
- Wave 2 (pgmctl client):
  - 2a: cobra skeleton + global flags + kubeconfig + auth + TLS (US1
    foundation).
  - 2b: status + get + list + describe + topology + health + lag (US1).
  - 2c: events + explain (US5).
  - 2d: doctor render + --check + --fix (US3).
  - 2e: dump (US2).
  - 2f: watch (US4).
  - 2g: confirmations + fence/unfence/set-config + failover/switchover/
       promote/restart/delete (US6).
- Wave 3 (cross-cutting): golden-file tests, integration tests, docs,
  release matrix, version-skew warnings, completion scripts.

Each task ends in a single atomic commit per Constitution.

---

## Stop point

This is where `/speckit-plan` ends. Next: `/speckit-tasks`.
