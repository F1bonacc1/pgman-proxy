# Quickstart — Stand up a 3-peer pgman-proxy cluster with embedded NATS

**Feature**: `002-embedded-nats-cluster`
**Audience**: A platform engineer following `SC-001` — deploy a working
3-peer proxy cluster fronting an existing pg-manager HA setup, with no
external NATS process anywhere on any host, in under 15 minutes.

## Prerequisites

- Three Linux hosts (`peer-a`, `peer-b`, `peer-c`) reachable from each
  other on TCP port `6222`.
- An existing 3-node `pg-manager` PostgreSQL HA cluster reachable from
  the three hosts.
- The `pgman-proxy` binary placed on each host (e.g., at `/usr/local/bin/pgman-proxy`).
- A user `pgman-proxy` on each host with a writable directory at
  `/var/lib/pgman-proxy/` (Constitution: run as non-root).

## Step 1 — Generate the shared cluster credential (≈ 30 sec)

On a single workstation, generate **one** strong password — every peer
in the cluster will use the same one (RD-001a: NATS v2.14 cluster
routes only support shared username/password, not per-peer NKeys):

```sh
$ pgman-proxy cluster-secret-gen
password: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
# 52-character base32-lowercase, 32 random bytes of entropy
```

Distribute the password to all three hosts via your secret manager
(or plain SSH + restrictive `chmod 0600` for a quickstart). The
**username** is your operator-chosen non-secret identifier (e.g.
`pgman-proxy-prod`); it MAY appear in plaintext config.

## Step 2 — Generate cluster TLS material (≈ 2 min)

For a quickstart, use a self-signed CA and three server certs (one per
peer):

```sh
$ openssl req -new -x509 -days 365 -newkey ed25519 -nodes \
    -keyout cluster-ca.key -out cluster-ca.pem \
    -subj '/CN=pgman-proxy-cluster-ca'
$ for peer in peer-a peer-b peer-c; do
    openssl req -new -newkey ed25519 -nodes \
      -keyout $peer-key.pem -out $peer.csr \
      -subj "/CN=$peer.pgman-proxy"
    openssl x509 -req -in $peer.csr -CA cluster-ca.pem -CAkey cluster-ca.key \
      -CAcreateserial -days 365 -out $peer-cert.pem
  done
```

Distribute each peer's `*-cert.pem` and `*-key.pem` to its host, plus
`cluster-ca.pem` to all three. (For production, use your existing PKI;
this step replaces nothing.)

## Step 3 — Write the per-peer config (≈ 5 min, parallelisable)

On each host (`peer-a`, `peer-b`, `peer-c`), drop a config file at
`/etc/pgman-proxy/config.yaml`:

```yaml
# Same data-plane / control-plane / observability blocks as feature 001.
# Only the cluster: block is new in feature 002.

cluster:
  cluster_id:    prod-east-1
  cluster_name:  pgman-proxy-prod
  node_id:       peer-a              # change per host
  declared_size: 3

  client_listen:
    host: 127.0.0.1
    port: 4222
  routes_listen:
    host: 0.0.0.0
    port: 6222

  peers:                              # this peer's siblings only
    - peer-b.internal:6222
    - peer-c.internal:6222

  tls:
    cert_file: { file: /etc/pgman-proxy/peer-a-cert.pem }
    key_file:  { file: /etc/pgman-proxy/peer-a-key.pem }
    ca_file:   { file: /etc/pgman-proxy/cluster-ca.pem }

  username: pgman-proxy-prod                  # cluster credential username (non-secret; same on every peer)
  password: { env: PGMAN_PROXY_CLUSTER_PASSWORD }   # SecretRef — same value on every peer

  jetstream_dir: /var/lib/pgman-proxy/jetstream
```

Adjust `node_id`, `peers`, and `cert_file`/`key_file` per host. **Every
peer ships the same `username` and resolves the same `password`** —
that's how cluster routes mutually authenticate (RD-001a). Set
`PGMAN_PROXY_CLUSTER_PASSWORD` in the systemd unit's `EnvironmentFile=`
on each host to the value generated in Step 1.

## Step 4 — Start the peers (≈ 1 min)

```sh
# On each host:
$ sudo systemctl start pgman-proxy
$ sudo systemctl status pgman-proxy
```

The systemd unit (`deploy/systemd/pgman-proxy.service`) ships with
`ExecReload=/bin/kill -HUP $MAINPID` so step-7 rotations work.

## Step 5 — Verify the cluster is healthy (≈ 1 min)

On any host:

```sh
$ curl -sk https://localhost:9091/v1/status | jq '.cluster.embedded_nats'
{
  "server_name": "peer-a",
  "ready": true,
  "client_listen_addr": "127.0.0.1:4222",
  "routes_listen_addr": "0.0.0.0:6222",
  "tls_enabled": true,
  "routes_meshed": 2,
  "replicas_factor": 3,
  "replicas_overridden": false,
  "jetstream_storage_bytes": 12345,
  "storage_degraded": null,
  "last_route_up_at":   "2026-05-09T15:32:11.000Z",
  "last_route_down_at": null,
  "last_reload_at":     null
}
```

`routes_meshed: 2` on a 3-peer cluster means this peer sees both its
siblings — the SC-001 success criterion.

Confirm there is no external `nats-server` process on any host:

```sh
$ pgrep -af nats-server
# (no output)
```

## Step 6 — Run a write through the cluster (≈ 1 min)

```sh
$ psql 'host=peer-a port=5432 user=app dbname=app' -c \
  'BEGIN; INSERT INTO heartbeat(ts) VALUES (now()); COMMIT;'
INSERT 0 1
```

The proxy on `peer-a` routes the write to whichever peer holds the
leadership lease (which may or may not be peer-a). Repeat against
`peer-b` and `peer-c`; all three should accept the write.

## Step 7 — Rotate the cluster credential (≈ 1 min, anytime)

Demonstrates FR-010a + FR-014a (no flag-day, hot-reload):

Demonstrates FR-010a (single-step rotation per RD-001a) + FR-014a
(SIGHUP-driven hot-reload of the password):

```sh
# 1. Generate a new cluster password.
$ pgman-proxy cluster-secret-gen
password: bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb

# 2. Update PGMAN_PROXY_CLUSTER_PASSWORD on every peer to the new value
#    (your secret-manager / EnvironmentFile workflow).
$ for h in peer-a peer-b peer-c; do
    ssh $h 'sudo sed -i "s|^PGMAN_PROXY_CLUSTER_PASSWORD=.*|PGMAN_PROXY_CLUSTER_PASSWORD=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb|" /etc/pgman-proxy/secrets.env'
  done

# 3. SIGHUP each peer to re-read the new password and re-handshake routes.
$ for h in peer-a peer-b peer-c; do
    ssh $h 'sudo systemctl reload pgman-proxy'  # = SIGHUP
  done
```

Confirm via `pgman_proxy_embedded_nats_routes_meshed == 2` on every peer
throughout the procedure (cluster never lost quorum). NATS server's
`ReloadOptions` re-authenticates existing routes against the new
credential; peers that haven't yet picked up the new password retry
on the next heartbeat and re-handshake successfully.

## Verification matrix

| Spec ID | What you've shown |
|---|---|
| SC-001 | 3-peer cluster up in under 15 minutes; no `nats-server` process on any host |
| SC-002 | Single-peer would start in under 5 s (try with `declared_size: 1`, `peers: []`) |
| SC-003 | Kill peer-c; remaining peers retain `routes_meshed=1` and a leader within 5 s p99 |
| SC-004 | Step 5's `/v1/status` JSON answers all three operator questions in this quickstart |
| SC-006 | Same integration test suite from 001 runs unchanged against this topology |
| SC-007 | Step 7 rotates the cluster credential with zero `nats` CLI invocations |
| SC-009 | Adding a stray `nats: { url: ... }` block to `config.yaml` and restarting fails closed with the migration message |

## Troubleshooting

- **Embedded NATS fails to start** → check exit code (78=config,
  73=storage, 75=startup-timeout) and the `embedded_nats.server_stopped`
  event with `reason` field.
- **`routes_meshed=0`** → check sibling host firewalls allow inbound
  6222/tcp; check `cluster_route.auth_failed{kind=...}` events for
  whether `invalid_credential`, `cluster_name_mismatch`, or
  `protocol_error` is the cause (per RD-001a kinds).
- **`storage_degraded`** event fires → disk full or `jetstream_dir`
  unwritable. Peer self-fences automatically; resolve the storage issue
  and the peer recovers on its own (or restart it).
- **Legacy 001 config loaded** → exit 78 with explicit migration
  message naming the deprecated `nats.*` keys; follow the migration
  checklist in `contracts/config.md`.
