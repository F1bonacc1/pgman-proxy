# Feature Specification: Embedded NATS Cluster for pgman-proxy Coordination

**Feature Branch**: `002-embedded-nats-cluster`
**Created**: 2026-05-09
**Status**: Draft
**Input**: User description: "pgman currently relies on an external nats server. that's a mistake. it should ship with it's internal embedded nats server that will be used for coordination between the instances and for leader election. similar to ../pg-manager/examples/three_node_nats/main.go. each replica of pgman will have it's own instance of the nats server, all the replicas form a nats cluster."

## Context & Relationship to Feature 001

Feature `001-active-active-pg-proxy` established the baseline product: a thin
active/active proxy + lifecycle manager that wraps `../pg-manager` and uses
NATS for coordination. **001's FR-003 explicitly forbade embedding a NATS
server**, requiring every operator to provision NATS externally.

Feature 002 **reverses that constraint**. Operating experience (and the
project owner's stated intent) shows that requiring an external NATS dependency
adds an out-of-band moving part that operators must run, secure, monitor, and
upgrade — for a coordination plane that only `pgman-proxy` peers need to talk
on. The proxy now ships with NATS in-process: each replica boots its own
embedded NATS server, and the replicas mesh into a single NATS cluster that
supplies leader election and the existing pg-manager coordination event family.

This spec amends 001's FR-003 (and the related Assumption "NATS is
operator-provisioned") in place. It does **not** change any other 001
requirement: the proxy still routes leader-aware traffic, still surfaces the
control-plane LCM operations, still emits the same observability schema. The
only thing that changes is **where the NATS server runs**.

This change touches the project Constitution v1.1.0 (specifically the
"Topology & Dependencies" subsection of Additional Constraints, which currently
states "NATS is a hard runtime dependency" — externally provisioned). A
constitution amendment to v1.2.0 (or 2.0.0) MUST land alongside or before this
feature's plan; the amendment is in-scope for the planning phase, not this
specification.

## Clarifications

### Session 2026-05-09

- Q: How should cluster-route connections between embedded NATS servers authenticate? → A (initial intent): NKey (Ed25519 seed) per peer with a shared cluster name; per-peer identity in audit. **Amended 2026-05-09 during `/speckit-implement` Phase 2**: NATS server v2.14 cluster routes only support a single shared username/password pair (research.md RD-001a). Per-peer NKey-on-routes is not in the upstream protocol. Reconciled by the user to: shared cluster username + password (sourced via SecretRef per FR-010); per-peer identity in audit logs preserved via the NATS server-name field, set to the pgman-proxy node ID at startup.
- Q: What TLS posture should the cluster-routes listener have? → A: TLS required on non-loopback binds; loopback plaintext OK; named, audit-logged explicit-ack opt-out for non-loopback plaintext (mirrors 001 FR-033)
- Q: What replication factor should the JetStream KV streams use? → A: Auto-derive from cluster size — R=1 for single-peer, R=2 for two-peer (degraded; no fault tolerance), R=3 for clusters of 3+ peers (capped at 3); operator-overrideable only with explicit out-of-range opt-in
- Q: Should the peer-routes list and authorized auth state be runtime-reloadable? → A: Hot-reload on SIGHUP, scoped to peer-routes only (authorized-keys list dropped per the RD-001a amendment; cluster credential rotation is now a single-secret SIGHUP flow); all other config remains startup-only

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Stand up a 3-peer pgman-proxy cluster with no external NATS (Priority: P1)

A platform engineer wants to run a 3-peer `pgman-proxy` cluster in front of an
existing `pg-manager` PostgreSQL HA setup **without provisioning, securing, or
monitoring a separate NATS deployment**. They install the same `pgman-proxy`
binary on three hosts, hand each peer the addresses of its two siblings, and
expect the three peers to mesh into a working coordination cluster on their
own — no extra processes, no extra containers, no extra service to page on.

**Why this priority**: This is the entire point of the feature. If a 3-peer
cluster cannot stand up without an external NATS process, the feature has not
been delivered. It is the smallest deployable shape that exercises clustering,
leader election, and coordination event flow end-to-end.

**Independent Test**: On three otherwise-empty hosts, install only the
`pgman-proxy` binary, configure each peer with its node identity and the
addresses of the other two peers, start all three. Verify with operator-visible
output (logs, metrics, control-plane status) that: (a) every peer reports its
embedded NATS server as healthy, (b) the three peers report a 3-route mesh,
(c) exactly one peer is elected leader, (d) `psql` traffic to any peer routes
writes to the elected leader. No `nats-server` process exists anywhere on the
test hosts.

**Acceptance Scenarios**:

1. **Given** three hosts with the `pgman-proxy` binary and a configuration
   listing the three peer addresses, **When** the operator starts each peer,
   **Then** within the configured startup budget every peer reports its
   embedded NATS server `ready`, the cluster routes converge on a 3-peer mesh,
   one peer becomes leader via the existing pg-manager NATS leadership
   mechanism, and the data-plane listener accepts client TCP connections.
2. **Given** a 3-peer pgman-proxy cluster running with embedded NATS,
   **When** the operator runs `ps`/`systemctl`/`docker ps` on every host,
   **Then** there is no `nats-server` (or equivalent external NATS) process —
   each peer's NATS is hosted entirely inside the `pgman-proxy` process.
3. **Given** a 3-peer cluster, **When** one peer process is terminated,
   **Then** the remaining two peers retain a healthy 2-route mesh, leader
   election continues to converge (or the surviving leader retains its lease),
   and data-plane traffic continues to be routed to the current leader within
   the failover budget defined in 001 (SC-002).
4. **Given** a 3-peer cluster, **When** the terminated peer is restarted,
   **Then** it rejoins the embedded NATS mesh automatically, re-acquires its
   `pg-manager` adapter subscriptions, and resumes participation without
   operator action — and without any external NATS service.

---

### User Story 2 - Run a single-peer pgman-proxy with no external dependencies (Priority: P1)

A platform engineer running a small or development PostgreSQL setup wants
`pgman-proxy` to work as a single self-contained binary — no external NATS,
no external coordination plane at all — while still exercising the same code
paths the HA topology uses (so single-peer is not a separate code path).

**Why this priority**: Single-peer is the smallest non-trivial deployable
shape and is the unit of local development, CI smoke tests, and operator
"first-touch" experiences. If single-peer requires external NATS today, it
prevents the user from running `pgman-proxy --config x.yaml` and seeing
something work; this feature must keep the single-peer ergonomics **simpler**
than today, not more complex.

**Independent Test**: Start one `pgman-proxy` peer with its in-process NATS
configured for stand-alone operation (no peer routes). Verify that: (a) the
peer reports its embedded NATS healthy, (b) the peer becomes leader of a
1-peer cluster via the same `pg-manager` NATS leadership adapter the HA case
uses, (c) `psql` connections through the proxy succeed, (d) on graceful
shutdown the embedded NATS server stops cleanly with no orphan goroutines or
files left behind.

**Acceptance Scenarios**:

1. **Given** a single host with only the `pgman-proxy` binary, **When** the
   operator starts it with a single-peer configuration, **Then** the peer
   reaches `ready` state without contacting any external NATS endpoint and
   the same NATS-backed leader-election code path used in the 3-peer case is
   exercised end-to-end.
2. **Given** a single-peer cluster, **When** the operator scales out by
   starting a second peer with a configuration that lists the original peer
   as a route, **Then** the two peers form a NATS cluster, agree on a single
   leader, and data-plane traffic converges on that leader. Reconfiguring
   the original peer to add the new sibling is performed by updating its
   peer-routes configuration and sending SIGHUP per FR-014a — no
   restart of the original peer is required.
3. **Given** any single-peer or multi-peer configuration, **When** the
   operator inspects the running configuration via the control-plane
   `Status`/`Diagnose` operation, **Then** the embedded NATS server's
   identity (server name, listen addresses for clients and routes, current
   peer count) appears in the response under a stable, documented schema.

---

### User Story 3 - Survive partition, restart, and rolling upgrade without operator NATS knowledge (Priority: P2)

An on-call SRE needs the embedded NATS cluster to behave **as a black-box
implementation detail**: they should never need to log into a peer to issue
NATS administrative commands, never need to clear NATS state by hand to
recover from a partition, and never need to know NATS subjects to debug a
coordination problem. All the answers should come from `pgman-proxy`'s own
observability surface (FR-006/FR-007/FR-008 in feature 001).

**Why this priority**: A coordination plane that requires NATS-specific
operator knowledge defeats the purpose of embedding it. P2 because it
follows the basic clustering deliverable (US1) and the small-deployment
deliverable (US2); without it, operators will be tempted to reach for an
external NATS again.

**Independent Test**: Induce three failure modes against a 3-peer cluster
without ever invoking a NATS CLI or NATS administrative API: (1) a network
partition isolating one peer for two minutes, (2) a `kill -9` on every
peer in sequence (not concurrently), (3) a rolling restart for a binary
upgrade. After each scenario, verify recovery using only `pgman-proxy`'s
metrics, structured logs, and control-plane `Status`. The operator MUST NOT
need to run any `nats-server`, `nats`, or `nsc` command, and MUST NOT need
to read NATS-internal state files.

**Acceptance Scenarios**:

1. **Given** a 3-peer cluster, **When** one peer is partitioned from the
   other two, **Then** the partitioned peer self-fences (stops serving
   writes per Constitution III) and the remaining quorum continues to
   serve, with both behaviours visible as structured events in the
   `pgman-proxy` log stream — no NATS-specific log archaeology is required.
2. **Given** a 3-peer cluster on binary version N, **When** the operator
   performs a rolling restart to upgrade to version N+1 (one peer at a
   time), **Then** at every step the cluster retains a quorum, leader
   election remains coherent, and the control-plane `Status` operation
   reports the in-flight upgrade truthfully — without manual NATS-cluster
   reconfiguration between peers.
3. **Given** a peer with stale on-disk NATS state (e.g., stopped while
   holding cluster routes that no longer exist), **When** the peer is
   restarted, **Then** it either reconciles automatically against the
   configured peer list, or it fails closed with a clear error pointing at
   the configuration; under no circumstance does it run with **silent**
   stale routes.

---

### Edge Cases

- **Configured peer list disagrees across peers**: Each peer is configured
  with the addresses of the other peers. If the lists are inconsistent
  (e.g., peer A lists [B, C], peer B lists [A], peer C lists [A]), the
  cluster MUST converge on the union of routes via NATS cluster gossip
  (NATS standard behaviour) and MUST emit a structured warning identifying
  the asymmetry so operators can fix the configuration.
- **Embedded NATS port already in use on this host**: A pre-existing
  process bound to either the configured client-listener port or the
  configured cluster-routes port MUST cause `pgman-proxy` to fail closed at
  startup, identifying which port and which competing process owns it
  (where determinable). Partial startup is forbidden.
- **No peer routes configured in HA configuration**: If the operator marks
  the deployment as multi-peer (e.g., expects a quorum) but supplies an
  empty peer-routes list, startup MUST fail closed with a clear error;
  silent single-peer behaviour while configured for multi-peer is forbidden.
- **Single-peer mode explicitly opts out of routes**: A configuration with
  `cluster.peers: []` AND an explicit single-peer marker (e.g., `cluster.size: 1`)
  MUST start successfully without route configuration and MUST mark the
  cluster as single-peer in observability output.
- **Cluster authentication mismatch between peers**: When a peer presents
  cluster credentials that don't match a sibling's configured username/
  password pair (e.g., during a credential rotation that was applied
  unevenly), the sibling MUST refuse the route and MUST log a clear,
  redacted (8-character password-prefix only) authentication-failure
  event identifying the rejecting peer's `node_id` and the rejected
  remote's claimed `server_name`. Routes MUST NOT silently fall back to
  unauthenticated.
- **Cluster routes exposed without authentication on a non-loopback bind**:
  Binding the cluster-routes listener to a non-loopback address MUST require
  cluster authentication to be configured. The only way to bind plaintext +
  unauthenticated cluster routes on a non-loopback address is the same
  pattern 001 uses for the control plane (FR-033): a named, audit-logged
  explicit-ack opt-in. Default is fail-closed.
- **Mixing embedded and external NATS in the same cluster**: Out of scope
  for this feature. Configuration MUST NOT permit pointing a peer at an
  external NATS URL while also enabling the embedded server. Validation
  MUST refuse the combination at startup.
- **JetStream storage path missing or unwritable**: When the embedded
  NATS server requires durable storage (e.g., for the leadership KV) and
  the configured storage path does not exist or is not writable by the
  process user, startup MUST fail closed with a clear path-and-permission
  error; the proxy MUST NOT silently fall back to in-memory storage when
  durability is required by the cluster topology.
- **Disk full while embedded NATS is running**: The embedded NATS server's
  inability to persist coordination state MUST surface as a structured
  health-degraded event; the peer MUST self-fence per Constitution III
  rather than continue serving writes on a coordination plane it cannot
  durably update.
- **Upgrade across an embedded NATS major version**: When `pgman-proxy`
  is upgraded to a binary that bundles a new NATS major version with a
  changed on-disk format, the upgrade path MUST either (a) be backward-
  compatible with the prior on-disk format, or (b) document a one-time
  state-clear procedure executable through `pgman-proxy`'s own CLI. An
  operator MUST NOT need NATS-version-specific tools.
- **External NATS configuration left over from feature 001**: Any
  pre-existing `nats.url` configuration pointing at an external NATS URL
  MUST be rejected at validation with an error explaining that external
  NATS is no longer supported and pointing at the embedded-server
  configuration block. Silent ignore is forbidden — operators must be told
  their old config is no longer wired.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: System MUST embed a NATS server inside every `pgman-proxy`
  process. The embedded NATS server is started and stopped by the
  `pgman-proxy` process lifecycle and MUST NOT require an out-of-process
  `nats-server` binary, container, or service.
- **FR-002**: System MUST NOT support pointing the proxy at an external
  NATS endpoint in v1 of this feature. The operator-provisioned NATS path
  established by feature 001 (FR-003) is **removed**. Configuration that
  references an external NATS URL MUST be rejected at validation with a
  migration-pointing error.
- **FR-003**: System MUST allow each replica's embedded NATS server to be
  meshed with the embedded NATS servers of its peers via NATS cluster
  routes, so that the replicas collectively form a single coordination
  cluster (publishing, subscribing, leader election, JetStream KV) without
  any external broker.
- **FR-004**: System MUST source the list of peer routes (the addresses of
  this peer's siblings' cluster-route listeners) from the same configuration
  surface used for the rest of the proxy (env vars, flags, config file)
  so peer-set changes do not require a different configuration mechanism
  than other proxy parameters.
- **FR-005**: System MUST allow a single-peer deployment shape — embedded
  NATS server with no peer routes — that exercises the same coordination
  code paths used in HA mode (i.e., the same `pg-manager` NATS adapters
  for leadership, state store, and event bus). Single-peer MUST NOT be a
  separate code path or a different binary.
- **FR-006**: System MUST expose the embedded NATS server's identity and
  cluster state through `pgman-proxy`'s existing observability surface —
  structured logs (FR-007 of 001), Prometheus metrics (FR-008 of 001), and
  the control-plane `Status` operation (FR-032 of 001) — at minimum:
  embedded-server name, client listener address, cluster-routes listener
  address, currently-meshed peer count, and last-route-error timestamp.
- **FR-007**: System MUST fail closed at startup if the embedded NATS
  server cannot bind its configured client listener, cannot bind its
  configured cluster-routes listener, cannot read its required durable-
  storage path (where applicable), or cannot satisfy any other startup
  invariant the embedded NATS server enforces. Partial startup is forbidden.
- **FR-008**: System MUST fail closed at startup if a multi-peer
  configuration is declared (peer routes expected) but no peer routes are
  configured.
- **FR-009**: System MUST authenticate cluster-route connections
  between peers using a **shared cluster credential** (`cluster.username`
  + `cluster.password`) scoped to a shared `cluster.name`. Per-peer
  identity in audit logs is provided by the NATS server-name field
  (set to the pgman-proxy node ID at startup) — the wire-level
  credential itself is the same on every peer because NATS server
  v2.14 cluster-route protocol does not support per-peer credentials
  (research.md RD-001a). Authentication MUST be required on any
  non-loopback bind. The only way to bind unauthenticated cluster
  routes on a non-loopback address is via a named, audit-logged
  explicit-ack opt-in (mirroring 001's FR-033 pattern for the
  control-plane plaintext bind). The cluster credential MUST be sourced
  via SecretRef (FR-010); plaintext credentials in config files are
  rejected at validation.
- **FR-010**: System MUST source the cluster username and password via
  the same secret-sourcing rules as the rest of the proxy (FR-017 of
  001): environment variables, file paths, or a documented secret-
  manager interface. Neither the username nor the password MUST be
  stored in plaintext config files committed alongside the binary. The
  password MUST be at least 16 bytes and SHOULD be generated via
  `pgman-proxy cluster-secret-gen` (RD-003 / RD-001a) which produces a
  base32-encoded 32-byte cryptographically random value.
- **FR-010a**: System MUST support rotating the shared cluster
  credential without a flag-day. The rotation procedure is:
  (a) update every peer's `cluster.password` SecretRef target to the
  new value; (b) signal each peer to reload via SIGHUP (FR-014a) — no
  peer restarts. NATS server's `ReloadOptions` accepts the cluster
  authorization change and re-handshakes existing routes against the
  new credential; the cluster MUST remain quorate at every step. The
  audit log MUST capture the redacted credential fingerprint (first 8
  characters of the password's base32 encoding) on every cluster-route
  accept event so a rotation is forensically reconstructable.
- **FR-010b**: System MUST require **TLS** on the cluster-routes listener
  whenever it is bound to any non-loopback address. Operators MUST supply
  a server certificate and key (`cluster.tls.cert_file`, `cluster.tls.key_file`,
  and the trust-anchor input that allows verifying sibling peers' presented
  certificates). The only way to bind plaintext cluster routes on a
  non-loopback address is the named, audit-logged explicit-ack opt-in
  `cluster.tls.plaintext_explicit_ack: true` (parallel to 001 FR-033 for
  the control plane). Loopback binds MAY remain plaintext without the ack.
  TLS material MUST be sourced via the same rules as other secrets
  (FR-017 of 001) — never plaintext-config-resident. Cluster-credential
  authentication (FR-009) and TLS encryption (this FR) are independent
  layers; both apply on non-loopback binds in v1.
- **FR-011**: System MUST allow operators to configure the embedded NATS
  server's durable-storage location (for JetStream KV used by the
  pg-manager leadership/state-store adapters). Defaults MUST favour
  durability over ephemerality when the deployment shape is multi-peer;
  in-memory storage MAY be the default for single-peer deployments.
- **FR-011a**: System MUST configure the JetStream KV streams that back
  the `pg-manager` leadership and state-store adapters with a replication
  factor derived from the **declared cluster size** at startup, per the
  following table:

  | Declared cluster size | Replication factor | Peer-loss tolerance | Operator note |
  |-----------------------|--------------------|---------------------|---------------|
  | 1 (single-peer)       | 1                  | 0                   | Coordination state lives only on the local peer. Loss of the peer = loss of state. |
  | 2                     | 2                  | 0                   | Degraded transient shape; documented with a startup warning that no peer-loss tolerance exists. Operators SHOULD scale to 3 promptly. |
  | 3 or more             | 3                  | 1                   | Standard production shape; replication factor capped at 3 because NATS JetStream's safe maximum is 5 and a coordination plane gains no fault-tolerance benefit beyond R=3. |

  The proxy MUST emit a startup log event and a Prometheus gauge reporting
  the declared cluster size and the resulting replication factor so an
  operator can verify the topology without reading source. Overriding the
  derived factor MUST require an explicit, named, audit-logged opt-in
  (`cluster.replication_factor_override`) that the proxy logs at every
  startup so misconfiguration is loud, not silent.
- **FR-012**: System MUST drain and stop the embedded NATS server cleanly
  on SIGINT/SIGTERM, within the same shutdown budget that governs the rest
  of the proxy (FR-014 of 001). On clean shutdown the proxy MUST NOT leave
  orphan listening sockets, lock files, or partially-written JetStream
  state files.
- **FR-013**: System MUST emit a structured event (with stable schema) for
  every meaningful embedded-NATS lifecycle transition: server-start,
  server-ready, route-up, route-down, server-stop, and storage-degraded.
  These events MUST flow through the same logging pipeline as other proxy
  events (FR-007 of 001) and MUST be visible to operators without NATS-
  specific tooling.
- **FR-014a**: System MUST hot-reload, on receipt of `SIGHUP`, exactly
  two configuration surfaces — the **peer-routes list** (FR-004) and
  the **cluster password** (FR-010) — without restarting the embedded
  NATS server, without dropping established cluster routes that remain
  valid under the new configuration, and without interrupting in-flight
  client data-plane connections (the data-plane listener from 001 is
  unaffected by SIGHUP). All other configuration keys (cluster username,
  cluster name, cluster-routes listener address, TLS material, JetStream
  storage path, replication-factor override, embedded-server identity,
  every 001 configuration key) remain **startup-only** in v1 and a
  SIGHUP that targets them MUST be ignored with a clear structured
  warning naming the keys that were skipped. Reload outcomes (added
  routes, removed routes, password rotated, skipped keys) MUST be
  captured as a single structured event in the same logging pipeline as
  other proxy events (FR-007 of 001) so the operator can audit the
  reload from the log alone.
- **FR-014**: System MUST NOT introduce a new operator persona who must
  understand NATS administration. Day-2 operations on the embedded NATS
  cluster (joining a new peer, removing a peer, rotating cluster
  credentials) MUST be performable via `pgman-proxy`'s own configuration
  and signals; they MUST NOT require NATS CLI tools.
- **FR-015**: System MUST keep the existing pg-manager NATS adapter wiring
  (`Leadership`, `StateStore`, `EventBus`) untouched at the API surface.
  The embedded server MUST be a drop-in for the connection these adapters
  open today; no pg-manager API change is part of this feature.
- **FR-016**: System MUST NOT change any feature 001 functional requirement
  other than FR-003 (embedded forbid) and the assumption "NATS is
  operator-provisioned." All other 001 requirements (data-plane routing,
  control-plane LCM, observability schema, deployment modes, fail-closed
  rules, audit pipeline) carry forward unchanged.
- **FR-017**: System MUST document the bundled NATS version and the
  feature set it relies on (e.g., JetStream KV) in repository
  documentation. Version bumps that change wire compatibility or on-disk
  format MUST be flagged in release notes per Constitution V.
- **FR-018**: System MUST allow configuration of the embedded NATS server's
  client listener address (defaults to loopback in HA mode — only the
  cluster-routes listener needs to face peers; the client listener is for
  the in-process pg-manager adapter to dial). The client listener MUST NOT
  default to all-interfaces; doing so would expose internal NATS subjects
  beyond the host trust boundary.
- **FR-019**: System MUST allow configuration of the cluster-routes
  listener address. It MUST default to all-interfaces in HA mode (since
  peers must reach it) and to disabled (or loopback) in single-peer mode
  (where no remote peer needs it).
- **FR-020**: System MUST verify that the configured peer-routes list does
  not include this peer's own cluster-routes address (self-loop). If a
  self-loop is detected, the proxy MUST emit a structured warning and
  exclude the self-route; it MUST NOT fail startup over a self-loop because
  identical configurations rolled out across peers commonly contain one.

### Key Entities

- **Embedded NATS server** — the in-process NATS server hosted by every
  `pgman-proxy` peer, started during proxy startup and stopped during
  proxy shutdown. Replaces the external `nats-server` process implied by
  feature 001's FR-003.
- **Cluster route** — a NATS protocol connection between two embedded
  NATS servers in distinct `pgman-proxy` peers, over which subscriptions,
  publishes, and JetStream state are propagated.
- **Peer routes list** — the operator-supplied addresses (host:port) of
  the cluster-routes listeners of this peer's siblings. Drives the initial
  mesh; NATS cluster gossip extends the mesh thereafter.
- **Embedded server identity** — the stable per-peer name the embedded
  NATS server presents to its siblings. Defaults to (and is observably
  derived from) the `pgman-proxy` node ID so that NATS-side and
  pg-manager-side logs reconcile.
- **JetStream durable-storage path** — the per-peer filesystem path where
  the embedded NATS server persists JetStream state used by the
  pg-manager leadership / state-store adapters. Optional in single-peer
  in-memory mode.
- **Cluster credential** — the shared `username` + `password` pair
  every peer presents when opening cluster routes (research.md
  RD-001a; constrained by upstream NATS v2.14 protocol). Username is
  conventionally a non-secret cluster identifier; password is the
  actual secret material, ≥ 16 bytes, generated via `pgman-proxy
  cluster-secret-gen`. Sourced via SecretRef (FR-010) — never
  plaintext-config-resident. The same pair on every peer; per-peer
  identity in audit logs comes from the **embedded server identity**
  entity (NATS server-name = pgman-proxy node ID), not from the wire
  credential.
- **Cluster name** — the shared identifier all peers in one logical
  `pgman-proxy` cluster declare on their embedded NATS configuration.
  Used alongside the cluster credential (FR-009) to prevent two
  unrelated clusters' peers from accidentally meshing if their network
  paths cross.
- **Embedded-NATS lifecycle event** — a structured log/metric record
  emitted on each meaningful state transition of the embedded server
  (start, ready, route-up, route-down, stop, storage-degraded), flowing
  through the existing `pgman-proxy` observability pipeline.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A platform engineer following the published quickstart can
  stand up a 3-peer `pgman-proxy` cluster fronting an existing
  `pg-manager` PostgreSQL HA setup in under 15 minutes with **zero**
  external NATS processes running on any host (verifiable by `ps`
  inspection on every host); the same 15-minute budget that 001's SC-001
  required, now without a separate NATS deployment step.
- **SC-002**: Single-peer startup time (from process exec to `ready`
  health) is under 5 seconds on a developer laptop, with no external
  network calls during startup other than to the upstream PostgreSQL
  itself.
- **SC-003**: After a controlled kill of one peer in a 3-peer cluster,
  the remaining peers retain a healthy 2-peer NATS mesh and converge on
  a leader within the same 5-second p99 failover budget defined in 001
  (SC-002). The embedded NATS server's recovery does not extend the
  failover budget beyond 001's baseline.
- **SC-004**: An operator can answer "is the coordination cluster
  healthy?", "how many peers are meshed right now?", and "when did the
  last route flap occur?" using only `pgman-proxy`'s metric, log, and
  control-plane `Status` output — without invoking any `nats`,
  `nats-server`, or `nsc` command.
- **SC-005**: Memory and CPU overhead added by the embedded NATS server,
  measured at idle on a single-peer deployment, stays within these
  caps (set during /speckit-plan Phase 0 / RD-005, back-ported here
  on completion of Phase 2):
  - **Memory**: ≤ 60 MB RSS p95 at idle (single-peer, JetStream
    enabled with file storage on a 256 MB sidecar pod).
  - **CPU**: < 0.5% of one core p95 at idle.

  The numbers leave practical headroom for the `pgman-proxy` binary
  to run as a sidecar under tight per-pod budgets. Asserted in CI
  smoke once Phase 3 integration coverage lands.
- **SC-006**: 100% of the integration tests required by 001 (multi-replica
  topology, leader election, leader fencing, NATS-outage recovery) run
  unchanged against the embedded NATS server. No test suite forks the code
  path for "external NATS" vs "embedded NATS"; there is only one path.
- **SC-007**: Repository contains zero references to running `nats-server`
  as an external process, container, or service in production
  documentation, quickstart guides, or default deployment manifests —
  verifiable by an automated grep gate in CI mirroring 001's SC-006
  pattern.
- **SC-008**: A rolling restart of all three peers in a 3-peer cluster
  for a binary upgrade completes with zero windows of total quorum
  loss and zero operator-issued NATS commands. End-to-end budget
  (set during /speckit-plan Phase 0 / RD-006, back-ported here on
  completion of Phase 2):
  - **End-to-end**: ≤ 60 s p99 for all three peers.
  - **Per-peer**: ≤ 20 s = 5 s drain + 5 s exit + 5 s new-binary boot
    + 5 s embedded-NATS re-mesh + leadership re-confirm.
  Asserted by `tests/integration/embedded_cluster_test.go` (T027).
- **SC-009**: A configuration that references an external NATS URL
  (carry-over from feature 001) is rejected at validation with a clear
  error message that names the deprecated key and points at the embedded
  configuration block. An automated test asserts this rejection so the
  guarantee cannot regress.

## Assumptions

- **Embedded NATS is the only supported coordination plane in v1 of this
  feature.** External NATS is removed, not toggled. Operators with an
  existing external NATS deployment migrate by deleting that deployment
  and updating their `pgman-proxy` configuration to use embedded mode;
  the project does not ship a "both" or "either" mode.
- **NATS server library is the upstream `nats-server` Go module.** This
  project does not fork, rewrite, or substitute the NATS server. The
  embedded server is the canonical NATS server linked into the proxy
  binary, configured programmatically.
- **The same `pg-manager` NATS adapters used today continue unchanged.**
  Leadership, state store, and event bus are still consumed via
  `github.com/f1bonacc1/pg-manager/adapters/nats`. The embedded server is
  invisible to those adapters; they dial it like any other NATS endpoint.
- **Peer discovery is operator-configured, not automatic.** Each peer is
  given the addresses of its siblings via configuration. Service-discovery
  integrations (DNS-based, Consul, Kubernetes endpoints) are out of scope
  for v1 — they remain the responsibility of the deployment platform.
- **JetStream is the durable-storage substrate used by the embedded
  server**, matching the feature set the `pg-manager` NATS adapters
  already require. No alternative storage backend is offered.
- **Authentication between cluster routes is mandatory on non-loopback
  binds.** The single fail-closed exception is the named, audit-logged
  explicit-ack opt-in (mirrors 001's FR-033 control-plane pattern).
- **TLS on cluster routes is mandatory on every non-loopback bind**
  (FR-010b). Cluster-credential authentication (FR-009) and TLS
  encryption are independent, layered defences applied together. The
  only fail-closed exception is the named, audit-logged
  `cluster.tls.plaintext_explicit_ack` opt-in. Loopback binds MAY remain
  plaintext without the ack. Mutual TLS (peers verifying each other's
  client certs in addition to the shared cluster credential) is **out
  of scope for v1** — the upstream NATS protocol already constrains
  cluster-route auth to the shared credential model (research.md
  RD-001a); layering mTLS on top would add PKI complexity without
  adding wire-level per-peer identity, since per-peer identity is
  already surfaced through the NATS server-name field in audit logs.
- **No new operator persona.** Operators continue to interact with
  `pgman-proxy` only — through its config, signals, control plane, and
  observability surface. NATS-specific operator tooling is explicitly
  not a runtime requirement of this feature.
- **Constitution amendment lands alongside this feature's plan.** The v1.1.0
  Architecture Overview and "Topology & Dependencies" subsection (which
  currently describe NATS as externally provisioned) require a MINOR or
  MAJOR amendment. The amendment is in scope for `/speckit-plan`, not
  this spec.
- **All other feature 001 assumptions carry forward unchanged.** The proxy
  still does not manage virtual IPs, still treats authentication as
  passthrough, still defers backup-backend selection to the operator,
  still rejects Kubernetes/Helm code, etc.
