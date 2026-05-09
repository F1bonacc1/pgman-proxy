# Deploying pgman-proxy under systemd

The same `pgman-proxy` binary serves three deployment topologies — distinguished
only by configuration. The two unit-file templates in this directory cover the
common operator surfaces; tune them to your fleet before enabling.

## Modes at a glance

| Mode | Unit | Default bind | Restart on PG restart? | Typical hosting |
|------|------|--------------|------------------------|-----------------|
| `standalone` | `pgman-proxy.service` | all-interfaces | n/a (no colocated PG) | dedicated VM in front of one PostgreSQL primary |
| `microservice` | `pgman-proxy.service` (3+ peers) | all-interfaces | n/a | one peer per host, embedded-NATS coordinated (feature 002) |
| `sidecar` | `pgman-proxy-sidecar.service` | **loopback** | yes — `BindsTo=postgresql.service` | one peer colocated with one PostgreSQL instance |

Mode is selected by the `PGMAN_PROXY_DEPLOYMENT_MODE` env var (or
`deployment_mode:` in YAML). Sidecar mode auto-rewrites `0.0.0.0:N` /
`[::]:N` / `:N` listener addresses to `127.0.0.1:N` so off-host clients
cannot reach the proxy or the observability surface (FR-013). Operators
who need a different bind address can supply it explicitly — explicit
beats implicit.

## Standalone & microservice — `pgman-proxy.service`

Drop a unit-override fragment under `/etc/systemd/system/pgman-proxy.service.d/`:

```ini
# /etc/systemd/system/pgman-proxy.service.d/override.conf
[Service]
Environment="PGMAN_PROXY_DEPLOYMENT_MODE=microservice"
Environment="PGMAN_PROXY_CLUSTER_ID=prod"
Environment="PGMAN_PROXY_CLUSTER_NAME=pgman-proxy-prod"
Environment="PGMAN_PROXY_CLUSTER_DECLARED_SIZE=3"
Environment="PGMAN_PROXY_NODE_ID=node-a"
Environment="PGMAN_PROXY_PEERS=node-a,node-b,node-c"
Environment="PGMAN_PROXY_CLUSTER_ROUTE_PEERS=node-b.internal:6222,node-c.internal:6222"
Environment="PGMAN_PROXY_CLUSTER_USERNAME=pgman-proxy-prod"
Environment="PGMAN_PROXY_CLUSTER_PASSWORD_ENV=PGMAN_PROXY_CLUSTER_PASSWORD"
Environment="PGMAN_PROXY_CLUSTER_JETSTREAM_DIR=/var/lib/pgman-proxy/jetstream"
EnvironmentFile=-/etc/pgman-proxy/secrets.env   # holds PGMAN_PROXY_CLUSTER_PASSWORD, PGMAN_PROXY_CONTROL_TOKEN, LOCAL_DSN
```

Reload + enable:

```bash
systemctl daemon-reload
systemctl enable --now pgman-proxy
```

`Restart=on-failure` and a fixed `RestartSec=5` cover supervisor-driven
recovery. Exit code `77` (`EX_SINGLETON`) is documented as a rare
recoverable condition; `78` (`EX_CONFIG`) is operator-actionable and the
unit deliberately does NOT restart on it (handled by the
`RestartPreventExitStatus=78` line in the template).

## Sidecar — `pgman-proxy-sidecar.service`

The sidecar template uses `After=postgresql.service` and
`BindsTo=postgresql.service` so that when systemd stops or restarts the
local Postgres, the sidecar is taken with it. This matches the spec's
"single supervisor, two siblings" expectation (US2 acceptance scenarios).

```ini
# /etc/systemd/system/pgman-proxy-sidecar.service.d/override.conf
[Service]
Environment="PGMAN_PROXY_DEPLOYMENT_MODE=sidecar"
Environment="PGMAN_PROXY_CLUSTER_ID=prod"
Environment="PGMAN_PROXY_CLUSTER_NAME=pgman-proxy-prod"
Environment="PGMAN_PROXY_CLUSTER_DECLARED_SIZE=3"
Environment="PGMAN_PROXY_NODE_ID=node-a"
Environment="PGMAN_PROXY_PEERS=node-a,node-b,node-c"
Environment="PGMAN_PROXY_CLUSTER_ROUTE_PEERS=node-b.internal:6222,node-c.internal:6222"
Environment="PGMAN_PROXY_CLUSTER_USERNAME=pgman-proxy-prod"
Environment="PGMAN_PROXY_CLUSTER_PASSWORD_ENV=PGMAN_PROXY_CLUSTER_PASSWORD"
Environment="PGMAN_PROXY_CLUSTER_JETSTREAM_DIR=/var/lib/pgman-proxy/jetstream"
Environment="PGMAN_PROXY_POSTGRES_DATA_DIR=/var/lib/postgresql/17/main"
Environment="PGMAN_PROXY_POSTGRES_BIN_DIR=/usr/lib/postgresql/17/bin"
EnvironmentFile=-/etc/pgman-proxy/secrets.env
```

Listener addresses are deliberately omitted — sidecar defaults to
`127.0.0.1:6432` / `127.0.0.1:9090` / `127.0.0.1:9091`. Operator-supplied
addresses pass through unchanged.

## Restart semantics

`pgman-proxy` stores no local state that must persist across restarts.
A peer that crashes, gets killed, or is replaced rejoins via the
embedded-NATS cluster (feature 002); its cluster membership is
canonical in the JetStream KV bucket replicated across peers, not
locked to a single peer's disk (see `contracts/lifecycle.md` §
Restart-in-place semantics).

Common operator-facing exit codes (`contracts/lifecycle.md`):

| Code | Name | Restart? | Notes |
|------|------|----------|-------|
| 0 | `EX_OK` | n/a | clean shutdown after SIGTERM |
| 74 | `EX_OBS` | yes | metrics port busy at startup |
| 75 | `EX_DEPS` | yes | embedded NATS startup failed / pg-manager adapter init failed |
| 76 | `EX_LISTEN` | yes | data-plane port busy |
| 77 | `EX_SINGLETON` | yes (rare) | singleton-claim retry budget exhausted |
| 78 | `EX_CONFIG` | **no** | configuration error — operator MUST fix |
| 79 | `EX_DRAIN_TIMEOUT` | n/a (process already exiting) | drain budget exceeded |
| 80 | `EX_INTERNAL` | yes | unexpected panic |
| 81 | `EX_CONTROL` | yes | control-plane bind / initial audit emit failed |

The unit templates set `RestartPreventExitStatus=78` so misconfigured
peers don't enter a restart loop the operator has no way to interrupt.
