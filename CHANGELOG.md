# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed — pg-manager pin bumped to v0.5.0

`go.mod` now requires `github.com/f1bonacc1/pg-manager v0.5.0`
(previously v0.4.1), after the nightlies stabilized on the fixes the
bump carries. It picks up the 014/CR-009c election-livelock closure
(a SIGKILLed ex-primary can no longer re-acquire the lease and starve
survivors: the health-gated renewal path releases the leader key,
candidacy follows renewal health via `Manager.ShouldCampaign` /
`nats.WithCampaignGate`, and demotion fast-paths past a dead
postmaster), peer-address decoupling (all peer-DSN lookups route
through the resolver chain; DNS failures are transient in basebackup
retry; `TriggerBackup` dials `LocalPGAddr`/loopback instead of
`host=<NodeID>`), and a bounded retry for unclassified `pg_ctl start`
failures that fixed the compose-tier S1 restart flake. Transitive
bumps: `golang.org/x/crypto` 0.52.0, `golang.org/x/sys` 0.45.0. The
major-upgrade gate sentinel pinned by the feature-004 tests is
unchanged in v0.5.0 (`manager/backup_upgrade.go:103`).

### Security — Go toolchain bumped to 1.26.5 (GO-2026-5856, crypto/tls)

`govulncheck` flagged GO-2026-5856 in the standard library's
`crypto/tls` (reachable via NATS cluster TLS, the control-plane HTTPS
server, and the pgmctl SSE client), fixed in go1.26.5. The `go.mod`
`go` directive — which both `setup-go` and the govulncheck workflow
install from — now requires 1.26.5, and `deploy/docker/Dockerfile.bundle`
tracks it in lockstep (`golang:1.26.5-bookworm`). No source changes.

### Fixed — release-image build broken by stale Go base-image pin

`deploy/docker/Dockerfile.bundle` pinned `golang:1.26.3-bookworm`
while `go.mod` has required go ≥ 1.26.4 since the stdlib-CVE bump;
the official golang images set `GOTOOLCHAIN=local`, so the v0.2.0
tag's release-image workflow failed with "go.mod requires go >=
1.26.4 (running go 1.26.3)" instead of self-upgrading. The base image
now matches the directive (the goreleaser binary release was
unaffected — it reads the toolchain from `go-version-file: go.mod`).
Keep the two in lockstep when bumping the `go` directive.

### Changed — pg-manager pin bumped to v0.4.1

`go.mod` now requires `github.com/f1bonacc1/pg-manager v0.4.1`
(previously v0.3.0). The bump picks up two reconciler fixes that the
integration suite depends on and that the nightly workflow (which
checks out the pinned tag as the sibling build context) needs to run
reliably: pg_rewind now receives the leader's real conninfo instead
of a placeholder that wedged every demoted ex-primary, and the
destructive `postgres_crashed` verdict is debounced so a transient
probe blip during a planned promote can no longer park a healthy
primary in terminal `StateFailed`. The major-upgrade gate sentinel
pinned by the feature-004 tests is unchanged in v0.4.1
(`manager/backup_upgrade.go:102`).

### Added — feature 004 upgrade test coverage + nightly integration CI

Behavioral coverage for the existing upgrade surface
(`POST /v1/upgrade/prepare|execute`) across {minor, major} × {happy,
bad} paths (`specs/004-upgrade-tests/`), in two tiers: contract tests
with a fault-injected `fakeEngine`
(`internal/control/handlers_upgrade_test.go`) and integration tests
against the real engine, PostgreSQL 17, and embedded NATS
(`tests/integration/lcm_upgrade_test.go`). The minor happy path
proves exactly one postmaster restart; major execution pins the
upstream gate sentinel verbatim so a pg-manager bump that lifts the
gate fails loudly. A new nightly workflow
(`.github/workflows/nightly-integration.yml`) runs the full
`make integration` suite on a schedule — previously no CI executed
the integration tier at all. The full suite was also hardened to run
green end-to-end: the forced-failover test restores the peer it
kills, runs last (`zz_failover_test.go`), gates on cluster
convergence before selecting its victim, and dumps per-peer
control-plane diagnostics on failure; `make integration`/`make smoke`
now pass `-count=1` because Go's test cache cannot see Docker state.

### Fixed — observability gauges, SIGHUP reload, and failover tunables

Suite hardening surfaced production wiring gaps:
`pgman_proxy_embedded_nats_up`, `routes_meshed`, `replicas_factor`,
and `leadership_state` were registered but never set (now driven by
`internal/runtime/obs_gauges.go`); the SIGHUP reload handler existed
but was never installed in `main` (now wired, so
`sighup_reload_outcomes_total` moves); and two policy knobs required
by Constitution III were unbindable from the environment —
`PGMAN_PROXY_POLICY_FAILOVER_DELAY` and the new
`PGMAN_PROXY_CLUSTER_LEASE_TTL` (`cluster.lease_ttl`, zero keeps the
adapter's 5s default).

### Changed — SC-002 leader-failover budget revised to 45s p99

Constitution v1.3.0. Instrumenting the unplanned-failover pipeline
showed the p99 is dominated by structure, not timers: when the killed
primary also hosts the embedded-NATS JetStream KV leader (~1/3 of
boots) survivors spend 5–10s in JetStream re-election before the
lease can be read, and after promote the first commit stalls ~4s
until a standby re-attaches (correct no-data-loss behavior). Measured
~7s fast path, 16–29s colocated on a workstation; 45s adds margin for
shared CI runners. Spec references updated across 001/002.

### Added — feature 003 `pgmctl` operator CLI

The operator CLI ships as a kubectl-style client. Single statically-
linked Go binary; consumes only the documented `/v1/*` control plane
and the embedded-NATS observability surface. Build with `make pgmctl`;
regenerate docs with `make pgmctl-docs`.

**Subcommands** (see `docs/pgmctl/reference/` for per-command pages):
- Read: `status`, `health`, `topology`, `lag`, `get`, `list`, `describe`,
  `events`, `dump`, `doctor`, `explain`, `watch`.
- Mutating (single-resource, `[y/N]` prompt, `--yes` bypass):
  `fence`, `unfence`, `set-config`.
- Cluster-affecting (typed cluster-name confirmation, `--force --cluster <name>` bypass):
  `failover`, `switchover`, `promote`, `restart`.

**Server-side endpoints** added by 003 (all additive MINOR-version
changes to the 001 contract; see `contracts/control-plane-extensions.md`):
- `GET /v1/watch/{status,transitions,events,node/<id>}` — SSE
  streams with 15s keepalive, `Last-Event-ID` resumption, and
  `gap_marker` framing.
- `GET /v1/doctor/checks` + `POST /v1/doctor/run` + `POST /v1/doctor/fix` —
  eight v1 checks with severity, evidence, and an advisory-only
  fix mode in v1.
- `POST /v1/restart {target=postgres|proxy}` — local-mode restart;
  `target=proxy` requires detected process supervisor or
  `proxy.assume_supervised=true`.
- `GET /v1/history` — JetStream-backed event + audit history with
  `--since`, `--type`, `--node`, `--cursor`, `--list-types`.
- `POST /v1/config/set` — SIGHUP-equivalent reload trigger,
  restricted to a closed allow-list (`cluster.route_peers`,
  `cluster.password`).

**Upstream pin**: `github.com/f1bonacc1/pg-manager` now requires
`Manager.RestartPostgres(ctx context.Context) error`, shipped in
pg-manager `v0.3.0`. `go.mod` pins that tag. The module is private, so
local / CI / GoReleaser builds fetch it with `GOPRIVATE` + a git
credential, and the bundled image fetches it through a BuildKit secret
mount (the token never enters a layer, cache, or provenance). Local
integration builds (`tests/integration/Dockerfile`) instead redirect it
at the sibling `../pg-manager` source via a build-stage `replace`; use a
git-ignored `go.work` for cross-repo development.

**Configuration**: pgmctl reads `$XDG_CONFIG_HOME/pgmctl/config.yaml`
(mode `0600` required; loader refuses looser perms). Kubeconfig-style
contexts; one active per invocation. Bearer token sources:
`token-env`, `token-file`, `token-command`. New proxy-side config key
`proxy.assume_supervised: bool` (default false) overrides supervisor-
presence detection for `restart --target=proxy`.

**Observability**: nine new Prometheus series under
`pgman_proxy_watch_*`, `pgman_proxy_history_*`, and `pgman_proxy_fanout_*`.
New structured-log events: `watch.stream_started`, `watch.stream_closed`,
`proxy.supervisor_presence_detected`, `proxy.self_restart_initiated`,
`proxy.history_stream_ready`, `proxy.leader_changed` (synthesized from
state_transition; closes a pg-manager zombie-event gap pending
upstream `B-010`).

**Security invariants verified by test**:
- FR-009 — bearer-token leak audit (`internal/control/auth_leak_test.go`):
  token literal never appears in logs, response envelopes, or audit
  records across accepted / refused / mutating / sink-failure paths.
- FR-033 — strict-redact removes cluster id, node ids, hosts, IPs,
  and embedded-NATS listen addrs from dump artifacts
  (`internal/pgmctl/dump/redact_strict_test.go`).

### Fixed — v1 stability pass (SOAK-01 28k-row data loss)

- **STAB-01 — stale-standby promotion (REQ-DL-01 violation)**: leader
  election now consults a WAL-currency gate before `pg_promote()`.
  Wire-up of pg-manager's existing `PromotionEligible` /
  `PromoteLSNTolerance` machinery into the reconciler dispatch path,
  plus periodic `publishCurrentLSN` from every tick. A candidate more
  than 16 MiB (one WAL segment) behind the most-aligned peer self-
  refuses. SL-4 self-veto blocks candidates with accumulating
  stale-WAL ticks. New gauge `pgman_proxy_no_eligible_primary` fires
  when election is blocked by asymmetric staleness; the operator
  break-glass remains `Manager.Promote` (unchecked). Closes the
  SOAK-01 28,185-row data-loss event (pg-manager fix lands upstream
  via the local `replace` directive).
- **STAB-02 — divergent ex-primary wedged when PG won't start**:
  pg-manager now reads on-disk `pg_controldata` directly via a new
  `ControlDataReader` extension to `PostgresExecutor` and derives
  divergence from `(local_tli, local_lsn)` vs the cluster's published
  `pgmgr/<cluster>/timeline_fork/<tli>` record (written by every
  successful promote). The auto-rebootstrap pipeline now fires
  without requiring `PostgresUp=true`; the `!PostgresUp →
  EventPostgresCrashed` transition is suppressed while controldata
  divergence is observed, so the rebootstrap accumulator runs to
  completion instead of stranding the peer in operator-sticky
  `StateFailed`.
- **STAB-03 Part 1 — AutoRebootstrap timing knobs (wrapper)**: new env
  aliases `PGMAN_PROXY_POLICY_AUTO_REBOOTSTRAP_COOLDOWN` and
  `PGMAN_PROXY_POLICY_AUTO_REBOOTSTRAP_PERSISTENCE_WINDOW` plumbed
  through `internal/config/loader.go` and the policy literal in
  `internal/runtime/start.go`. The chaos rig
  `process-compose.yaml` sets `cooldown=30s, persistence_window=10s`
  so a single rebootstrap doesn't park the cluster for an hour. Zero
  values fall through to pg-manager's documented production defaults
  (1h cooldown, 5min window).
- **STAB-03 Part 2 — condition-keyed cooldown bypass (upstream)**:
  pg-manager's `RebootstrapHistory` now carries
  `LastTriggerCondition`. `CooldownElapsed` permits a fresh-condition
  bypass — a `stale_wal` recovery does not gate a later
  `divergent_ex_primary` recovery in the same cooldown window. Loop
  protection on same-condition repeats is preserved.

### Changed — milestone 002 (embedded NATS cluster) — IN PROGRESS

- **Breaking**: external NATS dependency removed. Every `pgman-proxy`
  peer now embeds a NATS server in-process via the upstream
  `github.com/nats-io/nats-server/v2` Go module. The peers mesh into a
  NATS cluster via NATS routes (TCP/6222 by default). 001's `nats.url`
  / `nats.creds_file` / `nats.token_env` configuration keys are
  fail-closed at validation with a migration-pointing error
  (FR-002, SC-009). The `--nats` CLI flag and the `NATS_URL` env-var
  alias are removed.
- **Cluster credential model** (RD-001a, discovered during
  /speckit-implement Phase 2): NATS server v2.14 cluster routes only
  support a single shared username/password pair, not per-peer NKey
  credentials as originally clarified at /speckit-clarify time. The
  spec was amended; the wire-level credential is shared, per-peer
  identity in audit logs comes from the NATS server-name field
  (= pgman-proxy node ID).
- **Constitution v1.1.0 → v1.2.0** (MINOR): the *Architecture
  Overview* and *Topology & Dependencies* subsection of *Additional
  Constraints* were broadened to describe NATS as embedded rather
  than operator-provisioned. No principle removed or redefined.
- **JetStream replication factor** auto-derived from cluster size
  (FR-011a / RD-004): R=1 / 2 / 3 for declared sizes 1 / 2 / ≥3,
  capped at 3. Operator override requires a named, audit-logged
  opt-in.
- **SIGHUP hot-reload** of two surfaces only (FR-014a): peer-routes
  list and cluster password. Every other config key is startup-only.
  `ExecReload=/bin/kill -HUP $MAINPID` added to the systemd unit.
- **New CLI subcommand** `pgman-proxy cluster-secret-gen` produces a
  base32-encoded 32-byte cryptographically random cluster password.
  Operators do not need any NATS-ecosystem CLI tool to run
  `pgman-proxy` (FR-014).

### Changed — runtime / config defaults

- **Default `Policy.LivenessInterval`** lowered from `5s` to `2s` in
  `internal/config/config.go` `Defaults()`. Drives reconciler tick
  cadence + per-peer postgres liveness probes + (3× interval) →
  `QuorumSnapshotStaleAfter`. Net effect on production: a confirmed
  primary failure starts the failover-delay clock ~2.5× faster, the
  quorum snapshot ages to "stale" at 6s instead of 15s, and CPU /
  NATS-heartbeat overhead grows by a single `SELECT 1` per node every
  2s (negligible). Operators wanting the legacy 5s cadence override
  via `PGMAN_PROXY_POLICY_LIVENESS_INTERVAL=5s`. pg-manager's library
  default is unchanged at 5s.

### Added — startup_with_pgdata split-brain guard

- **Pre-emptive `standby.signal` write** on every startup with an
  initialized PGDATA (`internal/runtime/start.go::ensureStandbySignalIfInitialized`).
  Honors pg-manager's documented "assume standby until proven primary"
  contract at the postgres level — previously pg-manager declared
  `role=standby` while postgres-on-disk could still come up as a
  primary, producing a structural split-brain window that depended on
  auto-demote's stability + cooldown gates to close. The rightful
  primary now pays one extra `pg_ctl promote` cycle on startup
  (idempotent, ~tens of ms). Chaos-rig verification: six scenarios
  including `docker restart primary` and `cascading kill` now recover
  in ~10s with two clean streaming standbys; before this change those
  two scenarios reliably birthed two- or three-primary split-brain.

### Added — config knobs

- New env vars wire previously-hidden pg-manager `AutoDemote` fields
  through the proxy config (zero values pass through to pg-manager's
  documented defaults: `Cooldown=1h`, `LeadershipStabilityWindow=15s`,
  `ProbeTimeout=5s`):
  - `PGMAN_PROXY_POLICY_AUTO_DEMOTE_COOLDOWN`
  - `PGMAN_PROXY_POLICY_AUTO_DEMOTE_LEADERSHIP_STABILITY_WINDOW`
  - `PGMAN_PROXY_POLICY_AUTO_DEMOTE_PROBE_TIMEOUT`

  Defaults are unchanged; the chaos rig uses overrides in
  `process-compose.yaml`. Split the proxy's `AutoRecoveryCfg` (now used
  only for `AutoRebootstrap`) from a new `AutoDemoteCfg` carrying the
  three duration fields.

### Added — milestone 001 (active/active proxy + LCM control plane)

#### Data-plane proxy (US1)
- Active/active topology in front of any pg-manager-managed PostgreSQL
  cluster. Three peers coordinate via NATS (leader-elect + state store +
  event bus); each peer accepts client traffic on `proxy.listen_addr`
  and proxies to the current leader without any VIP / floating IP.
- Mode-aware listener defaults (US2 / FR-013): `standalone`,
  `microservice`, `sidecar`. Sidecar mode rewrites all-interfaces binds
  (`0.0.0.0:N` / `[::]:N` / `:N`) to `127.0.0.1:N` so a colocated proxy
  is unreachable from outside the pod.

#### LCM control plane (US4)
- Authenticated HTTP+JSON control plane (default `:9091`) exposing
  every `pg-manager` `Manager` LCM method: `Status`, `Diagnose`,
  `Switchover`, `Failover`, `Promote`, `Fence`, `Unfence`,
  `UpdateTopology`, `TriggerBackup`, `PrepareUpgrade`, `ExecuteUpgrade`.
- Bearer-token auth with **hot rotation** (FR-031): tokens re-read on
  every request from `control.auth.token_env` or `control.auth.token_file`.
  Constant-time compare via `crypto/subtle`. Audit `actor` field shows
  `bearer:<sha256-prefix>` — never the secret.
- Dual-sink audit: every request lands on slog + the NATS subject
  `pgman_proxy.<cluster_id>.audit.lcm`. Mutating ops fail-closed
  (`audit_unavailable`, HTTP 503) when either sink rejects (FR-028).
- Leader routing in two modes (FR-026): `forward` (NATS req/reply
  bounded by `control.leader_route_timeout`, default 30s, FR-034) or
  `redirect` (HTTP 307 with `Location` to the leader).
- TLS REQUIRED on non-loopback control-plane binds (FR-033). Plaintext
  on a non-loopback address requires the explicit, audit-logged opt-in
  `control.tls.plaintext_explicit_ack: true`.
- `TriggerBackup` returns `backup_executor_missing` (HTTP 412) when no
  operator-supplied `BackupExecutor` is wired (FR-030). Reference
  filesystem implementation lives at `examples/backup-fs/` — out of
  tree by design (Constitution IV).

#### Observability (US3)
- slog JSON logs with stable field schema: `cluster_id`, `node_id`,
  `component`, optional `trace_id` / `span_id`. Every documented log
  event from `contracts/observability.md § Required event names` is
  emitted at the documented level with the documented fields.
- Prometheus metric set under `pgman_proxy_*`: connection / query /
  coordination / LCM groups. Histograms use the documented bucket
  layout. Per-peer `cluster_id` + `node_id` const-labels.
- W3C trace-context (`traceparent`) propagation: inbound HTTP echoes
  on `/healthz`, `/readyz`, `/metrics`, and `/v1/*`; the audit record
  carries `trace_id`/`span_id`; coordination events read `traceparent`
  from `nats.Msg.Header` and surface it on the log line.
- `/healthz` (liveness, `200` once past arg-parsing) and `/readyz`
  (NATS-up + listener-up + manager-past-singleton) on `obs.health_addr`
  (default `:9090`); `/metrics` on the same listener; `/debug/pprof`
  served only when `obs.log_level == debug`.

#### Process lifecycle
- 11-step startup gate sequence with documented exit codes
  (`EX_OK=0`, `EX_OBS=74`, `EX_DEPS=75`, `EX_LISTEN=76`,
  `EX_SINGLETON=77`, `EX_CONFIG=78`, `EX_DRAIN_TIMEOUT=79`,
  `EX_INTERNAL=80`, `EX_CONTROL=81`).
- Graceful shutdown stops the control plane FIRST (so no new mutating
  LCM call lands while the engine winds down), then the data plane,
  then `manager.Stop()`, then NATS `Drain()`. Bounded by
  `shutdown.drain_budget` (default 30s).
- Signal handling: SIGINT / SIGTERM trigger graceful shutdown;
  SIGHUP / SIGUSR1 logged-only in v1 (reserved for config reload /
  diag dumps).
- `--version`, `--print-config` (redacted JSON, FR-017),
  `--config <path>` plus per-key flag overrides (`--cluster-id`,
  `--node-id`, `--peers`, `--nats`, `--listen`, `--switch-policy`,
  `--log-level`, `--metrics`).

#### Configuration
- Layered loader: flags > env > YAML > defaults. Canonical
  `PGMAN_PROXY_*` env vars plus backward-compat aliases (`NATS_URL`,
  `CLUSTER_ID`, `NODE_ID`, `PEERS`, `PGDATA`, `PG_BINDIR`,
  `PROXY_LISTEN`, `LOCAL_DSN`).
- Cross-field validation aggregated through `MultiError`: required
  keys, identifier regexes, peer-set membership, switch-policy enum,
  TLS-disable explicit-ack, control-plane TLS / plaintext-ack /
  loopback rules, leader-route enum + range, mutually-exclusive token
  sources.
- `postgres.hba_extras` / `postgres.conf_extras` (US1 scaffold) — the
  bootstrap leader's `PostInitDB` hook appends these to `pg_hba.conf`
  and `postgresql.conf` so peers can stream replication. Operators
  MUST supply at least one `host replication ...` entry (the library
  refuses to synthesise its own).

### Out of scope (still — do not file as a defect)
- Kubernetes / Helm / CRD / controller-runtime / admission-webhook
  surfaces — separate project per FR-015.
- VIP / keepalived / floating IPs.
- Restore / PITR / a bundled backup backend.
- mTLS for the control plane.
- Per-operation RBAC.
- gRPC / Protobuf surface (HTTP+JSON only in v1).
