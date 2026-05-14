# Feature Specification: `pgmctl` — Operator CLI for pgman-proxy

**Feature Branch**: `003-pgmctl-cli`
**Created**: 2026-05-14
**Status**: Draft
**Input**: User description: "develop a cli executable (like kubectl) with cobra, named pgmctl. it's purpose is to connect to pgman-proxy and get its full status, every bit of information that can be useful for debugging the current pg-manager, pgman-proxy nats and postgres state. it should present a human friendly output with color green to signify a healthy state of things, yellow for warning or attention, red for errors. it should support doctor mode in which it can analyze, propose and apply fixes (interactive). it should support status with -o or --output json for automation. it should support read only operations status, get, list, describe, watch, events, logs, topology, health, lag, explain, doctor. Mutating operations on a single resource (confirmation prompt; --yes to skip): fence, unfence, set-config. Cluster-affecting operations (confirmation with cluster name verification; --force to skip): failover, switchover, promote, restart, delete. ... It should also support full state dump mode that will have ALL the information need to understand what caused a failure. Any failure. in one shot - without asking for more details. If required pgman-proxy and pg-manager api can be expanded to support pgmctl"

## Context & Relationship to Features 001 / 002

Feature `001-active-active-pg-proxy` shipped the proxy + control-plane HTTP
listener (`:9091` by default, bearer-auth, JSON envelope, audit pipeline).
Feature `002-embedded-nats-cluster` removed the external NATS dependency and
added an `embedded_nats.*` observability surface that already flows into
`Status` and structured logs.

**Feature 003 is purely an operator-experience layer**: a single statically-
linked binary `pgmctl` that consumes those existing surfaces, presents them
in human-readable, color-coded form, and supplies the missing ingredients
operators need to debug, repair, and audit a running cluster from one
terminal. It is **a client**, not a control-plane peer; it never embeds
NATS, never talks to PostgreSQL directly, never opens cluster-route ports.

Where the existing surfaces are insufficient (read-only event tailing,
granular doctor checks, decommission/restart semantics), this feature
expands the pgman-proxy control plane minimally rather than having
`pgmctl` reach into pg-manager internals or scrape NATS subjects
directly. Any such expansion is enumerated in the requirements below
and is scoped to **additive, MINOR-version** changes to the 001
control-plane contract (Constitution V). Structured-log retrieval is
intentionally **not** part of this expansion — operators consume the
proxy's structured-log stream via whatever host log sink they already
use (stdout/journald/syslog/log shipper).

This spec does not modify any 001 or 002 requirement. It does not amend
the constitution.

## Clarifications

### Session 2026-05-14

- Q: Where should the cluster store event and audit history that `pgmctl events --since …`, `pgmctl get audit --since …`, and the P1 dump rely on? → A: JetStream-backed durable stream — events are published to a JetStream stream with replication factor matching 002 FR-011a (R derived from declared cluster size); any peer can serve history queries; retention is a documented, operator-configurable window; survives single-peer restart up to that window.
- Q: Should the proxy de-duplicate mutating requests by client-supplied `request_id` so pgmctl can safely retry on transport failure? → A: No server-side dedup. pgmctl never retries any mutating operation on transport or timeout failure; the operator reconciles via `pgmctl get events --request-id <id>`. Mid-mutation ambiguity is surfaced explicitly to the operator, not papered over by client-side retry or server-side cached-result replay.
- Q: How does pgmctl reach per-peer slices (logs, embedded-NATS mesh state, configs) when FR-006 binds it to a single peer per invocation? → A: Single connection, server-side fan-out. pgmctl opens exactly one control-plane connection to one peer per invocation; that peer fans out per-peer slice requests to its siblings over the embedded NATS request/reply mesh and returns aggregated results. Unreachable siblings appear as per-slice errors in the aggregated response, never as a whole-request failure. pgmctl MUST NOT open control-plane connections to multiple peers in one invocation.
- Q: What does `pgmctl restart <node>` restart — the managed PostgreSQL, the pgman-proxy peer process, or both? → A: Both, with `--target` selector. `pgmctl restart <node> --target=postgres` (default) restarts pg-manager's managed PostgreSQL on the target node via the control-plane endpoint. `pgmctl restart <node> --target=proxy` restarts the pgman-proxy peer process itself; this is implemented as a privileged in-process self-terminate-and-supervisor-respawn flow (the proxy exits cleanly under SIGTERM after draining; the host supervisor — systemd / s6 / tini — restarts it). pgmctl MUST NOT shell into the host, MUST NOT invoke systemd directly, and MUST NOT assume any specific supervisor. The proxy peer self-restart endpoint is fail-closed when no supervisor is detected (proxy started without a supervisor parent) — pgmctl surfaces the refusal verbatim.
- Q: What server-side retention window should back `pgmctl logs --since …` and the dump's per-peer log slice? → A: **Removed from v1.** `pgmctl logs` is dropped from the requirement set; structured-log retrieval is out of scope. Operators read pgman-proxy's structured-log stream via whatever host-level log sink they already use (stdout capture, journald, syslog, log shipper). The dump artifact no longer carries a structured-log slice. The cluster's queryable post-mortem material in v1 is the event/audit history stream (FR-016a) plus the doctor-check battery plus per-peer Status/topology snapshots — that is the contract the post-mortem story (US2 P1) is now graded against.

## User Scenarios & Testing *(mandatory)*

### User Story 1 — One-glance cluster health for a paging on-call (Priority: P1)

A primary on-call SRE has just been paged for "writes failing on the
production cluster". They open a terminal, type `pgmctl status`, and need
to know within five seconds: is there a primary? is there a leader? is the
embedded NATS mesh whole? which nodes are healthy / fenced / failed? what
is replication lag on each standby? what is the most recent transition?
The answer must be readable without grepping, and the colours must convey
severity at a glance (green / yellow / red) for someone scanning the
screen under time pressure.

**Why this priority**: This is the day-zero use case the whole tool
exists to support. If `pgmctl status` does not give the on-call a
correct, color-coded, one-screen summary, every other capability is
secondary. It is the MVP slice — a tool that only printed this one view
would already be useful.

**Independent Test**: Against a running 3-peer cluster (real or
fixture), run `pgmctl status` and verify (a) the cluster name, leader,
and primary are correctly identified; (b) each peer's role, fence state,
and replication lag is shown; (c) the embedded-NATS mesh count is
shown; (d) colours render only when stdout is a TTY; (e) `--output
json` produces stable machine-parseable output that does not include
ANSI escapes; (f) the command completes inside `--timeout` (default 10s)
or fails with a clear connection error.

**Acceptance Scenarios**:

1. **Given** a healthy 3-peer cluster with one primary, two streaming
   standbys, and a meshed embedded-NATS cluster, **When** the operator
   runs `pgmctl status`, **Then** every node line renders in **green**,
   the cluster summary lines render in **green**, and the exit code is
   `0`.
2. **Given** a 3-peer cluster where one standby's replication lag exceeds
   the warn threshold but is below the fail threshold, **When** the
   operator runs `pgmctl status`, **Then** that node's lag column
   renders in **yellow**, the cluster summary remains green or yellow
   depending on whether quorum is intact, and the exit code is `0`.
3. **Given** a 3-peer cluster where a peer is in `failed` state or the
   leader is unknown, **When** the operator runs `pgmctl status`, **Then**
   the affected lines render in **red**, the cluster summary renders in
   **red**, and the exit code is non-zero (distinct from connection-
   failure exit codes).
4. **Given** the operator runs `pgmctl status --output json` in a
   pipeline (stdout not a TTY), **Then** output contains no ANSI escape
   sequences, schema is stable across patch releases, and a `jq`
   expression like `.cluster.leader.node_id` resolves without parse
   errors.
5. **Given** the operator's terminal does not support 256-color or
   `NO_COLOR` is set or `--no-color` is supplied, **When** the operator
   runs `pgmctl status`, **Then** output contains no ANSI escape
   sequences and severity is conveyed by an unambiguous text marker
   (e.g., `[OK]`, `[WARN]`, `[FAIL]`).
6. **Given** pgman-proxy is unreachable (refused / DNS / TLS error),
   **When** the operator runs `pgmctl status`, **Then** the tool exits
   with a documented "connection failure" exit code distinct from
   "cluster unhealthy", and the error message names the endpoint, the
   underlying cause, and the next thing to try.

---

### User Story 2 — One-shot full state dump for a post-mortem (Priority: P1)

A senior engineer is writing a post-mortem for a failover that did not
behave as expected. They need every scrap of state that was visible to
the cluster at the moment things went wrong — status snapshot, recent
state transitions, recent control-plane audit records, embedded-NATS
lifecycle event tail, replication slot inventory, current topology,
doctor-check output, and the proxy's effective configuration (redacted)
— all in **one** invocation. They must not need to remember which
subcommand prints which slice, and they must not need to ask anyone
running the cluster for "one more thing". (Structured-log retrieval
is intentionally out of scope per the 2026-05-14 clarification;
operators read the proxy's structured-log stream from the host's
existing log sink and attach that separately when needed.)

**Why this priority**: Post-mortem material is time-perishable; the
information must be captured in the moment, by anyone, without
specialised knowledge of which subcommand prints which slice. A tool
that requires five separate commands to assemble the post-mortem will,
in practice, miss one of them every time. This is the second slice the
tool earns its keep on.

**Independent Test**: Trigger a failover in a fixture cluster, then run
`pgmctl dump --output <path>`. Verify the resulting artifact contains
(a) the full `Status` snapshot, (b) the last N audited LCM operations
from the durable history stream (FR-016a), (c) the last N state-
transition events from the durable history stream, (d) the topology
snapshot, (e) the effective configuration with secrets redacted, (f)
the output of every built-in doctor check, (g) clock skew measurements
between the local host and each peer, (h) per-peer embedded-NATS mesh
state. The artifact must be a single file or a single directory tree
that can be attached to a ticket. (Structured-log slices are
intentionally out of scope per the 2026-05-14 clarification.)

**Acceptance Scenarios**:

1. **Given** a running cluster and an operator on any host that can
   reach pgman-proxy, **When** they run `pgmctl dump`, **Then** the tool
   produces a single self-contained artifact in the configured
   `--output` location whose contents are sufficient to reconstruct
   the cluster's observable state at the moment of capture — without
   the operator answering any prompts.
2. **Given** the operator runs `pgmctl dump` against a degraded cluster
   where one peer is unreachable, **When** the dump completes, **Then**
   the artifact still includes everything reachable, clearly marks the
   unreachable peer's slice as missing with the underlying error, and
   exits with a documented "partial dump" exit code distinct from a
   clean dump.
3. **Given** the dump contains configuration material, **When** the
   operator opens the artifact, **Then** every secret-bearing field is
   redacted using the same redaction rules pgman-proxy uses on
   `--print-config` and structured logs.
4. **Given** the operator runs `pgmctl dump --redact-level=strict`,
   **When** the dump completes, **Then** every host:port pair, every
   IP, every node ID is replaced with stable placeholders so the
   artifact can be attached to an external ticket or vendor.
5. **Given** the operator runs `pgmctl dump --output -`, **When**
   stdout is captured into a file, **Then** the tool emits a single
   tar stream (or equivalent single-stream format) suitable for
   shipping into a S3 bucket or pasting into a ticket attachment field.

---

### User Story 3 — Interactive doctor: find, propose, fix (Priority: P1)

An on-call SRE wants `pgmctl doctor` to be the single tool they reach
for when something is wrong but they do not yet know what. The tool must
run a battery of named checks against the cluster's reachable state,
report each check's outcome (`PASS` / `INFO` / `WARN` / `FAIL` /
`UNKNOWN`) with a one-line human explanation and, for failing checks,
a **named, narrow** suggested fix that is safe to apply with one
keystroke and is reversible or has a documented rollback. With
`--fix`, the tool walks each failing check, prompts before applying
its fix, and re-runs the check after applying to confirm progress.

**Why this priority**: Without a doctor mode, `pgmctl` is only a window
into state; with one, it is a tool. The doctor's value depends on the
checks being correct and the fixes being safe. P1 because the user
emphasised it as the centrepiece capability.

**Independent Test**: Induce three specific failure modes against a
fixture cluster — a stalled standby (no WAL advancement for >2 min), an
orphaned replication slot from a removed peer, and a fenced peer that
the operator forgot to unfence — then run `pgmctl doctor`. Verify each
failure surfaces as a distinct FAIL with a distinct, named, suggested
fix. Run `pgmctl doctor --fix -y` and verify each fix runs to
completion, the relevant check re-runs and converts to `PASS`, and the
final exit code is `0`.

**Acceptance Scenarios**:

1. **Given** a healthy cluster, **When** the operator runs
   `pgmctl doctor`, **Then** every built-in check (US-listed v1
   battery) returns `PASS` or `INFO`, the overall exit code is `0`, and
   nothing has been mutated.
2. **Given** a cluster where exactly one check fails, **When** the
   operator runs `pgmctl doctor`, **Then** that check renders in
   **red** with a named identifier (`replication.lag-acceptable`,
   `slots.no-orphans`, etc.), a one-line human explanation including
   the offending value, and either (a) a `suggested fix:` line
   describing a named remediation, or (b) `no-auto-fix: <reason>` when
   no safe automation exists. Other checks render in **green** or
   **yellow**.
3. **Given** the operator runs `pgmctl doctor --fix` against a cluster
   with one failing check that has a suggested fix, **When** the
   prompt appears, **Then** it states the check name, the proposed
   action, and the expected effect; **When** the operator answers `y`,
   **Then** the fix runs, the underlying check re-runs, and the new
   status is displayed (ideally PASS) before the tool exits.
4. **Given** the operator runs `pgmctl doctor --fix -y` against the
   same cluster, **When** the tool runs, **Then** every fix is applied
   without prompting; **Given** any fix would breach a documented
   cluster-affecting safety boundary (e.g., promoting a node implicitly),
   **Then** that fix is still skipped with a clear "use `--force` plus
   `--cluster=<name>` to opt in" message — `-y` never escalates a
   single-resource fix to a cluster-affecting one.
5. **Given** the operator runs `pgmctl doctor --check
   replication.lag-acceptable`, **When** the tool runs, **Then** only
   that check executes, only its result is reported, exit code reflects
   only that check's outcome, and no other check is run (so an
   automated CI dashboard can pin a specific check).
6. **Given** a check requires data the proxy cannot retrieve (host-
   level disk space when no metrics agent is configured), **When** the
   check runs, **Then** the outcome is `UNKNOWN` (not `FAIL` and not
   `PASS`) with a one-line explanation pointing at what would be
   needed; exit code reflects `UNKNOWN` as a documented value distinct
   from `FAIL`.
7. **Given** the operator runs `pgmctl doctor --output json`, **When**
   the tool runs, **Then** output is a stable, schema-versioned JSON
   document where each check is a top-level object keyed by check
   name, with `status`, `severity`, `message`, `evidence`, and an
   optional `suggested_fix.action` field — colours and ANSI escapes
   are suppressed.

---

### User Story 4 — Live streams for an active incident (Priority: P2)

During an active incident the operator wants to see things change in
real time: cluster status redrawn every second, every state transition
as it happens, every event as it is emitted, the state of one specific
node as it transitions. They are watching for a stall, a flap, or a
recovery signal — they do not want to refresh `pgmctl status` on
keypress, and they do not want to scrape the cluster's event firehose
by hand.

**Why this priority**: P2 because static commands cover the post-
incident slice; live commands cover the during-incident slice. Without
them, the operator is reduced to `watch -n1 pgmctl status` which is
flickery, expensive on the server, and loses transitions between polls.

**Independent Test**: Open `pgmctl watch status` in one terminal, then
in another terminal trigger a failover. The first terminal must render
each intermediate state (leader-lost, promoting, demoting, ready)
within one second of the corresponding transition reaching the proxy,
without redrawing on poll cycles when nothing has changed.

**Acceptance Scenarios**:

1. **Given** the operator runs `pgmctl watch status`, **When** the
   cluster state changes (any field renders differently), **Then** the
   screen updates within one second of the change reaching the proxy
   peer, only the changed portion is highlighted, and no flicker is
   introduced on unchanged ticks.
2. **Given** the operator runs `pgmctl watch transitions`, **When**
   any peer publishes a state transition, **Then** a single new line
   is appended in the format `TIME NODE FROM -> TO  reason="..."` and
   coloured per the destination state's severity.
3. **Given** the operator runs `pgmctl watch events`, **When** any
   pg-manager / pgman-proxy event is emitted (state transitions,
   leader changes, route up/down, fence/unfence, audit decisions),
   **Then** each event appears as a single line in the order the
   server emitted it; lost connection causes a single, clearly-marked
   gap line (not silent loss).
4. **Given** the operator runs `pgmctl watch node node-2`, **When**
   `node-2` transitions, **Then** the screen updates with the new
   state and a recent-transitions sparkline / table for that node
   only; other nodes are not shown.
5. **Given** the underlying watch stream from the proxy drops, **When**
   `pgmctl watch …` detects the drop, **Then** the tool reconnects
   with exponential backoff up to a documented ceiling, the reconnect
   attempts are visible on the status bar, and on permanent failure
   the tool exits non-zero with the underlying error.

---

### User Story 5 — Plain-English explain mode (Priority: P2)

A less experienced operator asks the cluster "why isn't failover
progressing?" or "why isn't node-3 promoting?" via
`pgmctl explain failover-stuck` / `pgmctl explain node-not-promoting
node-3`. The tool inspects current state, the recent transition log,
recent events, and the doctor check battery, and emits a paragraph of
plain-English explanation grounded in *observed facts from the cluster*
(not generic advice). It identifies the most likely cause, points at
the specific signals that support that diagnosis, and lists the next
investigative or remediation steps in priority order.

**Why this priority**: Less critical than status / dump / doctor, but
it is the bridge between observability and action for an operator who
does not yet have the cluster's failure-mode vocabulary internalised.
P2.

**Independent Test**: Stall a standby's WAL replay against a fixture
cluster, then run `pgmctl explain replication-broken <node>`. Verify
the output (a) names the affected node, (b) cites the specific check
that failed and the offending value, (c) reproduces the most relevant
event or transition record (with timestamps) from the history stream
(FR-016a), and (d) recommends two-to-three next actions ordered by
safety.

**Acceptance Scenarios**:

1. **Given** an operator runs `pgmctl explain <subject>` against any
   supported subject, **When** the tool runs, **Then** output is a
   structured human-readable narrative with a "Diagnosis", "Evidence",
   and "Suggested next steps" section, where the Evidence section
   quotes verbatim from transitions and audit records (with
   timestamps) drawn from the durable history stream (FR-016a) and
   the Suggested-next-steps section names concrete `pgmctl`
   invocations the operator can copy-paste.
2. **Given** the explain subject does not apply (`explain
   failover-stuck` on a cluster that is healthy and not failing over),
   **When** the tool runs, **Then** it emits "Subject does not apply
   right now: <one-line reason>" and exits non-zero with a documented
   "subject-not-applicable" exit code.
3. **Given** the operator runs `pgmctl explain --output json`,
   **When** the tool runs, **Then** the diagnosis is rendered as
   structured JSON suitable for piping into a chat/ticket integration.

---

### User Story 6 — Safe mutating operations with confirmation walls (Priority: P2)

An operator needs to fence a misbehaving standby, unfence a previously-
fenced node, change a hot-reloadable configuration value, fail over to a
specific peer, switch over to a planned target, promote the local peer,
restart pg-manager-managed PostgreSQL on a node, or decommission a peer
from the cluster. Each operation is dangerous in proportion to its
blast radius — and the confirmation experience must reflect that
proportion: per-resource ops prompt for `y/N` with the operation and
target shown; cluster-affecting ops require typing the cluster name
verbatim. The tool must *never* execute a cluster-affecting operation
just because `-y` was passed; `-y` only skips per-resource prompts.

**Why this priority**: Mutating capabilities are what makes the tool a
day-2 operations console, not a read-only viewer. P2 because the read-
only slice already delivers most of the day-1 value; mutating ops are
where mistakes have the largest blast radius and therefore should land
later, with the safety scaffolding well-tested.

**Independent Test**: Run every mutating subcommand against a fixture
cluster, both without a confirmation override and with the matching
override flag. Verify that (a) without override, the tool reads from
stdin and aborts on anything other than the documented acceptance
token, (b) with the matching override flag, the operation proceeds
without prompting, (c) cluster-affecting operations refuse `-y` alone
without `--force` plus a `--cluster=<name>` match, and (d) every
operation, whether accepted or rejected, produces a single audit-
trail line on the server side via the existing 001 audit pipeline.

**Acceptance Scenarios**:

1. **Given** the operator runs `pgmctl fence node-2`, **When** stdin
   is a TTY, **Then** the tool prints "About to FENCE node-2 in
   cluster <name>. Continue? [y/N]:", reads one line, and aborts on
   anything other than `y` / `yes`.
2. **Given** the operator runs `pgmctl fence node-2 --yes`, **When**
   the tool runs, **Then** it skips the prompt and submits the
   operation; **When** `--yes` is supplied without a TTY (pipeline
   context), it is honoured.
3. **Given** the operator runs `pgmctl failover --target node-3`,
   **When** stdin is a TTY, **Then** the tool prints "About to
   FAILOVER cluster <name> to node-3. Type the cluster name to
   confirm:", reads one line, and aborts unless the line exactly
   matches `<name>`.
4. **Given** the operator runs `pgmctl failover --target node-3
   --force --cluster <name>`, **When** the tool runs, **Then** it
   skips the typed-name prompt; **Given** `--force` is supplied
   without `--cluster <name>` matching the live cluster, **Then** the
   tool aborts non-zero.
5. **Given** the operator runs any mutating subcommand and the
   server returns `audit_unavailable` per 001 FR-028, **Then** the
   tool exits non-zero, surfaces the underlying error, and reports
   that the operation was refused (not silently ignored).
6. **Given** the operator runs `pgmctl set-config <key>=<value>` for
   a key that is **not** in the hot-reload allow-list (peer routes
   list, cluster password handle per 002 FR-014a), **Then** the tool
   refuses the request before sending it and names the keys that
   *are* in the allow-list.
7. **Given** the operator runs `pgmctl restart <node>`, **When** the
   tool runs, **Then** the named target is interpreted as
   "restart pg-manager's managed PostgreSQL on this node" and is
   gated by the same cluster-name typed-confirmation as failover.
8. **Given** the operator runs `pgmctl delete <node>`, **When** the
   tool runs, **Then** the named target is interpreted as
   "decommission this peer from the topology" (cluster-affecting,
   typed cluster-name confirmation) and is wired to the server's
   topology-update path; it does **not** delete files or kill
   processes on the target host.

---

### Edge Cases

- **Multiple proxies in different clusters**: The operator can declare
  multiple endpoints in a config file with named contexts (analogous to
  kubeconfig). `pgmctl --context prod-east status`, `pgmctl config
  use-context prod-east`. The default context can be overridden by
  `--endpoint <url>` (single-shot) without writing config. Unset
  endpoint with no context → exit `EX_CONFIG` (78) with a clear "no
  endpoint configured" message and an example of how to set one.
- **`pgmctl` and `pgman-proxy` version skew**: The tool MUST announce
  the server's reported version on every connection and warn (yellow)
  on minor-skew, refuse on major-skew, with `--insecure-skip-version-
  check` to override. Cross-major operation is not supported in v1.
- **Watch streams against a peer that loses quorum mid-stream**: The
  stream MUST surface a single, clearly-marked "stream degraded:
  reason=<...>" line and reconnect; it MUST NOT silently lose
  transitions. If the peer permanently fails, the tool exits non-zero.
- **Doctor check that mutates state without `--fix`**: Forbidden. Every
  check must be read-only. Any check that would require a probe-style
  mutation (e.g., write+read a heartbeat row to test write path) MUST
  be opt-in behind `--allow-write-probes` and clearly named.
- **Confirmation prompt while stdin is not a TTY and `-y` / `--force`
  is not supplied**: Refuse the operation, exit non-zero, name the
  flag needed. Never assume "y" in a pipeline.
- **`pgmctl dump` against a partially-reachable cluster**: Capture
  everything reachable, mark unreachable peers' slices clearly, and
  exit with the documented "partial dump" code. Never write an empty
  dump silently.
- **Color in a non-color terminal**: Detect via `isatty(fileno(stdout))`,
  `TERM`, and `NO_COLOR` (per `no-color.org` convention); render
  text-marker fallback (`[OK]`/`[WARN]`/`[FAIL]`). `--no-color`
  unconditionally suppresses ANSI.
- **A doctor fix that requires a host-level action `pgmctl` cannot
  perform** (e.g., grow the WAL disk): The fix's `action` classification
  is `advisory` and `--fix` does NOT attempt to execute it — it prints
  the recommended command for the operator to run on the host.
- **Network timeout mid-mutation**: The tool MUST surface the
  ambiguity ("the operation was sent but its outcome is unknown") and
  recommend `pgmctl get events --request-id <id>` to reconcile.
  Never silently retry a mutating operation on timeout.
- **TLS to the control plane on a non-loopback bind**: 001 FR-033
  requires TLS on non-loopback control-plane binds. `pgmctl` MUST
  honour TLS verification by default; `--insecure-skip-tls-verify`
  is a documented escape hatch that warns loudly.
- **No bearer token configured**: Refuse to invoke any operation; print
  the documented sources (`PGMCTL_TOKEN` env, configured token-source
  in the active context), exit `EX_CONFIG`.

## Requirements *(mandatory)*

### Functional Requirements

#### Distribution and shape

- **FR-001**: System MUST ship as a single statically-linked binary named
  `pgmctl`, built from the same module as `pgman-proxy` so the two stay
  version-locked. The binary MUST run on `linux/amd64` and
  `linux/arm64`; `darwin/amd64` and `darwin/arm64` SHOULD be built for
  developer ergonomics. Windows is out of scope for v1.
- **FR-002**: Subcommands MUST follow a verb-noun structure consistent
  with the user's stated surface — `status`, `get`, `list`, `describe`,
  `watch`, `events`, `topology`, `health`, `lag`, `explain`,
  `doctor`, `dump`, `fence`, `unfence`, `set-config`, `failover`,
  `switchover`, `promote`, `restart`, `delete`, `version`, `config`.
  `logs` is intentionally not provided — structured-log retrieval
  is out of scope for v1 (operators consume the proxy's structured-
  log stream via the host's existing log sink). Unknown subcommands
  MUST exit `EX_USAGE` (64) with a "did you mean" hint.
- **FR-003**: System MUST provide shell-completion scripts for `bash`,
  `zsh`, and `fish` via `pgmctl completion <shell>`.

#### Global flags

- **FR-004**: System MUST accept these global flags on every subcommand:
  - `--output`, `-o` — one of `table` (default), `json`, `yaml`, `wide`.
  - `--no-color` — suppress ANSI escapes unconditionally.
  - `--quiet`, `-q` — suppress non-essential output (no banners, no
    timing summaries); only the requested data is emitted.
  - `--verbose`, `-v` — increase verbosity; repeatable (`-vv`, `-vvv`)
    to add request/response logging, then full protocol logging.
  - `--timeout <duration>` — overall command timeout (default 10s).
  - `--yes`, `-y` — skip confirmation prompts on **single-resource**
    mutating operations; MUST NOT escalate to cluster-affecting ops.
  - `--force` — skip the typed-cluster-name confirmation on
    **cluster-affecting** operations; MUST require a matching
    `--cluster <name>` to take effect.
  - `--endpoint <url>` — single-shot proxy endpoint override.
  - `--context <name>` — pick a configured context.
  - `--cluster <name>` — pin the expected cluster name (validated
    against the server's reported cluster id before any cluster-
    affecting operation).
  - `--insecure-skip-tls-verify` — disable TLS verification (warned).
  - `--insecure-skip-version-check` — proceed despite minor / major
    skew between client and server.
- **FR-005**: `--quiet` and `--verbose` MUST be mutually exclusive on
  the same invocation. `--no-color` MUST take precedence over any
  setting that would emit colour.

#### Connectivity, auth, configuration

- **FR-006**: System MUST connect to **exactly one** pgman-proxy peer
  per invocation over HTTPS by default; opening control-plane
  connections to additional peers in the same invocation is
  forbidden. The endpoint is resolved in this precedence order:
  `--endpoint` flag, `--context` flag, current context from config
  file, `PGMCTL_ENDPOINT` env, error.
- **FR-006a**: For any command whose result depends on data held by
  peers other than the connected one (per-peer embedded-NATS mesh
  state, per-peer configuration, per-peer doctor checks, the full
  state dump), the **connected peer** MUST fan out
  to its siblings over the embedded NATS request/reply mesh and
  return an aggregated response. Per-sibling failures (unreachable,
  timeout, auth) MUST appear as per-slice errors inside the
  aggregated response, with the slice's `status` field set to
  `partial` or `failed` and the underlying error captured verbatim;
  a per-sibling failure MUST NOT cause the whole request to fail.
  The fan-out request/reply protocol is an additive MINOR-version
  change to the 001 contract and is wired in `/speckit-plan`
  Phase 1.
- **FR-007**: System MUST support a kubeconfig-style configuration
  file at a documented location (default
  `$XDG_CONFIG_HOME/pgmctl/config.yaml`, falling back to
  `~/.config/pgmctl/config.yaml`) that defines named contexts (each
  with endpoint URL, bearer-token source, TLS material, optional
  cluster name). `pgmctl config view`, `pgmctl config use-context
  <name>`, `pgmctl config set-context …`, `pgmctl config
  delete-context …` are the four required management subcommands.
- **FR-008**: System MUST authenticate to the pgman-proxy control
  plane using the existing bearer-token scheme (001 FR-024). Tokens
  MUST be sourced via env var name, file path, or operator-supplied
  command — never stored inline in the config file (consistent with
  the 001 secrets policy).
- **FR-009**: System MUST NOT log, print, dump, or otherwise emit the
  plaintext bearer token in any output mode (status, dump, verbose,
  trace). The token MAY be referenced by source identifier in
  diagnostic output.
- **FR-010**: System MUST validate the server's announced cluster id
  against `--cluster <name>` before invoking any cluster-affecting
  operation, even when `--force` is supplied. Mismatch → exit non-zero
  before any request is sent.

#### Read-only data surface

- **FR-011**: `pgmctl status` MUST render a compact cluster summary
  including: cluster id, primary node, leader node, peer count, mesh
  count, per-peer (node id, role, fence state, replication lag, last
  transition timestamp), embedded-NATS state per peer, control-plane
  reachability, time-of-snapshot. The summary MUST fit on one
  terminal screen for the common 3-peer case at 80×24.
- **FR-012**: `pgmctl get <resource>` and `pgmctl list <resource>`
  MUST support, at minimum, these resource kinds: `nodes`, `peers`,
  `slots` (replication slots), `events`, `audit`, `topology`,
  `config` (effective merged proxy config, redacted), `version`.
  `describe <resource>/<name>` MUST emit the verbose form of `get`.
  `events` and `audit` query a **durable, JetStream-backed history
  stream** maintained by the cluster (FR-016a), so historical queries
  succeed across single-peer restarts up to the configured retention
  window. Other resources reflect the current state of the connected
  peer.
- **FR-013**: `pgmctl topology` MUST render the cluster topology
  (peers, their roles, their slots, their advertised endpoints) as a
  human-readable tree by default and as JSON/YAML on `-o`.
- **FR-014**: `pgmctl health` MUST emit a one-line per-component
  rollup (`control-plane: OK`, `embedded-nats: OK`, `primary: OK`,
  `quorum: OK`, `replication: WARN — node-3 lag 200MB`) suitable for
  use as the body of a higher-level monitor's status check.
- **FR-015**: `pgmctl lag` MUST render per-standby replication lag in
  bytes and in time (where derivable from WAL replay rate), with
  thresholds for warn / fail driven by `--warn` / `--fail` flags or
  cluster-side defaults.
- **FR-016**: `pgmctl events [--since <duration>] [--node <id>]
  [--type <kind>]` MUST stream the cluster's event history (state
  transitions, leader changes, fence/unfence, audit decisions, NATS
  lifecycle) in chronological order, with each event rendered as a
  single line in the documented `TIME TYPE NODE DETAILS` form.
- **FR-016a**: System MUST persist the event and audit history to a
  **JetStream-backed durable stream** carried by the embedded NATS
  cluster, replicated at the same factor 002 FR-011a derives for the
  leadership KV (R=1 / R=2 / R=3 by declared cluster size). Retention
  MUST be operator-configurable as a time window (default: `24h`)
  and a size bound (default: `256 MiB`), with the smaller of the
  two governing. Any peer the operator connects pgmctl to MUST be
  able to serve history queries up to the retention boundary — a
  single-peer restart MUST NOT lose history within the window.
  Event-stream gaps (e.g., quorum loss during a partition) MUST be
  surfaced to pgmctl as a structured gap marker, never silently
  dropped. The history stream's existence and configuration is an
  additive MINOR-version change to the 001 contract.
- **FR-017**: *(intentionally removed in clarification 2026-05-14;
  structured-log retrieval is out of scope for v1)*
- **FR-018**: `pgmctl explain <subject> [<arg>]` MUST emit a
  diagnosis with three named sections — Diagnosis, Evidence, Suggested
  next steps — for the documented v1 subject set: `failover-stuck`,
  `node-not-promoting <node>`, `replication-broken <node>`,
  `leader-election`, `current-state`, `last-event`. Subjects beyond
  this set MUST exit `EX_USAGE` with the list of supported subjects.

#### Watch streams

- **FR-019**: `pgmctl watch status` MUST redraw the screen within one
  second of the server reporting a state change, with only the
  changed cells redrawn and unchanged cells left intact (no flicker).
- **FR-020**: `pgmctl watch transitions`, `pgmctl watch events`,
  `pgmctl watch node <id>` MUST be append-only streams; lost
  connection inserts a clearly-marked gap line and the client
  reconnects with exponential backoff up to a documented ceiling.
- **FR-021**: Watch streams MUST be plumbed through a server-side
  push transport (server-sent events or equivalent long-lived HTTP)
  exposed on the existing control-plane listener — `pgmctl` MUST NOT
  subscribe to NATS subjects directly. New control-plane endpoints
  required to satisfy this requirement are additive minor-version
  changes to the 001 contract (Constitution V).

#### Doctor

- **FR-022**: `pgmctl doctor` MUST run, by default, the v1 built-in
  battery: `cluster.has-primary`, `cluster.has-leader`,
  `cluster.quorum`, `nodes.all-reachable`, `nodes.no-failed-state`,
  `replication.all-streaming`, `replication.lag-acceptable`,
  `replication.no-wal-gaps`, `slots.no-orphans`, `slots.not-bloated`,
  `disk.has-space`, `disk.wal-not-filling`, `clock.skew-acceptable`,
  `postgres.responding`, `postgres.version-consistent`,
  `tls.certs-valid`, `backups.recent`, `backups.verifiable`. Checks
  MUST be discoverable via `pgmctl doctor --list`.
- **FR-023**: Each doctor check MUST yield exactly one of `PASS`,
  `INFO`, `WARN`, `FAIL`, `UNKNOWN`, a one-line human message
  carrying the offending value when applicable, an `evidence` slice
  (raw values the check inspected), and, where automation is safe,
  a `suggested_fix` object with `action_name`, `description`, and
  `blast_radius` (one of `single-resource`, `cluster-affecting`,
  `advisory`).
- **FR-024**: `pgmctl doctor --check <name>` MUST run only the named
  check; exit code reflects only that check's outcome.
- **FR-025**: `pgmctl doctor --fix` MUST iterate the failing checks
  whose `suggested_fix.action_name` is non-empty, prompt before
  applying each, re-run the underlying check after applying, and
  report each fix's outcome (`applied`, `skipped`, `failed`,
  `still-failing`). With `-y`, single-resource fixes apply without
  prompting; cluster-affecting fixes still require the typed
  cluster-name flow (FR-029). `advisory` fixes are NEVER auto-applied.
- **FR-026**: The doctor check battery and the set of named fixes
  are server-driven: `pgmctl` MUST query the proxy for the available
  checks and fixes rather than hard-coding them, so adding a new
  check on the server side does not require a `pgmctl` release. New
  control-plane endpoints required to satisfy this requirement are
  additive minor-version changes (Constitution V).
- **FR-027**: System MUST refuse to run any check that would mutate
  cluster state unless invoked under `--allow-write-probes`. Default
  checks are read-only.

#### Mutating operations — per-resource (single-resource blast radius)

- **FR-028**: `pgmctl fence <node>`, `pgmctl unfence <node>`,
  `pgmctl set-config <key>=<value>` MUST be classified as
  single-resource mutating operations. Each MUST prompt for `[y/N]`
  before sending the request unless `--yes` is supplied. The prompt
  MUST name the operation, the target, and the cluster id.
  `set-config` MUST only accept keys in the server-published
  hot-reload allow-list (002 FR-014a); other keys MUST be refused
  client-side with a clear error naming the keys that ARE allowed.

#### Mutating operations — cluster-affecting (cluster blast radius)

- **FR-029**: `pgmctl failover`, `pgmctl switchover`, `pgmctl
  promote`, `pgmctl restart <node>`, `pgmctl delete <node>` MUST be
  classified as cluster-affecting. Each MUST require the operator
  to type the live cluster name verbatim before sending the request
  unless `--force --cluster <name>` is supplied AND `<name>` matches
  the cluster's reported id. `-y` alone MUST NOT bypass this
  confirmation. The prompt MUST state the cluster id, the operation,
  the target (where applicable), and the expected blast radius in
  plain English.
- **FR-030**: `pgmctl delete <node>` MUST be implemented as a
  topology-update operation (decommission peer) routed through 001's
  `UpdateTopology` LCM op. It MUST NOT remove files, kill processes,
  or otherwise touch the target host directly.
- **FR-031**: `pgmctl restart <node>` MUST accept a `--target`
  selector with two values: `postgres` (default) and `proxy`. Both
  values are cluster-affecting and gated by the typed-cluster-name
  confirmation (FR-029).
- **FR-031a**: `pgmctl restart <node> --target=postgres` MUST be
  implemented as a request to pg-manager's managed-process restart
  pathway via the control plane; `pgmctl` MUST NOT shell into the
  target host.
- **FR-031b**: `pgmctl restart <node> --target=proxy` MUST be
  implemented as a privileged in-process **self-terminate** flow on
  the target peer: the peer drains in-flight data-plane and
  control-plane work (within 001's shutdown budget per FR-014),
  emits its `shutdown` lifecycle event with `reason=operator_restart`,
  exits with a documented exit code, and relies on the host's PID-1
  supervisor (systemd / s6 / tini / docker / kubernetes) to respawn
  the process. `pgmctl` MUST NOT invoke systemd, MUST NOT assume a
  specific supervisor, and MUST NOT shell into the target host.
- **FR-031c**: The `--target=proxy` self-terminate endpoint MUST
  fail closed when the peer has no detectable supervisor (e.g., the
  peer was started directly from a developer shell with no PID-1
  respawner). The proxy MUST detect supervisor presence at startup
  (parent-PID heuristic + documented config knob to override) and
  refuse `--target=proxy` requests when supervision is absent;
  `pgmctl` MUST surface the refusal verbatim with the documented
  remediation ("re-run under systemd/s6/tini, or set
  `proxy.assume_supervised: true` to opt into self-terminate at
  the operator's own risk").
- New control-plane endpoints required by FR-031a / FR-031b are
  additive minor-version changes (Constitution V).

#### Full state dump

- **FR-032**: `pgmctl dump` MUST capture, in a single artifact,
  every slice listed in US2's independent test, fetched in parallel
  with a per-slice timeout (default 10s) and an overall timeout
  driven by `--timeout` (default 60s for dump). Unreachable slices
  are recorded as missing entries with their underlying error; the
  dump MUST NOT block indefinitely on a stalled slice.
- **FR-033**: `pgmctl dump` MUST redact secrets using the same
  redaction rules pgman-proxy uses on `--print-config` and audit
  records; secrets MUST NOT leak even at `-vvv`. With
  `--redact-level=strict`, host:port pairs, IPs, and node IDs are
  replaced with stable, deterministic placeholders so the artifact
  is shareable externally.
- **FR-034**: `pgmctl dump` MUST emit a single tar stream when
  `--output -` is used so the dump can be piped to cloud storage or
  to a ticket attachment field without manifest gymnastics.
- **FR-035**: `pgmctl dump` MUST embed, alongside the captured
  data, a manifest naming (a) the pgmctl version and build commit,
  (b) the pgman-proxy peer version and build commit observed at
  dump time, (c) the wall-clock time the dump started and ended,
  (d) the set of slices that succeeded, partially-succeeded, or
  failed, with per-slice durations.

#### Output, color, and exit codes

- **FR-036**: Color rendering MUST follow the convention: green =
  PASS / healthy, yellow = INFO / WARN / attention, red = FAIL /
  unreachable. Color MUST be auto-disabled when stdout is not a TTY,
  when `--no-color` is supplied, or when `NO_COLOR` is set
  (per `no-color.org`).
- **FR-037**: Non-zero outcomes MUST use a stable, documented exit
  code table — at minimum: `0` clean success; `1` non-failure
  warnings present and `--strict` was supplied (otherwise `0`);
  `2` cluster unhealthy / check FAIL; `3` cluster partially reachable
  (partial dump); `64` `EX_USAGE` (bad subcommand / flag);
  `65` connectivity / TLS / auth failure (distinct from "cluster
  unhealthy"); `78` `EX_CONFIG` (missing endpoint, malformed config
  file); `124` overall `--timeout` exceeded.
- **FR-038**: JSON / YAML output MUST be schema-versioned at the
  document root (e.g., `apiVersion: pgmctl/v1`) so downstream
  automation can pin to a version and reject unknown ones.

#### Audit and traceability

- **FR-039**: Every mutating operation `pgmctl` initiates MUST be
  attributable in the server's audit log to the user who ran it.
  `pgmctl` MUST propagate an operator identifier — derived from the
  bearer token's `actor` per 001 — and a `request_id` (ULID) on every
  request; the `request_id` MUST be printed on stdout for the
  operator to cross-reference with the server's audit pipeline.
  `pgmctl` MUST NOT retry mutating operations on transport,
  handshake, send-side, or read-side timeout failure; the
  `request_id` exists for operator reconciliation, not for client
  retry. The proxy MUST NOT de-duplicate mutating requests by
  `request_id`: a re-sent request with the same id is treated as a
  new operation (a second request_id should be generated for any
  intentional re-submission). Read-only operations MAY be retried
  by the client on transport failure.
- **FR-040**: System MUST honour the server's `audit_unavailable`
  fail-closed contract (001 FR-028): when audit emission is
  unavailable on the server, mutating ops are refused; `pgmctl` MUST
  surface that refusal verbatim, not retry, and not downgrade.

### Key Entities

- **Context** — A named connection profile in the pgmctl configuration
  file binding an endpoint URL, a bearer-token source, optional TLS
  trust material, and an optional `expected-cluster` value. Multiple
  contexts may be defined; one is current at any time. Conceptually
  analogous to a kubeconfig context.
- **Doctor check** — A server-published, named, read-only inspection
  with a documented identifier (e.g., `replication.lag-acceptable`),
  a severity, a one-line human message, an evidence payload, and an
  optional `suggested_fix` reference. Defined server-side (FR-026);
  pgmctl discovers and renders.
- **Suggested fix** — A server-published, named action with a blast-
  radius classification (`single-resource`, `cluster-affecting`,
  `advisory`) and a description. Applied via `pgmctl doctor --fix`
  subject to the confirmation rules of that blast-radius class.
- **Watch stream** — A long-lived HTTP server-sent-event subscription
  to a server-side event source (status snapshot, transition log,
  event log, per-node state). Reconnects on drop with exponential
  backoff, surfaces gap markers on partial loss.
- **Dump artifact** — A single self-contained file (or single tar
  stream) bundling status snapshot, recent transitions, recent
  events and audit records (drawn from the FR-016a durable history
  stream), topology, redacted config, per-node clock-skew
  measurements, per-node NATS mesh state, doctor-check battery
  output, and a manifest of slice outcomes and timings. Structured
  proxy logs are intentionally excluded (out of scope for v1 per
  the 2026-05-14 clarification).
- **Severity** — One of `PASS`, `INFO`, `WARN`, `FAIL`, `UNKNOWN`
  for doctor checks; rendered as green / yellow / yellow / red /
  yellow respectively when color is enabled; `[OK]` / `[INFO]` /
  `[WARN]` / `[FAIL]` / `[UNKNOWN]` markers otherwise.
- **Output format** — One of `table` (default human view), `json`,
  `yaml`, `wide` (more columns; same shape as table). All non-table
  formats are schema-versioned (FR-038).
- **Confirmation class** — Either *single-resource* (prompt `[y/N]`;
  `-y` bypasses) or *cluster-affecting* (type cluster name verbatim;
  `--force --cluster <name>` bypasses, with name match enforced).
  `advisory` doctor fixes are never auto-applied.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: From cold start (no shell history, no cached state) a
  paged on-call SRE can run `pgmctl status` and identify the
  cluster's leader, primary, and any unhealthy node in **under
  10 seconds** of wall-clock time, on a 3-peer cluster, with no prior
  `pgmctl` experience beyond reading `pgmctl --help`. Verified by a
  scripted operator-task fixture.
- **SC-002**: `pgmctl status` against a healthy 3-peer cluster
  returns within **1.5 seconds** p95 over a healthy network on a
  developer laptop (excluding TLS handshake reuse). Verified in CI
  integration tests.
- **SC-003**: `pgmctl dump` against a healthy 3-peer cluster
  completes within **15 seconds** p95 and produces an artifact
  smaller than 10 MiB on a steady-state cluster. Against a partially-
  reachable cluster, dump completes within **`--timeout`** (default
  60s) and exits with the partial-dump code documented in FR-037.
- **SC-004**: 100% of the v1 doctor checks listed in FR-022 are
  implemented, named, and documented with a one-line human message
  and an `evidence` payload; checks that have no safe automated fix
  yet are explicitly classified as `advisory` so an operator never
  encounters an "applied?" prompt that cannot be honoured.
- **SC-005**: A v1 `pgmctl` binary running against an N-1 minor
  version of `pgman-proxy` produces a single, clearly-rendered
  version-skew warning and continues to render every read-only
  subcommand without crashing on unknown fields. Cross-major
  combinations refuse to proceed with a documented exit code.
- **SC-006**: Every mutating subcommand has a corresponding
  integration test that asserts (a) the prompt's wording, (b) the
  default-N behaviour, (c) the `-y` / `--force --cluster <name>`
  override behaviour, (d) the rejection of `--yes` for cluster-
  affecting ops, (e) the rejection of `--force` without a matching
  `--cluster <name>`, (f) the audit record on the server side. No
  mutating subcommand ships without all six.
- **SC-007**: Output across `--output json` and `--output yaml`
  conforms to a schema-versioned, documented contract; an automated
  test parses the output and asserts a fixed schema on every CI run.
  Schema changes are MINOR-version events per Constitution V.
- **SC-008**: Color rendering: against a fixed fixture, golden-file
  tests assert that PASS rows render in green, WARN rows render in
  yellow, FAIL rows render in red, and zero ANSI escapes appear
  under `--no-color`, `NO_COLOR=1`, or a non-TTY stdout. No
  exceptions.
- **SC-009**: `pgmctl watch status` redraws within **1 second** p95
  of a server-observed state change in a fixture cluster, with no
  redraw at all on idle ticks (no-op poll). CPU usage on a 3-peer
  steady-state fixture stays below **1%** of one core p95 on a
  developer laptop.
- **SC-010**: Doctor mode achieves a **≥90% true-positive rate**
  against the documented v1 failure-mode fixture set (stalled
  standby, orphaned slot, lingering fence, disk-fill simulation,
  clock-skew injection, version-mismatch injection) and a **≤5%
  false-positive rate** on a healthy baseline fixture, measured on
  the CI fixture suite. Below those bars the check ships in
  `advisory` blast-radius only.

## Assumptions

- **Bearer-token reuse, no new auth scheme**: `pgmctl` consumes the
  existing 001 control-plane bearer-token model (FR-024). Per-user
  RBAC, mTLS, and SSO are explicitly out of scope for v1.
- **HTTPS as the transport, JSON as the wire**: The control plane is
  HTTP+JSON (001 FR-024). Watch streams use server-sent events over
  the same listener (no separate gRPC surface in v1).
- **Server-side surface expansion is allowed and additive**: This
  feature requires the following minor, additive server-side
  expansions of the 001 control-plane contract: (1) server-sent-
  event stream endpoints for status / transitions / events /
  per-node, (2) a doctor-check / doctor-fix discovery and execution
  endpoint, (3) a managed-PostgreSQL restart endpoint (FR-031a) and
  a peer self-terminate endpoint (FR-031b/c) for
  `pgmctl restart --target=postgres|proxy`, (4) a JetStream-backed
  durable history stream for events and audit records (FR-016a)
  with a query endpoint that supports `--since` / `--type` /
  `--node` filtering, and (5) an inter-peer request/reply fan-out
  subject set on the embedded NATS mesh (FR-006a) so the connected
  peer can aggregate per-peer slices. Each lands as a MINOR-version
  change to the 001 contract and is wired in `/speckit-plan`
  Phase 1. A logs-tail endpoint is intentionally **not** in this
  set — `pgmctl logs` was removed from v1 per the 2026-05-14
  clarification.
- **Doctor checks are server-driven**: The catalogue of checks and
  the catalogue of named fixes live on the server, not in `pgmctl`.
  This keeps the operator binary stable while the check battery
  evolves and ensures a single source of truth.
- **Replication-lag thresholds default from cluster metadata**: The
  warn / fail bytes-and-time thresholds default from a value
  published by the proxy (operator-configurable) and may be
  overridden per-invocation with `--warn` / `--fail`.
- **`restart` covers two targets, selectable with `--target`**:
  `--target=postgres` (default) restarts pg-manager's managed
  PostgreSQL process on the node; `--target=proxy` restarts the
  pgman-proxy peer process itself via a privileged self-terminate
  flow that relies on the host's PID-1 supervisor to respawn (no
  systemd/shell access from pgmctl). Host-reboot remains out of
  scope. Naming consistency with `kubectl rollout restart` is
  intentional for the postgres target.
- **`delete` means "decommission this peer from the topology" via
  `UpdateTopology`**: Not "delete files", not "kill the process".
  Cluster-affecting confirmation applies.
- **`set-config` is bounded to the hot-reload allow-list**:
  Per-002 FR-014a, only peer-routes list and cluster password handle
  are reloadable today. Other keys are startup-only and `pgmctl
  set-config` refuses them client-side, naming the allow-list.
- **Multi-context configuration uses a kubeconfig-style file at a
  documented location**: `$XDG_CONFIG_HOME/pgmctl/config.yaml`
  (falling back to `~/.config/pgmctl/config.yaml`). Tokens are sourced,
  not inlined, consistent with the 001 secrets policy.
- **Watch redraw rate is one second**: This is a deliberate ceiling
  on the human-attention budget; finer granularity is invisible to
  an operator under stress. Watch transport pushes only on change
  so steady-state CPU stays low.
- **No bundled UI, no daemon, no plug-in system**: `pgmctl` is a
  single static binary with built-in subcommands. A `kubectl plugin`-
  style extension model is intentionally not included in v1.
- **No emoji in output by default**: Colour and bracket markers are
  the conveyance channels; emoji are not used. Consistent with the
  rest of the project's tooling.
- **All other feature 001 and 002 assumptions carry forward**: The
  proxy continues to fail-closed on audit-unavailable, the embedded
  NATS continues to be invisible to operators, etc.
