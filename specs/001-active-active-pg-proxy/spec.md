# Feature Specification: Active/Active PostgreSQL HA Proxy + Lifecycle Manager (pgman-proxy v1)

**Feature Branch**: `001-active-active-pg-proxy`
**Created**: 2026-05-09
**Status**: Draft (amended 2026-05-09 to add full HA-cluster lifecycle management)
**Input**:
- *Original*: "build an opinionated active/active postgresql proxy that wraps ../pg-manager library and scaffolds it based on NATS as a message bus and leader elector; deployable as standalone process, microservice, or sidecar in pgsql pod; almost everything that needs to be built already exists in ../pg-manager/examples/three_node_nats/main.go; direct proxy, shouldn't support VIP (virtual IP) infrastructure"
- *Amendment*: "in addition to being a proxy pgman-proxy should manage the full lifecycle (LCM) of pgsql HA cluster. all of that LCM should be provided by ../pg-manager."

**Scope summary**: `pgman-proxy` is **two things in one binary**:
1. The **data-plane proxy** described in US1–US3 below (route client traffic
   to the current leader, no VIP).
2. The **control-plane lifecycle manager** described in US4 below, which
   surfaces the LCM operations already implemented in `../pg-manager`
   (`Manager.Switchover`, `Failover`, `Promote`, `Fence`/`Unfence`,
   `UpdateTopology`, `TriggerBackup`, `PrepareUpgrade`/`ExecuteUpgrade`,
   `Status`, `Diagnose`) and the automatic recovery loops the library
   already runs (`AutoRebootstrap`, `AutoDemote`, singleton-claim retry).

All LCM **logic** lives in `../pg-manager`; this repository only adds the
external trigger surface, authentication, audit, and observability glue
required to operate that engine in production (Constitution IV: Thin
Scaffold over pg-manager).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Stand up a 3-peer proxy cluster in front of an existing pg-manager cluster (Priority: P1)

A platform engineer running an HA PostgreSQL setup managed by pg-manager wants to put a thin, opinionated proxy in front of it so application clients have a small set of stable endpoints (one per host). Each proxy peer is colocated with a PostgreSQL host, joins the same NATS-coordinated cluster as the underlying pg-manager peers, and routes incoming PostgreSQL traffic to whichever peer currently holds the leader lease — without any virtual-IP, ARP, or load-balancer-VIP infrastructure.

**Why this priority**: This is the core deliverable. Without it the project doesn't exist. It captures the active/active topology, NATS coordination, leader-aware routing, and the explicit no-VIP constraint.

**Independent Test**: Deploy the proxy alongside an existing 3-node pg-manager cluster (e.g., the `three_node_nats` example), connect a client (`psql`) to any peer's listen address, run a `BEGIN; INSERT ...; COMMIT;` workload, and verify that all writes land on the current leader regardless of which peer the client connected to. Repeat after killing the leader and confirm clients reconnecting to any peer continue to write to the new leader within the failover budget.

**Acceptance Scenarios**:

1. **Given** a 3-node pg-manager cluster healthy on NATS, **When** an operator starts a `pgman-proxy` peer on each host pointing at the same NATS URL and cluster ID, **Then** every peer accepts client TCP connections on its configured listen address within the configured startup budget and reports itself ready in observability output.
2. **Given** all three proxy peers are running and a client is connected to a non-leader peer, **When** the client issues a write transaction, **Then** the write is forwarded to the current leader and committed; no `cannot execute INSERT in a read-only transaction` error is returned to the client.
3. **Given** all three proxy peers are running, **When** the leader process is killed and pg-manager elects a new leader via NATS, **Then** existing client connections see the configured switch behaviour (default: hard-close), reconnecting clients reach the new leader through any proxy peer, and the failover is observable as a sequence of structured events.
4. **Given** a proxy peer cannot reach NATS at startup, **When** the binary is launched, **Then** it exits non-zero with a clear error and never opens the client listen port.

---

### User Story 2 - Run as a sidecar inside a PostgreSQL pod/host (Priority: P2)

A platform engineer running PostgreSQL hosts (containerised or otherwise — but explicitly **not** as a Kubernetes workload owned by this project) wants the proxy to run inside the same execution unit as PostgreSQL, sharing its lifecycle. The sidecar form is a single binary that local applications connect to via the loopback address, while remote applications connect to remote sidecars.

**Why this priority**: The sidecar form materially simplifies deployment for teams that already supervise PostgreSQL processes; it shares failure domain and IP identity with the database it fronts. It is the topology that most cleanly removes the VIP requirement.

**Independent Test**: Run a single PostgreSQL instance with `pgman-proxy` colocated as a sibling process under the same supervisor (`systemd`, `s6`, `tini`, or equivalent), confirm that local applications using `host=127.0.0.1 port=<proxy_port>` connect successfully, then confirm that the same binary configuration works when run as a separate process on a separate host (microservice form) without code changes.

**Acceptance Scenarios**:

1. **Given** a single PostgreSQL host with `pgman-proxy` started as a sibling process by the same supervisor, **When** the supervisor terminates the proxy, **Then** the proxy drains in-flight queries within the configured shutdown budget before exit and the supervisor's restart policy brings it back without operator intervention.
2. **Given** a sidecar deployment, **When** the colocated PostgreSQL crashes but pg-manager re-elects another peer as leader, **Then** the local proxy continues to accept connections from local clients and routes them to the remote leader (the sidecar's value as a stable endpoint survives loss of the local database).
3. **Given** any deployment mode, **When** the operator inspects the running configuration, **Then** the same binary, the same configuration schema, and the same observability surface are reported (no mode-specific code paths surfaced to operators).

---

### User Story 3 - Observe coordination and routing without writing custom integration code (Priority: P3)

An operator on an incident call needs to answer "who is leader right now?", "when was the last failover?", and "is any peer running with a stale lease?" using only the proxy's standard observability output, in under a minute, without reading source code.

**Why this priority**: Observability is required by the constitution (Principle V) and is the difference between a proxy that ships and one that gets ripped out after the first incident. P3 because the core (US1) and deployment (US2) deliver shippable value first, then this story locks in operability.

**Independent Test**: Connect a Prometheus scraper and a JSON log collector to a running 3-peer cluster, trigger a leader failover, and verify that the operator can answer the three incident-call questions above using only the metric and log output (no `gdb`, no `strace`, no source-code reading).

**Acceptance Scenarios**:

1. **Given** a running proxy peer, **When** Prometheus scrapes the metrics endpoint, **Then** it receives counters and gauges for connection counts, query latency (p50/p95/p99), error rates broken down by SQLSTATE, NATS round-trip latency, current leadership state for this peer, and lease-renewal failures.
2. **Given** a leader failover, **When** the operator searches structured logs by cluster ID, **Then** they see a contiguous sequence of events covering leader change, write-routing flip, in-flight connection drain, and new leader confirmed — each event tagged with the same trace context.
3. **Given** a stale-lease condition (peer's NATS lease has expired but the peer is still running), **When** the operator inspects the metrics, **Then** the peer's leadership-state gauge reports `not-leader` and a `lease_renewal_failures_total` counter is non-zero, and the peer is **not** routing writes locally.

---

### User Story 4 - Manage the full lifecycle of a PostgreSQL HA cluster (Priority: P2)

A platform engineer wants `pgman-proxy` to be the single operational
surface for the cluster — not just routing client traffic, but
performing every recurring lifecycle operation on the underlying HA
cluster: bootstrapping a fresh cluster, planned switchovers, manual
failover, fencing/unfencing peers, topology changes, triggering
backups, and performing minor / major version upgrades. The engineer
expects all of those operations to be **executed by `../pg-manager`**
(the wrapped engine) and only **surfaced** by `pgman-proxy` through an
authenticated control surface, so the engine of record stays in one
place.

**Why this priority**: Without LCM the proxy is half a product —
operators still need a second tool (or hand-rolled scripts) to drive
day-2 operations. P2 because the proxy data-plane (US1, US2) must work
first; once it does, US4 turns the binary into a complete operational
surface and is what makes the project production-defensible.

**Independent Test**: Stand up a 3-peer cluster from scratch using
only `pgman-proxy`'s control surface (no manual `initdb`, no hand-run
`pg_basebackup`). Then, against the running cluster, drive an end-to-
end LCM rotation using only that surface: `Status` (read), `Switchover`
to a named peer, `Fence` then `Unfence` an out-of-policy peer,
`UpdateTopology` to add and later remove a peer, `TriggerBackup`,
`PrepareUpgrade` + `ExecuteUpgrade` for a minor version bump. Confirm
each operation completes successfully, is recorded in the audit log,
and is observable via metrics.

**Acceptance Scenarios**:

1. **Given** an empty PostgreSQL data directory and a healthy NATS
   cluster, **When** the operator starts a `pgman-proxy` peer with
   bootstrap-mode configuration on the first node and follower
   configuration on the rest, **Then** the cluster is initialised by
   `../pg-manager` (initdb on the first peer, pg_basebackup on the
   followers) without any operator-issued PostgreSQL commands, and
   `/readyz` reports `200` on every peer once streaming replication is
   established.
2. **Given** a healthy 3-peer cluster, **When** the operator submits a
   `Switchover(target=peer-b)` request to any peer's control endpoint
   with valid credentials, **Then** the peer forwards the request to
   the current leader, `../pg-manager` performs the switchover,
   client connections are handled per the configured switch policy,
   and the request returns success only after the new leader is
   confirmed via NATS lease.
3. **Given** a peer that is mis-replicating, **When** the operator
   submits a `Fence(target=peer-c)` request, **Then** `../pg-manager`
   marks the peer fenced, automatic failover excludes it, and the
   action is recorded in both the structured audit log and a NATS
   audit subject.
4. **Given** an LCM request without valid credentials, **When**
   the operator submits any non-`Status` LCM operation, **Then** the
   request is rejected with an authentication error, no engine-side
   action is taken, and the rejection is recorded in the audit log
   with the source address and (where present) credential identifier.
5. **Given** a healthy cluster, **When** the operator submits a
   `TriggerBackup` request to any peer, **Then** the peer forwards the
   request to the current leader, `../pg-manager` runs the backup via
   the operator-supplied backup executor, and the returned backup ID is
   visible in subsequent `Status` responses.
6. **Given** a healthy cluster running PostgreSQL 17.x, **When** the
   operator submits a `PrepareUpgrade` followed by `ExecuteUpgrade`
   request for a minor-version target, **Then** `../pg-manager`
   performs the rolling restart per its documented upgrade strategy
   without requiring manual operator intervention on individual peers,
   and a final `Status` reports the new version.
7. **Given** any LCM request that requires the current leader, **When**
   it arrives at a non-leader peer, **Then** the request is forwarded
   to the current leader through NATS (or the request returns a
   redirect with the current leader's identity), and **never** executed
   directly on the receiving peer.

---

### Edge Cases

- **NATS unreachable on startup**: Proxy MUST refuse to open its client listen port and exit non-zero with a diagnostic message identifying NATS as the failed dependency.
- **NATS becomes unreachable while running**: Proxy MUST stop serving writes through this peer until coordination is re-established; in-flight read transactions MAY continue per the configured switch policy.
- **Misconfigured cluster ID**: A peer joining with a different cluster ID than its neighbours MUST log an unmistakable error and MUST NOT participate in leader election.
- **TLS verification failure for upstream PostgreSQL**: Proxy MUST refuse the upstream connection and surface the failure to the client; it MUST NOT downgrade to a less strict TLS mode.
- **Listener port already in use**: Proxy MUST exit non-zero at startup with a clear error; partial startup is forbidden.
- **Client sends a write at the moment of leader transition**: Per the configured switch policy (default: hard-close), the client connection is closed; clients are expected to reconnect and retry. The proxy MUST NOT silently route the write to a now-non-leader.
- **Configuration change at runtime** (e.g., topology shrink): Proxy SHOULD pick up the change without a restart when pg-manager's `UpdateTopology` is invoked; otherwise a restart is acceptable.
- **Graceful shutdown timing**: SIGTERM MUST drain in-flight queries within a configurable budget; on budget expiry the proxy MUST close remaining connections and exit promptly.
- **Two operators start two proxies on the same host with overlapping listen ports**: The second startup MUST fail closed, not silently usurp the first.
- **LCM request to a non-leader peer**: The receiving peer MUST forward the request to the current leader (or return a structured redirect identifying the leader). It MUST NOT execute leader-only LCM operations locally (Constitution III: lease verified at the moment of action).
- **LCM request while leadership is changing**: The peer MUST refuse the request with a transient error advising the caller to retry, rather than silently sending it to a stale leader.
- **LCM request without `BackupExecutor` configured**: A `TriggerBackup` request when no operator-supplied backup backend is wired MUST be rejected with a clear error pointing at the configuration gap; partial backup state MUST NOT be created.
- **LCM request during cluster bootstrap**: While `../pg-manager` is still performing initdb / pg_basebackup, non-`Status`/`Diagnose` LCM requests MUST be rejected with a `cluster bootstrapping` error.
- **Concurrent LCM requests**: Two simultaneous in-flight `Switchover` (or other state-mutating) requests MUST result in exactly one being accepted by `../pg-manager`'s engine; the other MUST receive a structured conflict error.
- **Audit log unavailable**: If the audit pipeline (structured log + NATS audit subject) cannot be written, mutating LCM requests MUST be rejected with a `audit unavailable` error rather than executed silently.
- **Authentication-token rotation**: Rotating the control-plane credentials MUST NOT require a process restart; tokens are re-read from their source on each request.
- **Plaintext control-plane bind on non-loopback without ack**: Startup MUST fail closed with a clear error pointing at `control.tls.cert_file`/`key_file` or the named `control.tls.plaintext_explicit_ack` opt-in (FR-033).
- **Forward-mode leader-route timeout**: A leader-routed request whose reply does not arrive within `control.leader_route_timeout` MUST return `leader_route_timeout` and audit the timeout — not silently retry, not silently execute locally (FR-034).

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: System MUST be a single self-contained binary deployable in three topologies — standalone process, microservice (multi-replica), and sidecar (colocated with PostgreSQL) — using the same configuration schema and observability surface.
- **FR-002**: System MUST source all runtime configuration from environment variables, command-line flags, and/or a configuration file. No default value MAY expand the trust boundary (e.g., disabling TLS verification, allowing plaintext credentials in shared logs).
- **FR-003**: System MUST coordinate cluster membership and leader election exclusively via a NATS server provided by the operator; the binary MUST NOT embed its own NATS server in production builds.
- **FR-004**: System MUST accept incoming PostgreSQL wire-protocol connections at a configured TCP listen address (host:port). The system MUST NOT manage virtual IP addresses, gratuitous ARP, keepalived, or any layer-3 floating-address mechanism.
- **FR-005**: System MUST route writes to the current leader as identified by the NATS-backed leadership lease. On leader transition, the system MUST apply the configured switch policy (default: hard-close existing connections) so clients reconnect and reach the new leader.
- **FR-006**: System MUST publish/subscribe to the standard pg-manager coordination event topics on NATS (e.g., `auto_rebootstrap.*`, `auto_demote.*`, `divergence.*`, `conninfo.reconciled`) so coordination state is observable end-to-end without per-deployment custom code.
- **FR-007**: System MUST emit structured (JSON) logs at a configurable verbosity level, with a documented stable field schema, including cluster ID, node ID, and trace context propagation fields.
- **FR-008**: System MUST expose a Prometheus-compatible metrics endpoint covering at minimum: connection counts (open / accepted / closed), query latency histograms, error rates by SQLSTATE, NATS round-trip latency, current leadership state, lease-renewal failures, and process-level metrics (CPU, memory, goroutines).
- **FR-009**: System MUST expose a documented health/readiness endpoint suitable both for process supervisors (returning non-zero when fail-closed conditions are met) and for sidecar liveness probes.
- **FR-010**: System MUST refuse to start ("fail closed") when any of the following holds: NATS is unreachable, required configuration keys are missing, TLS verification cannot be performed (e.g., missing CA bundle when `verify-full` is requested), or the listen address is already in use.
- **FR-011**: System MUST self-fence stale leadership: it MUST NOT serve writes via the local PostgreSQL when its NATS lease cannot be confirmed at the moment of action, regardless of whether it was leader earlier.
- **FR-012**: System MUST tolerate restart-in-place — exit and restart of any peer MUST NOT leave the cluster in an inconsistent state, and the restarted peer MUST resume coordination without operator action.
- **FR-013**: System MUST run cleanly under a non-root operating-system user with the minimum filesystem footprint required for logging and local state.
- **FR-014**: System MUST handle SIGINT and SIGTERM gracefully: drain in-flight queries up to a configurable budget, then close remaining connections and exit zero on a clean shutdown.
- **FR-015**: System MUST NOT include any Kubernetes API client code, Helm chart, CRD, controller loop, admission webhook, or operator-bundle artefact. Such concerns belong to a separate downstream project; PRs introducing them MUST be rejected at review.
- **FR-016**: System MUST preserve PostgreSQL wire-protocol fidelity end-to-end: SQLSTATE codes, error severities, and protocol message ordering MUST round-trip byte-accurate against the upstream server unless an explicit, documented policy intercepts a specific message.
- **FR-017**: System MUST source secrets (database credentials, NATS auth tokens, TLS keys) from environment variables, files referenced by environment variables, or a documented secret-manager interface — never from disk-resident plaintext configuration files committed alongside the binary.
- **FR-018**: System MUST default upstream PostgreSQL TLS to `verify-full`. Weakening this requires an explicit, named, per-route configuration entry that is logged at startup so operators can audit it.
- **FR-019**: System MUST be deployable as a standalone single-peer cluster (one proxy + one PostgreSQL, NATS optional for coordination but required for HA mode) without code changes — same binary, different configuration.
- **FR-020**: System MUST NOT duplicate functionality already provided by `pg-manager`. Proxy code in this repository MUST be limited to: process lifecycle, configuration parsing, NATS wiring, deployment-mode adapters, observability glue, and CLI surface. Logic that arguably belongs in `pg-manager` MUST be flagged at review and routed upstream.

#### Lifecycle Management (LCM) Requirements

- **FR-021**: System MUST expose an authenticated control-plane surface that allows authorised operators to invoke the lifecycle operations already implemented by `../pg-manager`: `Status`, `Diagnose`, `Switchover`, `Failover`, `Promote`, `Fence`, `Unfence`, `UpdateTopology`, `TriggerBackup`, `PrepareUpgrade`, and `ExecuteUpgrade`. The control surface MUST be reachable on a configured listen address distinct from the data-plane listener (FR-004) and the observability listener (FR-008).
- **FR-022**: System MUST delegate **all** LCM logic to `../pg-manager`. The control plane in this repository MUST be limited to: request decoding, authentication/authorisation, leader-routing, audit emission, and result encoding. It MUST NOT contain promote/demote/initdb/basebackup/switchover/upgrade logic of its own.
- **FR-023**: System MUST be capable of bootstrapping a fresh PostgreSQL HA cluster by hosting `../pg-manager`'s bootstrap path: the first peer to successfully claim the singleton lease performs `initdb`, subsequent peers perform `pg_basebackup` against the elected leader. No manual operator-issued `initdb` or `pg_basebackup` invocations MUST be required.
- **FR-024**: System MUST host `../pg-manager`'s automatic recovery loops (`AutoRebootstrap`, `AutoDemote`, singleton-claim retry, divergence parking) in-process and MUST surface every transition through the existing observability surface (FR-006, FR-007, FR-008). It MUST NOT silently disable, gate, or alter those loops.
- **FR-025**: System MUST authenticate every non-`Status`/`Diagnose` control-plane request. Authentication tokens MUST be sourced from environment variables, file paths, or a secret-manager interface (consistent with FR-017). Requests with missing, expired, or unrecognised credentials MUST be rejected without engine-side side effects.
- **FR-026**: System MUST forward every leader-only LCM request to the current leader peer. Requests received by a non-leader peer MUST either (a) be transparently forwarded over the cluster's NATS coordination plane and the result returned to the caller, or (b) return a structured response identifying the current leader for client-side retry. Behaviour MUST be configurable; default is transparent forward.
- **FR-027**: System MUST emit a structured audit record for every LCM request — accepted **and** rejected — capturing at minimum: timestamp, operation name, target (where applicable), caller credential identifier (or `anonymous`), source network address, outcome (`accepted`/`rejected`/`failed`), and latency. The audit record MUST be written both to the structured log (FR-007) and to a dedicated NATS audit subject (`pgman_proxy.<cluster_id>.audit.lcm`).
- **FR-028**: System MUST refuse mutating LCM requests when the audit pipeline cannot be written (Constitution II: fail-closed safety; mutating actions without audit are forbidden).
- **FR-029**: System MUST refuse mutating LCM requests during cluster bootstrap (until `../pg-manager` reports the cluster is past initial bring-up) and during in-flight leadership transitions; the rejection MUST identify the transient condition and advise retry.
- **FR-030**: System MUST surface backup execution by exposing a `BackupExecutor`-shaped configuration knob that an operator can wire to a backend of their choice (filesystem, S3, custom adapter); the binary MUST NOT bundle a specific backup backend. `TriggerBackup` requests with no `BackupExecutor` configured MUST be rejected with a clear configuration-gap error.
- **FR-031**: System MUST honour control-plane authentication-token rotation **without process restart**: tokens are re-read from their source on each request (or via a documented refresh interval) so credential rotation is a runtime operation.
- **FR-032**: System MUST report current cluster lifecycle state (current leader, peer roles, replication health, in-flight LCM operations, last successful backup ID, current PostgreSQL version) via the `Status` operation, in a stable response schema; renames or removals are MINOR-version events (consistent with Constitution V observability stability).
- **FR-033**: System MUST default the control-plane listener to **TLS-required** whenever the listen address is non-loopback. Operators MUST supply a certificate and key (`control.tls.cert_file` + `control.tls.key_file`) when binding to any non-loopback address. The only way to bind plaintext on a non-loopback address is to set the explicit, named, audit-logged opt-in `control.tls.plaintext_explicit_ack: true` (mirroring FR-018's `tls_mode=disable` rule). Loopback binds (`127.0.0.1` / `::1` / Unix socket) MAY remain plaintext without the ack because the trust boundary is the local kernel.
- **FR-034**: System MUST bound the time the receiving peer waits when forwarding a leader-only LCM request via NATS request/reply. The wait budget MUST be configurable (`control.leader_route_timeout`, default 30s, range `(0, 5m]`). On timeout, the receiving peer MUST return a structured error (`leader_route_timeout`) and MUST NOT execute the operation locally; the audit record MUST capture the timeout and the leader identity at the moment of the request.

### Key Entities

- **Proxy peer** — a single running `pgman-proxy` instance, identified by a stable node ID, attached to one PostgreSQL host (in sidecar/standalone mode) or to a logical replica slot (in microservice mode).
- **Cluster** — the set of proxy peers and pg-manager peers sharing a cluster ID and a NATS endpoint; one cluster fronts one logical PostgreSQL deployment.
- **Client connection** — a TCP session from a PostgreSQL client driver into a proxy peer's listen address.
- **Upstream connection** — a TCP session from a proxy peer to a PostgreSQL backend (the local one or a peer's, depending on leadership and routing policy).
- **Leadership lease** — the NATS-backed claim held by exactly one peer at a time; identifies the current writer and is the gate every leader-only action MUST verify at the moment of action.
- **Coordination event** — a NATS-published message in the pg-manager event family (failover, demote, rebootstrap, divergence, conninfo reconciled) that the proxy peer either acts on or surfaces to observability.
- **Switch policy** — an operator-chosen behaviour applied to existing client connections at the moment of leader transition (default: hard-close; alternatives are deferred to a future spec).
- **Control-plane endpoint** — the authenticated listener (separate from the data-plane and observability listeners) on which LCM requests arrive. Owned by this repository.
- **LCM operation** — a named lifecycle action invocable through the control-plane endpoint, mapping 1:1 to a `../pg-manager` `Manager` method. Examples: `Status`, `Diagnose`, `Switchover`, `Failover`, `Promote`, `Fence`, `Unfence`, `UpdateTopology`, `TriggerBackup`, `PrepareUpgrade`, `ExecuteUpgrade`.
- **Audit record** — a structured event capturing the who/what/when/outcome of an LCM request. Emitted both to the structured log and to a dedicated NATS audit subject. Owned by this repository.
- **Backup executor** — an operator-supplied backend implementing `../pg-manager`'s `BackupExecutor` interface. This repository ships **no** built-in backend; operators wire one (filesystem, S3, custom) at deployment time.
- **Upgrade plan** — a description of a planned PostgreSQL version upgrade, owned by `../pg-manager`. The control plane forwards plans verbatim; it does not interpret them.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A platform engineer following the published quickstart can deploy a working 3-peer proxy cluster in front of an existing 3-node pg-manager cluster in under 15 minutes, ending with a successful `psql` connection through every peer that returns the same `SELECT pg_is_in_recovery()` result for the leader.
- **SC-002**: After a forced leader failover in the CI multi-replica test topology, a freshly reconnected client reaches the new leader through any proxy peer with no client-side configuration change, and the proxy reports the leader change as a structured observability event within 5 seconds at the 99th percentile.
- **SC-003**: On simple-query workloads on a local-loopback benchmark, the additional latency introduced by the proxy hop stays under 1 ms at the 99th percentile; any regression greater than 10% relative to the published baseline is flagged in the PR description.
- **SC-004**: An operator can answer "who is leader right now?", "when was the last failover?", and "is any peer running with a stale lease?" using only metric and structured-log output, in under 1 minute, without reading source code.
- **SC-005**: All three deployment modes (standalone, microservice, sidecar) pass smoke tests in CI on every release; failure of any mode blocks the release.
- **SC-006**: The repository contains zero references to Kubernetes API objects, Helm chart files, CRD definitions, controller-runtime libraries, or admission webhook patterns — verifiable by an automated grep gate in CI.
- **SC-007**: Mean time to recovery for a proxy peer crash is under 10 seconds when run under a standard process supervisor — measured as time from process exit to time the restarted peer reports `ready` in its health endpoint.
- **SC-008**: 100% of new wire-protocol or coordination surface introduced in any PR ships with at least one integration test that exercises a real PostgreSQL server and a real NATS server in CI; PRs without such coverage are blocked.
- **SC-009**: A platform engineer can bootstrap a fresh 3-peer HA cluster from empty data directories using only `pgman-proxy`'s control surface (no manual `initdb`, no manual `pg_basebackup`) in under 10 minutes from "binaries placed" to "all three peers report `/readyz=200` with streaming replication established".
- **SC-010**: 100% of LCM requests — accepted, rejected, and failed — produce a structured audit record on the structured log **and** the NATS audit subject; an automated test asserts no LCM operation can complete without both records.
- **SC-011**: 100% of mutating LCM requests received by a non-leader peer either complete on the leader transparently (default) or return a redirect identifying the leader; an automated test asserts neither silent local execution nor request loss.
- **SC-012**: A planned switchover (`Switchover` LCM operation) on a healthy 3-peer cluster completes end-to-end (from request received to new leader confirmed in `Status`) in under 15 seconds at the 99th percentile, on the CI multi-replica topology.
- **SC-013**: 0 LCM logic resides in this repository — verifiable by an automated grep gate that asserts no occurrence of PostgreSQL DDL, `pg_basebackup`, `pg_rewind`, `initdb`, `pg_upgrade`, `pg_ctl promote`, or replication-slot manipulation in the source tree (Constitution IV; FR-022).

## Assumptions

- **pg-manager already provides the engine**: The proxy uses `pg-manager`'s built-in topology-aware TCP proxy (`pgmanager.ProxyConfig`, accessed via `m.Proxy()`) as the wire-level execution path. The new code in this repository is a deployment scaffold around it, not a re-implementation.
- **NATS is operator-provisioned**: A reachable NATS server (with the feature set required by `pg-manager`'s NATS adapters — e.g., JetStream KV) is provisioned and authenticated by the operator. The proxy does not manage NATS lifecycle.
- **Routing is leader-aware, not read/write split**: Initial scope routes all client traffic to the leader. Read/write splitting (sending reads to followers) is **out of scope for v1** and may be a future spec; this assumption keeps the routing model simple and matches the reference example.
- **Authentication is passthrough**: PostgreSQL authentication exchanges (MD5, SCRAM, certificate, etc.) round-trip between client and upstream without proxy-side translation. Proxy-side user mapping or pluggable auth is **out of scope for v1**.
- **TLS material is operator-supplied**: TLS certificates and keys are provided by the operator via filesystem paths or a secret-manager interface; certificate rotation and ACME integration are out of scope for v1.
- **Mode is configuration, not code**: "Standalone", "microservice", and "sidecar" are deployment topologies, not separate code paths or build targets. The same binary runs in all three; deployment shape is determined by how many peers exist and where they are placed.
- **Direct proxy = TCP listener on each peer**: Clients connect to a peer's host:port directly. No virtual IP, no floating address, no DNS-failover trickery is part of this project. Any external traffic-distribution mechanism (DNS round-robin, client-side multi-host connection strings, external load balancer) is **the operator's choice and outside this spec**.
- **Single-cluster scope**: One proxy peer fronts one cluster. Multi-tenant proxies (one peer fronting many clusters with routing per database) are out of scope for v1.
- **Constitution applies**: All seven principles from `.specify/memory/constitution.md` v1.1.0 govern this feature. In particular, Principles III (Active/Active Coordination Correctness), IV (Thin Scaffold over pg-manager), and VII (Scope Discipline — no Kubernetes/Helm) are directly load-bearing for the design choices above.
- **LCM logic lives entirely in `../pg-manager`**: Every operation in US4 is implemented by an existing `Manager` method in the wrapped library (`Switchover`, `Failover`, `Promote`, `Fence`, `Unfence`, `UpdateTopology`, `TriggerBackup`, `PrepareUpgrade`, `ExecuteUpgrade`, `Status`, `Diagnose`). The amendment introduces **no** new lifecycle algorithms in this repository.
- **Control-plane shape (default, overridable)**: The control plane is an authenticated HTTP API on a dedicated listener distinct from the data-plane and observability ports. Bearer-token authentication backed by an env-var or file-supplied secret is the v1 default; reads (`Status`, `Diagnose`) MAY be configured as unauthenticated, mutating operations MUST always require a token. mTLS and per-operation RBAC are deferred to a future spec.
- **Control-plane bind defaults are mode-aware** *(clarified 2026-05-09)*: In **sidecar** deployment mode the listener defaults to **loopback-only** (`127.0.0.1:9091`) — the sidecar's value is local-app stability, so cross-host control is uncommon and exposing it would expand the trust boundary by default. In **standalone** and **microservice** modes the listener defaults to **all-interfaces** (`0.0.0.0:9091`) — centralised SRE tooling is the common case there. In every mode the operator can flip the default explicitly via `control.listen_addr`. Authentication policy (FR-025) does not change with bind address: bearer token required for mutating ops regardless of where the listener is bound.
- **Backup backend is operator-supplied**: This repository ships no built-in backup backend. `TriggerBackup` works only when an operator wires a `BackupExecutor` (filesystem, S3, custom) at deployment time. Shipping a default backend is **out of scope for v1** and would violate Constitution IV.
- **Restore is out of scope for v1**: Point-in-time restore is destructive and warrants a dedicated spec; v1 supports backups only. Operators perform restore via `../pg-manager`'s native interfaces directly until then.
- **Bootstrap idempotency is delegated**: Whether a fresh peer runs `initdb` or `pg_basebackup` is decided by `../pg-manager`'s singleton-claim and state-store logic — this repository neither overrides nor short-circuits that decision.
- **Audit pipeline reliability is non-negotiable**: A mutating LCM request that cannot be audited is refused (FR-028). Operators MUST size NATS and the log pipeline accordingly; we trade a small availability surface for an explicit accountability guarantee.
