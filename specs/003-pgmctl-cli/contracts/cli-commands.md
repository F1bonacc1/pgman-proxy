# Contract — `pgmctl` Command Surface

**Feature**: `003-pgmctl-cli` · **Phase**: 1 · **Date**: 2026-05-14
**Status**: Locked at `/speckit-plan` time. Flag renames, exit-code
remappings, and JSON/YAML schema renames are MINOR-version events
(Constitution V).

---

## Synopsis

```text
pgmctl [global-flags] <command> [command-flags] [args...]
```

`pgmctl` runs in the foreground, prints to stdout/stderr, and exits
non-zero on any failure per `data-model.md § ExitCode`.

## Global flags

| Flag | Type | Default | Notes |
|---|---|---|---|
| `--output`, `-o` | enum | `table` | `table` / `json` / `yaml` / `wide`. |
| `--no-color` | bool | `false` | Suppresses ANSI unconditionally. |
| `--quiet`, `-q` | bool | `false` | Suppress non-essential output. |
| `--verbose`, `-v` | count | `0` | Repeatable: `-v`, `-vv`, `-vvv`. |
| `--timeout` | duration | `10s` | Overall command timeout. |
| `--yes`, `-y` | bool | `false` | Skip single-resource prompts. |
| `--force` | bool | `false` | Skip cluster-name prompt (requires `--cluster <name>`). |
| `--endpoint` | URL | — | Single-shot endpoint override. |
| `--context` | string | — | Configured context name. |
| `--cluster` | string | — | Expected cluster id; pinned before any cluster-affecting op. |
| `--insecure-skip-tls-verify` | bool | `false` | Warned when `true`. |
| `--insecure-skip-version-check` | bool | `false` | Allows minor / major skew. |
| `--strict` | bool | `false` | Treat WARN as non-zero exit. |

`--quiet` and `--verbose` are mutually exclusive (FR-005). Unknown flags
exit `EX_USAGE` (64). Positional args where none expected exit `EX_USAGE`.

---

## Read-only commands

### `pgmctl status`

Renders a one-screen cluster summary (FR-011). Calls
`GET /v1/status` (existing 001) once; renders the response with the 002
`embedded_nats.*` sub-block included.

**Output shape (table)**:

```text
Cluster: prod-east                                Snapshot: 13:42:09Z
Leader:  node-1   Primary: node-1   Peers: 3/3 reachable
Mesh:    3 routes meshed  ·  embedded NATS: OK on every peer

NODE     ROLE       FENCE   LAG       LAST TRANSITION
node-1   primary    -       -         13:00:11Z  start
node-2   standby    -       8 KB      12:59:47Z  attach
node-3   standby    -       6.4 MB!   12:59:55Z  attach
```

Color: green when every line is OK; yellow when at least one node is
WARN; red when any FAIL / unknown leader / no primary.

**JSON shape**:

```jsonc
{
  "apiVersion": "pgmctl/v1",
  "kind": "ClusterStatus",
  "captured_at": "2026-05-14T13:42:09Z",
  "cluster": { /* 001 Status object verbatim */ }
}
```

**Exit codes**: `0` clean, `2` unhealthy, `3` partial, `65` network.

---

### `pgmctl get <resource> [<name>]` / `pgmctl list <resource>` / `pgmctl describe <resource>/<name>`

Resource kinds (FR-012): `nodes`, `peers`, `slots`, `events`, `audit`,
`topology`, `config`, `version`.

| Resource | Endpoint | Notes |
|---|---|---|
| `nodes` / `peers` | `GET /v1/status` | Derived from Status.Peers. |
| `slots` | `GET /v1/diagnose` | Derived; replication-slot block. |
| `events` | `GET /v1/history?category=event` | History stream (RD-002). |
| `audit` | `GET /v1/history?category=audit` | History stream. |
| `topology` | `GET /v1/topology` (existing 001) | Tree render by default. |
| `config` | `GET /v1/config` (existing) | Effective merged, redacted. |
| `version` | `GET /v1/version` (existing) | Includes embedded NATS version (002). |

`describe` is the verbose form of `get` (multi-line per record).

---

### `pgmctl topology`, `pgmctl health`, `pgmctl lag`

| Command | Endpoint | Render |
|---|---|---|
| `topology` | `GET /v1/topology` | ASCII tree by default. |
| `health` | `GET /v1/status` + `GET /v1/doctor/run` | One-line-per-component rollup (FR-014). |
| `lag` | `GET /v1/status` + `GET /v1/diagnose` | Per-standby lag in bytes + time. Thresholds via `--warn` / `--fail` or cluster defaults. |

---

### `pgmctl events [--since <duration>] [--node <id>] [--type <kind>] [--limit <n>]`

Queries the history stream (RD-002 / FR-016 / FR-016a). Default `--since
30m`, `--limit 1000`.

**Output**: one line per event in `TIME TYPE NODE DETAILS` form. JSON /
YAML emits an array of `HistoryEvent` objects.

---

### `pgmctl explain <subject> [<arg>]`

Subjects (FR-018): `failover-stuck`, `node-not-promoting <node>`,
`replication-broken <node>`, `leader-election`, `current-state`,
`last-event`.

Output structure:

```text
DIAGNOSIS
  <one-line probable cause>

EVIDENCE
  <RFC3339Nano>  <event-type>  <details>            from: history (id=<ulid>)
  <RFC3339Nano>  doctor:<name> <status> <message>   from: doctor
  ...

SUGGESTED NEXT STEPS
  1. <concrete pgmctl invocation>
  2. <concrete pgmctl invocation>
  3. <concrete pgmctl invocation>
```

JSON form mirrors the three sections as `diagnosis`, `evidence[]`,
`suggested_next_steps[]`.

**Subject does not apply** → exit `EX_SUBJECT_NA` (4) with a one-line
message naming the reason.

---

### `pgmctl doctor [--list] [--check <name>] [--fix] [--strict]`

Calls `GET /v1/doctor/checks` for discovery (FR-026) and
`POST /v1/doctor/run` for execution (RD-008).

`--check <name>` runs only one check. `--list` enumerates the catalogue
without running anything.

`--fix` walks `FAIL` checks with a non-nil `suggested_fix`, prompts per
`SuggestedFix.blast_radius`, applies via `POST /v1/doctor/fix`, re-runs
the check.

`-y` bypasses `single-resource` fix prompts. Cluster-affecting fixes
still require typed cluster-name; `advisory` fixes are never auto-applied.

**JSON output**:

```jsonc
{
  "apiVersion": "pgmctl/v1",
  "kind": "DoctorReport",
  "captured_at": "...",
  "summary": { "pass": 14, "info": 1, "warn": 2, "fail": 1, "unknown": 0 },
  "checks": [ /* CheckResult[] */ ]
}
```

Exit code: `0` if no FAIL; `2` if any FAIL; `5` if no FAIL but UNKNOWN
present.

---

### `pgmctl watch <topic> [args]`

Topics (FR-019 / FR-020 / FR-021):
- `status` — differential cell redraw (RD-010).
- `transitions` — append-only state transitions.
- `events` — append-only history-stream events.
- `node <node-id>` — append-only state of one node.

Transport: SSE per RD-004. Reconnects with exponential backoff; emits
gap-marker line on degradation.

Always interactive — `--output json` / `--output yaml` is **rejected**
for `watch` (use `events --since 0` for a streamable JSON tail).

Ctrl-C exits cleanly with code `0`.

---

### `pgmctl dump [--output <path>|-] [--since <duration>] [--redact-level normal|strict] [--per-slice-timeout <duration>]`

Captures every slice in parallel (FR-032). Default `--timeout 60s`,
`--per-slice-timeout 10s`.

Output:
- `--output <path>`: writes `<path>` as a single `.tar.gz`.
- `--output -`: writes raw uncompressed tar to stdout.

Manifest schema: `DumpManifest` (data-model.md). Slice layout: see
RD-011.

Exit codes: `0` clean, `3` partial (some slices `failed`), `124` overall
timeout.

---

## Mutating commands — single-resource

### `pgmctl fence <node> [-y] [--force]`

Calls `POST /v1/fence` (existing 001). Prompts unless `-y`.

Prompt text:

```text
About to fence node-2 in cluster prod-east. Continue? [y/N]:
```

**Semantics (corrected 2026-05-29).** Fence is a *promotion-eligibility*
marker, **not** a failover. pg-manager's `EventOperatorFence` preserves
the node's role (`NewRole: curRole`) and the `StateFenced` act-phase is
a no-op, so fencing a node neither demotes it nor moves writes off it —
it only makes the node ineligible for **future** promotion / failover
candidate selection (001 spec.md:123-124) and excludes it from
auto-rebootstrap/auto-demote (FR-010). Earlier prompt copy claimed fence
makes a node "ineligible to serve writes"; that overstated the effect
and is removed.

**Current-primary guard (FR-028a).** Because fencing the live primary is
a no-op for writes yet leaves an incoherent snapshot (a node that is
both serving writes and marked fenced — `pgmctl doctor`'s
`cluster.has-primary` then reports "no primary observed" while
`cluster.has-leader` still passes), `pgmctl fence <primary>` is
**refused** with `EX_USAGE` (64) and a message pointing the operator at
`failover` / `switchover`. The guard is best-effort: if `GET /v1/status`
cannot be fetched/decoded it warns and proceeds (strictly additive — no
previously-working invocation starts failing). Pass `--force` to fence
the current primary anyway (emits a warning).

---

### `pgmctl unfence <node> [-y]`

Calls `POST /v1/unfence` (existing). Mirror of `fence`.

---

### `pgmctl set-config <key>=<value> [-y]`

Calls a new endpoint `POST /v1/config/set` documented in
`control-plane-extensions.md`. Client-side allow-list (mirrors 002
FR-014a hot-reload list):

| Allowed key | Source of truth |
|---|---|
| `cluster.peer_routes` | 002 FR-004 / FR-014a |
| `cluster.password` (reference; e.g., reload SecretRef target) | 002 FR-010 / FR-010a |

Any other key is **REFUSED client-side** with a clear error naming the
allowed keys.

---

## Mutating commands — cluster-affecting

### `pgmctl failover [--target <node>] [--force --cluster <name>]`

Calls `POST /v1/failover` (existing 001). Typed cluster-name prompt
unless `--force --cluster <name>` matches the server's cluster id.

Prompt text:

```text
About to FAILOVER cluster prod-east to node-3.
Estimated blast radius: WRITE DOWNTIME of up to 5 seconds.
Type the cluster name to confirm:
```

---

### `pgmctl switchover --target <node> [--force --cluster <name>]`

Calls `POST /v1/switchover` (existing). Same prompt shape; `target` is
required.

---

### `pgmctl promote [--force --cluster <name>]`

Calls `POST /v1/promote` (existing; local-only per 001). Same prompt.

---

### `pgmctl restart <node> [--target postgres|proxy] [--force --cluster <name>]`

Calls the new `POST /v1/restart` endpoint
(`control-plane-extensions.md`). `--target` defaults to `postgres`.

Prompt text differs by target:

```text
# --target=postgres (default)
About to RESTART managed PostgreSQL on node-2 in cluster prod-east.
Blast radius: that node will be unavailable for the duration of restart
(typically 5–30s); if it is the primary, a failover MAY be triggered by
pg-manager's lease watcher.
Type the cluster name to confirm:
```

```text
# --target=proxy
About to RESTART the pgman-proxy peer process on node-2 in cluster prod-east.
Blast radius: that peer will exit; its host supervisor (systemd/s6/tini/k8s)
will respawn it. Data-plane connections through this peer will close;
clients will reconnect to a sibling. The cluster's data plane on other
peers is unaffected.
Type the cluster name to confirm:
```

---

### `pgmctl delete <node> [--force --cluster <name>]`

Calls `POST /v1/topology` (existing 001 `UpdateTopology`) with a
"decommission peer" payload (FR-030).

Prompt text:

```text
About to DECOMMISSION node-2 from cluster prod-east.
Blast radius: node-2 will be removed from the active topology; its
replication slots will be released; clients still connected to it
will lose their session. The pgman-proxy peer process on node-2 will
NOT be killed; you must stop it manually after this completes.
Type the cluster name to confirm:
```

---

## Auxiliary commands

### `pgmctl version [-o json|yaml]`

Prints pgmctl version, build commit, Go version, and (when reachable)
the server's pgman-proxy version + build commit.

Skew rules (Edge Cases):
- Patch skew: silent.
- Minor skew: yellow warning.
- Major skew without `--insecure-skip-version-check`: refuse with
  `EX_VERSION_SKEW` (67) before any other request.

---

### `pgmctl config view [--show-secrets] [--minify]`

Renders the active configuration (FR-007). `--show-secrets` is REFUSED
in non-TTY contexts (never in pipelines).

### `pgmctl config use-context <name>`

Switches the `current-context`.

### `pgmctl config set-context <name> [--endpoint ...] [--token-file ...] [...]`

Creates or updates a context. Secrets are sourced (never inlined); the
flags accept *source references*, not values (`--token-env NAME`, not
`--token VALUE`).

### `pgmctl config delete-context <name>`

Removes a context; refuses to delete `current-context` without
`--force`.

### `pgmctl completion <shell>`

Emits shell completion (FR-003) for `bash`, `zsh`, `fish`.

---

## Stdout / stderr discipline

| Stream | Content |
|---|---|
| stdout | Requested data: tables, JSON, YAML, watch redraws, dump bytes (`--output -`), `request_id` on mutating-op acceptance. |
| stderr | Structured logs at `-v` and above, error messages, deprecation warnings, version-skew warnings, supervisor-not-detected refusals from `restart --target=proxy`. |

`--quiet` suppresses banner / timing summaries on stdout; logs on
stderr remain at level `error` and above.

---

## Output schema versioning (FR-038)

Every non-table output (`json`, `yaml`) is rooted in a document with:

```jsonc
{
  "apiVersion": "pgmctl/v1",
  "kind": "<EntityName>",
  ...
}
```

`apiVersion` bumps are MINOR-version events; downstream automation MAY
pin to `pgmctl/v1` and reject unknown majors.

---

## Out-of-scope for v1 (record so they don't leak into implementation)

- `pgmctl plugin` extension surface.
- Windows binaries.
- A web UI / dashboard.
- Per-operation RBAC inside the bearer-token scheme (every authenticated
  caller can invoke any operation, per 001 § Auth).
- Keychain-backed token storage (use `token_command` as escape hatch).
- `pgmctl logs` — removed at /speckit-clarify Q5.
