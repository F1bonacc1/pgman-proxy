# Contract: Configuration Schema

**Feature**: 001-active-active-pg-proxy · **Phase**: 1 · **Date**: 2026-05-09

Authoritative configuration surface for `pgman-proxy`. Layered loading:
flags > env vars > YAML file > built-in defaults. Required keys MUST be
satisfied by **at least one** of those layers. Validation runs after
merging; missing or invalid values exit non-zero with code `EX_CONFIG`
(see `lifecycle.md`).

---

## YAML schema (one peer)

```yaml
cluster:
  id: prod-pg-east           # required
node:
  id: pg-east-a              # required
peers:                        # required, non-empty
  - pg-east-a
  - pg-east-b
  - pg-east-c

nats:
  url: tls://nats.internal:4222  # required
  connect_timeout: 10s            # default 10s
  reconnect_wait: 2s              # default 2s
  max_reconnects: -1              # default infinite
  creds_file: /etc/pgman-proxy/nats.creds   # optional, mutually exclusive with token
  # token: $NATS_TOKEN            # never inline; env-ref only

proxy:
  listen_addr: 0.0.0.0:6432       # required
  dial_timeout: 5s                # default 5s
  switch_policy: hard_close       # default hard_close ; alts: drain | pause

postgres:
  bin_dir: /usr/lib/postgresql/17/bin    # required
  data_dir: /var/lib/postgresql/data     # required
  local_dsn_env: LOCAL_DSN               # env-var name; required (see Secrets)
  port: 5432                              # default 5432
  tls_mode: verify-full                   # default verify-full ; "disable" requires the ack
  tls_disable_explicit_ack: false         # default false
  peer_dsns: {}                           # optional override; otherwise derived from peers

policy:
  failover_delay: 30s
  switchover_delay: 30s
  promote_timeout: 60s
  liveness_interval: 5s
  liveness_failures: 3
  quorum_sync:
    min_sync: 1
  auto_rebootstrap:
    enabled: false
  auto_demote:
    enabled: false

obs:
  log_level: info             # default info
  metrics_addr: :9090         # default :9090
  health_addr: :9090          # default :9090 (shared with metrics)
  otel:
    endpoint: ""              # empty = noop tracer

control:
  listen_addr: ""             # mode-aware default — empty = use the mode default:
                              #   sidecar      → 127.0.0.1:9091 (loopback-only)
                              #   standalone   → 0.0.0.0:9091   (all-interfaces)
                              #   microservice → 0.0.0.0:9091   (all-interfaces)
  leader_route_mode: forward  # default forward ; alts: redirect
  leader_route_timeout: 30s   # default 30s — forward-mode wait budget
  auth:
    token_env:  PGMAN_PROXY_CONTROL_TOKEN   # name of env var holding the bearer token; mutually exclusive with token_file
    token_file: ""                          # path to a file containing the bearer token; mutually exclusive with token_env
    allow_unauth_reads: false               # default false ; when true, /v1/status and /v1/diagnose accept unauthenticated GETs
  tls:
    cert_file: ""                           # PEM cert; required if listen_addr is non-loopback (FR-033)
    key_file:  ""                           # PEM key;  required if listen_addr is non-loopback (FR-033)
    plaintext_explicit_ack: false           # default false ; setting true allows plaintext bind on a non-loopback address (named, audit-logged exception, mirrors postgres.tls_disable_explicit_ack)

shutdown:
  drain_budget: 30s           # default 30s
```

---

## Environment-variable mapping

Every YAML key has a corresponding env-var. Env wins over YAML, flags
win over env. Names follow the `PGMAN_PROXY_<UPPER_SNAKE_CASE>` rule
with `.` replaced by `_`. Examples:

| YAML key                           | Env var                                    |
|------------------------------------|--------------------------------------------|
| `cluster.id`                       | `PGMAN_PROXY_CLUSTER_ID`                   |
| `node.id`                          | `PGMAN_PROXY_NODE_ID`                      |
| `peers`                            | `PGMAN_PROXY_PEERS` (CSV)                  |
| `nats.url`                         | `PGMAN_PROXY_NATS_URL`                     |
| `proxy.listen_addr`                | `PGMAN_PROXY_PROXY_LISTEN_ADDR`            |
| `postgres.bin_dir`                 | `PGMAN_PROXY_POSTGRES_BIN_DIR`             |
| `postgres.data_dir`                | `PGMAN_PROXY_POSTGRES_DATA_DIR`            |
| `postgres.tls_mode`                | `PGMAN_PROXY_POSTGRES_TLS_MODE`            |
| `obs.metrics_addr`                 | `PGMAN_PROXY_OBS_METRICS_ADDR`             |
| `control.listen_addr`              | `PGMAN_PROXY_CONTROL_LISTEN_ADDR`          |
| `control.leader_route_mode`        | `PGMAN_PROXY_CONTROL_LEADER_ROUTE_MODE`    |
| `control.leader_route_timeout`     | `PGMAN_PROXY_CONTROL_LEADER_ROUTE_TIMEOUT` |
| `control.auth.token_env`           | `PGMAN_PROXY_CONTROL_AUTH_TOKEN_ENV`       |
| `control.auth.token_file`          | `PGMAN_PROXY_CONTROL_AUTH_TOKEN_FILE`      |
| `control.auth.allow_unauth_reads`  | `PGMAN_PROXY_CONTROL_AUTH_ALLOW_UNAUTH_READS` |
| `control.tls.cert_file`            | `PGMAN_PROXY_CONTROL_TLS_CERT_FILE`        |
| `control.tls.key_file`             | `PGMAN_PROXY_CONTROL_TLS_KEY_FILE`         |
| `control.tls.plaintext_explicit_ack` | `PGMAN_PROXY_CONTROL_TLS_PLAINTEXT_EXPLICIT_ACK` |
| `shutdown.drain_budget`            | `PGMAN_PROXY_SHUTDOWN_DRAIN_BUDGET`        |

Backward-compatible aliases (matching `three_node_nats/main.go`'s env
names) MUST also be honoured at startup:
`NATS_URL`, `CLUSTER_ID`, `NODE_ID`, `PEERS`, `PGDATA`, `PG_BINDIR`,
`LOCAL_DSN`, `PROXY_LISTEN`. When both an alias and a canonical env
var are set, the canonical name wins; mismatch is logged at startup.

---

## Command-line flags

A subset of the most frequently tweaked keys is exposed as flags for
operational convenience. Flag values override env and YAML.

```text
--config <path>             # YAML file path
--cluster-id <id>           # short for cluster.id
--node-id <id>              # short for node.id
--peers <csv>               # short for peers
--nats <url>                # short for nats.url
--listen <addr>             # short for proxy.listen_addr
--switch-policy <enum>      # short for proxy.switch_policy
--log-level <enum>          # short for obs.log_level
--metrics <addr>            # short for obs.metrics_addr
--print-config              # render the merged, validated config to stdout and exit
--version                   # print version + git SHA + go version + pg-manager version, exit 0
```

`--print-config` MUST redact secrets (see Secrets).

---

## Secrets

Secrets MUST NOT appear inline in YAML. Two acceptable forms:

1. **Env-var indirection**: a YAML key ends in `_env` and names an env
   var holding the value. Example: `postgres.local_dsn_env: LOCAL_DSN`.
2. **File path indirection**: a YAML key ends in `_file` and names a
   filesystem path readable by the proxy user. Example:
   `nats.creds_file: /etc/pgman-proxy/nats.creds`.

Validation MUST reject any YAML where a known-secret key
(`local_dsn`, `token`, `password`, `tls_key`) is set inline.

`--print-config` and the structured-log "config_loaded" event MUST
replace the value of any secret-bearing key with the string
`***REDACTED***`.

---

## Validation outcomes

| Outcome                         | Behaviour                                                                |
|---------------------------------|--------------------------------------------------------------------------|
| All required keys present, all values pass type+regex validation | proceed to runtime startup gates                          |
| Required key missing             | exit `EX_CONFIG` (78); log a single `error` with the offending key path  |
| Type/regex/enum violation        | exit `EX_CONFIG` (78); log offending key + observed value (redacted)     |
| TLS-disable without explicit ack | exit `EX_CONFIG` (78); log `tls_mode=disable rejected without ack` (FR-018) |
| Inline secret detected           | exit `EX_CONFIG` (78); log `inline secret refused` with the key path     |
| YAML parse error                 | exit `EX_CONFIG` (78); log file path and parser error                    |
| `control.auth.token_env` and `control.auth.token_file` both set | exit `EX_CONFIG` (78); log `control.auth tokens are mutually exclusive`  |
| `control.auth.token_env`/`token_file` neither set when mutating-auth is enabled | exit `EX_CONFIG` (78); log `no control-plane token source configured` (FR-025) |
| Non-loopback `control.listen_addr` without `control.tls.cert_file` + `control.tls.key_file` AND without `control.tls.plaintext_explicit_ack: true` | exit `EX_CONFIG` (78); log `control plane plaintext bind on non-loopback rejected without ack` (FR-033) |
| `control.tls.cert_file` set without `control.tls.key_file` (or vice versa) | exit `EX_CONFIG` (78); log offending half (FR-033) |
| `control.leader_route_mode` not in {`forward`, `redirect`} | exit `EX_CONFIG` (78); enum violation (FR-026) |
| `control.leader_route_timeout` ≤ 0 or > 5 minutes | exit `EX_CONFIG` (78); log offending value (FR-034) |

Exit codes are defined in `lifecycle.md`.
