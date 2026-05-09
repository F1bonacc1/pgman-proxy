# Contract — Configuration Schema (delta from feature 001)

**Feature**: `002-embedded-nats-cluster`
**Phase**: 1
**Status**: Locked at `/speckit-plan` time; changes require a follow-up
spec.

This contract documents what changes in the proxy's configuration schema
when feature 002 lands. Source of truth for parsing and validation lives
in `internal/config/`.

## Removed keys (from 001)

The entire `nats:` block from feature 001 is **removed**:

```yaml
# REMOVED — DO NOT USE — fail-closed at validation
nats:
  url: nats://...
  connect_timeout: 5s
  reconnect_wait: 2s
  max_reconnects: -1
  creds_file: /etc/.../nats.creds
  token_env: NATS_TOKEN
```

Validation rejects any of these keys with the error:

```text
config: nats.* keys are no longer supported (feature 002 removed external
NATS); see cluster.* for the embedded coordination plane and migration
guide in specs/002-embedded-nats-cluster/quickstart.md
```

## Added keys

```yaml
cluster:
  cluster_id: prod-east-1            # required; was nats.cluster_id implicitly
  cluster_name: pgman-proxy-prod     # required; presented on cluster routes
  node_id: peer-a                    # required; unique per peer
  declared_size: 3                   # required; drives replication-factor derivation (FR-011a)

  client_listen:
    host: 127.0.0.1                  # default; non-loopback requires explicit override
    port: 4222                       # default

  routes_listen:
    host: 0.0.0.0                    # default in HA mode
    port: 6222                       # default
    enabled: true                    # default true in HA; false permitted only in single-peer

  peers:                             # HOT-RELOADABLE on SIGHUP (FR-014a)
    - peer-b.internal:6222
    - peer-c.internal:6222

  tls:
    cert_file: { file: /etc/pgman-proxy/cluster-cert.pem }
    key_file:  { file: /etc/pgman-proxy/cluster-key.pem }
    ca_file:   { file: /etc/pgman-proxy/cluster-ca.pem }
    plaintext_explicit_ack: false    # default false; true requires audit log + named opt-in

  username: pgman-proxy-prod              # cluster credential username (non-secret; FR-009 / RD-001a)
  password:                                 # SecretRef — never plaintext-config-resident; HOT-RELOADABLE
    env: PGMAN_PROXY_CLUSTER_PASSWORD       # generate via `pgman-proxy cluster-secret-gen`

  jetstream_dir: /var/lib/pgman-proxy/jetstream   # required when declared_size >= 2
  replication_factor_override: null  # default null; setting it is audit-logged at every startup

  connect_timeout: 5s
  reconnect_wait: 2s
```

## Validation matrix

| Configuration shape | Outcome | FR / SC |
|---|---|---|
| Legacy `nats:` block present | Fail-closed at validation | FR-002, SC-009 |
| `cluster.declared_size > 1` AND `cluster.peers` empty | Fail-closed at validation | FR-008 |
| `cluster.declared_size = 1` AND `cluster.peers` non-empty | Warn + ignore peers | spec assumption |
| `cluster.routes_listen` non-loopback AND `cluster.tls` unset AND `plaintext_explicit_ack=false` | Fail-closed at validation | FR-010b |
| `cluster.routes_listen` non-loopback AND `cluster.username` or `cluster.password` unset | Fail-closed at validation | FR-009 |
| `cluster.password` (resolved) shorter than 16 bytes | Fail-closed at validation | FR-010 |
| `cluster.declared_size >= 2` AND `cluster.jetstream_dir` empty | Fail-closed at validation | FR-011 |
| `cluster.jetstream_dir` set but unwritable | Fail-closed at startup | spec edge case |
| `cluster.peers` contains this peer's own `routes_listen` address | Warn + exclude (no fail) | FR-020 |
| `cluster.replication_factor_override` set | Accept; audit-log at every startup | FR-011a |
| `cluster.password` value present inline in plaintext config (not via SecretRef) | Fail-closed at validation | FR-010 |

## Hot-reload allow-list (SIGHUP)

Only these two keys are reloadable; everything else is startup-only and
emits a structured `reload_skipped` warning if changed:

- `cluster.peers`
- `cluster.password` (the SecretRef target value, re-read from source on SIGHUP)

Specifically NOT reloadable:

- `cluster.cluster_id`, `cluster.cluster_name`, `cluster.node_id`
- `cluster.username` (paired with the password but treated as identity;
  rotating it would change the route handshake username on one side
  without coordination — operators rotate username via restart only)
- `cluster.client_listen`, `cluster.routes_listen`
- `cluster.tls.*`
- `cluster.declared_size`, `cluster.replication_factor_override`
- `cluster.jetstream_dir`
- All non-`cluster.*` keys (data-plane, control-plane, observability)

## Defaults summary

| Key | Default | Mode-specific |
|---|---|---|
| `cluster.client_listen.host` | `127.0.0.1` | All modes |
| `cluster.client_listen.port` | `4222` | All modes |
| `cluster.routes_listen.host` | `0.0.0.0` | HA modes |
| `cluster.routes_listen.port` | `6222` | HA modes |
| `cluster.routes_listen.enabled` | `true` | HA modes; `false` permitted in single-peer |
| `cluster.tls.plaintext_explicit_ack` | `false` | All modes |
| `cluster.declared_size` | (required, no default) | All modes |
| `cluster.connect_timeout` | `5s` | All modes |
| `cluster.reconnect_wait` | `2s` | All modes |
| `cluster.replication_factor_override` | `null` | All modes |

## Migration from 001

Operators upgrading a 001 deployment to 002 perform exactly these steps,
**in order**:

1. Generate one strong cluster password via `pgman-proxy
   cluster-secret-gen` (RD-003 / RD-001a); store it in env or
   secret-manager per FR-010.
2. Choose a non-secret `cluster.username` (e.g., `pgman-proxy-prod`) —
   this MAY appear in plaintext config.
3. Replace the entire `nats:` block in each peer's config with the
   `cluster:` block above. Every peer ships the **same**
   `cluster.username` and resolves the **same** `cluster.password`
   SecretRef target.
4. Stop the external NATS deployment (per the user's stated intent —
   it is "a mistake").
5. Roll the peers per the rolling-restart procedure documented in
   `quickstart.md`.

If the operator skips step 3 (legacy `nats.url` left in config), the
proxy fails closed at validation per FR-002 — they are told exactly
what's wrong and where to look. SC-009 asserts this in CI.
