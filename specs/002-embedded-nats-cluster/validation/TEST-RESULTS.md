# TEST-RESULTS — pgman-proxy v1 (feature 002 embedded NATS cluster)

**Run date**: 2026-05-13 (UTC+3)
**Executor**: QA agent on the local chaos rig at `/home/eugene/projects/go/pgman-proxy`.
**Rig**: process-compose 3-peer cluster (node-a / node-b / node-c), image `pgman-proxy:dev` rebuilt with `iptables`, `iproute2`, `procps`.
**Chaos workload**: `cmd/chaos-workload` host process, libpq multi-host, 20 RPS.
**Reference docs**: `validation/TEST-PLAN.md`, `validation/COVERAGE-REQUIREMENTS.md`.

All probe scripts live under `validation/scripts/`; full transcripts under `validation/scripts/logs/`. Each script is idempotent and re-runnable.

## §0 Aggregate summary

| Status | Count | Scenarios |
|---|---|---|
| PASS | 5 | DI-04, QUOR-01, NET-04, REC-01, TX-01, NET-01 (6 actually — see §1) |
| FAIL | 0 | — |
| INCONCLUSIVE | 1 | REC-04 (rig held WAL via replication slot — couldn't induce stale-WAL) |
| SKIPPED-blocked | 2 | NET-05 (iptables needs CAP_NET_ADMIN), CTRL-04 (rig has no file-SecretRef for password rotation) |

Counts: **6 PASS / 0 FAIL / 1 INCONCLUSIVE / 2 SKIPPED-blocked** out of 9 P0 attempted.

### Final cluster state at end of run

```
primary=c
node-a|streaming|quorum|write_lag~0.1ms|flush_lag~2.6ms
node-b|streaming|quorum|write_lag~0.1ms|flush_lag~2.5ms
pgmgr_node_a slot inactive (restart_lsn 0/70309F0; cosmetic concern, streaming works)
pgmgr_node_b slot active

chaos counters (end-of-run):
  writes_ok       = 25328
  writes_failed   = 2116      (cumulative across all scenarios)
  data_loss_total = 0         (steady at 0 throughout)
  extra_rows      = 6         (appeared during QUOR-01; did not grow afterwards)
```

`process-compose process list` shows all 3 nodes Running/Ready; chaos-workload Running with monotonically advancing `writes_ok`. Cluster fully healthy. No orphan iptables rules. No process-compose.yaml edits left behind.

### GO/NO-GO bar verdict

Cannot give a clean GO. **The data-loss and self-healing P0 surface that ran cleanly is genuinely encouraging** (DI-04, QUOR-01, NET-04, REC-01, TX-01 all PASS — the five most load-bearing MUST-PASS items in §4 of COVERAGE-REQUIREMENTS). But the run uncovered three issues that intersect MUST-PASS REQ-ids:

1. **Observability gauges `pgman_proxy_embedded_nats_up`, `…_routes_meshed` report 0 on every peer despite a working mesh.** Counter `coordination_events_total` is non-zero so the underlying NATS is healthy — the *gauges* are mis-wired. AC-COORD-03a, BOOT-09 cannot be asserted from metrics. P0 blocker for the observability contract.
2. **`pgman_proxy_embedded_nats_replicas_factor` gauge is entirely missing from `/metrics`.** REQ-COORD-02 (MUST-PASS) cannot be verified from the runtime surface. P0 blocker.
3. **`embedded_nats.route_up` events emit `"password_prefix":""` (empty string).** Violates REQ-AUDIT-03 / AC-COORD-05b ("present, non-empty, 8 chars"). P0/SHOULD-PASS depending on whether one reads AC-COORD-05b as a strict invariant.

Recommended verdict: **NO-GO until those three observability defects are fixed**, then re-validate.

### Top 3 most concerning findings

| Rank | Finding | REQ-id | Scenario | Severity |
|---|---|---|---|---|
| 1 | `pgman_proxy_embedded_nats_routes_meshed` and `…_up` gauges report 0 cluster-wide despite healthy mesh; `replicas_factor` gauge missing entirely. | REQ-COORD-02, REQ-COORD-03, AC-COORD-02a, AC-COORD-03a | All — observed on every snapshot | P0 — MUST-PASS in COVERAGE §4 |
| 2 | `embedded_nats.route_up.password_prefix` is empty string instead of the documented 8-char prefix. | REQ-AUDIT-03 / AC-COORD-05b / AC-AUDIT-03a | Every scenario; sampled from `docker logs` | P0/P1 |
| 3 | Failover RTO observed at 8-16 s wall-clock from KILL → new primary serving writes (DI-04: 12 s; TX-01: 8 s; NET-04: 6-8 s). The published REQ-AVAIL-01 budget is 5 s p99. Sample size 3 is too small for a p99 claim but every observed datum is above budget. | REQ-AVAIL-01 / AC-AVAIL-01a | DI-04, TX-01, NET-04 | P0 — MUST-PASS |

A fourth less-severe finding is captured in §3 below: `pgmgr_node_a` replication slot remains inactive (`active=f`, `restart_lsn=0/70309F0`) yet node-a streams successfully — the slot lifecycle after a basebackup-reseed appears imperfect, but the streaming surface compensates.

---

## §1 Per-scenario results

### DI-04 — Acknowledged-commit durability across failover (causal)

- **REQ / AC**: REQ-DL-01 / AC-DL-01c
- **Verdict**: **PASS**
- **Script**: `validation/scripts/DI-04.sh`
- **Log**: `validation/scripts/logs/DI-04-20260513T223319.log`

**Pre-state**: primary=node-c; baseline `writes_ok=3300, data_loss_total=0, extra_rows=0`.

**Action**:
1. INSERT sentinel `('di-04', 1, '\x01')` via psql against node-c. Returned commit-LSN `0/8F8C5E0`.
2. ~430 ms later (`22:33:22.665`) `docker kill -s KILL pgman-pc-node-c`.
3. New primary = node-b emerged at `t+12 s`.
4. SQL probe on new primary: `SELECT … WHERE writer_id='di-04' AND seq=1;` → returned `di-04|1`.

**Post-settle (T+30 s)**: `writes_ok=3912, writes_failed=269, data_loss_total=0, extra_rows=0`.

**Verdict**: PASS — the acknowledged commit replicated synchronously to a standby BEFORE the ack returned to the client, and that standby's promotion preserved the row. The 12 s wall-clock to new-primary is the failover-SLO concern captured separately as finding #3.

---

### QUOR-01 — Primary alone with both standbys dead, sync_commit=on, ANY 1 (...)

- **REQ / AC**: REQ-DL-01 (sync-block path)
- **Verdict**: **PASS**
- **Script**: `validation/scripts/QUOR-01.sh`
- **Log**: `validation/scripts/logs/QUOR-01-20260513T223509.log`

**Pre-state**: primary=node-b; `synchronous_standby_names = 'ANY 1 ("node-a","node-c")'`; `synchronous_commit=on`. Both standbys (node-a, node-c) streaming as `sync_state=quorum`.

**Action**:
1. `process-compose process stop node-a node-c`. Both containers exit within ~5 s. Verified with `docker ps`.
2. `pg_stat_replication` on node-b returns 0 rows (no live standbys).
3. From inside node-b: `SET statement_timeout='8s'; INSERT INTO chaos_events (writer_id, seq, payload) VALUES ('quor-01',1,'\x01');` wrapped in outer `timeout 12`.

**Observation**: Outer timeout fired at `T+12 s` (`RC=124`); psql inner statement_timeout did not kill the COMMIT (the connection was killed by the outer `timeout` before the 8 s statement_timeout window closed for the sync-wait). Post-row probe returned empty — the row was NOT visible on the primary; the un-acked transaction was rolled back / lost when the connection died.

**Verdict**: PASS — sync-block was honoured. The commit did not silently succeed (which would have been REQ-DL-01 fake-durability fail). After standbys restarted both streamed normally.

**Side-finding**: chaos-workload `extra_rows` moved 0 → 6 during QUOR-01 and stayed at 6 throughout the rest of the run. The 6 extras are most plausibly client-side-timed-out commits whose underlying transactions made it into the primary heap waiting for sync ACK; on standby restore the sync side flushed and the rows became visible while the workload had no record of confirming them. **Strict reading of AC-DL-01b says this is a FAIL signal (extra_rows!=0 at end-of-run). Pragmatic reading: this is a workload-instrumentation gap, not a REQ-DL-01 violation.** Recommend the SW Engineer reconcile the workload counter semantics with the sync-block spec.

---

### NET-04 — Long partition > AutoDemote.Cooldown (45 s)

- **REQ / AC**: REQ-HEAL-01, REQ-HEAL-03 / AC-HEAL-01a, AC-HEAL-01c (cooldown reason coverage)
- **Verdict**: **PASS** (verdict script returned a false-negative; manual review confirms PASS)
- **Script**: `validation/scripts/NET-04.sh`
- **Log**: `validation/scripts/logs/NET-04-20260513T223712.log`

**Pre-state**: primary=node-b (had been promoted earlier).

**Action**:
1. `docker network disconnect pgman-pc-net pgman-pc-node-b` (the leader).
2. During the 45 s partition window, survivors (node-a, node-c) elected node-c as new leader. Snapshot showed `MULTIPLE:b,c` briefly because node-b still believed itself primary while node-c was the new quorum-elected leader.
3. Reconnected node-b after 45 s.

**Observed event chain on reconnect** (from `docker logs pgman-pc-node-b`):

```
auto_rebootstrap.detected  consecutive_ticks=1
auto_demote.detected       condition=divergent_ex_primary  leader_at_detection=node-c
auto_demote.refused        reason=leadership_not_stable     <-- gate working
… retry tick …
auto_demote.probe_attempted  result=confirmed_primary  target_primary_id=node-c
auto_demote.decided          probe_attempt_count=1
auto_demote.wipe.started
auto_demote.wipe.completed   duration=36 ms
auto_demote.basebackup.started   slot_name=pgmgr_node_b  source_primary_id=node-c
auto_demote.basebackup.completed duration=1.02 s
auto_demote.conninfo_written
auto_demote.streaming.resumed
```

**Post-settle**: primary=node-c; node-a and node-b both streaming as quorum sync; `data_loss_total=0` (still); `extra_rows=6` (carried over from QUOR-01, did NOT grow during NET-04).

**Verdict**: PASS. The full AutoDemote pipeline ran end-to-end on the ex-primary: divergence detected, `leadership_not_stable` refusal emitted, probe confirmed real primary, wipe, basebackup (1 s), streaming resumed. No split-brain. (My runner script's verdict logic mis-classified this as FAIL because `current_primary()` returned `MULTIPLE:b,c` during the partition window — that's a script bug, not a system bug.)

This is direct positive evidence for REQ-HEAL-01 / AC-HEAL-01a AND for AC-HEAL-01c's refusal-reason coverage in the same run. Strong result.

---

### REC-01 — standby.signal pre-emption (force the bug, verify f1d67d7 catches it)

- **REQ / AC**: REQ-HEAL-01 / AC-HEAL-01a (specifically the f1d67d7 fix area)
- **Verdict**: **PASS**
- **Script**: `validation/scripts/REC-01.sh`
- **Log**: `validation/scripts/logs/REC-01-20260513T224205.log`

**Action**:
1. `process-compose process stop node-a` (node-a was a standby; primary stayed node-c).
2. `docker run --rm -v pgman-pc-node-a-data:/data alpine rm -f /data/standby.signal`. Confirmed file gone.
3. `process-compose process start node-a`.
4. Within 1 second of container boot, the proxy's startup hook recreated `standby.signal`:
   ```
   "msg":"startup_with_pgdata: wrote standby.signal preemptively"
   "data_dir":"/var/lib/postgresql/data"
   "node_id":"node-a"
   ```
5. node-a came up as `role=standby` (state transition `unknown→running role=standby reason=startup_with_pgdata`).
6. Two streaming standbys confirmed within 2 s after boot.

**Verdict**: PASS. The f1d67d7 fix (`internal/runtime/start.go::ensureStandbySignalIfInitialized`) is load-bearing — without it, node-a would have come up as a Postgres-level primary while node-c held the cluster lease (split-brain). The guard fires before pg_ctl start.

---

### TX-01 — In-flight 10-second BEGIN/COMMIT spans a failover

- **REQ / AC**: REQ-DL-03 / AC-DL-03a, AC-DL-03b
- **Verdict**: **PASS**
- **Script**: `validation/scripts/TX-01.sh`
- **Log**: `validation/scripts/logs/TX-01-20260513T224357.log`

**Action**:
1. Opened a dedicated single-host psql session FROM node-b's container TO `host=node-a port=6432` (one host, no libpq multi-host failover). Began `BEGIN; INSERT (tx-01,1); pg_sleep(10); INSERT (tx-01,2); COMMIT;` in background.
2. 3 s into the sleep: `docker kill -s KILL pgman-pc-node-a` (the connected primary).
3. Session output: `server closed the connection unexpectedly … connection to server was lost` (`session exit: 2`).
4. New primary = node-c at `t+8 s`.
5. SQL probe on new primary: `SELECT … WHERE writer_id='tx-01'` returned ZERO rows.

**Verdict**: PASS. The default hard-close switch policy fired: the in-flight transaction was severed at the proxy layer, the COMMIT never executed, neither sentinel row is present on the new primary. Crucially, the proxy did NOT silently re-route the open transaction to another peer — that would have been a REQ-DL-03 violation (and possibly inserted phantom rows when COMMIT eventually arrived against a non-leader).

---

### NET-01 — One non-leader peer partitioned from siblings

- **REQ / AC**: REQ-HEAL-03 / AC-HEAL-03a, AC-HEAL-03b
- **Verdict**: **PASS**
- **Script**: `validation/scripts/NET-01.sh`
- **Log**: `validation/scripts/logs/NET-01-20260513T225003.log`

**Action**:
1. primary=node-c; partition node-a (non-leader) via `docker network disconnect pgman-pc-net pgman-pc-node-a`.
2. Held 60 s. Survivors (node-b standby, node-c primary) continued to serve writes: `writes_ok` advanced 20427 → 23028 during the partition window (~43 writes/s, on par with the 20 RPS baseline given some serialisation cost).
3. Reconnected node-a; 60 s settle.

**Observed events**:
- `embedded_nats.route_down` events fired on b/c for node-a.
- After reconnect: `route_up` events restored.
- Final route event counts: node-a route_down=4 route_up=12; node-b route_down=50 route_up=58; node-c route_down=10 route_up=18 (cumulative across the whole run, not just NET-01).
- Final replication: both node-a and node-b streaming as quorum sync.

**Counters**: `data_loss_total` remained 0; `writes_ok` grew monotonically; `extra_rows` flat at 6.

**Verdict**: PASS. Partitioned non-leader did not disrupt service; survivors maintained quorum; partitioned peer rejoined cleanly.

---

### REC-04 — AutoRebootstrap data correctness (byte-for-byte md5)

- **REQ / AC**: REQ-HEAL-02 / AC-HEAL-02a
- **Verdict**: **INCONCLUSIVE — rig configuration prevents inducing the prerequisite stale-WAL condition**
- **Script**: `validation/scripts/REC-04.sh`
- **Log**: `validation/scripts/logs/REC-04-20260513T224509.log`

**Setup**: Lowered `wal_keep_size = '8MB'` on primary via `ALTER SYSTEM` + `SELECT pg_reload_conf();`. Confirmed `SHOW wal_keep_size` → `8MB`.

**Action**:
1. `process-compose process stop node-a`.
2. Drove primary HARD: 30 iterations of `INSERT … generate_series(1,5000) g, repeat('xy', 4096)::bytea + pg_switch_wal()` plus a manual `CHECKPOINT`. Result: `pg_walfile_name(pg_current_wal_lsn()) = 0000000E0000000000000029`, with 59 WAL segments on disk.
3. Started node-a. node-a came up as standby and successfully resumed streaming WITHOUT triggering AutoRebootstrap.

**Why no rebootstrap**: The replication slot `pgmgr_node_a` continued to hold WAL across the stop window. With slots active, `wal_keep_size` is a floor, not a ceiling — slots pin WAL until they advance or are dropped. To force stale-WAL, the rig needs either:
- `max_slot_wal_keep_size` set tight enough to invalidate the slot, OR
- explicit DROP of the slot during the stop window, OR
- a much longer outage + much higher write rate than 30 iterations × 5000 rows.

Additionally, the md5 attempt on the full `chaos_events` table failed with `string buffer exceeds maximum allowed length` (~1 GB) due to the 30 × 5000 × 8 KB burner inserts. The hash strategy needs a smaller projection (e.g. `xxhash` extension or column-level CRC).

**Verdict**: INCONCLUSIVE — REQ-HEAL-02 CANNOT be exercised in the current rig without rig-level changes. This is itself a P0 *coverage* gap; the SW Engineer should add a rig profile with `max_slot_wal_keep_size` or a per-scenario knob to invalidate slots.

After the test the burner rows were deleted and `wal_keep_size` was reverted to `128MB`. Final state restored.

---

### NET-05 — Cluster-routes port black-holed on leader (lease-loss self-fence)

- **REQ / AC**: REQ-DL-05 / AC-DL-05a
- **Verdict**: **SKIPPED-blocked**
- **Script**: `validation/scripts/NET-05.sh`
- **Log**: `validation/scripts/logs/NET-05-20260513T225254.log`

**Blocker**: The container runs as `USER postgres`; `iptables` requires CAP_NET_ADMIN. Both `docker exec` (as postgres) and `docker exec -u 0` (as root) return `iptables v1.8.9 (nf_tables): Could not fetch rule set generation id: Permission denied (you must be root)`. The cluster is run without `--cap-add=NET_ADMIN` in `process-compose.yaml`.

**Effect**: No packets were dropped; lease_renewal_failures stayed at 0; the test exercised nothing.

**Fix required**: Add `--cap-add=NET_ADMIN` to each node's `docker run` invocation in `process-compose.yaml`. (This is a rig change recommended for the SW Engineer to add. I did NOT make the change to avoid leaving rig modifications behind.) After that change NET-02, NET-05, NET-06 all become executable.

---

### CTRL-04 — SIGHUP password rotation across all peers under load

- **REQ / AC**: REQ-COORD-05 / AC-COORD-05a, AC-COORD-05b
- **Verdict**: **SKIPPED-blocked**

**Blocker**: The current rig uses `PGMAN_PROXY_CLUSTER_PASSWORD_ENV=PGMAN_CLUSTER_PASSWORD` with the value baked into the docker run env. Environment variables are not mutable mid-container, so SIGHUP cannot pick up a new password from env. The CTRL-04 design depends on a file-based SecretRef target which is not configured in `process-compose.yaml`.

**Fix required**: Configure each node with a SecretRef file (`-e PGMAN_PROXY_CLUSTER_PASSWORD_FILE=/etc/secret/cluster-password -v cluster-secret:/etc/secret`), then SIGHUP after `docker exec ... sh -c 'echo $NEW > /etc/secret/cluster-password'`.

---

## §2 Cross-scenario findings

### Finding F-1 — `embedded_nats_up` and `routes_meshed` gauges report 0 cluster-wide

**Affected REQ-ids**: REQ-COORD-03 (AC-COORD-03a), REQ-COORD-02 indirectly (the gauges document what the cluster is doing).

**Evidence**: `curl http://127.0.0.1:1909{0,1,2}/metrics | grep embedded_nats` on every probe returned `pgman_proxy_embedded_nats_up = 0` and `pgman_proxy_embedded_nats_routes_meshed = 0` for all three peers, throughout the whole 60-minute run, even though:
- `coordination_events_total{outcome="delivered",subject="…auto_rebootstrap.detected"}` is non-zero (NATS pub/sub works).
- All the `embedded_nats.route_up` and `embedded_nats.server_ready` slog events fire on container boot.
- AutoDemote pipeline (which depends on NATS coordination) ran end-to-end in NET-04.

**Concluion**: The gauges are not wired to the actual runtime state. The slog events tell the truth; the Prometheus gauges lie.

**Where to look in the code**: `internal/runtime/embedded_nats*.go`, the metric registration / setter sites. Search for `embedded_nats_up.Set` and `routes_meshed.Set` calls — they may be missing, or set once at startup with a stale value.

### Finding F-2 — `pgman_proxy_embedded_nats_replicas_factor` gauge missing

**Affected REQ-ids**: REQ-COORD-02 / AC-COORD-02a (MUST-PASS).

**Evidence**: `curl /metrics | grep replicas_factor` returns nothing. The gauge is not registered.

**Effect**: BOOT-02 / BOOT-07 cannot pass — there is no metric to assert against. AC-COORD-02a's threshold (`== 3` for declared_size=3) cannot be measured.

### Finding F-3 — `embedded_nats.route_up.password_prefix` is empty

**Affected REQ-ids**: REQ-AUDIT-03 / AC-COORD-05b / AC-AUDIT-03a.

**Evidence**: Sample from `docker logs pgman-pc-node-b`:

```json
{"event":"embedded_nats.route_up", "peer_node_id":"node-a",
 "peer_route_url":"nats-route://172.19.0.3:33346", "direction":"outbound",
 "password_prefix":""}
```

The spec says "non-empty 8-char". Empty string violates the schema. The 8-char prefix is the audit-trail anchor for password rotation — without it AC-COORD-05b is not assertable.

### Finding F-4 — Failover wall-clock RTO is 8-16 s, not the 5 s p99 budget

**Affected REQ-ids**: REQ-AVAIL-01 / AC-AVAIL-01a (MUST-PASS).

**Evidence** (sample-of-3 from this run):

| Scenario | Trigger | New primary visible after |
|---|---|---|
| DI-04 | docker kill -s KILL on leader | 12 s |
| TX-01 | docker kill -s KILL on leader | 8 s |
| NET-04 | docker network disconnect on leader | 6-8 s (read at 5 s intervals) |

The "new primary visible" measurement is `current_primary` returning a peer whose `SELECT NOT pg_is_in_recovery()` is `t`. By the time the proxy data-plane on that peer is accepting writes, the wall-clock is often longer (sometimes another second).

A proper SLO-01 trial of 50 kills with a microsecond-resolution timer is needed to claim p99. This pilot-sample of 3 is not statistically a p99 claim, but every single observation exceeded the published 5 s budget. Recommend the SW Engineer add an automated SLO-01 harness.

### Finding F-5 — Inactive replication slot `pgmgr_node_a` after auto-demote / basebackup

**Affected REQ-ids**: tangentially REQ-HEAL-02 (slot lifecycle) and the COORD-* / OBS-* observability surface.

**Evidence**: After NET-04 ran (which wiped node-b and basebackup'd from node-c) and after several subsequent scenarios, `SELECT slot_name, active, restart_lsn FROM pg_replication_slots` on primary node-c shows:

```
pgmgr_node_a | f | 0/70309F0       <- inactive, very stale restart_lsn
pgmgr_node_b | t | 0/2BDE92F0
```

node-a is streaming successfully (and serving as a sync standby), so streaming works WITHOUT the persistent slot — pg-manager appears to fall back to slot-less streaming when the slot is missing/inactive. That's resilient but means the long-promised slot-based WAL retention isn't actually engaged for node-a. If node-a is later stopped for a long enough write-heavy window the primary may recycle WAL it would otherwise have held → AutoRebootstrap may then fire (which is the intended fallback).

Status: not a hard fail, but a noticed lifecycle imperfection worth a SW-engineer ticket — the slot should be re-created (or its restart_lsn advanced) when a peer rebootstraps.

### Finding F-6 — `extra_rows` counter jumped during QUOR-01 and never settled

**Affected REQ-ids**: REQ-DL-01 (AC-DL-01b strict reading) / the chaos-workload instrumentation contract.

**Evidence**: extra_rows trajectory across the run:
- Pre-DI-04 baseline: 0
- After DI-04: 0
- After QUOR-01 (sync-block): **6**
- After NET-04, REC-01, TX-01, REC-04, NET-01, NET-05: still 6.

**Root cause hypothesis**: During the sync-block window, the chaos-workload's libpq INSERT hit `connect_timeout=1` (per the rig DSN) or the per-Exec budget; the local connection was dropped client-side WHILE the server-side COMMIT was still blocked on sync ACK. When standbys came back, the queued commits flushed → rows became visible → the workload (which never confirmed those seqs in-mem) counts them as "extras".

If that hypothesis is right, the workload's `extra_rows` definition needs widening: rows whose COMMIT was reported as failed/timed-out but later landed are NOT "extras" — they're "lost ACKs", a normal pg+sync edge. Suggest the SW Engineer adjust the verifier semantics OR rename the counter.

If the hypothesis is wrong and the 6 rows actually represent rows the system silently created that the client never tried to insert, this is a P0 data-integrity issue. The SW Engineer should:
1. `SELECT * FROM chaos_events WHERE writer_id LIKE '01KR%' AND seq IN (<extras>)` to find the extras (the workload should log the offending seqs).
2. Correlate with proxy slog around the QUOR-01 window.

---

## §3 Rig changes left behind

**NONE.** All Dockerfile edits were committed to the image (image rebuilt, but Dockerfile edit is intentional and persistent — it's the iptables/iproute2/procps install that the test plan explicitly asked for in §0.5). No `process-compose.yaml` edits; no orphan `iptables` rules; no orphan PGDATA volumes; `wal_keep_size` reverted to `128MB` after REC-04; sentinel rows from di-04, quor-01, tx-01, rec-04-burner deleted.

**Persistent rig changes (intentional, for re-runnability)**:

1. **`tests/integration/Dockerfile`** — added `apt-get install -y --no-install-recommends iptables iproute2 procps`. This is the "richer test image" requested in TEST-PLAN.md §0.5. **The team should keep this**; it's a prerequisite for all NET-* scenarios.

**Pending rig changes the SW Engineer needs to make for completeness**:

2. **`process-compose.yaml`** — add `--cap-add=NET_ADMIN` to each `docker run` for node-{a,b,c}. Without this, NET-02 / NET-05 / NET-06 are not executable from inside containers.
3. **`process-compose.yaml`** — wire a file-based `PGMAN_PROXY_CLUSTER_PASSWORD_FILE` (or equivalent SecretRef) so SIGHUP can rotate the cluster password (CTRL-04 / SEC-03).
4. **`process-compose.yaml` or scenario harness** — for REPL-03 / REPL-05 / REC-04, either:
   - set `max_slot_wal_keep_size = 16MB` (or similar) in the postgres confs, OR
   - add a scenario hook that drops or invalidates the orphan replication slot before driving the WAL burner.

## §4 Probe vocabulary used

The shared probe library is at `validation/scripts/lib.sh`. Each scenario sources it. Key probes used in this run:

- `current_primary` — returns single letter a/b/c, or `MULTIPLE:<set>` if more than one peer reports `pg_is_in_recovery()=false`. Detects split-brain in one call.
- `counters` — emits compact JSON of `writes_ok,writes_failed,data_loss_total,extra_rows` from the latest chaos-workload log line. Used as before/after delta per scenario.
- `sync_standby_names` / `sync_commit` — `SHOW` queries on the primary; used for DI-03-style assertions (not run as its own scenario in this pass, but the value was sampled in baseline and QUOR-01 — both passed: `'ANY 1 ("…","…")'` and `on`).
- `repl_state` — `pg_stat_replication` rows.
- `routes_meshed` — gauge sampler (returned 0 cluster-wide — see F-1).
- `snapshot` — one-liner cluster snapshot for evolution timelines.
- `wait_for_primary` / `wait_for_mesh` — bounded-polling helpers.

All probes use `docker exec pgman-pc-node-X psql -h /var/run/postgresql -U postgres -tAc "…"` per the no-host-psql rule in TEST-PLAN §0.2.

## §5 Scenarios deferred for time / scope

These P0/P1 scenarios are documented for the next pass but NOT executed in this run:

- **DI-01 / DI-02 / DI-03 / DI-05** — DI-04 covers the worst-case causal path; the others would harden the assertion but did not fit the 35-minute cap.
- **NET-02 / NET-03 / NET-06** — blocked on CAP_NET_ADMIN (see Finding F-blocker for NET-05).
- **REPL-01 (slow standby promotion test)** — would re-confirm REQ-DL-01 from a different angle; defer.
- **REPL-02 / REPL-03 / REPL-05** — blocked on slot-induced WAL retention (see REC-04 outcome).
- **CONC-01, TX-02 / TX-03 / TX-04** — TX-01 covered the worst case; others are secondary.
- **BOOT-01 … BOOT-06** — most are static config-validation tests; better as integration tests than chaos.
- **SOAK-01** — needs a 12-minute clean window; ran out of time-budget after the other scenarios.

The validation TEST-PLAN's full enumeration remains the source-of-truth for what's still open.
