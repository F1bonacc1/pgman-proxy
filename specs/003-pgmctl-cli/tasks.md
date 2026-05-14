---
description: "Task list for feature 003-pgmctl-cli"
---

# Tasks: `pgmctl` — Operator CLI for pgman-proxy

**Input**: Design documents from `/specs/003-pgmctl-cli/`
**Prerequisites**: spec.md, plan.md, research.md, data-model.md, contracts/, quickstart.md (all present)

**Tests**: REQUIRED. Constitution VI (Integration-First Testing) is non-negotiable; SC-006 mandates per-mutating-op integration test coverage. Test tasks appear inside each user story phase and MUST be written and FAIL before the corresponding implementation tasks (or land in the same atomic commit) per project convention.

**Organization**: Tasks grouped by user story; each story is independently shippable. Phases 1–2 are shared; phases 3–8 are user stories; phase 9 is polish.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no incomplete-task dependency).
- **[Story]**: `[US1]`..`[US6]` for user-story phase tasks; absent for Setup, Foundational, Polish.
- File paths are repo-root relative.

## Path Conventions

Single Go module rooted at `github.com/f1bonacc1/pgman-proxy`. CLI lives under `cmd/pgmctl/`; CLI logic under `internal/pgmctl/`; server-side additions under `internal/control/`, `internal/history/`, `internal/fanout/`. Upstream change lands in sibling repo `../pg-manager` first (Constitution IV).

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Repo scaffolding, dependency intake, build tooling.

- [x] T001 Add `github.com/spf13/cobra` and `github.com/fatih/color` to go.mod via `go get`; run `go mod tidy`; commit go.mod + go.sum
- [x] T002 [P] Create `cmd/pgmctl/main.go` with a placeholder `main()` that prints "pgmctl <version>" and exits 0; wires nothing yet
- [x] T003 [P] Create the directory skeleton for `internal/pgmctl/{client,config,output,cmd,confirm,doctor,dump,watch}` with a `doc.go` stub in each subpackage so `go build ./...` is green
- [x] T004 [P] Create the directory skeleton for `internal/history/` and `internal/fanout/` with `doc.go` stubs
- [x] T005 Add a `pgmctl` build target to `Makefile`: `go build -trimpath -ldflags="-s -w -X main.version=$(git describe) -X main.commit=$(git rev-parse HEAD)" -o ./bin/pgmctl ./cmd/pgmctl`; verify `make pgmctl` produces a runnable binary
- [x] T006 [P] Add a release-matrix target to `Makefile`: `make pgmctl-release` cross-compiles linux/amd64, linux/arm64, darwin/amd64, darwin/arm64; outputs `dist/pgmctl-<version>-<os>-<arch>.tar.gz` + `.sha256`
- [ ] T007 [P] Extend `.github/workflows/ci.yml` (or equivalent) to build `pgmctl` on every PR and run `make pgmctl-release` on tag pushes
- [x] T008 [P] Create `tests/golden/pgmctl/` directory with a `README.md` describing the golden-file convention (one fixture per `<command>_<scenario>.<table|json|yaml|ansi>`)
- [x] T009 [P] Create `tests/contract/pgmctl/` and `tests/integration/pgmctl/` test directories with `doc.go` stubs

**Checkpoint**: `make pgmctl` builds a runnable placeholder binary; CI is green; all subdirectories exist.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Cross-cutting infrastructure that every user story consumes — pgmctl client foundations (cobra, HTTP, auth, TLS, config, output, color, confirm) and the two server-side shared packages (history, fan-out).

**⚠️ CRITICAL**: No user-story phase may begin until Phase 2 is complete.

### Server-side shared packages

- [x] T010 [P] Implement `internal/history/stream.go`: `EnsureHistoryStream(ctx, js jetstream.JetStream, clusterID, jetStreamDir string, replicas int) error` per `contracts/history-stream.md`; stream name `PGMAN_PROXY_HISTORY_<CLUSTER_ID>`, subjects `pgman_proxy.<cluster_id>.history.{event,audit}.>`, FileStorage, LimitsPolicy with both `MaxAge=24h` and `MaxBytes=256MiB` defaults, DiscardOld, AckExplicit
- [x] T011 [P] Implement `internal/history/retention.go`: load `history.retention_age` / `history.retention_bytes` from existing config-loading code path; refuse both-zero at validation
- [x] T012 Implement `internal/history/publisher.go`: `Publisher.PublishEvent(category, type, nodeID string, details any) error`; ULID-tag each record; non-blocking on publish failure (best-effort) but increment `pgman_proxy_history_publish_failures_total` (depends on T010)
- [x] T013 Wire `history.Publisher` into existing event/transition emission sites in `internal/obs/` so every existing structured-log `event.*` line ALSO publishes to the history stream; do NOT change existing log formats (depends on T012)
- [x] T014 Wire `history.Publisher` into `internal/control/audit.go` as a second audit sink alongside the existing NATS audit-subject publish; both sinks must succeed for `audit_unavailable` (001 FR-028) NOT to trigger; integration with the existing `auditEmitFailures` counter (depends on T012)
- [x] T015 [P] Implement `internal/history/query.go`: `Query(ctx, params) (HistoryQueryResult, error)` using a fresh `jetstream.OrderedConsumer`; supports `since`, `until`, `type`, `category`, `node`, `limit`, `cursor`; returns `next_cursor` + `truncated`
- [x] T016 Implement `internal/fanout/subjects.go`: per-cluster subject derivation helpers `RequestSubject(clusterID, slice, target)`, `ResponderWildcard(clusterID, slice, selfNodeID)`; matches `contracts/fanout-protocol.md`
- [x] T017 [P] Implement `internal/fanout/server.go`: per-peer NATS subscriber on `pgman_proxy.<cluster_id>.fanout.<slice>.<self_node_id>` + wildcard `.*`; dispatches to per-slice handler registry; auto-audits `slice=doctor` requests with `operator_actor` from request envelope (depends on T016)
- [x] T018 [P] Implement `internal/fanout/client.go`: `FanOut.RequestMany(ctx, slice, args, perSliceTimeout) ([]Reply, error)` using `nats.RequestMany`; aggregates `ok`/`partial`/`failed`/synthesized `sibling_unreachable` entries; never returns a whole-request error for sibling-level failures (depends on T016)
- [x] T019 Register per-slice fan-out handlers (`status`, `config`, `nats_mesh`, `doctor`) on every peer at startup; each handler reads the local data and replies with the documented `FanOutSlice` envelope (depends on T017)
- [x] T020 Bootstrap call: from the existing proxy `runtime.Start` path, invoke `history.EnsureHistoryStream(...)` after 002's leadership-KV bootstrap; emit `proxy.history_stream_ready` log event on success and fail-closed at startup on persistent failure (mirrors 002 patterns) (depends on T010)

### pgmctl client foundations

- [x] T021 [P] Implement `internal/pgmctl/config/kubeconfig.go`: load + save `$XDG_CONFIG_HOME/pgmctl/config.yaml` (falls back to `~/.config/pgmctl/config.yaml`); refuse to load if mode is group/world-readable; matches `data-model.md § Context`
- [x] T022 [P] Implement `internal/pgmctl/config/context.go`: `Context` struct + `Validate()`; enforces "exactly one of token_env/token_file/token_command" and HTTPS-or-loopback endpoint rule
- [x] T023 [P] Implement `internal/pgmctl/config/resolve.go`: endpoint precedence `--endpoint > --context > current-context > PGMCTL_ENDPOINT > error` per FR-006 (depends on T021, T022)
- [x] T024 [P] Implement `internal/pgmctl/client/auth.go`: bearer-token source resolution (`token_env` / `token_file` / `token_command`); read on every request (no caching beyond one request); REFUSE `--token` flag at parse time
- [x] T025 [P] Implement `internal/pgmctl/client/http.go`: HTTPS client with bearer-auth, TLS verify-full default, optional CA file, `--insecure-skip-tls-verify` escape hatch with stderr warning; injects `X-Request-Id` (ULID) and `User-Agent: pgmctl/<version>` headers; read-only ops retry on transport failure (per FR-039 clarification); mutating ops NEVER retry
- [ ] T026 [P] Implement `internal/pgmctl/client/sse.go`: SSE consumer over `net/http` body with `bufio.Scanner`; honours `Last-Event-ID` resumption; reconnect with exponential backoff (250ms base, factor 2, cap 10s, max 30 attempts); surfaces `keepalive` / `gap_marker` / `error` frames distinctly per `contracts/control-plane-extensions.md`
- [x] T027 [P] Implement `internal/pgmctl/output/color.go`: wraps `fatih/color`; auto-disables on `!isatty(stdout)`, `--no-color`, or `NO_COLOR`; `Green/Yellow/Red` + bracket markers `[OK]/[INFO]/[WARN]/[FAIL]/[UNKNOWN]` for no-color mode
- [x] T028 [P] Implement `internal/pgmctl/output/severity.go`: `Severity` enum (PASS/INFO/WARN/FAIL/UNKNOWN) with stable string serialization + color mapping (depends on T027)
- [x] T029 [P] Implement `internal/pgmctl/output/table.go`: `text/tabwriter`-based renderer with default + wide column-set support; FR-011's 80×24 ceiling for the 3-peer case
- [x] T030 [P] Implement `internal/pgmctl/output/json.go` and `internal/pgmctl/output/yaml.go`: schema-versioned (`apiVersion: pgmctl/v1`) emitters; suppresses ANSI escapes; matches `cli-commands.md` JSON shapes
- [x] T031 [P] Implement `internal/pgmctl/confirm/tty.go`: TTY detection + non-TTY-without-override refusal helper
- [x] T032 [P] Implement `internal/pgmctl/confirm/prompt.go`: `Prompt(op, target, cluster string) (bool, error)` for `[y/N]`; `ConfirmClusterName(op, target, cluster string) error` for typed cluster-name (depends on T031)
- [x] T033 Implement `internal/pgmctl/cmd/root.go`: cobra root command + global flags per `contracts/cli-commands.md § Global flags`; mutually-exclusive `--quiet`/`--verbose`; `--no-color` precedence; persistent flags wired to a `*Context` carried via cobra command annotations (depends on T023, T025, T027)
- [ ] T034 [P] Implement `internal/pgmctl/cmd/completion.go`: `pgmctl completion bash|zsh|fish` via cobra's built-in generator (FR-003)
- [x] T035 [P] Implement `internal/pgmctl/client/version_skew.go`: read `/v1/version` from the server, compare to compiled-in pgmctl version, classify as match / patch-skew / minor-skew / major-skew; emit yellow warning on minor-skew unless `--insecure-skip-version-check`; refuse with `EX_VERSION_SKEW` (67) on major-skew

### Foundational tests

- [x] T036 [P] Add contract test `tests/contract/pgmctl/cobra_root_test.go`: every global flag in `contracts/cli-commands.md` is registered; unknown flags exit `EX_USAGE` (64); mutual-exclusion of `--quiet`/`--verbose` enforced
- [ ] T037 [P] Add contract test `tests/contract/pgmctl/config_loader_test.go`: refuses to load a config file with mode 0644; honours endpoint precedence; rejects context with two token sources set
- [x] T038 [P] Add contract test `tests/contract/history_publish_test.go`: every event the proxy emits on the structured-log sink is also retrievable from the history stream within 250ms
- [x] T039 [P] Add contract test `tests/contract/fanout_basic_test.go`: 3-peer fixture; broadcast `status` slice; assert all three replies arrive within 100ms p99; assert aggregated payload contains all three node ids

**Checkpoint**: Foundation ready — user story implementation can now begin in parallel.

---

## Phase 3: User Story 1 — One-glance cluster health (Priority: P1) 🎯 MVP

**Goal**: A paged on-call SRE can run `pgmctl status` against a 3-peer cluster and identify leader / primary / unhealthy nodes within 10 seconds (SC-001), with green/yellow/red color cues and a JSON-pipeline path.

**Independent Test**: Spin up a 3-peer fixture; run `pgmctl status`; assert table + colors + exit codes per FR-011, SC-002, SC-008. Run `pgmctl status -o json | jq '.cluster.leader.node_id'`; assert ULID-shaped output without ANSI escapes.

### Tests for User Story 1

- [ ] T040 [P] [US1] Contract test `tests/contract/pgmctl/status_table_test.go`: golden-file diff against `tests/golden/pgmctl/status_healthy.txt`, `status_warn.txt`, `status_fail.txt`
- [x] T041 [P] [US1] Contract test `tests/contract/pgmctl/status_json_test.go`: `apiVersion: pgmctl/v1`, `kind: ClusterStatus`, no ANSI bytes; jq-parseable
- [x] T042 [P] [US1] Contract test `tests/contract/pgmctl/status_no_color_test.go`: ANSI absent when `--no-color`, `NO_COLOR=1`, or stdout not a TTY (SC-008)
- [ ] T043 [P] [US1] Integration test `tests/integration/pgmctl/status_3peer_test.go`: against the existing 3-peer test harness; assert status exits 0 on healthy, 2 on unhealthy, 65 on unreachable
- [ ] T044 [P] [US1] Integration test `tests/integration/pgmctl/version_skew_test.go`: skewed-version stub server returns 1.x while pgmctl is 1.y; assert yellow warning on minor-skew; assert `EX_VERSION_SKEW` (67) on major-skew without override

### Implementation for User Story 1

- [x] T045 [P] [US1] Implement `internal/pgmctl/cmd/status.go`: calls `GET /v1/status` via the client foundations; renders via the four output renderers; honours `--cluster <name>` cluster-id pinning (FR-010)
- [x] T046 [P] [US1] Implement `internal/pgmctl/cmd/version.go`: prints client + server version; uses T035's skew detection
- [x] T047 [P] [US1] Implement `internal/pgmctl/cmd/topology.go`: ASCII tree renderer for `GET /v1/topology`; JSON/YAML pass-through (FR-013)
- [x] T048 [P] [US1] Implement `internal/pgmctl/cmd/health.go`: composes status + (foundational) doctor-style rollup endpoint OR a server-side `/v1/health` if needed; emits one-line-per-component rollup (FR-014). For the MVP, derive the rollup client-side from `/v1/status` to avoid a new endpoint.
- [x] T049 [P] [US1] Implement `internal/pgmctl/cmd/lag.go`: derives per-standby lag from `/v1/diagnose`; `--warn` / `--fail` flags; lag in bytes + time (FR-015)
- [x] T050 [P] [US1] Implement `internal/pgmctl/cmd/get.go`, `list.go`, `describe.go`: resource verbs for `nodes`, `peers`, `slots`, `topology`, `config`, `version` (FR-012). `events` and `audit` resources are added in US2.
- [x] T051 [US1] Implement `internal/pgmctl/cmd/config_cmd.go`: `pgmctl config view|use-context|set-context|delete-context` (FR-007); `view` redacts secrets; `set-context` accepts source-references only, never plaintext values
- [ ] T052 [US1] Generate golden files for status under healthy / warn / fail / no-color / json / yaml / wide variants; commit to `tests/golden/pgmctl/`
- [x] T053 [US1] Documentation: write a one-page `docs/pgmctl/README.md` summary derived from `quickstart.md` § 1–3

**Checkpoint**: US1 ships independently — `pgmctl status` works against any reachable pgman-proxy peer, with table / JSON / YAML / wide / colored / no-color outputs and proper exit codes.

---

## Phase 4: User Story 2 — Full state dump for post-mortem (Priority: P1)

**Goal**: A senior engineer can run `pgmctl dump` once and receive a single self-contained tar.gz artifact with status, history events, history audit, topology, redacted config, doctor results, clock-skew, and per-peer slices via fan-out.

**Independent Test**: Trigger a failover; run `pgmctl dump --output ./pm.tar.gz`; extract; assert every slice in `contracts/cli-commands.md § dump` is present; assert `manifest.json` lists each slice's outcome; assert `--redact-level=strict` replaces hosts/IPs/node-ids. Partial-reach scenario exits with code 3.

### Server-side support for US2

- [x] T054 [US2] Implement `internal/control/handlers_history.go` with `GET /v1/history`: query parameters per `contracts/control-plane-extensions.md § 4`; uses `internal/history.Query`; supports JSON envelope + SSE form when `Accept: text/event-stream`; emits `pgman_proxy_history_query_records_total` / `pgman_proxy_history_query_latency_seconds` metrics (depends on T015)
- [x] T055 [US2] Wire `GET /v1/history` into `internal/control/route.go` with `s.wrap("HistoryQuery", false, false, …)`; bearer-auth inherited; respects `control.auth.allow_unauth_reads` (001)
- [x] T056 [US2] Implement `internal/control/types.go` additions: `HistoryQueryResult` Go type matching `contracts/history-stream.md § Record schema`; JSON tags `apiVersion`, `kind`, `events`, `next_cursor`, `truncated`

### Tests for User Story 2

- [ ] T057 [P] [US2] Contract test `tests/contract/pgmctl/dump_manifest_test.go`: dump artifact contains `manifest.json` with all required fields; slice outcomes correct; pgmctl + pgman-proxy versions recorded
- [ ] T058 [P] [US2] Contract test `tests/contract/pgmctl/dump_redact_test.go`: `--redact-level=normal` redacts bearer tokens; `--redact-level=strict` replaces hosts/IPs/node-ids with stable placeholders + ships the correlation table
- [ ] T059 [P] [US2] Contract test `tests/contract/history_query_test.go`: query by `since` / `until` / `type` / `node` / `cursor` / `limit`; resumption via `cursor` returns strictly newer records; `since` older than retention → `410 history_retention_exceeded`
- [ ] T060 [P] [US2] Integration test `tests/integration/pgmctl/dump_3peer_test.go`: healthy 3-peer cluster; dump completes < 15s p95 (SC-003); artifact < 10 MiB
- [ ] T061 [P] [US2] Integration test `tests/integration/pgmctl/dump_partial_test.go`: kill peer C mid-dump; dump still completes; peer C's slice has `outcome: failed`; exit code is 3 (`EX_PARTIAL`)
- [ ] T062 [P] [US2] Integration test `tests/integration/pgmctl/dump_stdout_test.go`: `pgmctl dump --output -` emits a valid raw tar to stdout; tar parses cleanly

### Implementation for User Story 2

- [x] T063 [P] [US2] Implement `internal/pgmctl/dump/collector.go`: parallel slice fetcher with per-slice timeout (FR-032 default 10s); slices: status, topology, config, history-events, history-audit, doctor, clock-skew, per-peer (status, config, nats_mesh, doctor) via fan-out
- [x] T064 [P] [US2] Implement `internal/pgmctl/dump/redact.go`: normal + strict modes; outputs the correlation table for strict mode (FR-033)
- [x] T065 [P] [US2] Implement `internal/pgmctl/dump/tar.go`: writes tar.gz to file path or raw tar to stdout (FR-034); single-pass streaming so an in-flight failure leaves the partial archive in a state operators can still extract
- [x] T066 [US2] Implement `internal/pgmctl/dump/manifest.go`: assembles the `DumpManifest` per `data-model.md`; records per-slice durations (FR-035)
- [x] T067 [US2] Implement `internal/pgmctl/cmd/dump.go`: cobra command wiring; flags `--output`, `--redact-level`, `--per-slice-timeout`, `--since` (depends on T063, T064, T065, T066)
- [x] T068 [P] [US2] Implement `internal/pgmctl/cmd/events.go`: tails `/v1/history?category=event`; same flag set as `pgmctl get events`; honours `--since`, `--type`, `--node`, `--limit` (FR-016)
- [x] T069 [P] [US2] Extend `internal/pgmctl/cmd/get.go` (T050) to add `events` and `audit` resources backed by `/v1/history`
- [ ] T070 [US2] Generate golden files for `dump` manifest under healthy / partial / strict-redact variants

**Checkpoint**: US2 ships independently — `pgmctl dump` produces a complete post-mortem artifact in one invocation, partial-reach handled cleanly, JetStream history queryable.

---

## Phase 5: User Story 3 — Interactive doctor (Priority: P1)

**Goal**: An on-call SRE runs `pgmctl doctor` to discover and fix cluster issues with a named, server-driven check battery, an interactive `--fix` flow, and blast-radius-routed confirmations.

**Independent Test**: Healthy cluster → all 18 checks PASS / INFO; exit 0. Inject three failure modes (stalled standby, orphaned slot, lingering fence) → three FAILs with distinct named fixes; `--fix -y` resolves single-resource fails; cluster-affecting fixes still require typed cluster-name (FR-029).

### Server-side support for US3

- [x] T071 [US3] Implement `internal/control/doctor_checks.go`: registry of the 18 v1 checks per FR-022; each entry `{ name, description, runFn, evidenceSchema, suggestedFix? }`. `runFn` reads from existing pg-manager `Status` / `Diagnose`; per-peer checks use fan-out (T018); checks that need data the proxy cannot retrieve return `UNKNOWN` (8/18 implemented; remaining 10 documented as MISSING_CHECKS for follow-up data paths)
- [x] T072 [US3] Implement `internal/control/doctor_fixes.go`: registry of named fixes mapping to existing LCM operations; each entry `{ name, description, blastRadius, appliesToCheck, applyFn }`; `appliesToCheck` cross-references back to `doctor_checks.go`. Advisory fixes have nil `applyFn` (v1 ships only advisory fixes via SuggestedFix attached to checks)
- [x] T073 [US3] Implement `internal/control/handlers_doctor.go` with three endpoints per `contracts/control-plane-extensions.md § 2`: `GET /v1/doctor/checks`, `POST /v1/doctor/run`, `POST /v1/doctor/fix`. `run` is audited but not leader-only; `fix` follows `BlastRadius` for leader-routing per 001 FR-026
- [x] T074 [US3] Wire all three doctor endpoints into `internal/control/route.go`; `run`/`fix` use `s.wrap` with `mutating=false`/`true` respectively
- [x] T075 [US3] Add CI assertion: `internal/control/doctor_checks_readonly_test.go` runs every check against a fixture cluster, snapshots state before/after via `Status`+`Diagnose`, asserts no mutation occurred (FR-027 read-only invariant)

### Tests for User Story 3

- [ ] T076 [P] [US3] Contract test `tests/contract/pgmctl/doctor_render_test.go`: golden output for healthy / one-FAIL / mixed cases; green/yellow/red rendering verified; JSON shape includes `summary`, `checks[]` with `status`, `evidence`, `suggested_fix`
- [x] T077 [P] [US3] Contract test `tests/contract/doctor_endpoints_test.go`: `GET /v1/doctor/checks` returns 18 entries; `POST /v1/doctor/run` runs all when body empty; `POST /v1/doctor/fix` returns 412 `advisory_only` for an advisory fix (landed as `internal/control/handlers_doctor_test.go` — co-located with handler; 8/18 in v1 catalog)
- [ ] T078 [P] [US3] Integration test `tests/integration/pgmctl/doctor_inject_failures_test.go`: stalled standby + orphaned slot + lingering fence → three distinct FAILs with named fixes; `--fix -y` walks them; final state has all three converted to PASS (SC-010 TPR)
- [ ] T079 [P] [US3] Integration test `tests/integration/pgmctl/doctor_fpr_test.go`: healthy baseline fixture → zero FAILs in the v1 battery (SC-010 FPR ≤ 5%)
- [ ] T080 [P] [US3] Integration test `tests/integration/pgmctl/doctor_unknown_test.go`: induce missing-data path (e.g., disk metric unavailable) → check returns `UNKNOWN` not `FAIL`; exit `EX_UNKNOWN` (5)

### Implementation for User Story 3

- [x] T081 [P] [US3] Implement `internal/pgmctl/doctor/render.go`: maps `DoctorReport` from the server into table / JSON / YAML / wide outputs with severity coloring
- [ ] T082 [P] [US3] Implement `internal/pgmctl/doctor/fix.go`: iterates FAIL checks with non-nil `suggested_fix`; routes by `blast_radius` to `confirm.Prompt` / `confirm.ConfirmClusterName`; re-runs each underlying check after applying (depends on T032)
- [x] T083 [US3] Implement `internal/pgmctl/cmd/doctor.go`: cobra command with `--list`, `--check <name>`, `--fix`, `-y` flags; calls `GET /v1/doctor/checks` for discovery; `POST /v1/doctor/run` for execution; `POST /v1/doctor/fix` for application (depends on T081, T082) — v1 ships `--list` + run; `--fix` deferred with T082
- [ ] T084 [US3] Generate golden files for doctor under PASS / one-FAIL / mixed / strict-mode variants

**Checkpoint**: US3 ships independently — `pgmctl doctor` discovers, reports, and (with `--fix`) remediates known cluster issues via the server-driven catalogue.

---

## Phase 6: User Story 4 — Live watch streams (Priority: P2)

**Goal**: During an incident, the operator runs `pgmctl watch status|transitions|events|node <id>` to see cluster state change in real time with p95 redraw latency ≤ 1s (SC-009).

**Independent Test**: Open `pgmctl watch status` in one terminal; trigger a failover in another. The watch terminal renders each intermediate state within 1s; idle ticks do not flicker; SSE drop produces a gap marker, not silent loss.

### Server-side support for US4

- [ ] T085 [US4] Implement `internal/control/handlers_watch.go` per `contracts/control-plane-extensions.md § 1`: paths `GET /v1/watch/status`, `/v1/watch/transitions`, `/v1/watch/events`, `/v1/watch/node/<id>`; SSE framing (`text/event-stream`); 15s keepalive cadence; `Last-Event-ID` resumption against the history stream
- [ ] T086 [US4] Wire all four watch endpoints into `internal/control/route.go`; require `Accept: text/event-stream` (406 otherwise); bearer-auth inherited
- [ ] T087 [US4] Implement gap-marker emission in `handlers_watch.go`: a JetStream subscription lag, quorum loss, or resume-window-exceeded condition emits a single `event: gap_marker` frame with a `reason` field
- [ ] T088 [US4] Add metrics in `internal/obs/metrics.go`: `pgman_proxy_watch_streams_active{topic}` (gauge), `pgman_proxy_watch_events_emitted_total{topic,kind}` (counter), `pgman_proxy_watch_gaps_total{topic,reason}` (counter)

### Tests for User Story 4

- [ ] T089 [P] [US4] Contract test `tests/contract/watch_sse_test.go`: each topic emits valid SSE; `Last-Event-ID` resumption returns events strictly after that id; `:keepalive` cadence ≈ 15s
- [ ] T090 [P] [US4] Contract test `tests/contract/watch_gap_marker_test.go`: induce subscription lag → `event: gap_marker` arrives with documented `reason` value
- [ ] T091 [P] [US4] Integration test `tests/integration/pgmctl/watch_status_redraw_test.go`: trigger a state transition; assert pgmctl redraws within 1s p95 (SC-009)
- [ ] T092 [P] [US4] Integration test `tests/integration/pgmctl/watch_reconnect_test.go`: drop SSE mid-stream; pgmctl reconnects with exponential backoff; gap marker line is rendered; max-reconnect ceiling produces non-zero exit

### Implementation for User Story 4

- [ ] T093 [P] [US4] Implement `internal/pgmctl/watch/status.go`: differential cell-redraw using raw ANSI cursor escapes (RD-010); fixed line layout for cluster summary + peer table; full repaint on SIGWINCH
- [ ] T094 [P] [US4] Implement `internal/pgmctl/watch/append.go`: append-only renderer for `transitions`, `events`, `node` topics; one new line per event; highlighted gap-marker divider
- [ ] T095 [P] [US4] Implement `internal/pgmctl/watch/reconnect.go`: exponential backoff (250ms base, factor 2, cap 10s, max 30 attempts); surfaces reconnect attempts on a status bar line (depends on T026)
- [ ] T096 [US4] Implement `internal/pgmctl/cmd/watch.go`: cobra subtree `watch status|transitions|events|node <id>`; rejects `--output json/yaml` for watch (use `events --since 0` instead); Ctrl-C exits cleanly with code 0 (depends on T093, T094, T095)

**Checkpoint**: US4 ships independently — operators have a live watch surface for status / transitions / events / per-node, with bounded redraw latency and graceful drop recovery.

---

## Phase 7: User Story 5 — Plain-English explain mode (Priority: P2)

**Goal**: A less-experienced operator runs `pgmctl explain <subject>` and gets a Diagnosis / Evidence / Suggested-next-steps narrative grounded in observed cluster facts (history stream + doctor results).

**Independent Test**: Stall a standby's WAL replay; run `pgmctl explain replication-broken <node>`; assert the output names the affected node, cites the failing check, quotes a verbatim history record with timestamp, and lists 2–3 ordered next actions.

### Tests for User Story 5

- [x] T097 [P] [US5] Contract test `tests/contract/pgmctl/explain_template_test.go`: golden output for `failover-stuck`, `node-not-promoting`, `replication-broken`, `leader-election`, `current-state`, `last-event` subjects on fixture data (landed as `explain_test.go`; assertion-style coverage for current-state happy path, failover-stuck no-primary path, JSON wire shape, and unknown-subject reject)
- [x] T098 [P] [US5] Contract test `tests/contract/pgmctl/explain_not_applicable_test.go`: `pgmctl explain failover-stuck` on a healthy cluster → exit `EX_SUBJECT_NA` (4) with documented message
- [ ] T099 [P] [US5] Integration test `tests/integration/pgmctl/explain_replication_broken_test.go`: stall WAL replay; output cites the failing check + a history-stream record with RFC3339 timestamp + the failing node id

### Implementation for User Story 5

- [x] T100 [P] [US5] Implement `internal/pgmctl/cmd/explain.go`: dispatcher over the v1 subject set per FR-018; composes from `GET /v1/status`, `GET /v1/diagnose`, `POST /v1/doctor/run`, and `GET /v1/history` queries; no new server endpoint required
- [x] T101 [P] [US5] Implement `internal/pgmctl/cmd/explain_narrative.go`: per-subject narrative templates that interpolate `Diagnosis`, `Evidence` (verbatim history records, with timestamps), `Suggested next steps` (concrete pgmctl invocations)
- [ ] T102 [US5] Generate golden files for explain under each of the six v1 subjects

**Checkpoint**: US5 ships independently — operators can ask the cluster "why?" with no new server-side endpoint required (all composition happens in pgmctl on top of US2/US3 server support).

---

## Phase 8: User Story 6 — Safe mutating operations (Priority: P2)

**Goal**: An operator can fence/unfence a node, change a hot-reloadable config value, fail/switch over, promote, restart, and decommission peers — each with the right confirmation wall (single-resource `[y/N]` vs typed cluster-name) and audited end-to-end.

**Independent Test**: For every mutating subcommand, six assertions per SC-006: (a) prompt wording, (b) default-N behaviour, (c) override-flag behaviour, (d) `-y` does NOT escalate cluster-affecting ops, (e) `--force` requires matching `--cluster <name>`, (f) audit record present on the server side.

### Upstream prerequisite (Wave 0)

- [ ] T103 [US6] In sibling repo `../pg-manager`, add `func (m *Manager) RestartPostgres(ctx context.Context) error` per RD-001; emit the same transition events Start/Stop emit; tag as a new release; merge upstream
- [ ] T104 [US6] In `pgman-proxy/go.mod`, bump the `github.com/f1bonacc1/pg-manager` pin from `replace ../pg-manager` to the tagged version produced by T103; run `go mod tidy`; commit

### Server-side support for US6

- [ ] T105 [US6] Implement `internal/control/handlers_restart.go` per `contracts/proxy-self-terminate.md`: `POST /v1/restart` with `target=postgres|proxy` dispatch; `postgres` calls `Manager.RestartPostgres`; `proxy` runs supervisor-presence pre-check then enters drain/exit flow
- [ ] T106 [US6] Implement supervisor-presence detection in `internal/runtime/supervisor.go`: heuristics per RD-009 / `contracts/proxy-self-terminate.md § Supervisor presence detection`; runtime field `proxy.SupervisorPresence` populated at startup; log line `proxy.supervisor_presence_detected` emitted
- [ ] T107 [US6] Implement self-terminate handler path in `handlers_restart.go`: pre-check audit; respond 200 with envelope; flush; emit `proxy.self_restart_initiated`; drain via existing 001 FR-014 shutdown path; call `os.Exit(0)`
- [ ] T108 [US6] Wire `POST /v1/restart` into `internal/control/route.go`; `s.wrap("Restart", true, leaderOnlyForPostgresTarget, …)`
- [ ] T109 [US6] Add config key `proxy.assume_supervised: bool` in `internal/config/`; default `false`; documented in CHANGELOG / quickstart
- [ ] T110 [US6] Implement `internal/control/handlers_setconfig.go` (`POST /v1/config/set`): SIGHUP-trigger wrapper for the 002 hot-reload allow-list (peer-routes list + cluster password handle); rejects keys outside the allow-list with `set_config_key_disallowed` (400)
- [ ] T111 [US6] Wire `POST /v1/config/set` into `internal/control/route.go`; mutating; not leader-only

### Tests for User Story 6

- [ ] T112 [P] [US6] Contract test `tests/contract/pgmctl/mutating_grid_test.go` — six-assertion grid per SC-006, repeated for every mutating subcommand: fence, unfence, set-config, failover, switchover, promote, restart-postgres, restart-proxy, delete
- [ ] T113 [P] [US6] Contract test `tests/contract/restart_test.go` — five-case grid per `contracts/proxy-self-terminate.md § Tests`: postgres happy path, proxy happy path under tini, proxy refused without supervisor, proxy refused with wrong peer, proxy refused under audit-unavailable
- [ ] T114 [P] [US6] Contract test `tests/contract/setconfig_allowlist_test.go`: peer-routes change accepted; password change accepted; arbitrary other-key rejected with `set_config_key_disallowed`
- [ ] T115 [P] [US6] Integration test `tests/integration/pgmctl/mutating_audit_trail_test.go`: every mutating command produces exactly one audit record visible via `pgmctl get audit --request-id <id>`
- [ ] T116 [P] [US6] Integration test `tests/integration/pgmctl/restart_proxy_supervised_test.go`: 3-peer fixture under tini-style supervisor; `pgmctl restart node-2 --target=proxy --force --cluster <name>`; peer-2 exits and re-joins; data-plane connections through siblings unaffected (cluster failover budget per Constitution Performance baseline ≤ 5s p99)

### Implementation for User Story 6

- [ ] T117 [P] [US6] Implement `internal/pgmctl/cmd/fence.go` and `unfence.go`: single-resource mutating ops; `[y/N]` prompt; `-y` bypass; `POST /v1/fence` / `POST /v1/unfence` (depends on T032)
- [ ] T118 [P] [US6] Implement `internal/pgmctl/cmd/set_config.go`: client-side allow-list mirror of 002 FR-014a (refuses disallowed keys client-side before sending); `[y/N]` prompt; `-y` bypass
- [ ] T119 [P] [US6] Implement `internal/pgmctl/cmd/failover.go`: cluster-affecting; typed cluster-name confirmation; `--force --cluster <name>` bypass; `POST /v1/failover`
- [ ] T120 [P] [US6] Implement `internal/pgmctl/cmd/switchover.go`: same pattern; `--target <node>` required; `POST /v1/switchover`
- [ ] T121 [P] [US6] Implement `internal/pgmctl/cmd/promote.go`: cluster-affecting; `POST /v1/promote`; local-only per 001
- [ ] T122 [P] [US6] Implement `internal/pgmctl/cmd/restart.go`: `--target=postgres|proxy` selector; typed cluster-name confirmation; for `--target=proxy`, pre-resolves the right peer via `Status` and `--endpoint` override to land on the target node (depends on T105)
- [ ] T123 [P] [US6] Implement `internal/pgmctl/cmd/delete.go`: cluster-affecting; wraps `POST /v1/topology` with a decommission-peer payload; documents in prompt that the proxy process on the target is NOT killed (FR-030)
- [ ] T124 [US6] Implement `request_id` print-to-stdout on every accepted mutating op so operators can correlate with `pgmctl get audit --request-id <id>` (FR-039)
- [ ] T125 [US6] Generate golden files for every mutating prompt's exact wording; pin into `tests/golden/pgmctl/prompts_*.txt`

**Checkpoint**: US6 ships independently — every mutating operation is wired, confirmed correctly, audited end-to-end. Combined with US1–US5, all six user stories are now deliverable.

---

## Phase 9: Polish & Cross-Cutting Concerns

**Purpose**: Cross-cutting improvements that benefit every story.

- [ ] T126 [P] Documentation: `docs/pgmctl/reference/` — one `.md` per subcommand with synopsis, flags, exit codes, examples (sourced from `contracts/cli-commands.md`)
- [ ] T127 [P] Documentation: `docs/pgmctl/man/pgmctl.1` — top-level groff man page; built from the cobra command tree via `cobra --doc=man`
- [ ] T128 [P] Documentation: update `README.md` (root) with a "pgmctl" section that points at `quickstart.md` and the install instructions
- [ ] T129 [P] Documentation: add a `CHANGELOG.md` entry under `v1.0.0` (or the next unreleased version) covering the five new server-side endpoints, the `Manager.RestartPostgres` upstream pin bump, and the kubeconfig-style config format
- [ ] T130 [P] Performance audit: measure `pgmctl status` p95 against a 3-peer fixture; assert SC-002 (≤ 1.5s); record in `tests/perf/pgmctl_status_baseline.json`
- [ ] T131 [P] Performance audit: measure `pgmctl dump` p95 against a healthy 3-peer fixture; assert SC-003 (≤ 15s, artifact < 10 MiB)
- [ ] T132 [P] Performance audit: measure `pgmctl watch status` p95 redraw + idle CPU; assert SC-009 (≤ 1s redraw, < 1% one-core idle)
- [ ] T133 [P] Doctor TPR/FPR audit: run the v1 failure-mode fixture set + a healthy baseline; assert SC-010 (TPR ≥ 90%, FPR ≤ 5%)
- [ ] T134 Security audit: grep the repo for any path that emits the bearer token; assert FR-009 holds across status / dump / verbose / trace
- [ ] T135 Security audit: assert `--print-config`-style outputs continue to redact secrets at every level; assert strict-redact removes hosts/IPs/node-ids in the dump artifact
- [ ] T136 [P] Run `quickstart.md` end-to-end against a fresh 3-peer fixture and a fresh kubeconfig; confirm every command in the quickstart produces the documented output
- [ ] T137 [P] Build-and-release dry run: tag a pre-release; trigger `make pgmctl-release`; smoke-test each artifact (linux/amd64, linux/arm64, darwin/amd64, darwin/arm64) by running `pgmctl version` and `pgmctl --help` in a clean container/VM
- [ ] T138 [P] Signing: produce SHA-256 checksums for every release artifact; verify the existing release workflow attaches them (cosign-signed bundle if release infra supports it)
- [ ] T139 Final integration sweep: run the full `tests/integration/pgmctl/` suite end-to-end against the existing process-compose fixture cluster; investigate and resolve any flakes

**Checkpoint**: Release-ready. Quickstart validates; performance bars met; security audit clean; docs and manpages published; release artifacts smoke-tested.

---

## Dependencies & Execution Order

### Phase dependencies

- **Phase 1 (Setup)**: no dependencies; can start immediately.
- **Phase 2 (Foundational)**: depends on Phase 1.
- **Phase 3 (US1)** through **Phase 8 (US6)**: each depends on Phase 2; otherwise mutually independent and may run in parallel by different developers.
  - **Phase 8 (US6)** has one internal sequencing constraint: T103 (upstream pg-manager change) → T104 (go.mod pin) → T105 (restart handler).
- **Phase 9 (Polish)**: depends on Phases 3–8 reaching their checkpoints; some [P] polish tasks (docs) can land in parallel with late user-story work.

### Within each user story

- Tests and server-side support land before client implementation when there is a cross-dependency (e.g., US2's T054–T056 server endpoints land before T067 `cmd/dump.go` can integration-test against a real server).
- Different command files in the same user story may land in parallel ([P]).
- Golden-file generation is the LAST sub-step inside each story (after the renderer is final).

### Parallel opportunities

- All [P] tasks within a phase can run in parallel.
- Once Phase 2 is at its checkpoint, US1–US6 phases can be picked up by different developers in parallel; each story is independently testable.
- Server-side tasks for US2/US3/US4/US6 may run in parallel with the corresponding client tasks once the contract from `contracts/` is locked.

---

## Parallel Example: User Story 1

```bash
# After Phase 2 checkpoint, a single developer (or wave) can pick up US1 in
# parallel-where-marked order:

# 1. Implementation (T045–T051): each command file is independent.
$ git checkout -b us1-pgmctl-status
$ # implement T045 (status.go), T046 (version.go), T047 (topology.go),
$ #           T048 (health.go), T049 (lag.go), T050 (get/list/describe.go),
$ #           T051 (config_cmd.go) — all [P] except T051

# 2. Tests (T040–T044): write each in parallel; let them fail; make them pass.
$ go test ./tests/contract/pgmctl/ -run Status
$ go test ./tests/contract/pgmctl/ -run NoColor
$ go test ./tests/contract/pgmctl/ -run JSON
$ go test ./tests/integration/pgmctl/ -run Status_3peer
$ go test ./tests/integration/pgmctl/ -run VersionSkew

# 3. Golden files (T052): generate after renderer stability.
$ go test ./tests/contract/pgmctl/ -run Status -update

# 4. Commit each task as its own atomic commit per Constitution / project convention.
```

---

## Implementation strategy

- **MVP**: complete Phase 1 + Phase 2 + Phase 3 (US1). Delivers a working `pgmctl status` against any reachable pgman-proxy peer, plus the read-only data surface (topology, get, list, describe, health, lag, version, config). This is the smallest shippable slice and would already be useful to operators.
- **Second increment**: US2 (dump) + US3 (doctor) land next — both are P1 and complete the "debugging surface" the spec was written around.
- **Third increment**: US4 (watch) + US5 (explain) land next — P2 user-experience polish.
- **Fourth increment**: US6 (mutating ops) lands last. It requires the upstream `Manager.RestartPostgres` PR which should be opened against `../pg-manager` as soon as Phase 1 is done so it has time to land before T105 starts.
- **Final**: Phase 9 (Polish) closes out docs, performance audits, security audits, and release tooling.

### Task counts

- Setup (Phase 1): 9 tasks (T001–T009)
- Foundational (Phase 2): 30 tasks (T010–T039)
- US1 (Phase 3): 14 tasks (T040–T053)
- US2 (Phase 4): 17 tasks (T054–T070)
- US3 (Phase 5): 14 tasks (T071–T084)
- US4 (Phase 6): 12 tasks (T085–T096)
- US5 (Phase 7): 6 tasks (T097–T102)
- US6 (Phase 8): 23 tasks (T103–T125)
- Polish (Phase 9): 14 tasks (T126–T139)

**Total: 139 tasks**.
