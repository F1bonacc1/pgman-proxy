# Research — `pgmctl` Operator CLI

**Feature**: `003-pgmctl-cli` · **Phase**: 0 · **Date**: 2026-05-14
**Status**: Resolved. All Technical-Context unknowns answered below.

Each topic is in **Decision / Rationale / Alternatives** format per the
Spec Kit convention. Topics are numbered to match plan.md § Phase 0.

---

## RD-001 — pg-manager Manager API delta

**Decision**: Add a single method to the upstream `Manager`:

```go
// RestartPostgres performs a clean stop followed by a fresh start of the
// managed PostgreSQL process. It is leadership-aware (returns ErrNotLeader
// on a peer that is not the leader for the target NodeID) and emits the
// same state-transition events Start / Stop emit individually.
func (m *Manager) RestartPostgres(ctx context.Context) error
```

The pgman-proxy `internal/control/handlers_restart.go` calls this primitive
when `--target=postgres`. The PR lands **in `../pg-manager` first**, is
tagged, and pgman-proxy bumps its `go.mod` pin in Wave 0 of `/speckit-tasks`.

**Rationale**: Constitution Principle IV forbids reimplementing missing
pg-manager behaviour proxy-side. Manager.Stop/Start exist; Restart does
not. Stop→Start in the proxy would duplicate (a) the Manager's state-store
update ordering, (b) lease re-acquisition, (c) observability event
sequencing, and would drift over time. One upstream method is the cheapest
correct path.

**Alternatives considered**:
- *Proxy-local Stop+Start wrapper*: rejected — explicit Constitution IV
  violation; would require an exception in Complexity Tracking, which we
  don't need given the upstream PR is trivial.
- *Send SIGHUP to the managed Postgres process*: rejected — SIGHUP is
  reload, not restart, and Postgres `restart` semantics include checkpoint
  + new postmaster, which only Manager can orchestrate safely.
- *Two endpoints (stop + start) instead of one restart*: rejected — the
  spec's confirmation flow (FR-029, typed cluster-name) wants exactly one
  destructive prompt, not two.

---

## RD-002 — JetStream history stream configuration

**Decision**:

- **Stream name**: `PGMAN_PROXY_HISTORY_<CLUSTER_ID>` (per-cluster
  isolation; matches 002's per-cluster KV-bucket convention).
- **Subjects**: `pgman_proxy.<cluster_id>.history.event.>` for state
  transitions / leader changes / fence/unfence / route up-down,
  `pgman_proxy.<cluster_id>.history.audit.>` for LCM audit records.
  Both subjects feed the same stream so a single query returns the
  interleaved chronological view.
- **Storage**: file-backed (`nats.FileStorage`); reuses 002's JetStream
  storage path (`embedded.JetStreamDir`).
- **Replicas**: derived from declared cluster size per 002 FR-011a
  (R=1 / R=2 / R=3 by single / 2-peer / 3+-peer). Override path:
  `cluster.replication_factor_override` (already exists from 002).
- **Retention**: `RetentionPolicy = LimitsPolicy` with **both**
  `MaxAge = 24h` and `MaxBytes = 256 MiB` (defaults; operator-tunable
  via two new config keys `history.retention_age` and
  `history.retention_bytes`). When either limit is breached, the
  stream discards from the oldest record (`DiscardPolicy =
  DiscardOld`).
- **Ack policy**: producer-side `AckExplicit` so a lost publish is
  visible as a `history.publish_failed` log + metric; consumer-side
  (query endpoint) reads via the JetStream `OrderedConsumer` API which
  needs no consumer state.
- **Duplicate-detection window**: `MaxMsgs = 0` (no message cap beyond
  bytes/age); `Duplicates = 0` (we tag each publish with a fresh
  ULID — duplicate prevention is push-side responsibility).
- **Query interface**: HTTP `GET /v1/history?since=<dur>&until=<rfc3339>
  &type=<kind>&node=<id>&limit=<n>` over the existing control-plane
  listener. Server-side translates filters into a JetStream
  `OrderedConsumer` with subject-pattern subscription
  (`pgman_proxy.<cluster_id>.history.event.>` and/or
  `.audit.>`), drains messages until `until` is reached or `limit` is
  hit, and emits a JSON array. Streaming form via SSE for the same
  endpoint when `Accept: text/event-stream`.

**Rationale**: JetStream is already a load-bearing primitive in this
codebase (002 leadership KV + state-store + event bus). A single
durable stream keyed by cluster id meets FR-016a's "any peer can serve
history" because JetStream streams replicate across peers; queries can
land on any peer. File-backed storage survives single-peer restart up
to retention. Subject-pattern fan-out is idiomatic NATS and avoids an
external store.

**Alternatives considered**:
- *Postgres-backed history table*: rejected — adds a write dependency
  on the very thing pgmctl exists to observe; the audit subject is
  meant to be available even when Postgres is sick.
- *In-memory ring buffer per peer*: rejected at /speckit-clarify Q1.
- *Two separate streams (events + audit)*: rejected — operators want
  interleaved chronology; merge-sort across two streams would
  introduce subtle ordering bugs at the boundary.
- *MaxAge only (no MaxBytes)*: rejected — without a size bound a
  high-flap cluster could pin the JetStream storage path to its limit
  inside a quiet retention window. Combined limit keeps both axes
  honest.

---

## RD-003 — Inter-peer NATS request/reply fan-out

**Decision**:

- **Subject scheme**: `pgman_proxy.<cluster_id>.fanout.<slice>.<node_id>`
  where `<slice>` is one of `status`, `config`, `nats_mesh`, `doctor`,
  and `<node_id>` is the **target** peer's node id. Each peer subscribes
  to subjects ending in its own node id and to a wildcard
  `pgman_proxy.<cluster_id>.fanout.<slice>.*` so a *broadcast* form is
  possible by sending to `*` with `nats.RequestMany`.
- **Payload envelope**: `{ "request_id": "<ulid>", "operator_actor":
  "<from-bearer>", "trace_id": "...", "deadline_ms": <n>, "slice":
  "<slice>", "args": { … } }` — JSON, schema-versioned with `version:
  1`.
- **Reply envelope**: `{ "request_id": "<ulid>", "node_id":
  "<responder>", "status": "ok"|"partial"|"failed", "data": { … },
  "error": { "code": "...", "message": "..." } }`.
- **Timeout**: default 5s per-slice (well under the 10s per-slice budget
  from FR-032); configurable via `--per-slice-timeout`. Unreachable
  siblings produce a single `failed` entry with `error.code =
  "sibling_unreachable"`; never a whole-request failure.
- **Authorization**: the connected peer authenticates the operator
  (existing bearer-token middleware); fan-out requests are emitted
  with the operator's `actor` so siblings' audit logs reflect the
  originating user. Siblings trust intra-cluster NATS auth (002 cluster
  credential) as the inter-peer trust anchor. No second auth layer.

**Rationale**: NATS request/reply is the idiomatic, latency-efficient
fan-out for intra-cluster RPC; one subject hierarchy per slice keeps the
fan-out boundary obvious. `RequestMany` lets a single call collect
N responses with a deadline. Reusing the existing inter-peer auth from
002 avoids a duplicate trust path.

**Alternatives considered**:
- *HTTP-to-sibling-control-plane*: rejected — pgmctl would need N TLS
  trust anchors; FR-006 binds it to one peer.
- *NATS JetStream queue group*: rejected — fan-out wants every sibling
  to respond, not exactly one; queue-group semantics are wrong.
- *gRPC streaming over the existing control plane*: rejected — adds a
  large dep tree (`google.golang.org/grpc`) for one use case; HTTP+JSON
  is the 001 contract.

---

## RD-004 — SSE wire format on the control-plane listener

**Decision**:

- **Path scheme**: `GET /v1/watch/<topic>` where `<topic>` is one of
  `status`, `transitions`, `events`, `node/<id>`.
- **Content type**: `text/event-stream; charset=utf-8`.
- **Event framing**: standard SSE — `event: <name>\ndata: <one-line
  JSON>\nid: <ulid>\n\n`. `<name>` is the event kind (`status_update`,
  `state_transition`, `cluster_event`, `gap_marker`). `id` is a ULID so
  pgmctl can resume after reconnect with `Last-Event-ID`.
- **Keepalive**: server emits `:keepalive\n\n` every **15s** of
  silence. pgmctl detects ≥ 30s without any event (data or keepalive)
  as a dropped stream → reconnect.
- **Reconnect backoff**: exponential, base 250ms, factor 2, capped at
  10s; max 30 attempts before exit-nonzero. Each reconnect attempt
  carries `Last-Event-ID` so the server can resume from the next
  event after that ULID. Resume window is bounded by the history
  stream's retention (RD-002).
- **Gap markers**: server emits an `event: gap_marker` with
  `data: { "reason": "stream_lag" | "quorum_lost" | "resume_window_exceeded" }`
  on any partial-loss condition. pgmctl renders the gap as a
  highlighted line in the append stream and a status-bar tick in the
  redraw mode.
- **Filter parameters**: `?since=<dur>`, `?type=<kind>`, `?node=<id>`
  echoed on the URL; mirror the history-stream query (RD-002).

**Rationale**: SSE is the right transport for ordered, server-pushed
events over HTTP — no extra dep, one direction, recoverable via
`Last-Event-ID`. The 15s keepalive matches the conventional NAT-traversal
budget for long-lived HTTPS. The reconnect strategy guarantees
"clearly-marked gap line, never silent loss" per FR-020.

**Alternatives considered**:
- *WebSocket*: rejected — bidirectional, more framing complexity; we
  only ever push from server to client.
- *Long-poll*: rejected — every reconnect re-authenticates; flaky on
  high-flap clusters; loses transitions during the poll gap.
- *NATS subject subscription from pgmctl*: rejected by FR-006a (single
  HTTP connection per invocation).

---

## RD-005 — Bearer-token sourcing and TLS trust resolution

**Decision**:

- **Token source precedence** (FR-008, mirrors 001's pattern):
  1. `--token <value>` flag (NOT supported — refused at parse to avoid
     leakage into shell history; pgmctl prints a one-line error pointing
     at the documented sources). This is a deliberate non-feature.
  2. `PGMCTL_TOKEN` env var (when set and non-empty).
  3. `token_file` path from the active context (read on every request;
     no in-memory cache beyond the single request's lifetime — supports
     001 FR-031 rotation).
  4. `token_command` from the active context (operator-supplied
     command whose stdout, trimmed, is the token; trustworthy because
     the operator authored both pgmctl config and the command).
- **TLS trust resolution**:
  1. If `tls.ca_file` is set in the active context, that file is the
     trust anchor (PEM bundle).
  2. Otherwise the system CA bundle via `crypto/tls`'s default.
  3. `--insecure-skip-tls-verify` disables verification and prints a
     loud yellow line on stderr.
  4. `tls.server_name` allows SNI override (helps with mesh proxies).
- **Cluster id pinning** (FR-010): operator may set `expected-cluster`
  in the context; pgmctl reads `Status` first, asserts cluster id
  match, refuses on mismatch BEFORE sending any mutating request,
  even with `--force`.

**Rationale**: Mirrors 001 FR-024 and aligns with the project's
"secrets are sourced, never inlined" policy. Refusing `--token` on the
flag list is the cheapest way to prevent secrets from landing in shell
history; the env / file / command channels cover every legitimate
workflow (CI, kubeconfig, vault).

**Alternatives considered**:
- *Inline token in config YAML*: rejected — Constitution / 001 secrets
  policy.
- *Keychain integration (`security`, `secret-tool`)*: deferred — nice
  but unnecessary for v1; `token_command` covers it via the operator's
  preferred wrapper.

---

## RD-006 — Color rendering

**Decision**: `github.com/fatih/color` for static commands; raw ANSI
cursor escapes for the watch-mode differential redraw.

`fatih/color` ships:
- `color.NoColor` global toggle (honoured automatically when stdout is
  not a TTY).
- `NO_COLOR` env var detection (per `no-color.org`).
- Composable sprint functions (`color.GreenString`, etc.) that pgmctl
  wraps in `internal/pgmctl/output/severity.go` so the mapping
  Severity → color lives in one place.

Watch mode uses raw ANSI: `\033[<row>;<col>H` (cursor positioning) +
`\033[K` (erase to line end) for the diff redraw; full-line repaint for
the few cells that changed. No alt-screen, no bracketed paste, no mouse
— a single full repaint is allowed on resize via `SIGWINCH`.

**Rationale**: Two render paths for two truly different needs. A full
TUI framework (bubbletea / lipgloss) is overkill for ≤ 24-line
differential redraws and would pull a 50+-file dep tree. `fatih/color`
is ~200 lines of dep code, well-maintained, and matches the kubectl-
ecosystem convention.

**Alternatives considered**:
- *`charmbracelet/lipgloss` + `bubbletea`*: rejected — heavy for the
  benefit; bubbletea's runtime owns the terminal in a way that
  complicates piped output and tests.
- *Roll own color helpers*: rejected — `NO_COLOR` semantics, TTY
  detection, and Windows VTERM bootstrap aren't worth re-implementing.

---

## RD-007 — kubeconfig-style configuration

**Decision**:

- **Path**: `$XDG_CONFIG_HOME/pgmctl/config.yaml` (FR-007); falls back
  to `~/.config/pgmctl/config.yaml`. On Linux only — Windows is out
  of scope for v1.
- **Schema** (versioned at top):

```yaml
apiVersion: pgmctl/v1
kind: Config
current-context: prod-east
contexts:
  - name: prod-east
    endpoint: https://pgman-proxy.prod-east.example.com:9091
    expected-cluster: prod-east
    token_file: /var/run/secrets/pgmctl/prod-east.token
    # or:
    # token_env: PGMCTL_PROD_EAST_TOKEN
    # or:
    # token_command: ["vault", "kv", "get", "-field=token", "kv/pgmctl/prod-east"]
    tls:
      ca_file: /etc/pki/ca-trust/source/anchors/pgmctl-prod.pem
      server_name: pgman-proxy.prod-east.svc
  - name: dev-laptop
    endpoint: https://127.0.0.1:9091
    token_file: ~/.cache/pgmctl/dev-token
    tls:
      insecure-skip-tls-verify: true
```

- **Endpoint resolution precedence** (FR-006): `--endpoint` flag >
  `--context` flag > `current-context` in config > `PGMCTL_ENDPOINT`
  env > error.
- **Subcommands** (FR-007): `config view`, `config use-context <name>`,
  `config set-context <name> [--endpoint ...] [--token-file ...] [...]`,
  `config delete-context <name>`. `config view` redacts every
  secret-bearing field by default and supports `--show-secrets` only
  when stdout is a TTY (never in pipelines).
- **File permissions**: pgmctl REFUSES to read the file if mode is
  group- or world-readable (mirrors ssh / kubectl behaviour). Refusal
  exits `EX_CONFIG` with an instruction on how to `chmod 600`.

**Rationale**: kubectl-pattern, well-understood by every SRE; the
schema is small enough to read by hand. The refusal on bad permissions
is one of the few places where pgmctl is more strict than kubectl
(which only warns) — appropriate given a stolen pgmctl config grants
cluster-mutation rights.

**Alternatives considered**:
- *Single-context only*: rejected — Q-clarify resolved multi-context
  as a v1 requirement.
- *TOML or JSON config*: rejected — YAML matches the existing
  pgman-proxy config family.

---

## RD-008 — Doctor check registry pattern

**Decision**:

- **Server-side check catalogue** lives in
  `internal/control/doctor_checks.go` as a slice of `Check` values:

```go
type Check struct {
    Name        string                  // e.g. "replication.lag-acceptable"
    Description string
    Run         func(ctx context.Context, env *checkEnv) Result
}

type Result struct {
    Status      string        // PASS|INFO|WARN|FAIL|UNKNOWN
    Message     string        // one-line human form, with offending value
    Evidence    any           // structured evidence (filtered to non-secret fields)
    SuggestedFix *FixRef      // optional; pointer to a Fix
}
```

- **Server-side fix catalogue** lives in
  `internal/control/doctor_fixes.go` with the same registry shape but
  with a `BlastRadius` (`single-resource` / `cluster-affecting` /
  `advisory`) and an `Apply(ctx, env) error` function. `advisory` fixes
  expose `Description` only; `Apply` is nil.
- **Discovery endpoint**: `GET /v1/doctor/checks` → JSON array of
  `{ name, description, suggested_fix?: { name, blast_radius,
  description } }`.
- **Execute endpoint**: `POST /v1/doctor/run` (body: `{ check?: string }`;
  empty = run all) → JSON map of name → Result.
- **Fix endpoint**: `POST /v1/doctor/fix` (body: `{ fix: string,
  args: any, request_id }`) — bearer-auth, audit-logged, returns the
  same envelope as 001's LCM ops. Cluster-affecting fixes inherit the
  leader-route rule from 001 FR-026.
- **Read-only invariant** (FR-027): `Check.Run` MUST NOT mutate state.
  CI test asserts `Run`s do not call any LCM mutator.

**Rationale**: A registry pattern (vs hard-coded handlers) lets the v1
check battery grow without touching pgmctl. pgmctl renders whatever the
server publishes; `/v1/doctor/checks` is the single source of truth.

**Alternatives considered**:
- *Client-side checks*: rejected — would require pgmctl to talk to
  Postgres / NATS directly, breaching the "pgmctl is a client" rule.
- *One endpoint per check*: rejected — N round-trips for `doctor` vs
  one fan-out request inside the server; bad latency budget.

---

## RD-009 — Proxy self-terminate supervisor detection

**Decision**: Multi-signal heuristic with an explicit operator
override.

The proxy detects supervision at startup and writes the result to a
runtime field consulted by the `/v1/restart` `target=proxy` handler:

1. **Container heuristic**: if `/.dockerenv` exists OR `/proc/1/cgroup`
   contains a kubernetes / docker / containerd cgroup path → considered
   supervised (PID-1 is the container runtime / kubelet; SIGTERM is
   respawn-on-exit by k8s deployment / docker `restart: always`).
2. **systemd heuristic**: if `os.Getenv("INVOCATION_ID") != ""` AND
   `os.Getenv("JOURNAL_STREAM") != ""` → considered supervised
   (systemd unit).
3. **s6 / runit heuristic**: if `os.Getenv("S6_OVERLAY_VERSION") != ""`
   OR parent process basename matches `/^(s6-supervise|runsv|runsvdir)$/`
   → considered supervised.
4. **tini / dumb-init heuristic**: if parent process basename is `tini`
   or `dumb-init` → considered supervised.
5. **Explicit operator override**: config key `proxy.assume_supervised:
   true` forces the answer to yes.
6. **Default**: not supervised.

The `/v1/restart?target=proxy` handler returns `412 Precondition Failed`
with `error.code = "supervisor_not_detected"` when the answer is no
unless the override is set. pgmctl renders the refusal verbatim with
the documented remediation.

**Rationale**: There is no portable POSIX way to ask "am I being
supervised?". The heuristics above cover ≥ 95% of real-world
deployments (systemd, k8s, docker, s6, tini); the explicit override
covers the long tail without a host shell from pgmctl. Default-no keeps
us fail-closed per Constitution II.

**Alternatives considered**:
- *Just check `os.Getppid() != 1`*: rejected — `1` is the container's
  PID 1 in kubernetes, which IS supervised; the check is inverted.
- *Hard-fail without an override path*: rejected — locks out legitimate
  development workflows where the operator knows they have a respawner
  pgmctl can't see.
- *Probe `kill -0` against ppid after self-exit*: rejected — by then
  the proxy is dead; can't recover if the check was wrong.

---

## RD-010 — Watch-mode redraw strategy

**Decision**: Differential cell update with a fixed line layout for
`watch status`; pure append for `watch transitions` / `events` /
`node`.

`watch status` keeps a stable line layout (cluster summary, blank,
table header, N peer rows, blank, footer). On each SSE event, pgmctl
diffs the new snapshot against the previous one cell-by-cell; for each
changed cell, it emits `\033[<row>;<col>H<new>\033[K`. Unchanged cells
are not touched. Window resize triggers a full repaint via SIGWINCH.

`watch transitions` / `events` / `node` are append streams: each event
prints one new line at the bottom; a `gap_marker` event prints a
highlighted divider line.

CPU at idle in steady state is dominated by SSE keepalive parsing —
measured target < 1% of one core p95 (SC-009).

**Rationale**: A fixed layout + cell diff is simpler than reactive TUI
frameworks for a 24-line view, and it composes naturally with `less -R`
(pipe-through preserves ANSI). Append-only streams are the most
forgiving consumption shape for an active incident.

**Alternatives considered**:
- *Full repaint every tick*: rejected — flickers under 50ms-class events.
- *Alt-screen + bubbletea*: rejected — owns terminal too aggressively,
  not friendly to `tee` / `script`.

---

## RD-011 — Dump artifact format

**Decision**:

- **Container**: gzipped tar (`.tar.gz`) when `--output <path>`; raw
  uncompressed tar when `--output -` (caller decides compression).
- **Layout** (file paths inside the tar):

```text
manifest.json                 # FR-035: versions, slice outcomes, timings
status.json                   # /v1/status snapshot
topology.json                 # /v1/topology snapshot
config.redacted.yaml          # effective config, redaction per --redact-level
events.json                   # history query result, last 24h or full retention
audit.json                    # history audit-subset
doctor.json                   # full check battery output
clock_skew.json               # client→peer skew measurements
peers/<node_id>/
  status.json                 # per-peer Status from fan-out
  embedded_nats.json          # per-peer embedded-NATS mesh state
  config.redacted.yaml        # per-peer effective config
  doctor.json                 # per-peer doctor results (subset)
```

- **Manifest schema** (`pgmctl/v1`):

```json
{
  "apiVersion": "pgmctl/v1",
  "kind": "DumpManifest",
  "pgmctl": { "version": "...", "commit": "...", "go_version": "..." },
  "pgman_proxy": { "version": "...", "commit": "..." },
  "captured_at": { "started": "...", "ended": "..." },
  "redact_level": "normal" | "strict",
  "cluster_id": "<redacted-or-real>",
  "slices": [
    { "name": "status",       "outcome": "ok",      "duration_ms":  43 },
    { "name": "events",       "outcome": "ok",      "duration_ms": 187 },
    { "name": "peers/node-3", "outcome": "failed",  "duration_ms": 5023, "error": "sibling_unreachable: deadline exceeded" }
  ]
}
```

- **Redaction (FR-033)**:
  - `--redact-level=normal` (default): bearer tokens, NATS cluster
    password fingerprints (first 8 chars only — already 002 FR-010a
    redaction), TLS material, env-var contents, any field tagged
    `secret:true` in pgman-proxy's config schema.
  - `--redact-level=strict`: above PLUS host:port pairs replaced with
    `host-<idx>:<port-idx>`, IPs replaced with `<ip-idx>`, node ids
    replaced with `node-<idx>`. Mapping table is included in the dump
    so the original engineer can correlate.

**Rationale**: tar.gz is the universally-understood archive format;
JSON-per-slice keeps the dump grep-able and `jq`-able for someone
opening the artifact in a ticket attachment viewer. The strict-redact
correlation table is the trade-off that lets us share dumps externally
without losing forensic value internally.

**Alternatives considered**:
- *Single big JSON document*: rejected — opens slowly in editors; one
  malformed slice contaminates the whole file.
- *zip*: rejected — Linux operators reach for `tar` first; gzip is
  ubiquitous on Linux distros pgmctl ships to.

---

## RD-012 — Cross-platform build matrix

**Decision**:

- **CI build matrix**: `linux/amd64`, `linux/arm64`, `darwin/amd64`,
  `darwin/arm64`. Static linkage on linux (`CGO_ENABLED=0`); dynamic
  linkage on darwin (Go default; no cgo dependencies in pgmctl).
- **Reproducible builds**: `-trimpath`, `-buildvcs=true`,
  `-ldflags="-s -w -X main.version=$(git describe) -X main.commit=$(git rev-parse HEAD)"`.
- **Distribution**: tarballs (`pgmctl-<version>-<os>-<arch>.tar.gz`)
  attached to GitHub release; checksums (`.sha256`) and a `cosign`-signed
  bundle. Homebrew tap is out of scope for v1.
- **Smoke test on every release**: each artifact runs `pgmctl version`
  and `pgmctl --help` in a clean container/image; tier-1 platforms also
  run a `pgmctl status` against a fixture cluster.

**Rationale**: Single Go module, no cgo on Linux, reproducible flags —
all standard Go practice. Static linkage means the binary works on
distros with disparate libc; on Darwin Go's defaults are appropriate.

**Alternatives considered**:
- *cgo for advanced terminal detection*: rejected — `golang.org/x/term`
  is sufficient.
- *Windows build*: rejected — Windows is out of scope per FR-001.
- *Single fat binary embedding `pgman-proxy`*: rejected — separate
  binaries match the operator mental model and let the proxy be
  upgraded independently of pgmctl.

---

## Open questions resolved

All Phase-0 NEEDS-CLARIFICATION markers from plan.md § Technical Context
are resolved above. No questions outstanding.

## Forward-looking notes (not blocking v1)

- **`pgmctl plugin` extension model**: deferred (Assumptions); revisit
  after operator field feedback once 003 ships.
- **Keychain-backed token storage**: deferred (RD-005); `token_command`
  is the v1 escape hatch.
- **Windows port**: deferred indefinitely; not in v1.
- **Web UI**: out of scope.
