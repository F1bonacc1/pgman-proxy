# COVERAGE-REQUIREMENTS — pgman-proxy v1 (feature 002 embedded NATS cluster)

**Status**: GO/NO-GO bar for the v1 ship of feature 002.
**Audience**: QA, SRE, downstream test-plan authors. The DBA-agent test
plan and the SW-engineer test harness will both consume the REQ-ids and
scenario-ids below verbatim.

This document does NOT define tests. It defines what tests MUST measure
and the thresholds they MUST meet.

Reference set:

- `.specify/memory/constitution.md` v1.2.0 (post-amendment)
- `specs/002-embedded-nats-cluster/{spec,plan,research}.md`
- `specs/002-embedded-nats-cluster/contracts/{config,observability,lifecycle,cluster-credentials,constitution-amendment}.md`
- `specs/001-active-active-pg-proxy/spec.md` (FR-001 … FR-034, SC-001 … SC-013)
- `../pg-manager/types.go` (`Policy`, `AutoDemotePolicy`, `AutoRebootstrapPolicy`)
- `cmd/chaos-workload/main.go` (counters: `writes_ok`, `writes_failed`, `data_loss_total`, `extra_rows`)
- `process-compose.yaml` (chaos-rig topology + policy overrides)

Scenario-id namespaces used by acceptance criteria below (the DBA agent's
test plan owns the concrete steps for each):

| Prefix | Class |
|---|---|
| `DI-NN` | Data integrity (counters + verifier-loop assertions) |
| `NET-NN` | Network partition / packet drop / black-hole |
| `KILL-NN` | Process termination (SIGKILL / SIGTERM / panic) |
| `RES-NN` | Resource exhaustion (disk-full, OOM, FD-limit) |
| `LCM-NN` | Control-plane lifecycle operations |
| `BOOT-NN` | Startup / validation / fail-closed |
| `SLO-NN` | Latency, RTO, and rate budgets |
| `OBS-NN` | Observability signal presence + shape |
| `SEC-NN` | Authz / TLS / secret-handling |
| `REPL-NN` | Replication-slot, WAL, divergent_ex_primary scenarios |
| `TX-NN` | In-flight transaction behaviour during failover |

---

## §1 Requirements enumeration

### Data-loss commitments

| REQ-id | Source | Stated commitment |
|---|---|---|
| `REQ-DL-01` | Constitution II; pg-manager `Policy.OnSyncStandbyLoss=BlockWritesOnSyncLoss` default; `process-compose.yaml` postgres conf | A `COMMIT` returning success to a client MUST be durable on the primary AND replicated synchronously to at least one standby (`synchronous_standby_names = 'ANY 1 (...)'`, `synchronous_commit = on`). |
| `REQ-DL-02` | 001 FR-005, FR-011; Constitution III | The proxy MUST NOT route writes to a node that does not currently hold the NATS leadership lease. Stale-leader writes are forbidden. |
| `REQ-DL-03` | 001 spec edge case "client sends a write at the moment of leader transition"; default switch policy hard-close | At leader transition, in-flight client sessions MUST be hard-closed (default switch policy); silent re-routing of an in-flight write to a now-non-leader is forbidden. |
| `REQ-DL-04` | 002 spec edge case "disk full while embedded NATS is running"; Constitution III | A peer whose coordination substrate cannot durably update (JetStream disk-full / path-unwritable / quota-exceeded) MUST emit `embedded_nats.storage_degraded` AND self-fence (stop serving writes). |
| `REQ-DL-05` | Constitution III "self-fence on lease loss"; 001 FR-011 | A peer whose NATS lease cannot be confirmed at the moment of a write MUST refuse the write. Lease-loss is checked at action time, not just at election time. |

### Failover SLO / Availability commitments

| REQ-id | Source | Stated commitment |
|---|---|---|
| `REQ-AVAIL-01` | 001 SC-002; 002 SC-003; Constitution "performance baseline" | After a forced leader failover, a freshly reconnected client MUST reach the new leader through any proxy peer within **5 s p99** for the leader-change-observed-as-structured-event signal. |
| `REQ-AVAIL-02` | 001 SC-007 | MTTR for a proxy peer crash under a standard process supervisor MUST be **< 10 s** from process-exit to `/readyz=200`. |
| `REQ-AVAIL-03` | 002 SC-002 | Single-peer startup time (process exec → `ready`) MUST be **< 5 s** on a developer laptop, no external network calls besides the upstream PostgreSQL. |
| `REQ-AVAIL-04` | 002 SC-008 / RD-006 | Rolling restart of all 3 peers MUST complete end-to-end in **≤ 60 s p99**, per-peer **≤ 20 s** (5 s drain + 5 s exit + 5 s boot + 5 s mesh + leadership re-confirm). |
| `REQ-AVAIL-05` | 001 SC-012 | Planned `Switchover` LCM on a healthy 3-peer cluster MUST complete (request → new-leader-confirmed-in-`Status`) in **< 15 s p99**. |
| `REQ-AVAIL-06` | 001 SC-003; Constitution "perf baseline" | Per-query overhead added by the proxy hop MUST stay **< 1 ms p99** on simple-query loopback bench. |

#### Derived RTO budget (informative — drives REQ-AVAIL-01)

`Policy.LivenessInterval = 2s` (proxy default, CHANGELOG entry 42984f7),
`LivenessFailures = 3` → confirmed-failure detection ≈ 6 s worst case.
`QuorumSnapshotStaleAfter = 3 × LivenessInterval = 6s`.
The 5 s p99 budget in REQ-AVAIL-01 measures **time from
`leadership.changed` event to first successful client write on the new
leader through any peer**, NOT the worst-case detection latency. See
**§5 Open Q-1** for the unstated detection-vs-routing split.

### Self-healing commitments

| REQ-id | Source | Stated commitment |
|---|---|---|
| `REQ-HEAL-01` | `Policy.AutoDemote.Enabled=true` (process-compose); pg-manager AutoDemotePolicy; CHANGELOG f1d67d7, c05e304 | When a peer comes up with PGDATA-on-disk reporting primary while the coordination plane has elected another leader (Ex-Primary Divergence, B-003), `AutoDemote` MUST wipe PGDATA + re-basebackup against the elected leader, gated by `LeadershipStabilityWindow` + `Cooldown` + per-leader probe. |
| `REQ-HEAL-02` | `Policy.AutoRebootstrap.Enabled=true`; pg-manager AutoRebootstrapPolicy | When a standby cannot resume streaming because required WAL has been recycled, `AutoRebootstrap` MUST wipe PGDATA + re-basebackup, gated by `PersistenceWindow` + `Cooldown` + cluster-wide lease. |
| `REQ-HEAL-03` | 002 US3 acceptance scenario 1; Constitution III; FR-013 `route_down` event | Network partition that isolates a peer MUST cause the partitioned peer to self-fence; remaining quorum MUST continue to serve. Both transitions visible as structured events. |
| `REQ-HEAL-04` | 002 spec US1 acceptance scenario 4 | A killed peer that is restarted MUST rejoin the NATS mesh, re-acquire pg-manager adapter subscriptions, and resume participation without operator action. |
| `REQ-HEAL-05` | CHANGELOG d239a6f — `data_loss_total` distinct-unresolved semantics | Transient verifier spikes on `data_loss_total` (stale-read window during failover) MUST settle to the pre-incident baseline within the failover budget. Verdict logic MUST compare **post-settle** to **pre-baseline**, not peak. |
| `REQ-HEAL-06` | pg-manager B-003 divergence path; AutoDemote spec | Detection of `DivergenceParkedEvent` (ex-primary still in `pg_is_in_recovery()=false` after another peer has the lease) MUST be observable AND resolved by AutoDemote when its gates are met. |
| `REQ-HEAL-07` | 002 spec US1 acceptance scenario 3; Constitution III | Leader election MUST converge after any single-peer loss within the SLO (REQ-AVAIL-01). |

### Liveness / readiness

| REQ-id | Source | Stated commitment |
|---|---|---|
| `REQ-LIVE-01` | 001 FR-009 | `/healthz` MUST return 200 once past arg-parsing (liveness, process-supervisor signal). |
| `REQ-LIVE-02` | 001 FR-009; CHANGELOG | `/readyz` MUST gate on NATS-up + listener-up + manager-past-singleton; non-200 when any fail-closed condition is met. |
| `REQ-LIVE-03` | 001 FR-014; 002 FR-012 | SIGTERM/SIGINT MUST drain in-flight queries within `shutdown.drain_budget` (default 30 s); exit code 0 on clean shutdown; embedded NATS shut down cleanly (no orphan socket / lock / partial JS state). |

### Control-plane availability

| REQ-id | Source | Stated commitment |
|---|---|---|
| `REQ-CP-01` | 001 FR-026 | Leader-only LCM requests on a non-leader peer MUST be transparently forwarded over NATS (default) OR returned as a 307 redirect identifying the leader. **Never** executed locally on a non-leader. |
| `REQ-CP-02` | 001 FR-034 | `control.leader_route_timeout` (default 30 s, range `(0,5m]`) MUST bound the wait on forwarded LCM; on timeout return `leader_route_timeout` AND audit the timeout. No silent retry, no silent local execution. |
| `REQ-CP-03` | 001 FR-028; SC-010 | Mutating LCM with an unavailable audit pipeline MUST be rejected (`audit_unavailable`, HTTP 503). 100 % of LCM (accepted, rejected, failed) MUST produce both slog + NATS-subject audit records. |
| `REQ-CP-04` | 001 FR-025, FR-031 | Bearer tokens MUST be re-read on every request (hot rotation, no process restart). Constant-time compare. `actor` field is `bearer:<sha256-prefix>`. |
| `REQ-CP-05` | 001 FR-029 | Mutating LCM during cluster bootstrap or in-flight leadership transition MUST be refused with a transient-condition error. |

### Coordination commitments

| REQ-id | Source | Stated commitment |
|---|---|---|
| `REQ-COORD-01` | 002 FR-001, FR-002 | The bundled binary MUST embed NATS in-process. NO external `nats-server` process MAY exist on any host in production deployment. Legacy `nats.url` config MUST fail-closed at validation (FR-002, SC-009). |
| `REQ-COORD-02` | 002 FR-011a | JetStream KV replication factor MUST be derived from `cluster.declared_size`: R=1 / 2 / 3 for sizes 1 / 2 / ≥3. R override is audit-logged. The R value MUST appear in `pgman_proxy_embedded_nats_replicas_factor` gauge AND in `Status.cluster.embedded_nats.replicas_factor`. |
| `REQ-COORD-03` | 002 FR-003, US1 acceptance 1 | Three peers configured with each other's route addresses MUST converge on a 3-route mesh AND elect exactly one leader within the configured startup budget. |
| `REQ-COORD-04` | 002 FR-014a / RD-001a; lifecycle.md SIGHUP section | SIGHUP MUST hot-reload exactly `cluster.peers` + `cluster.password`. Non-allow-list keys MUST be ignored with a structured `reload_skipped` warning. Reload latency target **< 1 s p99** on a 3-peer cluster. |
| `REQ-COORD-05` | 002 FR-010a; cluster-credentials.md | Cluster-password rotation via SIGHUP across all peers MUST maintain quorum throughout. Audit logs MUST carry the 8-char `password_prefix` on every `route_up` and `reload_applied`. |
| `REQ-COORD-06` | 002 FR-007; lifecycle.md exit codes | Embedded-NATS startup failures (port collision, missing TLS, missing credentials, unwritable JS path, ready timeout) MUST be fail-closed with the documented exit codes (78 CONFIG / 75 TEMPFAIL / 73 CANTCREAT). Partial startup forbidden. |
| `REQ-COORD-07` | 002 spec edge case "cluster-name mismatch" | A peer presenting a different `cluster.name` OR a mismatched credential MUST be refused at the route handshake AND produce `cluster_route.auth_failed` event. Routes MUST NOT silently fall back to unauthenticated. |

### Audit fail-closed

| REQ-id | Source | Stated commitment |
|---|---|---|
| `REQ-AUDIT-01` | 001 FR-027, FR-028; SC-010 | Every LCM request (accepted / rejected / failed) MUST produce a structured audit record on both slog AND `pgman_proxy.<cluster_id>.audit.lcm`. The record MUST include timestamp, op, target, actor, source addr, outcome, latency. |
| `REQ-AUDIT-02` | 001 FR-028 | Mutating LCM MUST be refused when either audit sink is unavailable. |
| `REQ-AUDIT-03` | observability.md route_up entry; FR-010a | Every `embedded_nats.route_up` event MUST include `peer_node_id` (sibling identity from NATS server-name) AND `password_prefix` (first 8 chars of base32 password). |

### Constitutional principles testable as code/operational gates

| REQ-id | Source | Stated commitment |
|---|---|---|
| `REQ-CON-01` | Constitution VII; 002 SC-007 | No Kubernetes / Helm / CRD / controller-runtime / webhook code in the repo. Automated grep gate in CI. |
| `REQ-CON-02` | 001 SC-013 | No PostgreSQL DDL / `pg_basebackup` / `pg_rewind` / `initdb` / `pg_upgrade` / `pg_ctl promote` / replication-slot manipulation in this repo's source tree. (Pinned: LCM logic lives in pg-manager.) |
| `REQ-CON-03` | Constitution I; 001 FR-016 | Pass-through wire bytes MUST NOT be re-encoded. SQLSTATE + severity round-trip byte-accurate. |
| `REQ-CON-04` | 001 FR-004; Constitution Architecture Overview | The proxy MUST NOT manage virtual IPs / gratuitous ARP / floating addresses. Verifiable by absence of related code. |
| `REQ-CON-05` | 001 FR-018; FR-033; 002 FR-009, FR-010b | Default upstream PG TLS is `verify-full`. Control-plane non-loopback bind requires TLS or explicit-ack opt-in. Cluster-routes non-loopback bind requires TLS+credentials or explicit-ack opt-in. |
| `REQ-CON-06` | 001 FR-017; 002 FR-010 | Secrets (DB creds, NATS auth, cluster password ≥ 16 bytes, TLS keys) MUST be sourced via SecretRef (env / file / secret-manager). Plaintext-in-config for these MUST fail-closed at validation. |
| `REQ-CON-07` | 001 assumption "no PITR / restore in v1" | No restore / PITR / bundled backup backend code. `TriggerBackup` without `BackupExecutor` returns `backup_executor_missing` (HTTP 412). |

---

## §2 Acceptance criteria

Each REQ-id below resolves to one or more measurable criteria. Signals
are drawn from `contracts/observability.md`, the chaos-workload
counters (`writes_ok` / `writes_failed` / `data_loss_total` /
`extra_rows`), Prometheus metrics, SQL probes, control-plane responses,
exit codes, and file-system artefacts.

**`data_loss_total` semantics note**: this counter is **distinct-unresolved**
(CHANGELOG d239a6f). Transient spikes during stale-read windows are
correct and expected behaviour — they MUST settle to pre-baseline within
the failover budget. Verdict logic compares **post-settle** to
**pre-incident-baseline**, not peak. Acceptance criteria below encode
this explicitly as `data_loss_total(t_end) == data_loss_total(t_start)`.

### REQ-DL-* (data loss)

| Criterion | Signal | Threshold | Scenario(s) |
|---|---|---|---|
| `AC-DL-01a` | chaos-workload `data_loss_total` | `dl(t_end) == dl(t_start)` (no NEW unresolved seqs) across the scenario window | `DI-01` baseline; every `KILL-*`, `NET-*`, `RES-*` |
| `AC-DL-01b` | chaos-workload `extra_rows` | `extras == 0` at scenario end | every `KILL-*`, `NET-*`, `RES-*`, `TX-*` |
| `AC-DL-01c` | `slog: DATA LOSS — acknowledged commit not readable` | Either absent for the entire window, OR every emitted line MUST be matched by a later `data loss resolved` line within the failover budget (REQ-AVAIL-01) | `DI-02` failover-under-write |
| `AC-DL-01d` | postgres `synchronous_standby_names` + `synchronous_commit` settings on the elected primary | Exactly `'ANY 1 (...)'` and `on` respectively, for every leader observed during the run | `BOOT-04`, sampled in every scenario |
| `AC-DL-02a` | proxy `slog: write_routed` event (or equivalent) destination node_id | Destination node_id MUST equal `leadership.current_leader_id` at routing time | `DI-02`, `NET-03` partition-during-write |
| `AC-DL-02b` | `pgman_proxy_lease_renewal_failures_total` on the previous leader | Non-decreasing through the transition; previous leader's leadership-state gauge → `not-leader` within REQ-AVAIL-01 | `KILL-01` kill-leader, `NET-03` |
| `AC-DL-03a` | TCP RST / FIN on client sessions held by previous-leader proxy | Within `switch_policy.hard_close` budget after `leadership.changed` event | `TX-01` in-flight-tx-at-failover |
| `AC-DL-03b` | chaos-workload `writes_failed` correlation with leader transition | `writes_failed` increments during the transition window are paired with `leadership.changed` events; no silent success | `TX-01` |
| `AC-DL-04a` | `embedded_nats.storage_degraded{kind="disk_full"\|"path_unwritable"\|"js_corruption"\|"quota_exceeded"}` | Emitted within 30 s of the simulated condition | `RES-02` disk-full-on-JS-path |
| `AC-DL-04b` | `pgman_proxy_embedded_nats_storage_degraded` gauge | == 1 with correct `kind` label | `RES-02` |
| `AC-DL-04c` | proxy refuses new writes on this peer | Verifiable by client INSERT receiving error (or routing to a non-degraded peer) | `RES-02` |
| `AC-DL-05a` | `lease_renewal_failures_total` non-zero AND leadership-state == `not-leader` AND no write activity on local upstream PG | Probe via PG log `received fast shutdown` correlation OR write-failure on local connect | `NET-04` lease-loss-without-process-death |

### REQ-AVAIL-* (failover SLO, throughput, MTTR)

| Criterion | Signal | Threshold | Scenario(s) |
|---|---|---|---|
| `AC-AVAIL-01a` | Time delta between SIGKILL of leader process and first successful chaos-workload INSERT against the new leader | **≤ 5 s p99** across ≥ 50 trials | `SLO-01` failover-RTO |
| `AC-AVAIL-01b` | `leadership.changed` event timestamp on each surviving peer | All survivors converge on identical `current_leader_id` within 1 s of first emission | `SLO-01` |
| `AC-AVAIL-02a` | Time delta from process exit to `/readyz == 200` under process-compose `restart: always` | **< 10 s p99** | `KILL-02` proxy-crash-restart |
| `AC-AVAIL-03a` | Single-peer cold start: process exec → `/readyz == 200` | **< 5 s** on the reference dev laptop | `BOOT-01` single-peer-cold |
| `AC-AVAIL-04a` | End-to-end rolling restart (rotate binary on all 3 peers) | **≤ 60 s p99**; per-peer **≤ 20 s** | `SLO-02` rolling-restart |
| `AC-AVAIL-04b` | Quorum maintained at every step during rolling restart | `pgman_proxy_embedded_nats_routes_meshed ≥ 1` on each survivor at all times; never 0 on more than one peer simultaneously | `SLO-02` |
| `AC-AVAIL-05a` | LCM `Switchover` → new-leader-confirmed-in-`Status` | **< 15 s p99** | `LCM-01` planned-switchover |
| `AC-AVAIL-06a` | Loopback simple-query bench p99 latency vs direct PG | **< 1 ms** added | `SLO-03` proxy-overhead-bench |

### REQ-HEAL-* (self-healing)

| Criterion | Signal | Threshold | Scenario(s) |
|---|---|---|---|
| `AC-HEAL-01a` | `slog: AutoDemoteAttemptedEvent` followed by `AutoDemoteAcceptedEvent` (or `AutoDemoteRefusedEvent` with documented `reason`) | Emitted within `LeadershipStabilityWindow + ProbeTimeout × ProbeFailureThreshold + epsilon` of divergence; PGDATA wiped + re-basebackup completes | `REPL-01` ex-primary-coldstart-with-stable-leader |
| `AC-HEAL-01b` | post-recovery: PG on the demoted node `pg_is_in_recovery() == true` AND streaming from current primary | SQL probe | `REPL-01` |
| `AC-HEAL-01c` | Cooldown enforced: a second divergence within `AutoDemote.Cooldown` MUST yield `AutoDemoteRefusedEvent{reason=cooldown_active}` | slog | `REPL-02` divergence-rapid-repeat |
| `AC-HEAL-02a` | `slog: AutoRebootstrapAttemptedEvent` → `AutoRebootstrapAcceptedEvent` with `PersistenceWindow` gate observed | Emitted only after WAL-stale condition held for ≥ `PersistenceWindow`; partition shorter than window produces ZERO rebootstraps | `REPL-03` stale-wal-recovery |
| `AC-HEAL-03a` | Partitioned peer's leadership-state gauge → `not-leader`; `embedded_nats.route_down{reason="peer_disconnect"}` on survivors | Within REQ-AVAIL-01 window | `NET-01` one-peer-isolated |
| `AC-HEAL-03b` | Surviving quorum continues to serve writes (`writes_ok` increases monotonically) | `writes_ok(t+30s) > writes_ok(t)` measured during partition | `NET-01` |
| `AC-HEAL-04a` | Restarted peer: `embedded_nats.server_ready` → `route_up` for each sibling → leadership-state gauge in {`leader`, `not-leader`} | All events emitted; no operator action required | `KILL-03` peer-restart |
| `AC-HEAL-05a` | `data_loss_total` time series across a failover | Peak MAY be non-zero (stale read); end-value within 60 s post-failover MUST equal pre-failover baseline | `DI-02` failover-under-write |
| `AC-HEAL-06a` | `DivergenceParkedEvent` emitted when AutoDemote disabled OR gates fail | slog | `REPL-04` divergence-no-auto-demote |
| `AC-HEAL-07a` | After single-peer kill, surviving 2-peer mesh re-elects a leader (or surviving leader retains lease) | `leadership.changed` (if applicable) within REQ-AVAIL-01 | `KILL-01` |

### REQ-LIVE-* (liveness, readiness, drain)

| Criterion | Signal | Threshold | Scenario(s) |
|---|---|---|---|
| `AC-LIVE-01a` | HTTP `GET /healthz` | 200 within 100 ms of process start past arg-parse | `BOOT-02` healthz-after-config-parse |
| `AC-LIVE-02a` | `/readyz` returns non-200 while NATS embedded server not ready / listener not bound / manager pre-singleton | HTTP status 503 (or documented non-200) | `BOOT-03` readyz-gate |
| `AC-LIVE-02b` | `/readyz` returns 200 only after all three gates pass | HTTP 200 | every scenario steady state |
| `AC-LIVE-03a` | SIGTERM → in-flight queries drain → exit code 0 | Within `shutdown.drain_budget`; `embedded_nats.server_stopped{reason="clean_shutdown"}` emitted | `KILL-04` graceful-sigterm |
| `AC-LIVE-03b` | Post-shutdown filesystem inspection | No orphan listening socket; no `*.lock`; no partial JS state file mid-write | `KILL-04` |

### REQ-CP-* (control-plane availability)

| Criterion | Signal | Threshold | Scenario(s) |
|---|---|---|---|
| `AC-CP-01a` | LCM mutating request to a non-leader peer (`forward` mode) | Request succeeds; audit record shows `routed_via=leader_<id>` | `LCM-02` non-leader-forward |
| `AC-CP-01b` | LCM mutating request to a non-leader peer (`redirect` mode) | HTTP 307 with `Location: https://<leader>:<port>...` | `LCM-03` non-leader-redirect |
| `AC-CP-01c` | Receiving peer NEVER executes a leader-only op locally | Audit + engine-side log verification | `LCM-02`, `LCM-03` |
| `AC-CP-02a` | Forward-mode request when leader is unreachable | Response: `leader_route_timeout`; audit captures timeout + leader-identity-at-request | `LCM-04` forward-timeout |
| `AC-CP-03a` | All LCM requests in a run produce slog + NATS audit subject | 1:1 correspondence; assertion runs on log+subject diff | `LCM-05` audit-completeness |
| `AC-CP-03b` | Mutating LCM with NATS audit subject black-holed (simulated) | HTTP 503 `audit_unavailable`; engine-side action NOT taken | `LCM-06` audit-failclosed |
| `AC-CP-04a` | Rotate `control.auth.token_env` between two requests; both succeed without proxy restart | HTTP 2xx both times | `SEC-01` token-rotation |
| `AC-CP-04b` | Audit `actor` field on every authenticated request | `bearer:<8-hex-sha256-prefix>`; raw token NEVER appears in any sink | `SEC-02` token-redaction |
| `AC-CP-05a` | Mutating LCM during cluster bootstrap | Rejected with `cluster_bootstrapping` (or equivalent transient code); audit shows reject | `LCM-07` bootstrap-window |
| `AC-CP-05b` | Mutating LCM mid-leadership-transition | Rejected with `leadership_transitioning`; audit shows reject | `LCM-08` transition-window |

### REQ-COORD-* (coordination / embedded NATS)

| Criterion | Signal | Threshold | Scenario(s) |
|---|---|---|---|
| `AC-COORD-01a` | `ps -ef \| grep nats-server` on every host | Zero matches (no out-of-process NATS) | `BOOT-05` no-external-nats |
| `AC-COORD-01b` | Configuration containing legacy `nats.url` key | Exit code 78 (CONFIG); error message names the deprecated key and migration block | `BOOT-06` legacy-nats-url-rejected |
| `AC-COORD-02a` | `pgman_proxy_embedded_nats_replicas_factor` gauge | == 1 for declared_size=1; == 2 for size=2; == 3 for size ≥ 3 | `BOOT-07` replicas-factor-derivation |
| `AC-COORD-02b` | JS KV bucket `Replicas` field (via NATS server reflection or `Status` endpoint) | Matches `replicas_factor` gauge | `BOOT-07` |
| `AC-COORD-02c` | With `cluster.replication_factor_override` set, override is logged at every startup | slog at startup; gauge label `overridden="true"` | `BOOT-08` r-override-loud |
| `AC-COORD-03a` | 3-peer cluster startup: each peer emits `route_up` for the other two; `routes_meshed` gauge == 2 on each | Within startup budget | `BOOT-09` 3-peer-mesh-converge |
| `AC-COORD-03b` | Exactly one peer holds the leadership lease (verified across all three `Status` responses) | Boolean | `BOOT-09` |
| `AC-COORD-04a` | SIGHUP after adding a new peer URL to `cluster.peers` | `embedded_nats.reload_applied{routes_added=[...]}` event emitted; new route comes up; reload latency **< 1 s p99** | `LCM-09` sighup-add-peer |
| `AC-COORD-04b` | SIGHUP after changing a non-allow-list key (e.g., `cluster.cluster_name`) | `reload_applied{skipped_keys=[...], skipped_reason=...}`; in-memory config unchanged for that key | `LCM-10` sighup-skipped-keys |
| `AC-COORD-05a` | Cluster-password rotation across all 3 peers via SIGHUP | `reload_applied{password_rotated=true, password_old_prefix, password_new_prefix}` on each peer; **quorum maintained** (`routes_meshed ≥ 1` on each peer throughout) | `SEC-03` password-rotation |
| `AC-COORD-05b` | `route_up.password_prefix` field on every event | Present, non-empty, 8 chars | `SEC-03`, `BOOT-09` |
| `AC-COORD-06a` | Embedded-NATS port already in use at startup | Exit code 78 (CONFIG); error names which port and (where determinable) competing process | `BOOT-10` port-collision |
| `AC-COORD-06b` | Multi-peer cluster declared but `cluster.peers` empty | Exit code 78 (CONFIG); FR-008 error | `BOOT-11` empty-peers-rejected |
| `AC-COORD-06c` | Non-loopback `routes_listen` without TLS and without `plaintext_explicit_ack` | Exit code 78 (CONFIG); FR-010b error | `BOOT-12` tls-missing-rejected |
| `AC-COORD-06d` | JS storage path unwritable at startup | Exit code 73 (CANTCREAT) | `BOOT-13` js-path-unwritable |
| `AC-COORD-07a` | Mismatched `cluster.name` or wrong password on inbound route | `cluster_route.auth_failed{kind="invalid_credential"\|"cluster_name_mismatch"}`; `pgman_proxy_embedded_nats_route_auth_failures_total{kind=...}` increments; route refused | `SEC-04` wrong-credential |

### REQ-AUDIT-*

| Criterion | Signal | Threshold | Scenario(s) |
|---|---|---|---|
| `AC-AUDIT-01a` | For every LCM request in a 100-request load: slog audit record + NATS subject `pgman_proxy.<cluster_id>.audit.lcm` message | 1:1 correspondence; required fields (ts, op, target, actor, source_addr, outcome, latency) present | `LCM-05` |
| `AC-AUDIT-02a` | Mutating LCM with audit pipeline unavailable | HTTP 503; engine-side action NOT taken (verified by `Status` unchanged) | `LCM-06` |
| `AC-AUDIT-03a` | `embedded_nats.route_up.peer_node_id` non-empty AND matches the sibling's `cluster.node_id`; `.password_prefix` non-empty 8-char | Field schema validator | every scenario |

### REQ-CON-* (constitutional gates)

| Criterion | Signal | Threshold | Scenario(s) |
|---|---|---|---|
| `AC-CON-01a` | CI grep gate for k8s.io / helm / controller-runtime / sigs.k8s.io / CRD / webhook | Zero matches | CI-only; not a runtime scenario |
| `AC-CON-02a` | CI grep gate for `pg_basebackup`, `pg_rewind`, `initdb`, `pg_upgrade`, `pg_ctl promote`, DDL strings, replication slot SQL inside this repo's source tree | Zero matches (LCM logic lives in pg-manager) | CI-only |
| `AC-CON-03a` | Wire-byte capture of pass-through traffic | Byte-accurate vs direct connection; SQLSTATE preserved | `SLO-04` wire-fidelity |
| `AC-CON-04a` | Grep for `ip addr add` / `keepalived` / `ARP` / VIP logic | Zero matches | CI-only |
| `AC-CON-05a` | Validation: control-plane non-loopback bind without TLS material and without `plaintext_explicit_ack` | Fail-closed at startup | `BOOT-14` control-tls-missing |
| `AC-CON-05b` | Validation: upstream PG `tls_mode != verify-full` without per-route opt-in | Fail-closed | `BOOT-15` weak-pg-tls |
| `AC-CON-06a` | Validation: `cluster.password` inline plaintext (not via SecretRef) | Exit 78; FR-010 error | `BOOT-16` plaintext-cluster-password |
| `AC-CON-06b` | Validation: resolved `cluster.password` shorter than 16 bytes | Exit 78 | `BOOT-17` weak-cluster-password |
| `AC-CON-07a` | `TriggerBackup` without `BackupExecutor` wired | HTTP 412 `backup_executor_missing` | `LCM-11` backup-missing-executor |

---

## §3 Coverage gap analysis

The current chaos-rig (process-compose driven, 6 manual scenarios) covers
a thin slice. Below, every REQ-id is mapped to its **current coverage**
in the existing rig and the **outstanding gap** for v1 ship.

| REQ-id | Currently covered by | Gap |
|---|---|---|
| `REQ-DL-01` (sync ANY 1 + sync_commit=on) | Implicit — chaos-workload counters never observed loss after CHANGELOG f1d67d7. No assertion on `synchronous_standby_names` value itself. | **GAP**: AC-DL-01d explicit probe missing. Add `DI-03` to read `pg_settings` from current primary and assert `'ANY 1 (...)'` + `synchronous_commit=on`. Also no test for "all standbys lost → writes block" path (`Policy.OnSyncStandbyLoss=BlockWritesOnSyncLoss`). |
| `REQ-DL-02` (no stale-leader writes) | Indirectly via chaos-workload's lack of `extra_rows` during failover. | **GAP**: no direct write-routing audit (`write_routed` events vs `current_leader_id` correlation). Add `DI-02b`. |
| `REQ-DL-03` (hard-close at transition) | Not directly tested — chaos-workload uses libpq multi-host with auto-retry; the hard-close is absorbed. | **MAJOR GAP**: no `TX-01` in-flight-tx-at-failover test that verifies a single connection holding an open BEGIN sees the connection RST'd, not silently routed. |
| `REQ-DL-04` (disk-full → storage_degraded + self-fence) | Not tested. | **MAJOR GAP**: no `RES-02` disk-full / quota-exceeded scenario. The `embedded_nats.storage_degraded` event has never fired in the rig. |
| `REQ-DL-05` (lease-loss self-fence on action) | Indirectly — kill scenarios trigger lease loss as a side effect. | **GAP**: no isolated test for "lease lost but process alive and PG still healthy" (`NET-04` — block NATS port without killing peer). |
| `REQ-AVAIL-01` (5 s p99 failover) | Chaos-workload measures `writes_failed` gap during kill; no p99 distribution. | **GAP**: ≥ 50 trial RTO histogram missing. Add `SLO-01`. The current rig measures presence/absence, not latency distribution. |
| `REQ-AVAIL-02` (< 10 s MTTR proxy crash) | `process-compose` `restart: always` exercises it; no automated assertion. | **GAP**: instrumented timing for `KILL-02` not collected. |
| `REQ-AVAIL-03` (single-peer < 5 s ready) | Not measured in HA chaos rig. | **GAP**: `BOOT-01` single-peer scenario needed; rig is 3-peer only. |
| `REQ-AVAIL-04` (rolling restart ≤ 60 s) | Not exercised. | **MAJOR GAP**: `SLO-02` rolling-restart-with-binary-swap. The rig has no rolling-restart automation. |
| `REQ-AVAIL-05` (Switchover < 15 s p99) | Not exercised. | **MAJOR GAP**: `LCM-01` planned-switchover not in the rig. |
| `REQ-AVAIL-06` (< 1 ms p99 proxy overhead) | Out of scope for chaos rig (perf benchmark). | **GAP**: `SLO-03` is a separate perf bench, not chaos. |
| `REQ-HEAL-01` (AutoDemote on divergence) | Six-scenario rig DID exercise this (CHANGELOG ref 9f5d37d, d239a6f, f1d67d7) — `docker restart primary` + cascading-kill. Status: covered post-CHANGELOG. | **GAP**: explicit `AutoDemoteRefusedEvent` reason-code coverage (cooldown, stability-window, probe-failure-threshold) not asserted. Add `REPL-02`. |
| `REQ-HEAL-02` (AutoRebootstrap on stale WAL) | NOT exercised — the rig's `wal_keep_size = 128MB` is large enough to prevent stale-WAL conditions. | **MAJOR GAP**: `REPL-03` requires inducing stale-WAL (long-isolated standby + bursty primary writes). Currently zero coverage. |
| `REQ-HEAL-03` (partition → self-fence) | The current rig uses **only `process-compose stop` and `docker kill`** — process death, not network partition. | **MAJOR GAP**: `NET-01` (partition one peer from the other two with `iptables`/`tc`/`docker network disconnect`) is entirely absent. Zero network-partition tests in the current rig. |
| `REQ-HEAL-04` (peer rejoin after kill) | Covered by `restart: always` in the rig. | OK. |
| `REQ-HEAL-05` (data_loss_total settles to baseline) | Implicitly covered by the new `data_loss_total` distinct-unresolved semantics (CHANGELOG d239a6f). | **GAP**: verdict logic must be encoded explicitly in the test plan, not just observed by humans tailing logs. Add `DI-02` automation. |
| `REQ-HEAL-06` (DivergenceParkedEvent) | Not asserted. | **GAP**: `REPL-04` with AutoDemote disabled; verify `DivergenceParkedEvent` emitted. |
| `REQ-HEAL-07` (election convergence) | Covered via kill scenarios. | OK (informally). |
| `REQ-LIVE-01` (`/healthz`) | Used by process-compose readiness_probe. | OK. |
| `REQ-LIVE-02` (`/readyz`) | Used by process-compose readiness_probe. | OK. |
| `REQ-LIVE-03` (drain on SIGTERM) | `process-compose stop` exercises it informally. | **GAP**: `KILL-04` assertion on (a) drain time ≤ budget, (b) clean exit code 0, (c) no orphan FS artifact. Currently not asserted. |
| `REQ-CP-01` (leader routing) | Not in chaos rig. | **MAJOR GAP**: `LCM-02` / `LCM-03` forward/redirect tests entirely absent. |
| `REQ-CP-02` (leader_route_timeout) | Not tested. | **GAP**: `LCM-04` needs leader-black-hole scenario. |
| `REQ-CP-03` (audit fail-closed) | Not in chaos rig. | **GAP**: `LCM-06` audit-fail-closed not tested. |
| `REQ-CP-04` (token rotation) | Integration test (covered in 001 integration). | OK at integration; **GAP** at chaos level (rotate-under-load not exercised). |
| `REQ-CP-05` (mutating during transient state) | Not tested. | **GAP**: `LCM-07`/`LCM-08`. |
| `REQ-COORD-01` (no external NATS, legacy URL rejected) | CI smoke test (SC-007 grep gate) covers absence; `BOOT-06` validation must be asserted. | OK on absence; assertion test for legacy URL needs to exist. |
| `REQ-COORD-02` (replication factor derivation) | NOT exercised. | **GAP**: `BOOT-07` / `BOOT-08`. The chaos rig hardcodes declared_size=3; no parametric test. |
| `REQ-COORD-03` (3-peer mesh + leader) | Implicitly exercised every rig start. | OK. |
| `REQ-COORD-04` (SIGHUP allow-list) | NOT exercised. | **MAJOR GAP**: `LCM-09`/`LCM-10` SIGHUP semantics zero coverage. |
| `REQ-COORD-05` (cluster-password rotation) | NOT exercised. | **MAJOR GAP**: `SEC-03` rotation-under-load entirely absent. |
| `REQ-COORD-06` (startup fail-closed) | Manual sanity only. | **GAP**: `BOOT-10`/11/12/13 all need automated coverage. |
| `REQ-COORD-07` (cluster-name / credential mismatch) | NOT exercised. | **GAP**: `SEC-04` wrong-credential / cluster-name-mismatch zero coverage. |
| `REQ-AUDIT-01` (LCM audit completeness) | Integration test in 001. | OK at integration; **GAP** under load + chaos overlap. |
| `REQ-AUDIT-02` (audit fail-closed) | Not in chaos rig. | **GAP**: see `AC-CP-03b`. |
| `REQ-AUDIT-03` (route_up has peer_node_id + password_prefix) | Implicitly verifiable in slog. | **GAP**: schema validator on slog needed. |
| `REQ-CON-01` ... `REQ-CON-07` | CI grep gates + 001 integration. | OK (CI-time, not chaos). |

**Cross-cutting honesty check.** The current 6-scenario chaos loop is
light on:

1. **In-flight transaction behaviour at failover** (REQ-DL-03) — zero coverage.
2. **Network partitions** (REQ-HEAL-03) — zero coverage; everything is process-kill.
3. **Replication slot / stale-WAL** (REQ-HEAL-02, REPL-*) — zero coverage.
4. **Explicit divergent_ex_primary detection** (REQ-HEAL-06) — implicitly covered for the "AutoDemote enabled" arm; the "disabled / refused" arm is not asserted.
5. **SIGHUP hot-reload semantics** (REQ-COORD-04, REQ-COORD-05) — zero coverage.
6. **Disk-full / storage-degraded on JetStream** (REQ-DL-04) — zero coverage; the `storage_degraded` event has never fired.
7. **Control-plane LCM under chaos** (REQ-CP-*) — zero coverage; LCM is integration-tested, not chaos-tested.
8. **Rolling binary upgrade** (REQ-AVAIL-04) — zero coverage.
9. **Replication factor derivation as a parametric test** (REQ-COORD-02) — zero coverage; rig hardcodes size=3.
10. **Cluster-name / credential mismatch route refusal** (REQ-COORD-07) — zero coverage.

---

## §4 GO/NO-GO bar

The bar is **what an SRE would accept as production posture for v1 of
this milestone**. Three tiers: MUST-PASS (ship blocker), SHOULD-PASS
(reviewable concession), NICE-TO-HAVE (defer-able).

### MUST-PASS — failure of any of these blocks ship

| REQ-id | Why ship-blocker | Failure mode that blocks ship |
|---|---|---|
| `REQ-DL-01` | Zero-data-loss is the entire product promise. A single acknowledged-but-lost commit makes pgman-proxy unfit to front production PG. | Any `DATA LOSS` line in `AC-DL-01c` that does not resolve within REQ-AVAIL-01, OR `synchronous_standby_names != 'ANY 1 (...)'` in `AC-DL-01d`. |
| `REQ-DL-02` | Routing a write to a stale leader is the worst-case active/active failure (Constitution III). | A `write_routed` event whose destination node_id is NOT the current `leadership.current_leader_id` at that timestamp. |
| `REQ-DL-03` | Silent in-flight write re-routing produces data loss the chaos counter cannot detect (the write completes against a now-non-leader). | `TX-01` shows ANY in-flight INSERT silently completing through the proxy after `leadership.changed` instead of receiving a connection RST. |
| `REQ-DL-04` | A peer whose coordination plane cannot persist MUST self-fence — otherwise it cannot honour the lease contract. | `RES-02` disk-full / quota-exceeded shows the peer continuing to advertise as leader-eligible AND serve writes. |
| `REQ-DL-05` | Lease-loss must be a write-blocker at action time, not just at next election cycle. | `NET-04` shows local upstream PG being written to while `lease_renewal_failures_total > 0` on this peer. |
| `REQ-AVAIL-01` | The 5 s p99 RTO is the published failover SLO. Regression below it is a published-spec violation. | `SLO-01` p99 RTO > 5 s across ≥ 50 trials. |
| `REQ-AVAIL-02` | A proxy that takes > 10 s to come back doubles the failover budget when it's the failing peer. | `KILL-02` p99 MTTR > 10 s. |
| `REQ-HEAL-01` | Without AutoDemote the cluster can't recover from B-003 divergence without operator action — operationally unshippable. | `REPL-01` does NOT produce an `AutoDemoteAcceptedEvent` within the documented budget after stable leadership + probe-confirmed primary on another peer. |
| `REQ-HEAL-03` | Self-fencing on partition is Constitution III, the non-negotiable. | `NET-01` shows the partitioned peer continuing to serve writes (i.e., NOT self-fencing). |
| `REQ-COORD-01` | The point of feature 002 is "no external NATS". A `nats-server` process observable on any host = feature not delivered. | `ps`-based assertion in `BOOT-05` finds an `nats-server` process. |
| `REQ-COORD-02` | FR-011a derivation is the partition-resilience guarantee. R=1 on a 3-peer cluster is a silent regression. | `pgman_proxy_embedded_nats_replicas_factor != 3` on any peer in a `declared_size=3` cluster (without `replication_factor_override`). |
| `REQ-COORD-03` | Without 3-peer mesh + leader election the cluster doesn't exist. | `BOOT-09` does not converge within the configured startup budget. |
| `REQ-COORD-06` | Fail-closed at startup IS Principle II. | Any of `BOOT-10`/11/12/13 does not exit with the documented exit code. |
| `REQ-CP-03` | Audit fail-closed (FR-028, SC-010) is mandatory; mutating LCM without audit is an SRE compliance gap. | `LCM-06` mutating LCM completes engine-side action when audit sink is down. |
| `REQ-AUDIT-01` | 100 % LCM audit completeness (SC-010) is a published guarantee. | `LCM-05` shows any LCM request without 1:1 slog + NATS subject correspondence. |
| `REQ-CON-01` | Constitution VII out-of-scope check is a hard CI gate. | Any k8s.io / helm / CRD / webhook code lands in the repo. |
| `REQ-CON-02` | Constitution IV thin-scaffold (SC-013) is a hard CI gate. | LCM SQL/binary invocation lands in this repo's source tree. |
| `REQ-CON-05` | TLS/auth defaults on non-loopback binds is the project's trust posture. | `BOOT-14` / `BOOT-12` permits a non-loopback plaintext bind without the named ack. |
| `REQ-CON-06` | Plaintext secrets in config is a security-incident-class regression. | `BOOT-16` accepts an inline plaintext `cluster.password`. |

### SHOULD-PASS — reviewable concession (file an issue + ship if accepted by the on-call SRE)

| REQ-id | Reason |
|---|---|
| `REQ-AVAIL-04` (rolling restart ≤ 60 s) | Important for ops but a regression here is a TOIL, not a correctness, issue. |
| `REQ-AVAIL-05` (Switchover < 15 s p99) | Planned switchover is rarely-used relative to involuntary failover. |
| `REQ-AVAIL-06` (< 1 ms p99 overhead) | Performance regression — needs a written justification but not a hard block. |
| `REQ-HEAL-02` (AutoRebootstrap stale-WAL) | Production deployments size `wal_keep_size` to make this rare; AutoRebootstrap is the safety net, not the steady-state. |
| `REQ-HEAL-05` (data_loss_total post-settle == baseline) | Practically dominated by REQ-DL-01 + REQ-AVAIL-01; a separate failure here is unusual. |
| `REQ-HEAL-06` (DivergenceParkedEvent emitted when AutoDemote disabled) | Important for the disable-AutoDemote operator path, but the chaos rig defaults to enabled. |
| `REQ-COORD-04` (SIGHUP allow-list semantics) | Hot-reload is a v1 feature but day-2 — peer-routes adds via restart are tolerable as a fallback. |
| `REQ-COORD-05` (password rotation under load maintains quorum) | Rotation is a planned maintenance op; if it requires a quiet window, that's documentable. |
| `REQ-COORD-07` (auth_failed on wrong credential) | Wrong credential is an operator-config bug, not an attack surface in v1's threat model. |
| `REQ-CP-01` (forward/redirect leader routing under chaos) | Integration-tested in 001; chaos coverage is "should". |
| `REQ-CP-02` (leader_route_timeout) | Important but bounded by REQ-CP-01. |
| `REQ-CP-04` (token rotation under load) | Hot rotation is integration-tested in 001; chaos overlap is bonus. |
| `REQ-CP-05` (mutating LCM during transient state) | Operator-facing nuisance, not data-loss. |

### NICE-TO-HAVE — defer-able beyond v1 ship

| REQ-id | Reason |
|---|---|
| `REQ-LIVE-01`, `REQ-LIVE-02`, `REQ-LIVE-03` | Already exercised informally by `process-compose` readiness probes and `process-compose stop`; explicit-assertion versions are polish. |
| `REQ-AUDIT-03` (`route_up` field schema validator) | Schema is stable in observability.md; manual log inspection is enough at v1. |
| `REQ-CON-03` (wire-byte fidelity) | Covered by 001 integration tests; not a chaos surface. |
| `REQ-CON-04` (no VIP code) | CI grep gate is enough. |
| `REQ-CON-07` (backup_executor_missing 412) | LCM contract test in 001; not a chaos scenario. |

---

## §5 Open questions / unknowns

These items in the spec are under-defined and SHOULD be resolved before
test execution to avoid post-hoc threshold-shopping.

### Q-1 — Failover SLO measurement split

`REQ-AVAIL-01` ("5 s p99") is stated against the **leader-change observed
as a structured event** (001 SC-002 wording). But operators care about
**time to first successful client write on the new leader**. These can
diverge significantly (event emission may happen before the new leader's
upstream PG is accepting writes). Decision needed: is the 5 s budget
measured to `leadership.changed` event, OR to first ACK from new leader?

**Proposed default**: measure both, hold the 5 s budget against the
**first successful write** (the operator-visible signal). The
event-emission timestamp is a debug breadcrumb, not the SLO.

### Q-2 — ANY-1 quorum under fully-degraded standby pool

`Policy.OnSyncStandbyLoss = BlockWritesOnSyncLoss` (pg-manager default,
process-compose inherits). What is the expected behaviour when **all**
standbys are unreachable simultaneously? The 002 spec is silent.
Behaviour candidates:

- Writes block (default `BlockWritesOnSyncLoss`) → `writes_failed`
  grows; chaos verdict?
- Eventually degrade to async (`DegradeToAsyncOnSyncLoss`) → not the
  configured default, but is this a tested fall-back?

**Decision needed**: which `OnSyncStandbyLoss` value is the v1
production default, and is "writes block during full standby outage"
considered acceptance criterion satisfied (REQ-DL-01 says yes; needs
explicit codification).

### Q-3 — RTO ceiling under partition (vs kill)

The 5 s p99 budget is empirically tuned to process-kill scenarios.
Partition-detection latency is governed by NATS route disconnect
timers, which are NOT necessarily the same as
`Policy.LivenessInterval`. Open: is the 5 s p99 expected to hold under
`NET-01` (partition) as well as `KILL-01` (process death)? If not, the
spec needs a partition-specific budget.

**Proposed default**: assert REQ-AVAIL-01 against `KILL-*` only; create
a separate `REQ-AVAIL-01b` for `NET-*` with a documented higher
threshold (suggestion: 10 s p99) once measured. Mark `REQ-AVAIL-01b` as
SHOULD-PASS for v1.

### Q-4 — `data_loss_total` post-settle equality vs tolerance

REQ-HEAL-05 currently demands `dl(t_end) == dl(t_start)`. Is **strict
equality** correct, or do we allow `dl(t_end) ≤ dl(t_start) + ε` for
some bounded ε (e.g., a single in-flight commit at the moment of
SIGKILL whose ACK was lost in the network)?

**Proposed default**: strict equality. If the spec intends a tolerance,
it MUST appear in REQ-DL-01 and be measurable.

### Q-5 — Chaos rig replacement for `wal_keep_size = 128MB`

The current `process-compose.yaml` sets `wal_keep_size = 128MB`, which
suppresses AutoRebootstrap scenarios (REQ-HEAL-02). To exercise
`REPL-03`, the rig needs a variant with a tight `wal_keep_size` AND a
write-heavy phase that recycles WAL before the isolated standby
returns. Decision: add a second compose profile, or make wal_keep_size
parametric per scenario?

### Q-6 — SIGHUP target environment

`REQ-COORD-04` requires SIGHUP to hot-reload peer-routes + password.
The current rig runs proxy peers **inside docker containers**; SIGHUP
to a docker process requires `docker kill -s HUP`. Confirm this is the
test mechanism (vs `process-compose` signal forwarding, vs systemd
`ExecReload`).

### Q-7 — Per-peer-loss tolerance below R for the JetStream KV

`REQ-COORD-02` derives R=3 for declared_size ≥ 3. But the actual KV
bucket may report `Replicas=3` while only 2 peers are online (during a
partition). Question: at what point does the KV become read-only vs
write-only vs unavailable? Operator runbook needs this codified;
otherwise `REQ-DL-04` storage-degraded semantics may overlap
ambiguously with "KV temporarily under-replicated".

### Q-8 — Acceptable steady-state `writes_failed` rate

The chaos-workload accumulates `writes_failed` during transitions. The
test plan needs a documented "steady-state" rate (ideally 0 outside
transition windows) and a "transition-window" budget (e.g., ≤ N
failures per transition). Currently informal.

---

## Document control

- All REQ-ids are stable identifiers. Renames require a follow-up entry
  in this section listing the old → new mapping.
- All scenario-id prefixes (`DI-`, `NET-`, `KILL-`, `RES-`, `LCM-`,
  `BOOT-`, `SLO-`, `OBS-`, `SEC-`, `REPL-`, `TX-`) are reserved. The
  DBA-agent test plan and the SW-engineer harness consume them
  verbatim.
- Source-of-truth for thresholds: this document. Any threshold drift
  between this and `spec.md` / `plan.md` is a defect in this document
  and MUST be reconciled here against the spec.
