# Contract — Cluster Credentials

**Feature**: `002-embedded-nats-cluster`
**Phase**: 1
**Supersedes**: the deleted `nkey-credentials.md` (the per-peer NKey
design from `/speckit-clarify` Q1 was invalidated during
`/speckit-implement` Phase 2 by upstream NATS v2.14's protocol
constraint that cluster-route auth supports only a shared
username/password pair — see research.md RD-001a).

This contract specifies how operators generate, distribute, store, and
rotate the **shared cluster credential** that authenticates cluster
routes between embedded NATS servers (FR-009, FR-010, FR-010a).

Per-peer identity in audit logs comes from the NATS **server-name**
field, which the proxy sets to its `cluster.node_id` at startup. The
server name appears in every `route_up` / `route_down` event so a
cross-peer audit is forensically reconstructable from the structured
log alone — even though the wire-level credential is the same on every
peer.

## Generation

Operators generate **one** strong cluster password using the bundled
subcommand. The username is operator-chosen (a non-secret cluster
identifier).

```sh
$ pgman-proxy cluster-secret-gen
password: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
# 52-character base32-lowercase, 32 random bytes of entropy
```

The subcommand:

- Calls `crypto/rand` directly; no NATS-ecosystem CLI required.
- Prints the password to stdout. Operators redirect or pipe.
- Exits 0 on success, non-zero on entropy failure.
- Does **not** write to disk; piping into a file (or directly into the
  operator's secret manager) is the operator's choice.

## Distribution

For a 3-peer cluster, **every peer ships the same credential**:

```yaml
# peer-a, peer-b, peer-c — identical cluster.username + cluster.password
cluster:
  name: pgman-proxy-prod
  username: pgman-proxy-prod                 # MAY be in plaintext config (non-secret)
  password: { env: PGMAN_PROXY_CLUSTER_PASSWORD }   # SecretRef — env / file / secret-manager
```

The username MAY appear in plaintext config because it is non-secret
(it's just a cluster identifier presented during the route handshake).
The password MUST be sourced via SecretRef.

## Storage rules

| Material | Plaintext config OK? | SecretRef required? | Audit-logged on use? |
|---|---|---|---|
| Cluster username | YES (non-secret) | NO | YES (full username on `route_up`) |
| Cluster password | NO | YES | YES (8-character prefix only) |
| TLS cert/key (RD-007) | NO | YES | NO |

## Rotation procedure (FR-010a)

The shared-credential model collapses the original three-step NKey
rotation procedure into a single SIGHUP-driven flow. NATS server
v2.14's `ReloadOptions` accepts changes to `Cluster.Authorization`
without restarting the server and re-handshakes existing routes
against the new credential.

### Single step: rotate password across the cluster

1. Generate the new password on a workstation:
   `pgman-proxy cluster-secret-gen`.
2. Update **every peer's** SecretRef target to the new password
   (e.g., update the env-var value in each peer's systemd
   `EnvironmentFile`, or rotate the secret-manager entry both peers
   resolve from).
3. Send `SIGHUP` to **every peer** (sequence is unimportant; all peers
   converge to the new credential within the SIGHUP fan-out).

   ```sh
   $ for h in peer-a peer-b peer-c; do
       ssh $h 'systemctl reload pgman-proxy'
     done
   ```

4. Each peer logs `embedded_nats.reload_applied{password_rotated=true}`
   with the new password's 8-character prefix.

The cluster maintains quorum throughout: NATS retains existing routes
during the credential swap and re-handshakes opportunistically — there
is no window in which a peer is unable to reach its siblings.

### Why the original three-step procedure is no longer needed

The Q1 NKey design used per-peer identity at the wire level, which
required adding new public keys to authorized lists *before* a peer
could present them. With shared credentials, every peer's credential
is identical, so there's no "add then swap then remove" ordering — a
single SIGHUP swap is atomic from the cluster's perspective.

## Failure scenarios

| Scenario | Expected behaviour |
|---|---|
| Operator rotates password on only some peers | Mid-rotation cluster has peers with mismatched passwords. NATS server's route handshake rejects mis-credentialed routes; siblings emit `cluster_route.auth_failed{kind="invalid_credential"}`. Cluster degrades to whichever subset shares a password. Operator runbook: complete the SIGHUP fan-out across remaining peers; the cluster re-converges automatically. |
| Operator's password in env is corrupted | Embedded server fails to validate config (FR-007); exit code 78 (CONFIG). |
| Password is shorter than 16 bytes | Validation fails at startup or SIGHUP; pre-existing peers are unaffected. |
| Operator leaks a peer's password | Treat as a security incident: rotate the cluster credential using the procedure above (every peer rotates because every peer shared the leaked credential). The 8-character prefix in audit logs lets the team identify when the leaked password was last accepted. |
| Operator forgets `cluster.password` entirely | Validation fails at startup with the FR-009 error pointing at the SecretRef configuration. |

## Validation rules

- `cluster.username` MUST be non-empty when `cluster.declared_size > 1`.
- `cluster.password` MUST be at least 16 bytes after SecretRef
  resolution (the function rejects anything shorter as too weak).
- Both MUST be sourced via SecretRef-equivalent indirection;
  in-config plaintext values for the password are fail-closed at
  validation.
- The username MAY be set inline in plaintext config (it is not
  secret).

## Out of scope (v1)

- Per-peer NKey credentials at the wire level. NATS v2.14 doesn't
  support this; revisiting requires either an upstream protocol change
  or an mTLS-as-identity pivot (the user explicitly declined the
  latter in `/speckit-clarify` Q2).
- Account-scoped credentials (`nats-io/jwt/v2`). Adds an operator
  persona FR-014 explicitly says shouldn't be required.
- Automatic password rotation on a timer. The proxy provides the
  primitives; the operator decides when to rotate.
- mTLS-as-identity. Per the RD-001a / Q2 sequence, identity comes from
  the NATS server-name field; TLS is transport (RD-007).
