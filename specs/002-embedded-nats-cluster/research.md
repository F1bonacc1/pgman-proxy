# Research — Embedded NATS Cluster for pgman-proxy

**Phase**: 0 (pre-design)
**Feature**: `002-embedded-nats-cluster`
**Date**: 2026-05-09

This document resolves every NEEDS CLARIFICATION the plan would otherwise
carry into Phase 1 design. Each entry follows the
**Decision / Rationale / Alternatives considered** shape required by the
plan template. Decisions are referenced from `plan.md` and downstream
contracts as `RD-NNN`.

## RD-001 — Embedded NATS bootstrap pattern

**Decision**: Programmatically embed NATS using the upstream Go module
`github.com/nats-io/nats-server/v2/server` package. Per peer:

1. Build `*server.Options` from `config.ClusterConfig` (host/port for
   client listener, host/port for cluster routes, route URLs, NKey, TLS,
   JetStream storage dir, server name = pgman-proxy node ID).
2. `s, err := server.NewServer(opts)` then `go s.Start()`.
3. Block in startup until `s.ReadyForConnections(timeout)` — fail-closed on
   timeout (FR-007).
4. The in-process pg-manager adapter dials the loopback client listener
   address (`nats://127.0.0.1:<client_port>`) — same `nats.go` client API
   the adapter already uses against external NATS; the embedded server is
   transparent to it (FR-015).
5. On `SIGTERM`/`SIGINT`, call `s.Shutdown()` after the data-plane drain
   completes; log lifecycle event `embedded_nats.server_stopped`.

**Rationale**: This is the upstream-supported embedded pattern. It uses
the same code path that the standalone `nats-server` binary uses; no
divergence to track. `ReadyForConnections` gives us a deterministic
fail-closed gate. Account-scoped JWT credentials (`nats-io/jwt/v2`) are
out of scope: shared cluster credentials with per-peer audit identity
(see RD-001a) is sufficient for v1, and accounts add a second concept
the operator persona explicitly should not need to learn (FR-014).

**Alternatives considered**:

- *Run nats-server as a sibling process spawned by pgman-proxy.* Rejected:
  this is "external NATS" with a different process boundary, not embedded.
  Defeats the operational-simplicity premise; doubles the failure surface
  (proxy alive but child crashed); makes graceful-shutdown sequencing
  harder.
- *Custom-built minimal coordination plane (Raft + KV in-process,
  bypassing NATS entirely).* Rejected: violates Constitution IV by
  duplicating the engine the pg-manager adapters already speak to.
  Would force a fork of `pg-manager/adapters/nats`. The user's stated
  goal is embedded NATS, not custom consensus.
- *NATS server hosted under `tetragon/embed`-style or similar
  abstraction.* Rejected: no upstream support; we'd be the only consumer.

## RD-001a — Cluster-route auth: upstream constraint discovery

**Decision** (logged 2026-05-09 during `/speckit-implement` Phase 2):
NATS server v2.14 (`server/opts.go:2043-2057`) explicitly rejects
multiple-user and token-based cluster authorization; the only
upstream-supported credential for cluster routes is **a single shared
`Cluster.Username` + `Cluster.Password` pair** per cluster. Per-peer
NKey credentials on cluster routes are not a feature of the NATS
protocol — NKeys exist for *client* connections, not for *route*
handshakes between sibling servers.

This invalidates the literal reading of `/speckit-clarify` Q1 ("NKey
(Ed25519 seed) per peer with a shared cluster name; per-peer identity
in audit"). The user, when shown the constraint, chose the pragmatic
reconciliation: **shared cluster password + server-name identity.**

The implementation:

1. Cluster authentication uses `Cluster.Username` + `Cluster.Password`,
   sourced via `SecretRef` (FR-010 unchanged in spirit; private
   material still never plaintext-config-resident).
2. Per-peer identity in audit logs comes from the NATS **server name**
   field, which is set to `pgman-proxy.cluster.node_id` at startup.
   NATS includes the server name in route INFO messages, and the
   embedded server's accept-route hook surfaces it as
   `peer_node_id` in `embedded_nats.route_up` / `route_down` events
   (`contracts/observability.md`).
3. Rotation of the shared cluster password is a flag-day operation
   (every peer must reload to the new password). To avoid downtime,
   operators run the rotation during a maintenance window: SIGHUP each
   peer with the new password — NATS server's `ReloadOptions` accepts
   the cluster authorization change on existing routes. This is
   simpler than the three-step NKey rotation procedure originally
   designed; spec FR-010a is amended in place.
4. The `pgman-proxy nkey-gen` subcommand from RD-003 is repurposed to
   generate **strong cluster passwords** (32 random bytes,
   base32-encoded) — the operator-facing primitive remains, just with
   different naming. The wire-level credential is no longer Ed25519.

**Spec amendments triggered by this discovery** (applied 2026-05-09):

- FR-009: rephrased from "per-peer NKey (Ed25519) credentials scoped to
  a shared cluster name" to "shared username and password per
  cluster, sourced via SecretRef; cluster name guards against unrelated
  clusters meshing".
- FR-010: simplified to "shared cluster credential sourced via SecretRef".
- FR-010a: rotation procedure simplified — single-step (password
  rotation across all peers via SIGHUP); no flag-day required because
  NATS supports route re-authentication on options reload.
- Q1 clarification line in *Clarifications* section: amended to note the
  upstream constraint discovered during implementation.
- Key Entities: "Peer NKey seed" / "Authorized-keys list" replaced with
  "Cluster credential" + "Cluster name guard"; the NKey-as-protocol-
  identity concept is dropped.
- `contracts/nkey-credentials.md`: rewritten as
  `contracts/cluster-credentials.md` (file rename); content updated
  for shared-password model.

**Rationale**: NATS server is the upstream we depend on (Constitution
IV — Thin Scaffold). We do NOT fork or extend NATS to add per-peer
NKey-on-routes; we work within the protocol. Per-peer audit identity
is preserved via server-name; per-peer credential rotation is replaced
by single-secret rotation, which is simpler operationally and arguably
easier to reason about for the no-NATS-CLI-knowledge persona (FR-014).

**Alternatives offered to the user** (and rejected by selection):

- *Switch to mTLS-as-identity*: would have reversed Q2 to require
  mutual TLS. More PKI work; preserves per-peer identity at wire level.
  Not selected.
- *Hash-of-authorized-keys derived shared password*: preserves NKey
  ergonomics but adds complexity and a custom-built scheme. Not
  selected.
- *Pause and re-clarify*: would have stalled feature 002. User chose
  forward progress.

## RD-002 — JetStream KV replication factor (Replicas) plumbing

**Decision**: **Upstream-first.** File a feature request and an
accompanying PR against `github.com/f1bonacc1/pg-manager` to extend the
`adapters/nats` constructors with a `WithReplicas(int)` option (or
equivalent), threading it into the `js.CreateKeyValue(...)` call site in
`adapters/nats/bucket.go`. `pgman-proxy` consumes the option, passing the
value derived in RD-004. Until the upstream change is merged, the
fallback below applies — but the plan **does not ship** without the
upstream change (Constitution IV is non-negotiable on this point).

**Fallback (only if upstream blocks for >2 weeks; needs a Constitution IV
exception logged in this spec's Complexity Tracking)**: pre-create the
cluster KV bucket inside `internal/embedded/bucket.go` **before** any
`pg-manager` adapter is constructed. The bucket-name format
(`bucketName(clusterID)`) is part of pg-manager's public adapter contract
(it's how the adapter looks the bucket up via `js.KeyValue(...)`). When
the adapter then calls `KeyValue(...)`, it finds the existing bucket and
adopts it without re-creating. This is a workaround, not a design — it
couples this repo to an internal naming convention of the adapter, which
the constitution Principle IV explicitly says we should not do.

**Rationale**: pg-manager's current `bucket.go` hardcodes `History: 8`
and omits `Replicas`, defaulting to 1. Without exposure of `Replicas` in
the adapter API, the FR-011a derivation table is unreachable through the
sanctioned API. Two paths: extend the upstream API (constitutional), or
work around it by pre-creating the bucket (un-constitutional). Choose
the constitutional path; document the un-constitutional fallback so the
team has an explicit out if upstream blocks, and so the constitutional
exception is recorded transparently rather than smuggled in.

**Alternatives considered**:

- *Skip per-cluster replication; accept R=1 always.* Rejected: makes
  the FR-011a guarantee a lie. R=1 means a single-peer-loss permanently
  loses the leadership lease state — the proxy would self-fence
  correctly, but the cluster could not converge on a new leader without
  manual intervention to restore the bucket. Defeats the active/active
  premise (Constitution III).
- *Compose a parallel KV bucket with the desired replicas and switch
  the adapter's lookup to it.* Rejected: same coupling problem as the
  fallback, and adds a second stream the adapter has no awareness of.

## RD-003 — NKey credential generation and rotation tooling

**Decision**: Ship a `pgman-proxy nkey-gen` subcommand that produces a
freshly-generated NKey seed (Ed25519, NATS server-role keytype) and its
public key on stdout in the canonical NATS-encoded forms. The subcommand
uses upstream `nats-io/nkeys` (`nkeys.CreateServer()`); the proxy does
not introduce its own crypto. Operators rotate NKeys per the FR-010a
three-step procedure documented in `contracts/nkey-credentials.md`.

**Rationale**: A subcommand on the same binary is the lowest-friction
path to the operator and keeps the "no NATS-specific operator persona"
promise (FR-014). Operators never invoke `nsc`, `nk`, or any
NATS-ecosystem CLI to use the proxy. The subcommand is ~30 LOC.

**Alternatives considered**:

- *Require operators to use upstream `nk` or `nsc`.* Rejected: violates
  FR-014's no-new-persona promise.
- *Auto-generate seeds at first startup.* Rejected: dangerous default —
  a secret material that operators don't know exists is a secret material
  they don't rotate, don't back up, and can't redeploy when a peer is
  rebuilt. Better to make seed generation an explicit, documented step.
- *Bundle seed-generation into the deploy/ recipes.* Rejected: deploy
  recipes are reference, not load-bearing — operators copy and modify
  them. The subcommand is the canonical surface.

## RD-004 — Replication-factor derivation from declared cluster size

**Decision**: Implement the FR-011a table verbatim in
`internal/embedded/replicas.go` as a pure function:

```text
func DeriveReplicas(declaredSize int) (replicas int, warning string) {
    switch {
    case declaredSize <= 0: panic("invariant: declared cluster size must be ≥ 1")
    case declaredSize == 1: return 1, ""
    case declaredSize == 2: return 2, "two-peer cluster has zero peer-loss tolerance; scale to 3 ASAP"
    default:                return 3, "" // capped at 3
    }
}
```

Override path: `cluster.replication_factor_override` in config — when set,
emits a `cluster.replicas_overridden` warning event with both the derived
and override values, plus the override is logged at every startup.

**Rationale**: A pure function with three branches is testable in
isolation and readable at a glance. The cap at 3 reflects NATS's own
operational guidance: R=5 has marginal availability gains for
coordination workloads (which write tens of events per minute, not per
second) and increases write-amp considerably. Operators who genuinely
need R=5 (multi-region) can use the override and accept the extra audit
noise.

**Alternatives considered**:

- *Smooth scaling (R = min(declaredSize, 5)).* Rejected: gives R=4 and
  R=5 sizes that are NATS-officially "supported but not recommended" for
  KV without a compelling reason. The use case here doesn't need it.
- *Operator-configured R always.* Rejected: shifts a "what's the right
  R for my N peers" question onto the operator; we know the answer.

## RD-005 — Memory & CPU baseline for embedded NATS at idle (sets SC-005)

**Decision**: Adopt the following budgets, anchored on upstream
nats-server idle measurements and validated in CI smoke once Phase 2
implementation lands:

- **Memory**: 60 MB RSS p95 cap at idle, single-peer, JetStream enabled
  with file storage on a 256 MB sidecar pod alongside PostgreSQL.
- **CPU**: < 0.5% of one core p95 at idle.

The plan commits these numbers; SC-005 in the spec is updated from "TBD"
to these caps in `/speckit-tasks` step.

**Rationale**: Upstream nats-server idle benchmarks (publicly published
synadia/nats-server release notes, version 2.10+) report ~40 MB RSS for
a single-server JetStream-enabled instance with no streams of meaningful
size. A 60 MB cap leaves 50% headroom for the leadership-lease KV stream
and ordinary garbage churn. CPU is ~0.1–0.2% of a core idle in those
benchmarks; 0.5% leaves margin for the SIGHUP reload path and route
heartbeats.

**Alternatives considered**:

- *Empirical-only — set the cap from a measurement of the actual binary
  before declaring SC-005.* Rejected: would block the spec from being
  testable until after implementation; we'd be writing a spec around a
  number rather than the other way round. Better to set a defensible
  ceiling now and assert it in CI.
- *Higher cap (200 MB) for safety.* Rejected: too lax; the sidecar
  argument needs a tight number to be a real argument.

## RD-006 — Rolling-restart timing budget for a 3-peer upgrade (sets SC-008)

**Decision**: 60 s end-to-end for rolling-restarting all 3 peers of a
3-peer cluster. Per-peer budget: 20 s = 5 s drain + 5 s exit + 5 s
new-binary boot + 5 s embedded-NATS re-mesh + leadership re-confirm.
Asserted by `tests/integration/embedded_cluster_test.go` invoking the
restart procedure.

**Rationale**: 001's SC-002 sets a 5 s p99 leader-failover budget;
restart adds drain + boot. The drain budget is already configured by
001's FR-014 (per-peer shutdown budget). 5 s for embedded-NATS re-mesh
is comfortably above NATS's typical sub-second route convergence time.

**Alternatives considered**:

- *30 s per peer (90 s total).* Rejected: too lax — the operator-facing
  story is "rolling restart is fast"; 90 s makes it not fast.
- *Set per-peer only and let total = 3 × per-peer.* Rejected: the total
  budget includes overlap (peer 2 can begin draining while peer 1's
  embedded NATS is still re-meshing); the integration test should
  measure the overlap-aware end-to-end.

## RD-007 — Cluster-routes TLS configuration shape

**Decision**: TLS for cluster routes uses the upstream nats-server
`Options.Cluster.TLSConfig` `*tls.Config` (server-role) field, plus a
peer-cert verification path. Configuration keys:

- `cluster.tls.cert_file` — server certificate, PEM-encoded.
- `cluster.tls.key_file` — server private key, PEM-encoded.
- `cluster.tls.ca_file` — CA bundle used to verify sibling-presented
  certificates on inbound routes.
- `cluster.tls.plaintext_explicit_ack` — bool, default `false`. The only
  way to bind plaintext on a non-loopback address (FR-010b).

mTLS-as-identity is **not** wired (Q2 clarification: NKey provides
identity; TLS provides transport). The `ca_file` exists so that the TLS
handshake itself isn't trivially MITM-able, not so that peers identify
each other by cert subject.

**Rationale**: Reuses the upstream nats-server TLS surface verbatim. No
new key types. The `ca_file` requirement for the inbound side prevents
the trivial "any cert is accepted" mode.

**Alternatives considered**:

- *Self-signed + skip-verify.* Rejected: any trusted-by-default plaintext
  variant violates Principle II (fail-closed safety).
- *Use system trust store.* Rejected: a coordination plane MUST NOT
  inherit the host's web-PKI trust posture; cross-trust accidents are
  too easy.

## RD-008 — Constitution amendment v1.1.0 → v1.2.0

**Decision**: MINOR amendment. Edit two locations in
`.specify/memory/constitution.md`:

1. **Architecture Overview** (top of *Core Principles*): change
   "coordination across active replicas (leader election, membership,
   control-plane events) flows through NATS" → "coordination across
   active replicas (leader election, membership, control-plane events)
   flows through a NATS cluster **embedded in the proxy peers
   themselves**".
2. **Additional Constraints → Topology & Dependencies**: replace "NATS
   is a hard runtime dependency for clustered operation. The proxy MUST
   document the minimum required NATS feature set" with "NATS is
   embedded in every proxy peer; the proxy ships with a bundled NATS
   server and MUST NOT require an external NATS service. The minimum
   required NATS feature set MUST be documented; bundled-version bumps
   that change wire compatibility or on-disk format are MAJOR-version
   events for the proxy."

Sync impact report at the top of the constitution gets a new entry
documenting the change. Bump from `1.1.0` to `1.2.0`. Last-amended date
becomes 2026-05-09. The amendment lands as part of the first execution
task in `/speckit-tasks` (see `contracts/constitution-amendment.md`
for the full diff text).

**Rationale**: No principle is removed or redefined; the architecture
broadens to reflect the embedded topology. That's the textbook MINOR
event per the constitution's own versioning policy. MAJOR would require
invalidating prior plans/code's compliance status — but 001 is
**broadened** by this change (its FR-003 is amended in 002's spec, in
plain sight, with traceability), not invalidated.

**Alternatives considered**:

- *Defer the amendment to a follow-up.* Rejected: the constitution
  currently asserts NATS is operator-provisioned; merging code that
  contradicts that assertion without amending it would be silently
  un-constitutional. The amendment must land alongside the feature.
- *MAJOR (v2.0.0).* Rejected: no principle removal/redefinition; no
  prior plan's compliance status is invalidated (001 is amended, not
  invalidated, and 001 hasn't shipped to production yet).

## Open items deferred to Phase 2 (`/speckit-tasks`) or implementation

- `nats-server` version pin: latest stable in the v2.10+ line at the
  moment `/speckit-tasks` runs. Pin in `go.mod`; recorded in
  `contracts/config.md` for operator visibility.
- Whether to expose the embedded server's monitoring HTTP port for
  troubleshooting: deferred to implementation. Default plan: disabled;
  operators get all the observability they need from `pgman-proxy`'s own
  surface (FR-014). If an operator escape-hatch is needed for support,
  add as a follow-up spec.
