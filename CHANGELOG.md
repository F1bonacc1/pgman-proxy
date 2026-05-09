# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
