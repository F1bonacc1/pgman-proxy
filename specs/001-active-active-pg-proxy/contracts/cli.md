# Contract: Command-Line Interface

**Feature**: 001-active-active-pg-proxy · **Phase**: 1 · **Date**: 2026-05-09

`pgman-proxy` is a single binary with no subcommands. All operational
behaviour is governed by configuration (see `config.md`).

---

## Synopsis

```text
pgman-proxy [flags]
```

`pgman-proxy` runs in the foreground, logs to stdout/stderr, and exits
non-zero on any fail-closed condition. It is intended to be supervised
by `systemd`, `s6`, `tini`, or any other PID-1 / process supervisor.

## Flags

| Flag                  | Type     | Default | Notes                                                              |
|-----------------------|----------|---------|--------------------------------------------------------------------|
| `--config <path>`     | string   | —       | YAML configuration file path. May be absent if env+flags suffice.  |
| `--cluster-id <id>`   | string   | —       | Override `cluster.id`.                                             |
| `--node-id <id>`      | string   | —       | Override `node.id`.                                                |
| `--peers <csv>`       | string   | —       | Override `peers`.                                                  |
| `--nats <url>`        | string   | —       | Override `nats.url`.                                               |
| `--listen <addr>`     | string   | —       | Override `proxy.listen_addr`.                                      |
| `--switch-policy <e>` | enum     | —       | `hard_close`/`drain`/`pause`.                                      |
| `--log-level <e>`     | enum     | —       | `debug`/`info`/`warn`/`error`.                                     |
| `--metrics <addr>`    | string   | —       | Override `obs.metrics_addr`.                                       |
| `--print-config`      | bool     | `false` | Render merged config (secrets redacted) to stdout, exit `0`.       |
| `--version`           | bool     | `false` | Print version metadata, exit `0`.                                  |
| `--help`              | bool     | `false` | Print flag help, exit `0`.                                         |

## Argument-parsing rules

1. Unknown flag → exit `EX_CONFIG` (78), print one-line error pointing at
   `--help`. The binary MUST NOT silently ignore unknown flags.
2. Conflicting flag (`--print-config` + `--version`) → exit `EX_CONFIG`,
   print one-line error.
3. Positional arguments are NOT accepted. Any positional → exit `EX_CONFIG`.

## stdout/stderr discipline

- `--print-config`, `--version`, `--help`: write to **stdout**, exit `0`.
- Structured logs and runtime errors: write to **stderr**.
- The wire-protocol traffic NEVER appears on either stream — the proxy
  forwards bytes between sockets and does not log payloads
  (Constitution II: never log secrets).

## Exit codes (cross-reference)

See `lifecycle.md` for the full table; CLI-relevant subset:

| Code | Symbol         | Meaning                                            |
|------|----------------|----------------------------------------------------|
| 0    | `EX_OK`        | Clean exit (signal-driven shutdown completed).      |
| 64   | `EX_USAGE`     | Reserved for future subcommand misuse; not used today. |
| 78   | `EX_CONFIG`    | Configuration error (parse / validate / unknown flag). |

## Examples

```bash
# Sidecar form, env-driven
LOCAL_DSN='host=/var/run/postgresql user=postgres sslmode=disable' \
PGMAN_PROXY_CLUSTER_ID=prod-east \
PGMAN_PROXY_NODE_ID=pg-east-a \
PGMAN_PROXY_PEERS=pg-east-a,pg-east-b,pg-east-c \
PGMAN_PROXY_NATS_URL=tls://nats.internal:4222 \
PGMAN_PROXY_PROXY_LISTEN_ADDR=127.0.0.1:6432 \
PGMAN_PROXY_POSTGRES_BIN_DIR=/usr/lib/postgresql/17/bin \
PGMAN_PROXY_POSTGRES_DATA_DIR=/var/lib/postgresql/data \
pgman-proxy

# Microservice form, file-driven
pgman-proxy --config /etc/pgman-proxy/config.yaml --node-id pg-east-b

# Validation dry run
pgman-proxy --config /etc/pgman-proxy/config.yaml --print-config
```
