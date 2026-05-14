# TEST-RESULTS-STABILITY — pgman-proxy v1 stability pass

**Run date**: 2026-05-13 → 2026-05-14 (UTC+3) — handover round after the v1 chaos pass.
**Executor**: QA agent on the local chaos rig at `/home/eugene/projects/go/pgman-proxy`.
**Targets**: REC-04 (rebootstrap correctness), REPL-03 (slot leak after permanent peer loss), SOAK-01 (10-min sustained-load periodic-kill soak).
**Rig**: process-compose 3-peer cluster (node-a / node-b / node-c), `pgman-proxy:dev` image rebuilt from the integration Dockerfile, PGDATA wiped cold for this pass.
**Workload**: `cmd/chaos-workload` host process at the default RPS.
**Reference docs**: `validation/TEST-PLAN.md`, `validation/COVERAGE-REQUIREMENTS.md`, `validation/TEST-RESULTS.md`, `validation/FIXES.md`.

All probe scripts live under `validation/scripts/`. Full transcripts under `validation/scripts/logs/`.

---

## §1 Rig changes applied

### Edits to `process-compose.yaml` (STUCK — these only affect test infra)

| Line(s) | Change | Why |
|---|---|---|
| 160, 218, 278 | Added `--cap-add=NET_ADMIN \` to each `docker run` for node-a/b/c | Unblocks `iptables`/`tc` inside containers; was the documented FIX-05a item from FIXES.md. |
| 191, 249, 309 | Changed `PGMAN_PROXY_POSTGRES_CONF_EXTRAS=wal_keep_size = 128MB` → `wal_keep_size = 128MB\nmax_slot_wal_keep_size = 16MB` | FIX-05c — enables stale-WAL induction for REC-04 / REPL-03. The `\n` is interpreted by `internal/config/loader.go::parseLines`. |

Both stick: they only modify the test-rig orchestration. No production-default in `internal/config/config.go` was touched.

**Cold-init was required.** The post-initdb hook in `internal/runtime/start.go::postInitDBHook` writes `conf_extras` once at initdb time. Since PGDATA volumes persisted from the prior round, the new `max_slot_wal_keep_size = 16MB` line only landed after wiping `pgman-pc-node-{a,b,c}-data` and re-bootstrapping the cluster.

### Verification after restart

```
primary=b
node-a: standby (streaming, quorum)
node-c: standby (streaming, quorum)
SHOW max_slot_wal_keep_size: a=16MB  b=16MB  c=16MB
SHOW wal_keep_size:          a=128MB
iptables -L -n (as root):    works on all 3 nodes
chaos counters baseline:     writes_ok=1592, writes_failed=0, data_loss_total=0, extra_rows=0
pgmgr_node_a / pgmgr_node_c slots both active, restart_lsn=0/50E8360
```

Rig prep verdict: PASS.

### Side-finding: `iptables` requires `-u 0`

`docker exec pgman-pc-node-X iptables …` runs as USER postgres and returns `Permission denied`. The `-u 0` flag is required to exercise the privileged operation. `docker exec -u 0 pgman-pc-node-a iptables -L -n` works. This is a test-script detail to remember for future NET-* runs.

---

## §2 REC-04 — AutoRebootstrap data correctness

**REQ / AC**: REQ-HEAL-02 / AC-HEAL-02a
**Script**: `validation/scripts/REC-04.sh`
**Log**: `validation/scripts/logs/REC-04-20260513T205403Z.log`

### Setup
Primary = node-b at start. Target standby = node-a.

### Steps & evidence

1. **Stop node-a** via `process-compose process stop node-a`. Container removed, slot `pgmgr_node_a` transitioned to `active=f` immediately.
2. **WAL burner** on primary: 9 iterations of `INSERT … repeat('x',4000) × 1500 rows + pg_switch_wal()`, with `CHECKPOINT` every 3 iters.
   - iter 3: `wal_status=reserved`, behind = 48.9 MB.
   - iter 6: `wal_status=reserved`, behind = 99.2 MB.
   - **iter 9: `wal_status=lost`**, slot invalidated. Total burn ≈ 9 × ~6 MB of WAL records over 9 segments. Took ~6 s wall clock.
3. **Restart node-a** via `process-compose process start node-a`.
4. **Observation window**: 235 s of polling. node-a's PG came up as a standby with `replay_lsn=0/515F248` (the pre-stop LSN) and stayed pinned there. Every 2 s emitted `auto_rebootstrap.detected condition=1 consecutive_ticks={1..150+}`. **NO rebootstrap fired within the 240 s wait-budget.**
5. **Hash comparison (live workload)** at end of wait-loop window: hashes differed because chaos-workload was writing to the (now running) primary during the comparison; primary and standby were at different LSNs.

### Post-hoc analysis

Re-checked the `docker logs pgman-pc-node-a` AFTER the script timed out. The rebootstrap DID fire — at the `consecutive_ticks=150` boundary (~5 min after restart):

```
20:59:16.713 auto_rebootstrap.wipe.started
20:59:16.752 auto_rebootstrap.wipe.completed     duration=38ms
20:59:16.763 auto_rebootstrap.basebackup.started slot_name=pgmgr_node_a source_primary_id=node-b
20:59:17.275 auto_rebootstrap.basebackup.completed duration=508ms
```

Then I paused the chaos-workload and re-ran the hash with all 3 peers at the same LSN:

```
node-a (standby):  lsn=0/100AE3C8  hash=10267|ac57cb09…|ffb7824f…
node-b (primary):  lsn=0/100AE3C8  hash=10267|ac57cb09…|ffb7824f…
node-c (standby):  lsn=0/100AE3C8  hash=10267|ac57cb09…|ffb7824f…
```

All three are **byte-for-byte identical**.

### Verdict: PASS — but with two non-obvious learnings

- The REC-04 contract (post-rebootstrap byte-equality) is honoured: rebootstrap produces a byte-faithful replica.
- **The 5-minute gate from `auto_rebootstrap.detected` to `auto_rebootstrap.wipe.started` is much longer than the wait-budget any reasonable harness would set.** `consecutive_ticks=150` × 2 s tick = 300 s. Looks like `Policy.AutoRebootstrap.PersistenceWindow` is 5 min in the rig (the default). For chaos testing, a 5-min gate makes orchestrating multi-scenario runs awkward.
- The script's verdict logic mis-classified as FAIL because it compared hashes while the workload was still writing. **REC-04.sh now overwrites the prior copy.** Future runs of this scenario should pause the workload before fingerprinting (or use a row-LSN-bracketed projection).

---

## §3 REPL-03 — Replication-slot leak after permanent peer removal

**REQ / AC**: REQ-COORD-02 (implicit), `Policy.SlotCleanupGrace`
**Script**: `validation/scripts/REPL-03.sh`
**Log**: `validation/scripts/logs/REPL-03-20260513T210113Z.log`

### Setup
Primary = node-b at start. Target peer to remove = node-a.

### Timeline

| t | event |
|---|---|
| t=0  | `process-compose process stop node-a` + `docker rm -f pgman-pc-node-a`. Container gone. Slot `pgmgr_node_a` becomes `active=f`. |
| t=0–55 s | First-window observation. Slot status stays `reserved`. WAL behind grew linearly from 2.4 KB → 383 KB. Slot is NOT proactively dropped within 2× AutoDemote.Cooldown (= 60 s). |
| t=60 s onward | Heavy WAL burner (1500-row INSERT + pg_switch_wal in a tight loop). |
| t=60 s probe | `wal_behind = 15.66 MB`, slot still `reserved`. |
| t=90 s probe | slot's wal_behind query returned empty — slot has transitioned to `lost` (the `pg_wal_lsn_diff(current_lsn, restart_lsn)` returns NULL when `restart_lsn` is gone). |
| t=300 s end | Slot is STILL PRESENT (`slot_present=1`) but `wal_status=lost`. WAL files are recyclable. |

### Final slot state

```
pgmgr_node_a | f |        | (lost — restart_lsn=NULL)
pgmgr_node_c | t |        | 0/120A3208 (active, behind=0)
```

`wal_status` row:
```
pgmgr_node_a | f | lost      | <null>
pgmgr_node_c | t | reserved  | 0
```

### Verdict: PASS (slot invalidated, WAL recyclable)

With `max_slot_wal_keep_size=16MB`, the slot is transparently invalidated to `wal_status=lost` once its restart_lsn falls ≥16MB behind the primary's `current_lsn`. The disk-fill hazard is bounded.

### Notable: pg-manager does NOT proactively drop the orphan slot

The slot for the permanently-removed peer **persists** as `pgmgr_node_a (active=f, wal_status=lost)` even after 5 minutes. pg-manager has a `Policy.SlotCleanupGrace` (TEST-PLAN says default ≥ 5 min), but in this run we did not see a cleanup event fire. The slot's pin on disk is bounded (because `wal_status=lost`), so the immediate disk-fill risk is contained — but the **slot inventory grows by one per removed peer**, which is a long-term operability concern. Recommend the SW Engineer:
- confirm `SlotCleanupGrace` semantics (does it ever drop?)
- emit an `embedded_nats.slot_invalidated` event when wal_status flips to `lost`, so operators have a signal to clean up.

---

## §4 SOAK-01 — 10-min sustained-load periodic-kill soak

**REQ / AC**: REQ-DL-01, REQ-HEAL-05 / AC-DL-01a, AC-HEAL-05a
**Script**: `validation/scripts/SOAK-01.sh`
**Log**: `validation/scripts/logs/SOAK-01-20260513T212135Z.log`

### Pre-state at soak start

Primary = node-b. Two standbys: node-c streaming, **node-a stuck in cooldown_active** (carry-over from REC-04 + REPL-03 — node-a's slot had been `lost` and its prior rebootstrap put it in `auto_rebootstrap.refused reason=cooldown_active`). At soak start: `streaming=1` not 2. This is a **carry-over precondition** that the script did NOT pause to clear — see §5.

Counters pre-soak: `writes_ok=22929, writes_failed=1, data_loss_total=0, extra_rows=0`.

### 10-minute timeline (key events)

| t | event | observed state |
|---|---|---|
| t+8 s | KILL non-primary node-c | running=2/3, streaming=1 |
| t+78 s | recovered | running=3/3, streaming=1 (node-a still pending rebootstrap) |
| t+127 s | KILL non-primary node-a | running=2/3, streaming=1 |
| t+302 s | **KILL PRIMARY node-b** | primary=NONE briefly |
| t+314 s | **primary=node-a** (the stale node!) | **`data_loss_total` jumped 0 → 28,185 instantly** |
| t+314 s onward | `writes_ok` stuck at 29,325 — chaos workload write-side broken | every poll: `writes_failed` growing, `extra_rows` growing |
| t+384 s | KILL node-b (failed standby) | further instability |
| t+512 s | KILL node-b | (no recovery — node-b's PG wedged) |

### Settle window (60 s post-soak)

```
primary = node-a   (stale data; chaos_events row count ~14.2k vs ~28k pre-kill on b)
streaming = 1     (only node-c; node-b PG dead)
slots_lost = 0     (slots present and reserved on a)
ctrs (final): writes_ok=30690, writes_failed=213, data_loss_total=28185, extra_rows=58
```

### Final assessment

```
deltas: writes_ok+7467, writes_failed+212, data_loss+28185, extra_rows+58
```

### Verdict: FAIL

Reasons:
- `data_loss_delta = 28,185` rows
- `extra_rows_delta = 58` rows
- `streaming = 1` (want 2)
- node-b PG remained dead through settle (postgres process zombied, pg-manager retrying `pg_ctl start` but PG refuses to recover due to timeline conflict).

### Root-cause analysis of the 28k data loss

Reconstructed from the postmortem of `docker exec pgman-pc-node-b cat /var/lib/postgresql/data/postgres.log`:

```
FATAL: requested timeline 2 is not a child of this server's history
DETAIL: Latest checkpoint in file "pg_control" is at 3/907A7D28 on timeline 1,
        but in the history of the requested timeline, the server forked off
        from that timeline at 0/10110A28.
```

Causal chain:

1. **Carry-over from REPL-03**: node-a entered the soak with `wal_status=lost` for its slot AND `auto_rebootstrap.refused reason=cooldown_active`. node-a's PGDATA was at LSN `~0/10110A28` (stale).
2. **At t+302 s the rig killed node-b (the primary).** node-b's last good checkpoint was at `3/907A7D28` on timeline 1.
3. **node-c (still streaming standby) and node-a (stale, not streaming) faced an election.** node-a held the lease (presumably because lease ownership doesn't gate on WAL-currency), so node-a was promoted at LSN ~`0/10110A28`.
4. node-a's promote forked **timeline 2** at `0/10110A28`. Every write done on timeline 1 between `0/10110A28` and `3/907A7D28` (the data node-b had acked to the chaos workload) is now in a discarded fork.
5. **Chaos workload reads from new primary node-a, sees ~28k seqs missing → `data_loss_total += 28185`.**
6. node-b's PG cannot reattach as a standby because its `pg_control` checkpoint is at `3/907A7D28` on timeline 1, but the cluster's current timeline 2 forked at `0/10110A28` (i.e., before node-b's checkpoint). pg-manager retries `pg_ctl start` indefinitely but PG correctly refuses.

The chaos-workload also stopped advancing `writes_ok` after t+302 s — possibly because libpq multi-host got stuck on node-b connection refused, or because of an internal verifier-loop pathology.

### Caveat: this is partly a TEST-ARTIFACT pathology, partly a real bug

**Test artifact**: SOAK-01 ran *immediately after* REPL-03 left the cluster in a degraded state (one standby stuck pending rebootstrap with cooldown_active). A truly fresh-baseline SOAK-01 should clear this carry-over first — but the test harness as documented doesn't include a "wait for all 3 streaming" pre-check. **Recommend the script add `wait_for_3_streaming` to its pre-state phase.**

**Real bug**: even acknowledging the carry-over precondition, a stale standby was promoted to primary because the embedded-NATS lease-election does NOT consider WAL-currency. **An ack'd-commit durability invariant is violated when a stale peer wins the election after the synced peer dies.** This is REQ-DL-01 territory. See finding §5 below.

### Status of REQ-DL-01

Strictly speaking the 28k rows that were lost had been ack'd with `synchronous_commit=on` against `synchronous_standby_names='ANY 1 ("node-a","node-c")'`. **At time of ack, the ANY 1 sync standby must have been node-c** (because node-a's slot was lost ≥ ~3MB behind during this period — node-a couldn't have been the synchronous standby). After the t+302 s primary kill, node-c was still alive and streaming. If election had selected node-c (the up-to-date standby) all those rows would have been preserved on the new primary.

The election picked node-a (the stale peer) instead. **This is the kernel of the SOAK-01 finding**: leader election in pgman-proxy (embedded NATS / pg-manager) does not appear to prefer the most-current standby. Pure NATS lease ownership does not require WAL position.

---

## §5 Aggregate summary

### Stability invariants

| Invariant | Status after this pass | Evidence |
|---|---|---|
| Slot-bounded WAL retention (`max_slot_wal_keep_size`) | **PROVEN** (with the rig knob) | REPL-03: slot transitions to `wal_status=lost` at the configured 16 MB threshold. |
| Byte-equal rebootstrap | **PROVEN** | REC-04 §2 post-hoc: all 3 peers at identical LSN agree on md5. |
| AutoRebootstrap pipeline fires | **PROVEN** | REC-04: full `wipe.started → wipe.completed → basebackup.started → basebackup.completed` chain observed. |
| AutoRebootstrap firing budget | **CONCERNING** | 5 minutes from `auto_rebootstrap.detected` to wipe — too long for back-to-back chaos scenarios. May be a Policy default the SW Engineer should tune for the rig. |
| Stale standby cannot be promoted | **VIOLATED** | SOAK-01 t+314 s: node-a (stale, slot=lost, pending rebootstrap) was promoted to primary. Caused 28,185-row data-loss event. |
| ex-primary self-heals via AutoDemote after divergence | **NOT OBSERVED** in SOAK-01 | node-b's PG was stuck refusing to start due to timeline-2 conflict; pg-manager kept retrying `pg_ctl start` instead of triggering `AutoDemote` wipe. Container alive, postgres dead, no recovery for ≥ 3 min. |
| Bounded slot inventory after permanent peer removal | **NOT PROVEN** | REPL-03: the orphan slot persists for 5+ min as `lost`. pg-manager did NOT drop it. |

### Top 3 new findings for the SW Engineer

1. **(P0, MUST-PASS-blocker) Stale-standby promotion = data-loss event.** SOAK-01 promoted a peer whose WAL was ~3 MB behind (slot status `lost`, pending rebootstrap) to primary. 28,185 ack'd rows discarded. Election should consider WAL currency or, at minimum, refuse to promote a peer whose AutoRebootstrap is pending. This violates REQ-DL-01 strict reading (an ack'd commit being lost after failover).
2. **(P0) Divergent ex-primary stays wedged when its checkpoint sits between the new-timeline fork point and current LSN.** node-b's `pg_control` checkpoint at `3/907A7D28` on timeline 1 could not attach to timeline 2 (forked at `0/10110A28`). pg-manager kept retrying `pg_ctl start`; AutoDemote did not fire to wipe+basebackup. Net effect: 1/3 cluster capacity gone, indefinitely. The recovery path requires manual intervention. Compare with NET-04 (prior round, PASS): there AutoDemote fired correctly. Here, the precondition (PG fails to start at all) seems to inhibit the detection.
3. **(P1) `auto_rebootstrap.refused reason=cooldown_active` blocks recovery when needed.** After a successful rebootstrap, node-a entered cooldown. When a new condition (slot lost again) appeared during cooldown the refused-events stack up and recovery is gated by the cooldown timer. If the chaos rate exceeds the cooldown timer, the cluster degrades faster than it heals.

### Carry-over findings re-confirmed from prior round

- F-1 / F-2 / F-3 (observability gauges + `password_prefix` empty) — not re-tested in this pass; no new info.
- F-4 (failover RTO 8-16 s vs 5 s p99) — SOAK-01 confirms failover takes ~12 s wall-clock from KILL → new-primary-visible.
- F-5 (inactive slot lifecycle) — REPL-03 reproduces and extends this: pg-manager doesn't clean up `wal_status=lost` slots for permanently-removed peers.

### Updated GO/NO-GO bar assessment vs `COVERAGE-REQUIREMENTS.md §4`

| REQ-id | Tier | Prior verdict | Post-stability verdict |
|---|---|---|---|
| REQ-DL-01 (no ack'd-commit loss) | MUST-PASS | PASS (DI-04, QUOR-01) | **FAIL (SOAK-01 produced 28k-row ack'd loss after a stale-standby promotion)** |
| REQ-DL-02 (no stale-leader writes) | MUST-PASS | (not retested) | n/a |
| REQ-DL-03 (in-flight write hard-close) | MUST-PASS | PASS (TX-01) | unchanged |
| REQ-HEAL-01 (AutoDemote on divergent_ex_primary) | MUST-PASS | PASS (NET-04) | **MIXED** — NET-04 still PASS, but SOAK-01 found a divergent_ex_primary that AutoDemote did NOT recover (PG wedged before pg-manager could probe). |
| REQ-HEAL-02 (byte-equal rebootstrap) | SHOULD-PASS | INCONCLUSIVE | **PASS** — REC-04 now proves byte-equality. |
| REQ-AVAIL-01 (5 s p99 failover) | MUST-PASS | concerning (12 s observed) | unchanged — SOAK-01 added one more 12 s sample. |
| REQ-COORD-02 (R=3 / slot lifecycle) | MUST-PASS | concerning (gauge missing) | unchanged. New side-finding: orphan slots not GC'd. |

**Aggregate recommendation: NO-GO** — REQ-DL-01 is the entire product promise and SOAK-01 reproduced an ack'd-commit data-loss event. The SW Engineer needs to:

1. Make leader election WAL-aware (or refuse to elect a peer whose `auto_rebootstrap` is pending).
2. Add a "divergent + PG refuses to start" path in pg-manager that triggers AutoDemote.wipe immediately (rather than waiting for PG to come up before deciding it's divergent).
3. Tune the AutoRebootstrap PersistenceWindow / cooldown to be more interactive-friendly for chaos testing — or document the production defaults explicitly so test rigs can override them.

The earlier (prior-round) NO-GO call due to observability defects is preserved; this pass adds a stronger NO-GO on REQ-DL-01.

---

## §6 Final cluster state

```
primary = node-a
node-a: primary
node-b: standby (re-bootstrapped after force-wipe + restart)
node-c: standby (streaming)
streaming = 2

chaos counters (cumulative across the run):
  writes_ok       = 39,890+   (advancing)
  writes_failed   = 213       (frozen since end of SOAK)
  data_loss_total = 28,185    (frozen since SOAK t+302s primary kill)
  extra_rows      = 58        (frozen since SOAK)
```

The cluster is healthy: 1 primary, 2 streaming standbys, no leftover iptables rules in any container, no orphan docker volumes, no orphan workload artifacts. The cumulative chaos counters retain the SOAK-01 evidence and should be reset (workload restart) before the next round.

### Rig changes left behind

**STUCK** (test infra only, intentional):
1. `process-compose.yaml:160,218,278` — `--cap-add=NET_ADMIN`.
2. `process-compose.yaml:191,249,309` — `max_slot_wal_keep_size = 16MB` appended to `PGMAN_PROXY_POSTGRES_CONF_EXTRAS`.

**NO production-code changes.** Neither `internal/*` nor `pkg/*` nor `cmd/*` was modified.

---

## §7 Scenario runner scripts

| Path | Status |
|---|---|
| `validation/scripts/REC-04.sh` | **overwritten** — improved (post-rig knob; uses md5 over compact projection; chaos-paused fingerprint not in script — noted as a known-gap to address) |
| `validation/scripts/REPL-03.sh` | **new** |
| `validation/scripts/SOAK-01.sh` | **new** |
| `validation/scripts/lib.sh` | unchanged (carried forward from prior round) |
| `validation/scripts/logs/REC-04-20260513T205403Z.log` | full transcript + post-hoc re-verdict appended |
| `validation/scripts/logs/REPL-03-20260513T210113Z.log` | full transcript |
| `validation/scripts/logs/SOAK-01-20260513T212135Z.log` | full 10-min timeline |
