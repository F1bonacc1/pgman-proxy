# Quickstart: pgman-proxy v1

**Feature**: 001-active-active-pg-proxy · **Phase**: 1 · **Date**: 2026-05-09

A platform engineer should be able to follow this guide and reach a
working 3-peer proxy cluster in under 15 minutes (SC-001).

---

## Prerequisites

- A 3-node `pg-manager`–managed PostgreSQL cluster reachable on a
  network you control (use `../pg-manager/examples/three_node_nats/`
  if you need a sandbox).
- A NATS server reachable from every proxy peer; JetStream MUST be
  enabled (the `pg-manager/adapters/nats` package uses JetStream KV
  for the leadership lease).
- One Linux host per proxy peer (typical: same hosts as the
  PostgreSQL nodes for sidecar mode, separate hosts for microservice
  mode).
- The `pgman-proxy` binary on each host (download from the release
  assets or `make build` from this repo).
- Optional: `psql` for the verification step.

---

## Step 1 — Place the binary

```bash
sudo install -m 0755 ./pgman-proxy /usr/local/bin/pgman-proxy
```

Run as a non-root user (FR-013). Create one if it does not exist:

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin pgman-proxy
```

---

## Step 2 — Write the per-peer configuration file

`/etc/pgman-proxy/config.yaml` on each host. Replace `<...>` values.

```yaml
cluster:
  id: prod-east
node:
  id: <hostname>            # e.g., pg-east-a
peers:
  - pg-east-a
  - pg-east-b
  - pg-east-c

nats:
  url: tls://nats.internal:4222
  creds_file: /etc/pgman-proxy/nats.creds

proxy:
  listen_addr: 0.0.0.0:6432   # use 127.0.0.1:6432 for sidecar mode
  switch_policy: hard_close

postgres:
  bin_dir: /usr/lib/postgresql/17/bin
  data_dir: /var/lib/postgresql/data
  local_dsn_env: LOCAL_DSN     # set the env var in the unit file
  tls_mode: verify-full

obs:
  log_level: info
  metrics_addr: :9090
  health_addr: :9090
```

Set `LOCAL_DSN` in the unit file or sidecar runtime env:

```text
LOCAL_DSN=host=/var/run/postgresql user=postgres sslmode=disable
```

(For TLS-to-PG, set `sslmode=verify-full` in the DSN and supply a CA
bundle via `PGSSLROOTCERT`.)

---

## Step 3 — Install a supervisor unit

### Option A — systemd (host or sidecar)

`/etc/systemd/system/pgman-proxy.service`:

```ini
[Unit]
Description=pgman-proxy (active/active PostgreSQL proxy)
After=network-online.target postgresql.service
Wants=network-online.target

[Service]
Type=simple
User=pgman-proxy
Group=pgman-proxy
EnvironmentFile=/etc/pgman-proxy/env
ExecStart=/usr/local/bin/pgman-proxy --config /etc/pgman-proxy/config.yaml
Restart=on-failure
RestartSec=5
LimitNOFILE=65536
ProtectSystem=strict
ProtectHome=yes
NoNewPrivileges=yes
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
```

`/etc/pgman-proxy/env`:

```text
LOCAL_DSN=host=/var/run/postgresql user=postgres sslmode=disable
```

Enable and start on each peer:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now pgman-proxy
```

### Option B — Docker Compose (microservice demo)

See `deploy/compose/docker-compose.yml` in the repo. `docker compose up -d`
brings up three peers + a single NATS instance + the reference
PostgreSQL nodes from `pg-manager`'s example.

---

## Step 4 — Verify

On any peer (or any client host that can reach a peer):

```bash
# Liveness / readiness
curl -sf http://<host>:9090/healthz && echo OK
curl -sf http://<host>:9090/readyz  && echo READY

# Metrics
curl -s http://<host>:9090/metrics | grep -E '^pgman_proxy_(connections|leadership)' | head

# End-to-end
PGPASSWORD=... psql "host=<host> port=6432 user=app dbname=app" \
  -c 'SELECT pg_is_in_recovery(), inet_server_addr();'
```

The query MUST return `f` (not in recovery) regardless of which peer
you queried — every peer routes writes to the current leader.

---

## Step 5 — Trigger a failover (smoke test)

Stop the leader's PostgreSQL process. On the smoke host:

```bash
# Identify current leader
curl -s http://<any-peer>:9090/metrics | \
  grep '^pgman_proxy_leadership_state.*state="leader"'

# Watch the leader change
watch -n1 'curl -s http://<any-peer>:9090/metrics | grep pgman_proxy_leader_changes_total'
```

Within 5s (SC-002), `pgman_proxy_leader_changes_total` increments and
the structured log on every peer carries a `leader changed` event with
the same `cluster_id`.

---

## Step 6 — Drive a planned LCM operation

The LCM control plane runs on `:9091` (in standalone/microservice
modes; loopback-only in sidecar mode by default). Set a bearer token
in the unit-file env:

```text
PGMAN_PROXY_CONTROL_AUTH_TOKEN_ENV=PGMAN_PROXY_CONTROL_TOKEN
PGMAN_PROXY_CONTROL_TOKEN=<a long random token>
```

Then run a planned switchover:

```bash
# Read the current cluster status
curl -sf http://<any-peer>:9091/v1/status \
  -H "Authorization: Bearer $PGMAN_PROXY_CONTROL_TOKEN" | jq

# Switch leadership to peer-b
curl -sfX POST http://<any-peer>:9091/v1/switchover \
  -H "Authorization: Bearer $PGMAN_PROXY_CONTROL_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"target":"peer-b"}' | jq
```

The response carries `outcome: "accepted"` and an `engine_result`
payload from `pg-manager`. The action is recorded in **two** places:

1. `pgman_proxy_lcm_requests_total{operation="Switchover",outcome="accepted"}`
   increments on the peer that received the request.
2. The audit pipeline emits one record to the structured log and one
   to the NATS subject `pgman_proxy.<cluster_id>.audit.lcm`.

If the audit pipeline is unhealthy, the request is **rejected** with
`error.code = "audit_unavailable"` (FR-028). Read-only ops (`Status`,
`Diagnose`) are not subject to that gate.

The full operation surface is in `contracts/lcm.md`:
`Status`, `Diagnose`, `Switchover`, `Failover`, `Promote`, `Fence`,
`Unfence`, `UpdateTopology`, `TriggerBackup`, `PrepareUpgrade`,
`ExecuteUpgrade`.

---

## Troubleshooting

| Symptom                                                      | Likely cause                                               | Fix                                                                |
|--------------------------------------------------------------|------------------------------------------------------------|--------------------------------------------------------------------|
| Process exits with code `78` (`EX_CONFIG`)                   | Missing required key, inline secret, or unknown flag       | Run `pgman-proxy --print-config --config /etc/pgman-proxy/config.yaml` |
| Process exits with code `75` (`EX_DEPS`)                     | NATS unreachable or auth failure                            | `nc -zv <nats-host> 4222`; check `nats.creds_file` permissions     |
| Process exits with code `76` (`EX_LISTEN`)                   | `proxy.listen_addr` already bound                          | Stop the conflicting process; verify with `ss -tlnp`               |
| `/readyz` returns `503` permanently                          | Lease lost, listener closed, or singleton-claim budget exhausted | Inspect logs for `lease renewal failed` or `manager start failed` |
| `pgman_proxy_lease_renewal_failures_total` keeps incrementing | NATS reachable but unstable (packet loss / firewall)       | Diagnose at the network layer; the proxy is correctly fail-closed  |
| Clients see `connection reset` after a failover              | Expected behaviour — `switch_policy: hard_close`          | Add reconnect logic on the client; or set `switch_policy: drain`   |

---

## What is intentionally NOT here

- **Kubernetes manifests**: out of scope (Constitution VII; FR-015).
  A separate downstream project owns the Kubernetes/operator surface.
- **Helm charts**: out of scope.
- **VIP / keepalived / floating IP setup**: out of scope (FR-004); this
  proxy is direct-TCP only.
- **Read/write splitting**: out of scope for v1 (Assumptions in
  `spec.md`); all client traffic is routed to the current leader.
- **Cert rotation / ACME**: out of scope for v1; operators provide
  TLS material via `creds_file` / `tls_*_file` keys.
