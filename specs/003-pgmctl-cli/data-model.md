# Data Model — `pgmctl` Operator CLI

**Feature**: `003-pgmctl-cli` · **Phase**: 1 · **Date**: 2026-05-14
**Status**: Locked at `/speckit-plan` time; renames after `/speckit-tasks`
are MINOR-version events per Constitution V.

This document enumerates the entities introduced by feature 003. Existing
entities from feature 001 (`Status`, `Diagnosis`, `Topology`, `Policy`,
LCM request/response envelopes) and feature 002 (`embedded_nats.*`
observability sub-block) are referenced but not re-stated here.

---

## Client-side entities (pgmctl)

### Context

A named connection profile in the pgmctl configuration file.

| Field | Type | Required | Notes |
|---|---|---|---|
| `name` | string | yes | Unique within `contexts[]`; lowercase kebab-case. |
| `endpoint` | URL | yes | Control-plane base URL (e.g., `https://pgman-proxy.prod-east:9091`). |
| `expected_cluster` | string | no | Validated against the server's reported cluster id before any cluster-affecting op (FR-010). |
| `token_env` | string | one-of | Name of an environment variable holding the bearer token. |
| `token_file` | path | one-of | Filesystem path read on every request (FR-031 of 001 — rotation without restart). |
| `token_command` | []string | one-of | Operator-supplied command; stdout (trimmed) is the token. |
| `tls.ca_file` | path | no | Trust anchor PEM bundle. Default: system store. |
| `tls.server_name` | string | no | SNI override. |
| `tls.insecure_skip_tls_verify` | bool | no | Default `false`; warned when `true`. |

**Validation rules**:
- Exactly one of `token_env` / `token_file` / `token_command` MUST be set.
- `endpoint` MUST use the `https://` scheme unless its host is a loopback
  address (mirrors 001 FR-033 trust-boundary rule).
- File-permission gate: pgmctl REFUSES to load the config if the
  containing file is group- or world-readable.

**Lifecycle**: Contexts are created/modified/deleted via `pgmctl config
set-context …` / `pgmctl config delete-context …`. The active context is
the one named in `current-context` at the top of the YAML; switched via
`pgmctl config use-context <name>`.

---

### DoctorCheck

A server-published, named, read-only inspection. Discovered via
`GET /v1/doctor/checks` (RD-008) and rendered client-side.

| Field | Type | Description |
|---|---|---|
| `name` | string | Stable identifier (e.g., `replication.lag-acceptable`). |
| `description` | string | One-line human description. |
| `suggested_fix` | `SuggestedFix?` | Optional; present only when an automated fix is registered for this check. |
| `evidence_schema` | string | Reference to the JSON schema for the evidence payload returned by a `run`. Stable; renames are MINOR. |

**Cardinality**: 18 checks in v1 (FR-022).

---

### CheckResult

The execution outcome of one `DoctorCheck`. Returned by
`POST /v1/doctor/run` (RD-008).

| Field | Type | Description |
|---|---|---|
| `name` | string | Matches a `DoctorCheck.name`. |
| `status` | `Severity` | `PASS` / `INFO` / `WARN` / `FAIL` / `UNKNOWN`. |
| `message` | string | One-line human form, with offending value if applicable. |
| `evidence` | object | Structured evidence; redacted by the same rules as `--print-config`. |
| `suggested_fix` | `SuggestedFix?` | Only when `status` is `FAIL` (or `WARN` for fixable warnings). |
| `executed_at` | RFC3339 | When the check ran on the server. |
| `node_id` | string? | Set for per-peer checks; null for cluster-level checks. |

---

### SuggestedFix

A server-published, named action with a blast-radius classification.

| Field | Type | Description |
|---|---|---|
| `name` | string | Stable identifier (e.g., `unfence-node`, `kick-replication`). |
| `description` | string | One-line human form of what the fix does. |
| `blast_radius` | enum | `single-resource` / `cluster-affecting` / `advisory`. |
| `applies_to_check` | string | Back-reference to the `DoctorCheck.name`. |
| `args_schema` | string? | Reference to the JSON schema for the `args` payload (if the fix takes any). |
| `apply_endpoint` | string | Always `/v1/doctor/fix` in v1; reserved for future extensibility. |

**Confirmation routing**:
- `single-resource` → pgmctl's `[y/N]` prompt; `-y` bypasses (FR-028).
- `cluster-affecting` → typed cluster-name confirmation; `--force
  --cluster <name>` bypasses (FR-029); `-y` alone is REFUSED.
- `advisory` → never auto-applied; pgmctl prints the description as a
  recommendation only.

---

### WatchStream

A long-lived SSE subscription to a server-side event source. Not a
serializable entity; described here for the client state machine.

| State | Description |
|---|---|
| `connecting` | TCP / TLS handshake in flight. |
| `streaming` | Receiving events; redraw / append on each. |
| `idle_keepalive` | No data event in ≥ 5s but keepalive arriving normally. |
| `degraded` | A `gap_marker` was just emitted; bottom status bar shows reason. |
| `reconnecting` | Connection dropped; in exponential backoff (RD-004). |
| `terminated` | Max reconnects exceeded or operator Ctrl-C. |

Transitions emit a single status-bar line (not a screen redraw) so the
data area stays stable.

---

### DumpArtifact

A single self-contained file (or stream) bundling cluster-wide state at
the moment of capture. Defined by `contracts/cli-commands.md § dump` and
RD-011.

| Field | Type | Description |
|---|---|---|
| `manifest` | `DumpManifest` | Always at `manifest.json` inside the tar. |
| `status` | `Status` | 001's Status snapshot. |
| `topology` | `Topology` | 001's Topology snapshot. |
| `config` | YAML | Effective merged proxy config, redacted. |
| `events` | array | History-stream query result. |
| `audit` | array | History-stream audit subset. |
| `doctor` | object | Full check battery output. |
| `clock_skew` | array | Per-peer skew measurements. |
| `peers/<node_id>/*` | tree | Per-peer slices via FR-006a fan-out. |

---

### DumpManifest

| Field | Type | Description |
|---|---|---|
| `apiVersion` | string | `pgmctl/v1`. |
| `kind` | string | `DumpManifest`. |
| `pgmctl` | object | `{ version, commit, go_version }`. |
| `pgman_proxy` | object | `{ version, commit }` observed at dump time. |
| `captured_at` | object | `{ started: RFC3339, ended: RFC3339 }`. |
| `redact_level` | enum | `normal` / `strict`. |
| `cluster_id` | string | Real id under `normal`; placeholder under `strict`. |
| `slices` | array | One entry per slice; `{ name, outcome, duration_ms, error? }`. |

**Outcome values**: `ok` / `partial` / `failed`.

---

### Severity

Enumeration with stable wire form and stable color mapping.

| Value | Color (TTY) | Marker (no-color) | Doctor exit-code contribution |
|---|---|---|---|
| `PASS` | green | `[OK]` | 0 |
| `INFO` | yellow | `[INFO]` | 0 |
| `WARN` | yellow | `[WARN]` | 1 only when `--strict` |
| `FAIL` | red | `[FAIL]` | 2 |
| `UNKNOWN` | yellow | `[UNKNOWN]` | distinct from FAIL; documented exit value |

`UNKNOWN` is **never** a substitute for `FAIL`; it explicitly means
"could not determine".

---

### OutputFormat

Enumeration: `table` (default), `json`, `yaml`, `wide`. The `wide`
variant uses the same data shape as `table` with additional columns
(node_id full, advertised endpoints, last transition reason).

All non-table formats include `apiVersion: pgmctl/v1` and a `kind` field
at the document root (FR-038).

---

### ConfirmationClass

Enumeration with stable behaviour mapping. Not a runtime entity per se;
the table below is the authoritative classification of every mutating
subcommand for `/speckit-tasks` to derive prompt-text strings from.

| Subcommand | Class | Override flag(s) |
|---|---|---|
| `fence <node>` | single-resource | `-y` / `--yes` |
| `unfence <node>` | single-resource | `-y` / `--yes` |
| `set-config <key>=<value>` | single-resource | `-y` / `--yes` |
| `failover [--target <node>]` | cluster-affecting | `--force --cluster <name>` |
| `switchover --target <node>` | cluster-affecting | `--force --cluster <name>` |
| `promote` | cluster-affecting | `--force --cluster <name>` |
| `restart <node> [--target postgres\|proxy]` | cluster-affecting | `--force --cluster <name>` |
| `delete <node>` | cluster-affecting | `--force --cluster <name>` |
| `doctor --fix` (per-fix) | follows the `SuggestedFix.blast_radius` |

---

### ExitCode

Stable, documented exit-code table (FR-037).

| Code | Symbol | Meaning |
|---|---|---|
| 0 | `EX_OK` | Clean success. |
| 1 | `EX_WARN_STRICT` | Non-failure warnings present AND `--strict` was supplied. |
| 2 | `EX_UNHEALTHY` | Cluster unhealthy / a doctor check returned `FAIL`. |
| 3 | `EX_PARTIAL` | Cluster partially reachable (partial dump). |
| 4 | `EX_SUBJECT_NA` | `pgmctl explain <subject>` — subject does not apply. |
| 5 | `EX_UNKNOWN` | All non-unknown checks passed but at least one returned `UNKNOWN` (distinct from `FAIL`). |
| 64 | `EX_USAGE` | Bad subcommand, bad flag combination, positional args present. |
| 65 | `EX_NETWORK` | Connectivity / TLS / auth failure (distinct from "cluster unhealthy"). |
| 67 | `EX_VERSION_SKEW` | Major-version skew between pgmctl and pgman-proxy without `--insecure-skip-version-check`. |
| 78 | `EX_CONFIG` | Missing endpoint, malformed config file, bad file permissions. |
| 124 | `EX_TIMEOUT` | Overall `--timeout` exceeded. |

Codes 6–63 reserved for forward-compatibility.

---

## Server-side entities (additive to features 001 / 002)

### HistoryEvent

A single record on the JetStream history stream
(`pgman_proxy.<cluster_id>.history.{event,audit}.>`).

| Field | Type | Description |
|---|---|---|
| `apiVersion` | string | `pgman-proxy/v1`. |
| `kind` | string | `HistoryEvent`. |
| `id` | ULID | Globally unique; used as the SSE `Last-Event-ID` resumption token. |
| `time` | RFC3339Nano | Wall-clock at emit. |
| `category` | enum | `event` / `audit`. |
| `type` | string | Event kind: `state_transition`, `leader_change`, `route_up`, `route_down`, `fence`, `unfence`, `storage_degraded`, `lcm_audit`, … |
| `cluster_id` | string | The owning cluster. |
| `node_id` | string? | Emitting / affected peer; null for cluster-wide. |
| `details` | object | Type-specific payload; schema per `type` is documented in contracts/. |
| `trace_id` | string? | W3C trace context. |
| `span_id` | string? | W3C trace context. |

**Retention**: defaults to `24h` OR `256 MiB` (whichever fills first);
operator-tunable via `history.retention_age` / `history.retention_bytes`.

**Replication**: derived per 002 FR-011a (R=1/2/3 from declared cluster
size); override path via `cluster.replication_factor_override`.

---

### FanOutSlice

The request/reply envelope for inter-peer fan-out (RD-003).

**Request payload** (published on `pgman_proxy.<cluster_id>.fanout.<slice>.<target_node>`):

| Field | Type | Description |
|---|---|---|
| `version` | int | `1`. |
| `request_id` | ULID | From the originating control-plane request. |
| `operator_actor` | string | From the bearer token; appears in the responder's audit log. |
| `trace_id` | string? | W3C. |
| `deadline_ms` | int | Per-slice timeout (default 5s). |
| `slice` | string | One of `status`, `config`, `nats_mesh`, `doctor`. |
| `args` | object | Slice-specific arguments. |

**Reply payload**:

| Field | Type | Description |
|---|---|---|
| `version` | int | `1`. |
| `request_id` | ULID | Echoes the request. |
| `node_id` | string | Responder's node id. |
| `status` | enum | `ok` / `partial` / `failed`. |
| `data` | object | Slice-specific result; absent when `status == failed`. |
| `error` | object? | `{ code, message }` when `status` is `failed` or `partial`. |
| `responded_at` | RFC3339Nano | For latency accounting. |

**Per-sibling error codes**: `sibling_unreachable`, `deadline_exceeded`,
`auth_failed`, `slice_internal`.

---

### RestartTarget

Enum carried in the body of `POST /v1/restart`.

| Value | Behaviour |
|---|---|
| `postgres` | Calls `Manager.RestartPostgres(ctx)` on the target peer (FR-031a). |
| `proxy` | Invokes the supervisor-checked self-terminate path (FR-031b/c). |

---

### SupervisorPresence

Resolved at proxy startup; queried by the `target=proxy` self-terminate
handler.

| Value | Meaning |
|---|---|
| `container` | `/.dockerenv` or k8s/docker/containerd cgroup detected. |
| `systemd` | `INVOCATION_ID` + `JOURNAL_STREAM` env vars set. |
| `s6_runit` | `S6_OVERLAY_VERSION` set or parent matches `s6-supervise`/`runsv`/`runsvdir`. |
| `tini` | Parent process basename is `tini` or `dumb-init`. |
| `override` | Operator set `proxy.assume_supervised: true`. |
| `none` | None of the above; `target=proxy` returns `412 supervisor_not_detected`. |

---

## State transitions

### Watch reconnect state machine

```text
        +-------------+
        |  connecting | --(handshake)-->  +-----------+
        +-------------+                   | streaming |
              ^                            +-----------+
              |                                 |
              | (backoff exceeded → terminate)  | (no data + no keepalive ≥ 30s)
              |                                 v
        +-------------+                   +---------------+
        | reconnecting| <-----(SSE drop)--|  idle/degraded|
        +-------------+                   +---------------+
```

`gap_marker` events do not change state; they are surfaced as a single
status-bar line and a render-stream divider.

### Doctor `--fix` per-check loop

```text
        +-----------+    suggested_fix?      +-----------+
        |  run check| --no--> next check     |  apply fix |
        +-----------+                        +-----------+
              |                                   |
              | FAIL + suggested_fix              v
              v                              +----------+
        +-----------+   single-resource      | re-run    |
        | prompt user| ---[y]----------->    | check     |
        +-----------+                        +----------+
              |                                   |
              | cluster-affecting & no --force    | PASS / still FAIL
              v                                   v
        +-----------+                        +----------+
        | refuse +  |                        | next     |
        | hint flag|                         | check    |
        +-----------+                        +----------+
```

`advisory` fixes never enter the apply branch.

---

## Validation rules summary

| Rule | Source |
|---|---|
| `Context.endpoint` MUST be `https://` unless loopback | RD-005 |
| Config file MUST be mode 0600 or stricter | RD-007 |
| `pgmctl` MUST refuse positional args (none accepted) | mirrors 001 CLI |
| `--quiet` and `--verbose` are mutually exclusive | FR-005 |
| `--no-color` takes precedence over any color-enabling setting | FR-005 |
| `--yes` MUST NOT bypass cluster-affecting confirmation | FR-029 |
| `--force` requires a matching `--cluster <name>` | FR-029 |
| Cluster-id mismatch refuses BEFORE sending any request | FR-010 |
| Bearer token MUST NEVER appear in any output mode | FR-009 |
| `set-config <key>` MUST be in the hot-reload allow-list | FR-028 |
| Doctor `Check.Run` MUST NOT mutate state (CI-asserted) | FR-027 |
| Mutating ops MUST NOT be retried client-side | FR-039 |
| `request_id` MUST NOT be reused server-side for dedup | FR-039 |
| `target=proxy` MUST refuse when `SupervisorPresence == none` | FR-031c |

---

## Field-naming conventions

- JSON over the wire: `snake_case`. Matches 001's existing convention.
- Go identifiers: `CamelCase`. Standard Go style.
- YAML config keys: `kebab-case` at the top level (e.g., `current-context`),
  `snake_case` inside contexts (e.g., `token_file`) to match the existing
  pgman-proxy config-key family.
- Subject names: lowercase dot-separated (e.g.,
  `pgman_proxy.<cluster_id>.history.event.state_transition`).
- ULIDs are lowercase in the wire format and the `Last-Event-ID` header.
