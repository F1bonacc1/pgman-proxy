# TEST-PLAN — pgman-proxy v1 (feature 002 embedded NATS cluster)

**Status**: Design — NOT executed. QA owns execution.
**Audience**: QA / SRE engineers writing the scripts that exercise the
scenarios below.
**Coupling**: Every scenario carries its `REQ-id` (from
`COVERAGE-REQUIREMENTS.md`) and `AC-id` (acceptance-criterion id from
the same document). When QA encodes pass/fail logic, those ids are the
contract.
**Authoring constraint**: The DBA who wrote this plan did NOT run the
tests. Every scenario is operationally concrete enough that QA can
write the script without further clarification — but step-by-step
empirical fine-tuning (timeouts on flaky boxes, retries on docker
container settling) is QA's responsibility.

This plan EXTENDS the existing 6-scenario chaos loop (kill primary,
SIGTERM standby, SIGSTOP primary 10s, docker restart primary, cascading
failover, rapid flap). The chaos loop is encoded informally on top of
the rig in `process-compose.yaml` + `cmd/chaos-workload/main.go`. This
plan covers **everything that loop does NOT** — data-integrity invariants
beyond aggregate counters, network partitions, replication pathologies,
in-flight workload survival, recovery semantics, quorum edges,
control-plane / coordination, long-soak.

---

## §0 Conventions, test-rig access, and reusable probes

### 0.1 Cluster reference

The chaos rig is the 3-peer container rig spun by `process-compose.yaml`
in the repo root. Topology:

| Peer | Container | Host port (proxy) | Host port (obs) | Network alias | NATS routes |
|---|---|---|---|---|---|
| node-a | `pgman-pc-node-a` | `127.0.0.1:16432 → :6432` | `127.0.0.1:19090 → :9090` | `node-a` | `node-a:6222` |
| node-b | `pgman-pc-node-b` | `127.0.0.1:16433 → :6432` | `127.0.0.1:19091 → :9090` | `node-b` | `node-b:6222` |
| node-c | `pgman-pc-node-c` | `127.0.0.1:16434 → :6432` | `127.0.0.1:19092 → :9090` | `node-c` | `node-c:6222` |

Shared docker network: `pgman-pc-net`. Image: `pgman-proxy:dev`
(built by `process-compose` `builder` step from
`tests/integration/Dockerfile`). PGDATA persists in docker volumes
`pgman-pc-node-{a,b,c}-data`.

The chaos-workload runs **as a host process** (NOT in a container) and
connects through libpq multi-host DSN
`host=127.0.0.1,127.0.0.1,127.0.0.1 port=16432,16433,16434 ...`.

### 0.2 No local psql constraint

The host MAY NOT have a matching PostgreSQL client installed. **Every
SQL probe in this plan MUST be issued one of two ways**:

1. **In-container** (preferred for primary probes):
   ```sh
   docker exec pgman-pc-node-a psql -U postgres -h /var/run/postgresql \
     -At -c "SELECT pg_is_in_recovery();"
   ```
   The Unix socket avoids host/network/TLS issues. The container image
   carries `psql` matching its own pg18 server.

2. **From the devbox shell** at `/home/eugene/projects/envs/pg` (when
   QA explicitly wants to drive load from outside the container):
   ```sh
   cd /home/eugene/projects/envs/pg && devbox shell -- psql \
     "host=127.0.0.1 port=16432 user=postgres dbname=postgres sslmode=disable" \
     -At -c "SELECT pg_is_in_recovery();"
   ```

Scenarios below default to method (1) unless they specifically need to
exercise the proxy path, in which case method (2) is used.

### 0.3 Reusable probe vocabulary

These probes are referenced by short name in scenarios below. QA encodes
them once.

- **`P-LEADER`** — current pg-manager leader as known by a peer:
  `curl -sS -H "Authorization: Bearer $TOKEN" http://127.0.0.1:19090/v1/status | jq -r '.cluster.leader_id // .leader_id'`.
  Run on all three peers; values MUST agree (modulo a transient
  REQ-AVAIL-01 window).

- **`P-MESH`** — embedded-NATS mesh size on a peer:
  `curl -sS http://127.0.0.1:19090/metrics | grep '^pgman_proxy_embedded_nats_routes_meshed '`.
  On a 3-peer healthy cluster the value is `2` on each peer.

- **`P-REPL-LAG`** — replication lag from primary view:
  ```sh
  docker exec pgman-pc-node-<primary> psql -U postgres -h /var/run/postgresql \
    -At -c "SELECT application_name, state, sync_state, write_lag, flush_lag, replay_lag \
            FROM pg_stat_replication;"
  ```

- **`P-IS-PRIMARY`** — is this peer's PG currently a primary:
  `docker exec pgman-pc-node-X psql -U postgres -h /var/run/postgresql -At -c "SELECT NOT pg_is_in_recovery();"` →
  `t` means primary, `f` means standby/recovering.

- **`P-SYNC-CONF`** — sync replication config on the primary:
  ```sh
  docker exec pgman-pc-node-<primary> psql -U postgres -h /var/run/postgresql -At -c \
    "SELECT name, setting FROM pg_settings WHERE name IN ('synchronous_standby_names','synchronous_commit');"
  ```

- **`P-LSN-MAX`** — committed-LSN on each PG (primary + standbys):
  `docker exec pgman-pc-node-X psql -U postgres -h /var/run/postgresql -At -c "SELECT pg_current_wal_flush_lsn();"`
  on the primary; `pg_last_wal_replay_lsn()` on standbys.

- **`P-CTRS`** — last chaos-workload counters line. The workload emits a
  JSON line every 5 s; QA tails `~/.local/share/process-compose/...log`
  or uses `process-compose log chaos-workload` and reads
  `writes_ok,writes_failed,data_loss_total,extra_rows`.

- **`P-EVENT-GREP`** — count of a structured-log event across all peers:
  ```sh
  for n in a b c; do
    docker logs pgman-pc-node-$n 2>&1 | grep -c '"event":"<EVENT>"'
  done
  ```

- **`P-CHAOS-DSN-SINGLE`** — single-target DSN for a specific peer (used
  when QA needs to bind the workload to one peer for routing tests):
  `host=127.0.0.1 port=1643<N> user=postgres dbname=postgres sslmode=disable connect_timeout=1`.

### 0.4 chaos-workload counter semantics (LOAD-BEARING)

The chaos-workload emits exactly these four counters as JSON-line
fields:

- `writes_ok` — cumulative successful INSERTs.
- `writes_failed` — cumulative INSERT failures.
- `data_loss_total` — **distinct-unresolved** acknowledged-but-unreadable
  seqs RIGHT NOW (CHANGELOG d239a6f, fix d239a6f). Transient spikes
  during a stale-read window are CORRECT — they MUST settle to baseline.
- `extra_rows` — rows in DB that were never confirmed by the writer.

**Verdict logic for every scenario in this plan**:

```text
baseline_dl  = data_loss_total at scenario start (after warm-up settle)
final_dl     = data_loss_total at scenario end (after recovery settle, T+30s
               after the last leader-converged event)
PASS         iff (final_dl == baseline_dl) AND (extra_rows == 0 at end)
```

Peak `data_loss_total` during the scenario MAY be non-zero; only the
**settled** value matters.

### 0.5 Rig prerequisites for THIS plan (request to QA infra)

Several scenarios below need tooling NOT currently in the dev image:

- **`iptables`** + **`iproute2` (`tc`)** in the container image so QA
  can run real network-partition + packet-loss scenarios from inside
  the rig without depending on host kernel state.
  → **Action**: rebuild `tests/integration/Dockerfile` with
  `apt-get install -y iptables iproute2`. The integration image lives
  in this repo and is rebuilt by the `builder` step.
- **`fallocate`** or pre-sized loopback files for disk-full scenarios.
- **Sudo-less ability to bring `docker network disconnect` and
  `docker network connect`** on `pgman-pc-net` from outside the
  container (host has docker socket access). This already works in
  the current rig.

### 0.6 Reset-between-scenarios protocol

Unless a scenario explicitly says "skip rig reset", QA runs this
between scenarios to restore a known-good state:

```sh
process-compose down                           # tears down containers + network
docker volume rm pgman-pc-node-a-data \
                 pgman-pc-node-b-data \
                 pgman-pc-node-c-data 2>/dev/null
process-compose up --tui=false                 # boots fresh rig
# Wait until /readyz on all three peers returns 200 AND P-LEADER agrees
# on all three peers AND P-MESH == 2 on each. Then warm chaos-workload
# for 60s and record baseline (P-CTRS).
```

Scenarios that explicitly say "preserve PGDATA" skip the `docker volume
rm`. Several REPL-* scenarios deliberately rely on dirty PGDATA from a
prior step.

### 0.7 Priority labelling

- **P0** — MUST pass to ship v1. Aligns with REQ-ids that map to
  REQ-DL-* (data loss) or to a Constitution principle.
- **P1** — SHOULD pass to ship v1. Aligns with REQ-AVAIL-*, REQ-HEAL-*,
  REQ-COORD-*.
- **P2** — Nice-to-have. Behavioural coverage beyond the v1 commitments.

---

## §1 Data-integrity invariants (DI-*)

### DI-01 — Per-row uniqueness invariant under failover

- **REQ / AC**: REQ-DL-01 / AC-DL-01a, AC-DL-01b
- **Already covered?**: Partially — the chaos loop's `data_loss_total`
  catches duplicates indirectly via `extra_rows`. This scenario
  encodes a HARD invariant on the table itself.
- **Precondition**: Fresh rig (per §0.6). Chaos-workload running ≥ 60 s.
- **Action**:
  1. Run the chaos loop's existing scenario "kill primary" (`docker kill --signal=KILL pgman-pc-node-<primary>`).
  2. After failover settles and the killed peer rejoins (P-MESH == 2 on
     each peer, P-LEADER agrees on each peer), wait 60 s.
  3. SQL probe on every peer:
     ```sh
     docker exec pgman-pc-node-X psql -U postgres -h /var/run/postgresql -At -c \
       "SELECT writer_id, seq, count(*) FROM chaos_events GROUP BY writer_id, seq HAVING count(*) > 1;"
     ```
- **Observable**: Output of the probe (rows with duplicates), chaos
  counters at scenario end.
- **PASS**: Probe returns ZERO rows on every peer. `final_dl ==
  baseline_dl`. `extra_rows == 0`.
- **FAIL**: Any duplicate `(writer_id, seq)` on any peer. The PK on
  `chaos_events` makes a duplicate physically impossible on the primary
  — a duplicate observed only on a standby is timeline-mismatch and is
  also a FAIL.
- **Recovery**: §0.6 reset.
- **Duration**: ~3 min.
- **Priority**: P0

### DI-02 — Monotonic LSN application across failover

- **REQ / AC**: REQ-DL-01, REQ-HEAL-07 / AC-HEAL-05a
- **Already covered?**: No — the chaos loop doesn't probe LSN at all.
- **Precondition**: Fresh rig. Workload running.
- **Action**:
  1. Sample max LSN on the current primary, record:
     `T0_LSN_primary = P-LSN-MAX(primary)`.
  2. Trigger leader kill (`docker kill -s KILL pgman-pc-node-<primary>`).
  3. Wait until P-LEADER converges on a new value on every surviving peer.
  4. Sample the new primary's `pg_current_wal_flush_lsn()` = `T1_LSN_primary`.
  5. Wait 30 s, sample `pg_last_wal_replay_lsn()` on the (now) standbys
     (including the rejoined old primary once it returns) = `T1_LSN_standbys`.
- **Observable**: `T0_LSN_primary`, `T1_LSN_primary`, the standby
  replay LSNs.
- **PASS**: `T1_LSN_primary >= T0_LSN_primary` (LSN is monotonic —
  the new primary's history extends the old one, never moves backward).
  Every standby's replay LSN catches up to within 1 WAL segment of
  `T1_LSN_primary` within 30 s.
- **FAIL**: `T1_LSN_primary < T0_LSN_primary` (timeline forked
  backwards — diverged promotion). Standby stuck below new primary
  by > 1 segment for > 30 s.
- **Recovery**: §0.6 reset.
- **Duration**: ~4 min.
- **Priority**: P0

### DI-03 — Sync-replication settings probe on every elected primary

- **REQ / AC**: REQ-DL-01 / AC-DL-01d
- **Already covered?**: NO — never asserted explicitly. This is the
  COVERAGE-REQUIREMENTS gap explicitly flagged for `REQ-DL-01`.
- **Precondition**: Fresh rig.
- **Action**:
  1. Identify current primary via P-IS-PRIMARY across all peers.
  2. Probe P-SYNC-CONF on the primary.
  3. Trigger 3 sequential `Switchover` LCM operations (or 3 leader
     kills with full recoveries between), so each peer becomes
     primary at least once.
  4. After each leader change, probe P-SYNC-CONF on the new primary.
- **Observable**: `synchronous_standby_names` value and
  `synchronous_commit` value on each leader instance.
- **PASS**: For every primary observation: `synchronous_standby_names`
  matches the regex `^ANY 1 \(` and `synchronous_commit = 'on'`.
- **FAIL**: Any leader observed with sync_standby_names not in `ANY 1
  (...)` form, or `synchronous_commit != on`.
- **Recovery**: §0.6 reset.
- **Duration**: ~6 min.
- **Priority**: P0

### DI-04 — Acknowledged-commit durability across failover (causal)

- **REQ / AC**: REQ-DL-01 / AC-DL-01c
- **Already covered?**: Partially. The workload's `data_loss_total`
  tracks distinct-unresolved seqs but doesn't pinpoint the WORST CASE
  causal: a commit ack'd at T-1 and unreadable at T+1 on the new
  primary.
- **Precondition**: Fresh rig. Workload running at 20 RPS for 60 s
  warm-up.
- **Action**:
  1. From a dedicated test harness using a single connection (not
     libpq multi-host) on the current primary's proxy peer (`P-CHAOS-DSN-SINGLE`
     pointed at the primary's port), execute:
     ```sql
     BEGIN;
     INSERT INTO chaos_events (writer_id, seq, payload)
       VALUES ('di-04', 1, '\x01');
     COMMIT;  -- record return time as T-1; record commit LSN
     ```
     Capture commit LSN via `pg_current_wal_flush_lsn()` IMMEDIATELY
     after.
  2. Within 100 ms of the COMMIT returning, `docker kill -s KILL` the
     primary container.
  3. Wait for the new primary to settle (P-LEADER converged).
  4. SQL probe on the new primary:
     `SELECT seq FROM chaos_events WHERE writer_id = 'di-04' AND seq = 1;`
- **Observable**: The probe output; the chaos-workload's existing
  `DATA LOSS` log line shape, if it fires.
- **PASS**: Row is present on the new primary. (Sync ANY 1 means at
  least one standby had the commit before the ack; that standby is
  guaranteed to be among the two survivors here — kill is on one peer,
  two survive.) `extra_rows == 0` at scenario end.
- **FAIL**: Row absent from the new primary. This violates the entire
  sync-replication contract and is a P0 ship-blocker.
- **Recovery**: §0.6 reset.
- **Duration**: ~3 min.
- **Priority**: P0

### DI-05 — No-extras invariant under retried-write storm

- **REQ / AC**: REQ-DL-02 / AC-DL-01b
- **Already covered?**: Partially — chaos loop tests `extra_rows == 0`
  passively. This forces the workload to retry hard during a failover.
- **Precondition**: Fresh rig. Modify the chaos-workload invocation to
  use `--write-timeout 30s` so writes that hit the failover window
  retry rather than fail-fast.
- **Action**:
  1. Sustained 50 RPS for 60 s.
  2. Trigger a 3-second SIGSTOP on the primary (`docker kill -s STOP
     pgman-pc-node-<primary>`), wait 3 s, then `docker kill -s CONT`.
  3. Continue load for 60 s.
- **Observable**: `extra_rows`, `writes_ok` deltas, PG primary key
  conflicts in PG logs.
- **PASS**: `extra_rows == 0` at end. Any duplicate-key error on the
  primary is matched 1:1 with a `writes_failed` increment (no silent
  duplicate insertion).
- **FAIL**: `extra_rows > 0`. Or duplicate-key errors that don't show
  up as `writes_failed`.
- **Recovery**: §0.6 reset.
- **Duration**: ~3 min.
- **Priority**: P0

---

## §2 Network partitions (NET-*)

> **All NET-* scenarios require the rig image to carry `iptables` and
> `iproute2` (`tc`). See §0.5.** Use `docker network disconnect / connect`
> for whole-peer isolation; use `iptables` for selective port blocking.

### NET-01 — One peer partitioned from the other two (symmetric)

- **REQ / AC**: REQ-HEAL-03 / AC-HEAL-03a, AC-HEAL-03b
- **Already covered?**: NO. The existing chaos loop only uses process
  signals, never iptables/network.
- **Precondition**: Fresh rig. Workload running. P-LEADER == `node-a`
  (or whichever; record it).
- **Action**:
  1. Identify the current leader and one non-leader.
  2. Partition the non-leader (say `node-c`) from both siblings AND
     from the host: `docker network disconnect pgman-pc-net pgman-pc-node-c`.
  3. Hold for 60 s.
  4. Reconnect: `docker network connect --alias node-c pgman-pc-net pgman-pc-node-c`.
  5. Wait 60 s.
- **Observable**:
  - On `node-c`: `embedded_nats.route_down{reason="peer_disconnect"}`
    for both siblings; leadership-state gauge → `not-leader`; node-c's
    proxy refuses writes (peer is fenced).
  - On `node-a` / `node-b`: `route_down` for `node-c`; P-MESH == 1 on
    each; leader unchanged (quorum 2/3 retained); `writes_ok` continues
    to increase.
  - After reconnect: `route_up` events; P-MESH back to 2; chaos
    `data_loss_total` settled.
- **PASS**:
  - Survivors: `writes_ok(t+60s) > writes_ok(t)` during partition.
  - Partitioned peer: leadership-state gauge `not-leader` throughout
    partition; writes routed there (if any) fail-fast.
  - Final: `final_dl == baseline_dl` AND `extra_rows == 0`.
- **FAIL**: Partitioned peer continues to serve writes locally;
  survivors lose quorum (zero leader); `data_loss_total` does not
  settle.
- **Recovery**: §0.6 reset (full).
- **Duration**: ~3 min.
- **Priority**: P0

### NET-02 — Asymmetric partition (one-way packet loss between leader and one peer)

- **REQ / AC**: REQ-HEAL-03 / AC-HEAL-03a
- **Already covered?**: NO.
- **Precondition**: Fresh rig. Workload running. Note the current
  leader.
- **Action**: From inside a sibling container (or via `docker exec` on
  the leader), drop INBOUND packets from one specific peer using
  iptables on the cluster-routes port:
  ```sh
  docker exec pgman-pc-node-<leader> iptables -I INPUT 1 \
    -p tcp --dport 6222 -s <node-b-container-ip> -j DROP
  ```
  Hold 30 s. Then `iptables -D INPUT 1`.
- **Observable**: `embedded_nats.route_down{reason="peer_disconnect"}`
  on both sides (NATS will detect the half-open route via its own
  liveness ping); the leader's view of `routes_meshed` drops to 1.
- **PASS**: Leader keeps writing through to surviving standby (sync ANY
  1 with two replicas → quorum still satisfied). After cleanup, mesh
  reconverges; `final_dl == baseline_dl`.
- **FAIL**: Both peers retain belief of an active route despite zero
  bytes flowing (route handshake without heartbeat keepalive — bug).
  Or leader self-fences (it shouldn't — quorum of 2 still reachable
  via the other standby).
- **Recovery**: Confirm iptables flushed; §0.6 reset.
- **Duration**: ~3 min.
- **Priority**: P1

### NET-03 — Brief partition flap (< 1 s)

- **REQ / AC**: REQ-HEAL-03, REQ-HEAL-07 / AC-HEAL-03a
- **Already covered?**: Indirectly — the existing rapid-flap process
  scenario kills processes, not links.
- **Precondition**: Fresh rig.
- **Action**:
  ```sh
  docker network disconnect pgman-pc-net pgman-pc-node-c
  sleep 0.5
  docker network connect --alias node-c pgman-pc-net pgman-pc-node-c
  ```
  Repeat 10 times with 5 s between flaps.
- **Observable**: `route_down` / `route_up` pairs in
  `pgman_proxy_embedded_nats_lifecycle_events_total{event=...}`. No
  leader change (the affected peer is a non-leader). Chaos counters
  flat.
- **PASS**: 10 down/up pairs observed on the affected peer's siblings;
  no leader election occurs; `data_loss_total` and `extra_rows` both
  zero at end.
- **FAIL**: Sub-second flap triggers a leader election (mesh stability
  not honoured by pg-manager). `writes_failed` increments.
- **Recovery**: §0.6 reset.
- **Duration**: ~3 min.
- **Priority**: P1

### NET-04 — Long partition > AutoDemote.Cooldown (45 s; rig has Cooldown=30s)

- **REQ / AC**: REQ-HEAL-01, REQ-HEAL-03 / AC-HEAL-01c
- **Already covered?**: NO.
- **Precondition**: Fresh rig. Confirm
  `PGMAN_PROXY_POLICY_AUTO_DEMOTE_COOLDOWN=30s`.
- **Action**:
  1. Partition the leader away from both standbys for 45 s.
     `docker network disconnect pgman-pc-net pgman-pc-node-<leader>`.
  2. While partitioned: continue workload against the 2-survivor pool.
     A new leader should emerge among them.
  3. After 45 s, reconnect.
- **Observable**:
  - The two survivors elect a new leader within REQ-AVAIL-01 budget.
  - The reconnected ex-primary detects divergence: its on-disk role is
    primary while quorum elected someone else. Expect `auto_demote`
    events: `DivergenceDetected` → `AutoDemoteAttempted` →
    `AutoDemoteAccepted` (PGDATA wiped, rebasebackup against the new
    leader).
- **PASS**: Cluster ends with one leader, two streaming standbys (one
  of which is the rebootstrapped ex-primary). `final_dl ==
  baseline_dl`. `extra_rows == 0`. SQL probe on the rebootstrapped
  peer: `pg_is_in_recovery() = true`.
- **FAIL**: Two primaries persist after recovery (split-brain). Or
  the rebootstrapped peer's `chaos_events` row count diverges from the
  new leader's.
- **Recovery**: §0.6 reset.
- **Duration**: ~5 min.
- **Priority**: P0

### NET-05 — Cluster-routes port black-holed but PG port reachable

- **REQ / AC**: REQ-DL-05 / AC-DL-05a
- **Already covered?**: NO — chaos loop only kills processes.
- **Precondition**: Fresh rig. Workload running.
- **Action**: On the leader container, block its NATS cluster-routes
  port from inbound only:
  ```sh
  docker exec pgman-pc-node-<leader> iptables -I INPUT 1 \
    -p tcp --dport 6222 -j DROP
  ```
  Hold for 20 s. Then flush.
- **Observable**: The leader's `pgman_proxy_lease_renewal_failures_total`
  counter increments; leadership-state gauge → `not-leader`; the
  proxy on that peer should refuse local writes (lease-loss self-fence).
  Critically, its local PG is still healthy — the peer must NOT
  silently serve writes that bypass the lease check.
- **PASS**: After 20 s, the leader's leadership gauge is `not-leader`;
  attempting an INSERT against `127.0.0.1:1643<leader>` fails (the
  proxy returns an error pointing at the new leader, or refuses). One
  of the standbys is now leader. `final_dl == baseline_dl`.
- **FAIL**: The peer continues to serve writes locally despite lease
  loss. (REQ-DL-05 violation.)
- **Recovery**: Flush iptables; §0.6 reset.
- **Duration**: ~3 min.
- **Priority**: P0

### NET-06 — Sustained packet loss on cluster-routes (10%, 30%, 50%)

- **REQ / AC**: REQ-HEAL-03 / —
- **Already covered?**: NO.
- **Precondition**: Fresh rig. Workload running.
- **Action**: Apply `tc qdisc` packet loss on cluster-routes (port
  6222) at the leader. Three sub-scenarios: 10%, 30%, 50% loss for
  120 s each.
  ```sh
  docker exec pgman-pc-node-<leader> tc qdisc add dev eth0 root netem loss 10%
  # ... hold 120 s ...
  docker exec pgman-pc-node-<leader> tc qdisc del dev eth0 root
  ```
- **Observable**: NATS route heartbeat behaviour under loss. At 10% the
  cluster should stay stable; at 30%+ NATS may declare routes down
  intermittently, producing `route_down` / `route_up` churn.
- **PASS**: Under 10% loss, no leader changes; `data_loss_total`
  stays at baseline. Under 30% / 50% loss, the cluster MAY elect a new
  leader, but `final_dl == baseline_dl` and `extra_rows == 0`
  post-recovery.
- **FAIL**: `data_loss_total` does not settle. Or a sustained
  unresolved partition develops where no peer holds the lease and the
  whole cluster stops serving writes for > 60 s.
- **Recovery**: Delete all tc qdiscs; §0.6 reset.
- **Duration**: ~10 min (3 sub-runs).
- **Priority**: P2

---

## §3 Replication pathologies (REPL-*)

### REPL-01 — Slow standby (artificial `recovery_min_apply_delay`)

- **REQ / AC**: REQ-DL-01, REQ-HEAL-07 / —
- **Already covered?**: NO.
- **Precondition**: Fresh rig. Workload running. Identify two
  standbys.
- **Action**:
  1. On one standby (say node-c), set `recovery_min_apply_delay = 30s`
     and reload:
     ```sh
     docker exec pgman-pc-node-c psql -U postgres -h /var/run/postgresql -c \
       "ALTER SYSTEM SET recovery_min_apply_delay = '30s';"
     docker exec pgman-pc-node-c psql -U postgres -h /var/run/postgresql -c "SELECT pg_reload_conf();"
     ```
  2. Continue load 90 s.
  3. Kill the primary.
- **Observable**: `pg_stat_replication` on primary: slow standby's
  `replay_lag` ≈ 30 s. The OTHER standby is the only one with current
  data. `sync_state` should still be `quorum` (ANY 1) and the FAST
  standby satisfies sync. On primary kill: pg-manager MUST promote the
  FAST standby, not the slow one.
- **PASS**: New primary is the fast standby (verified by
  `pg_current_wal_flush_lsn()` matching pre-kill primary's LSN). Slow
  standby is rebootstrapped or catches up. `final_dl == baseline_dl`.
- **FAIL**: Slow standby promoted → silent data loss (commits acked
  on primary + fast standby are missing on slow standby = new primary).
- **Recovery**: Reset `recovery_min_apply_delay`; §0.6 reset.
- **Duration**: ~5 min.
- **Priority**: P0

### REPL-02 — Stalled standby walreceiver (kill -STOP on PG backend)

- **REQ / AC**: REQ-HEAL-02 / AC-HEAL-02a
- **Already covered?**: NO.
- **Precondition**: Fresh rig. Workload running. Note `wal_keep_size =
  128MB` in the rig.
- **Action**:
  1. Find the walreceiver PID on a standby:
     ```sh
     docker exec pgman-pc-node-c pgrep -f 'walreceiver'
     ```
  2. SIGSTOP it: `docker exec pgman-pc-node-c kill -STOP <pid>`.
  3. Drive the primary HARD: bump chaos-workload RPS to 500 for 5
     minutes; this should write more than 128 MB of WAL and force the
     primary to recycle past the stalled standby's needed segment.
  4. SIGCONT the walreceiver. Standby will fail to resume streaming
     ("WAL segment required has been removed"). pg-manager's
     `AutoRebootstrap` should kick in.
- **Observable**: PG log on standby: "requested WAL segment ... has
  already been removed". slog events: `AutoRebootstrapAttempted`,
  `AutoRebootstrapAccepted`, PGDATA wipe + pg_basebackup.
- **PASS**: Standby is rebootstrapped within `PersistenceWindow +
  base-backup time`. Post-recovery: standby is streaming again,
  row-count matches primary. `final_dl == baseline_dl`.
- **FAIL**: Standby stuck forever; no `AutoRebootstrap` event; manual
  intervention needed. OR rebootstrap completes but row count diverges.
- **Recovery**: §0.6 reset.
- **Duration**: ~10 min.
- **Priority**: P1

### REPL-03 — Replication-slot leak after permanent peer loss

- **REQ / AC**: REQ-COORD-02 (implicit), `Policy.SlotCleanupGrace`
- **Already covered?**: NO.
- **Precondition**: Fresh rig. Workload running. Identify primary.
- **Action**:
  1. List HA replication slots on primary:
     ```sh
     docker exec pgman-pc-node-<primary> psql -U postgres -h /var/run/postgresql -At -c \
       "SELECT slot_name FROM pg_replication_slots WHERE slot_name LIKE 'pgmgr_%';"
     ```
     Record baseline (expect 2: one per standby).
  2. Permanently remove one standby: `process-compose stop node-c` AND
     `docker volume rm pgman-pc-node-c-data`. Do NOT restart it.
  3. Wait `SlotCleanupGrace + 60 s`. (Default is 5 min if zero-valued.)
- **Observable**: Slot for the removed standby should be cleaned up
  within the grace window. Until cleanup, primary's
  `pg_wal` accumulates.
- **PASS**: The orphan slot is dropped within `SlotCleanupGrace`.
  Primary's `pg_replication_slots` returns the 1 remaining standby's
  slot.
- **FAIL**: Slot persists indefinitely (WAL accumulation, eventual
  disk-full). Or slot is dropped immediately (cleanup grace not
  respected, premature drop on a transient partition is a different
  P0 bug).
- **Recovery**: `process-compose down`; full §0.6 reset.
- **Duration**: ~10 min.
- **Priority**: P1

### REPL-04 — Timeline mismatch after manually divergent promotion

- **REQ / AC**: REQ-HEAL-01, REQ-HEAL-06 / AC-HEAL-01a, AC-HEAL-06a
- **Already covered?**: Yes implicitly (the f1d67d7 standby.signal fix
  area), but never with a manual force.
- **Precondition**: Fresh rig. Workload running.
- **Action**:
  1. Partition node-a from node-b + node-c (`docker network
     disconnect`).
  2. While partitioned, manually force-promote node-a's PG via the
     proxy's control plane (`Promote` LCM) — but this is gated; alt:
     `docker exec pgman-pc-node-a psql -U postgres -h /var/run/postgresql -c \
     "SELECT pg_promote();"` to force a postgres-level promotion.
  3. Continue load on node-a (which now thinks it's primary on its own
     timeline).
  4. Reconnect node-a to the network. The "real" cluster (b+c) has
     elected a new leader and the chaos-workload has been writing
     there.
- **Observable**:
  - On reconnect: node-a is in `pg_is_in_recovery()=false` but
    pg-manager says role=standby. This is the EX-PRIMARY DIVERGENCE
    class.
  - Expect `DivergenceDetected` → `AutoDemoteAttempted` (gated by
    `LeadershipStabilityWindow + ProbeTimeout`) → `AutoDemoteAccepted`
    → PGDATA wipe + rebasebackup.
- **PASS**: node-a is rebootstrapped against the real primary; row
  count matches. The forked timeline's writes on node-a are discarded
  (they were never ack'd to the chaos-workload). `final_dl ==
  baseline_dl`. `extra_rows == 0`.
- **FAIL**: Both nodes persist as primary (split-brain). OR the
  chaos-workload sees `extra_rows > 0` (forked writes leaked through
  somehow). OR `divergent_ex_primary` state observed and AutoDemote
  never fires (gates stuck).
- **Recovery**: §0.6 reset.
- **Duration**: ~6 min.
- **Priority**: P0

### REPL-05 — Stale-WAL rebootstrap path (canonical)

- **REQ / AC**: REQ-HEAL-02 / AC-HEAL-02a
- **Already covered?**: NO — rig's `wal_keep_size = 128MB` is too
  generous.
- **Precondition**: Modified rig: set `PGMAN_PROXY_POSTGRES_CONF_EXTRAS`
  to lower `wal_keep_size` to `16MB`.
- **Action**:
  1. Stop one standby (`process-compose stop node-c`).
  2. Drive the primary to write > 64 MB of WAL (RPS 500 for 2 min).
     Watch `pg_walfile_name(pg_current_wal_lsn())` advance past the
     stopped standby's last-replayed segment.
  3. Bring node-c back (`process-compose start node-c`).
  4. Observe.
- **Observable**: PG log on node-c: "requested WAL segment ... has
  already been removed". slog: `AutoRebootstrapAttempted`,
  `AutoRebootstrapAccepted`. PGDATA wipe + pg_basebackup.
- **PASS**: node-c is rebootstrapped within `PersistenceWindow +
  base-backup time`. Post-recovery: streaming, row-count matches
  primary.
- **FAIL**: node-c stuck in a recovery loop without `AutoRebootstrap`
  ever firing. OR `AutoRebootstrap` fires inside `PersistenceWindow`
  (premature).
- **Recovery**: §0.6 reset; restore `wal_keep_size`.
- **Duration**: ~10 min.
- **Priority**: P1

---

## §4 In-flight workload (TX-*, CONC-*)

### TX-01 — In-flight 10-second BEGIN/COMMIT spans a failover

- **REQ / AC**: REQ-DL-03 / AC-DL-03a, AC-DL-03b
- **Already covered?**: NO — chaos-workload uses pooled libpq which
  auto-fails-over. This needs a DEDICATED client.
- **Precondition**: Fresh rig. Workload running.
- **Action**: From devbox shell, open a dedicated psql session against
  one peer:
  ```sh
  cd /home/eugene/projects/envs/pg && devbox shell -- \
    psql "host=127.0.0.1 port=16432 user=postgres dbname=postgres" \
      -c "BEGIN;
          INSERT INTO chaos_events (writer_id, seq, payload) VALUES ('tx-01', 1, '\x01');
          SELECT pg_sleep(10);
          INSERT INTO chaos_events (writer_id, seq, payload) VALUES ('tx-01', 2, '\x02');
          COMMIT;"
  ```
  While that command runs (during the 10 s sleep), kill the current
  primary: `docker kill -s KILL pgman-pc-node-<primary>`.
- **Observable**:
  - The client receives an error (connection reset / FATAL terminating
    connection due to administrator command / SSL connection closed
    unexpectedly).
  - The session is hard-closed (default switch policy).
  - Neither row of `('tx-01', 1)` nor `('tx-01', 2)` is committed.
- **PASS**: Client sees connection-closed error. SQL probe on the new
  primary: `SELECT * FROM chaos_events WHERE writer_id = 'tx-01';`
  returns ZERO rows (the transaction was open, no COMMIT, so nothing
  durable).
- **FAIL**: The client sees a "successful" COMMIT despite the
  underlying connection moving primaries mid-transaction (silent
  routing of in-flight tx, REQ-DL-03 violation). OR one row but not
  both is present (partial commit). OR both rows are present despite
  the COMMIT never returning success to the client.
- **Recovery**: §0.6 reset.
- **Duration**: ~3 min.
- **Priority**: P0

### TX-02 — Long-running SELECT spans a failover

- **REQ / AC**: REQ-DL-03 / —
- **Already covered?**: NO.
- **Precondition**: Fresh rig.
- **Action**: Start a long SELECT through a non-leader proxy:
  ```sql
  SELECT count(*), pg_sleep(15) FROM generate_series(1, 1000) g(i);
  ```
  Kill the primary during the sleep.
- **Observable**: Default switch policy is hard-close → client gets
  RST. Reconnecting to any peer reaches the new leader.
- **PASS**: Client sees connection reset. After reconnecting,
  SELECTs continue to work against the new leader.
- **FAIL**: Client receives a silently-completed result from a
  half-routed query (proxy stitched two backends together — the
  Constitution-I wire-fidelity rule prevents this).
- **Recovery**: §0.6 reset.
- **Duration**: ~2 min.
- **Priority**: P1

### TX-03 — LISTEN / NOTIFY channel across failover

- **REQ / AC**: REQ-DL-03 / —
- **Already covered?**: NO.
- **Precondition**: Fresh rig.
- **Action**:
  1. Listener (devbox shell):
     ```sql
     LISTEN ch1;
     ```
     Block on this session (e.g., `\watch` or a python pg listener).
  2. From another session through any peer:
     ```sql
     NOTIFY ch1, 'msg-pre-failover';
     ```
  3. Kill the primary.
  4. After failover, on the listener session: try to issue another
     statement.
- **Observable**: NOTIFY is in-memory pub/sub on the primary; failover
  invalidates any pending notification. The listener session is
  hard-closed.
- **PASS**: Listener session receives `msg-pre-failover` if it was
  flushed pre-kill; on kill, session is hard-closed (RST). New listener
  on the new primary sees subsequent NOTIFYs.
- **FAIL**: NOTIFY messages are silently dropped without the listener
  knowing the channel is dead (no connection close). OR notifications
  from the OLD primary's queue surface on the NEW primary's listeners
  (this would be a pg-manager bug, not a proxy bug, but flag it).
- **Recovery**: §0.6 reset.
- **Duration**: ~3 min.
- **Priority**: P2

### TX-04 — Prepared transaction across failover (PREPARE TRANSACTION)

- **REQ / AC**: REQ-DL-03 / —
- **Already covered?**: NO.
- **Precondition**: Fresh rig. NOTE: rig must set
  `max_prepared_transactions > 0`; check with
  `SHOW max_prepared_transactions;` — if 0, this scenario is N/A and
  QA records a config gap (the rig should expose this knob to
  exercise distributed-tx clients).
- **Action**:
  ```sql
  BEGIN;
  INSERT INTO chaos_events (writer_id, seq, payload) VALUES ('tx-04', 1, '\x01');
  PREPARE TRANSACTION 'gid-tx-04';
  -- (do not commit yet)
  ```
  Kill primary; observe the new primary's `pg_prepared_xacts`.
- **Observable**: A prepared transaction is durable in WAL once
  PREPARE TRANSACTION returns. So sync ANY 1 replicated it; the new
  primary should see it in `pg_prepared_xacts`.
- **PASS**: New primary's `pg_prepared_xacts` lists `gid-tx-04`.
  Issuing `COMMIT PREPARED 'gid-tx-04'` on the new primary materialises
  the row. `extra_rows == 0`.
- **FAIL**: Prepared transaction lost (not present on new primary).
  Severe replication-fidelity bug.
- **Recovery**: §0.6 reset.
- **Duration**: ~3 min.
- **Priority**: P1

### CONC-01 — High-concurrency writer storm across a failover

- **REQ / AC**: REQ-DL-01 / AC-DL-01a
- **Already covered?**: Implicitly by the chaos loop at 20 RPS. This
  pushes harder.
- **Precondition**: Fresh rig.
- **Action**: Run two chaos-workload instances in parallel, each with
  a distinct `--writer-id`, each at 100 RPS. Drive for 60 s, kill
  primary, wait 60 s, repeat the kill on the new primary.
- **Observable**: Both workloads' counters. Two primary keys are in
  play; PK uniqueness is enforced.
- **PASS**: Both workloads' `final_dl == baseline_dl` and `extra_rows
  == 0`.
- **FAIL**: Either workload reports unresolved data loss. Or
  `chaos_events` table contains rows from a writer not in either
  workload's confirmed set.
- **Recovery**: §0.6 reset.
- **Duration**: ~6 min.
- **Priority**: P1

---

## §5 Recovery semantics (REC-*)

### REC-01 — standby.signal pre-emption: force the bug, verify the fix catches it

- **REQ / AC**: REQ-HEAL-01 (the f1d67d7 fix area) / AC-HEAL-01a
- **Already covered?**: Indirectly — the existing rig validates the
  positive path (everything works). This forces the NEGATIVE path
  (the bug f1d67d7 was supposed to fix) to confirm the guard catches it.
- **Precondition**: Fresh rig. Identify current primary; the chaos
  loop's `docker restart primary` scenario is the trigger.
- **Action**:
  1. Stop process-compose for one peer: `process-compose stop node-a`.
     (Assume node-a is the primary.)
  2. WHILE the container is stopped, delete `standby.signal` from its
     PGDATA volume to simulate a pre-f1d67d7 startup:
     ```sh
     docker run --rm -v pgman-pc-node-a-data:/data alpine \
       sh -c 'rm -f /data/standby.signal && ls -la /data/PG_VERSION /data/postmaster.opts 2>/dev/null'
     ```
  3. Start node-a back up: `process-compose start node-a`.
- **Observable**: The fix at `internal/runtime/start.go::ensureStandbySignalIfInitialized`
  must re-create `standby.signal` BEFORE pg-manager calls `pg_ctl start`.
  Check on the running container:
  ```sh
  docker exec pgman-pc-node-a ls -la /var/lib/postgresql/data/standby.signal
  ```
  immediately after `process-compose start` reports readiness.
- **PASS**: `standby.signal` file is present (either re-created by the
  fix or already there). node-a comes up as standby and rejoins.
  `final_dl == baseline_dl`. No split-brain.
- **FAIL**: node-a comes up as a postgres-level primary while the
  cluster has another leader — that's the bug f1d67d7 was supposed to
  fix. Detection: `P-IS-PRIMARY` returns `t` on both node-a and the
  actual cluster leader simultaneously, for any window.
- **Recovery**: §0.6 reset.
- **Duration**: ~4 min.
- **Priority**: P0

### REC-02 — AutoDemote refusal under cooldown

- **REQ / AC**: REQ-HEAL-01 / AC-HEAL-01c
- **Already covered?**: NO — refusal-reason coverage missing
  per COVERAGE-REQUIREMENTS gap analysis.
- **Precondition**: Rig with `PGMAN_PROXY_POLICY_AUTO_DEMOTE_COOLDOWN=30s`
  (current default).
- **Action**:
  1. Trigger one ex-primary-divergence scenario (e.g., REPL-04 setup).
     Wait for `AutoDemoteAccepted`. This sets the cooldown clock.
  2. WITHIN 20 s of the AutoDemote completing (i.e., inside the 30 s
     cooldown), force another divergence on the same node (partition
     it again, manually promote on it, reconnect).
- **Observable**: slog event `AutoDemoteRefusedEvent{reason="cooldown_active"}`.
- **PASS**: The second divergence yields a refusal event with the
  cooldown reason; no second PGDATA wipe occurs (verified by
  `pgmgr_*` slot count and inode timestamps on PGDATA root). After
  cooldown elapses, a third trigger DOES wipe (positive control).
- **FAIL**: Second wipe happens inside cooldown (cooldown gate not
  honoured). OR refusal event has wrong `reason` field.
- **Recovery**: §0.6 reset.
- **Duration**: ~8 min.
- **Priority**: P1

### REC-03 — AutoDemote refusal under leadership-stability-window

- **REQ / AC**: REQ-HEAL-01 / AC-HEAL-01c (sibling)
- **Already covered?**: NO.
- **Precondition**: Fresh rig.
  `LEADERSHIP_STABILITY_WINDOW=5s` per the chaos rig override.
- **Action**: Force divergence WHILE rapidly flapping the leader
  (kill leader every 3 s for 30 s). The stability window
  should never close.
- **Observable**: `AutoDemoteRefusedEvent{reason="leadership_unstable"}`
  (or equivalent) repeatedly. NO PGDATA wipe.
- **PASS**: At least one refusal event with leadership-unstable reason
  during the flap window. PGDATA on the divergent peer is untouched
  (mtime check on `pg_wal/`).
- **FAIL**: Wipe occurs during instability. Or no refusal event is
  emitted (silent skip).
- **Recovery**: §0.6 reset.
- **Duration**: ~5 min.
- **Priority**: P1

### REC-04 — AutoRebootstrap data correctness (byte-for-byte)

- **REQ / AC**: REQ-HEAL-02 / AC-HEAL-02a
- **Already covered?**: NO — chaos loop checks counters, not bytes.
- **Precondition**: Rig modified for stale-WAL (per REPL-05).
- **Action**:
  1. Take a checksum of the chaos_events table on the current primary:
     ```sh
     docker exec pgman-pc-node-<primary> psql -U postgres -h /var/run/postgresql -At -c \
       "SELECT md5(string_agg(writer_id||':'||seq||':'||encode(payload,'hex'), ',' ORDER BY writer_id, seq)) FROM chaos_events;"
     ```
     Call this `H_primary`.
  2. Trigger AutoRebootstrap on one standby (REPL-05).
  3. After AutoRebootstrap completes and the standby is streaming,
     wait 30 s for catch-up, then compute the same checksum on the
     rebootstrapped standby: `H_standby_after`.
  4. Compute `H_primary_after` on the (still) primary at the same
     instant.
- **Observable**: All three hashes.
- **PASS**: `H_standby_after == H_primary_after`. The two MAY differ
  from the pre-rebootstrap `H_primary` because writes continued; what
  matters is they agree at the same wall-clock moment after catch-up.
- **FAIL**: Hashes differ → rebootstrap produced an inconsistent
  replica.
- **Recovery**: §0.6 reset.
- **Duration**: ~12 min.
- **Priority**: P0

---

## §6 Quorum edge cases (QUOR-*)

### QUOR-01 — Primary alone with both standbys dead, sync_commit=on, ANY 1 (...)

- **REQ / AC**: REQ-DL-01 (sync-block) / —
- **Already covered?**: NO.
- **Precondition**: Fresh rig.
- **Action**:
  1. Kill both standbys: `docker kill -s KILL pgman-pc-node-b pgman-pc-node-c`.
     Do NOT let process-compose restart them: set `restart: "no"` for
     this run, or run with `process-compose stop node-b node-c`.
  2. Issue an INSERT against the remaining primary via the proxy.
- **Observable**: Per `Policy.OnSyncStandbyLoss = BlockWritesOnSyncLoss`
  (the documented default), the primary's `synchronous_commit=on`
  COMMIT MUST hang waiting for a sync standby; if pg-manager rewires
  `synchronous_standby_names` to empty when no standbys are available,
  the commit returns but the sync-on-loss policy was violated.
- **PASS**: The INSERT either (a) hangs until a standby reappears, OR
  (b) returns an error indicating "waiting for sync standby". It MUST
  NOT silently complete and return success — that would mean ANY 1
  was bypassed.
- **FAIL**: Silent COMMIT success with zero sync replicas. (This is
  the "fake durability" failure mode and is a P0 ship-blocker.)
- **Recovery**: `process-compose start node-b node-c`; §0.6 reset.
- **Duration**: ~3 min.
- **Priority**: P0

### QUOR-02 — Degraded one-standby pool (write under steady-state ANY 1 with one sync target)

- **REQ / AC**: REQ-DL-01 / —
- **Already covered?**: Indirectly.
- **Precondition**: Fresh rig.
- **Action**: Kill ONE standby and let it stay down for 5 minutes.
  Chaos workload at 20 RPS throughout.
- **Observable**: Single surviving standby is the sole sync target.
  Continued writes; `pg_stat_replication` shows 1 row.
- **PASS**: `writes_ok` increases monotonically. `final_dl ==
  baseline_dl`. Killing the remaining standby would trigger QUOR-01.
- **FAIL**: Writes hang despite one healthy standby satisfying ANY 1.
- **Recovery**: Bring back the killed peer; §0.6 reset.
- **Duration**: ~6 min.
- **Priority**: P1

### QUOR-03 — Standby pool churn (crash-restart-crash-restart)

- **REQ / AC**: REQ-HEAL-04, REQ-HEAL-05 / —
- **Already covered?**: Partially (rapid flap scenario in chaos loop).
- **Precondition**: Fresh rig.
- **Action**: On one standby, loop 10 times:
  `process-compose stop node-c; sleep 8; process-compose start node-c;
  sleep 8`. While the workload runs at 20 RPS.
- **Observable**: Each restart cycle: peer rejoins, `route_up` fires,
  pg-manager catches it up, `pg_stat_replication` populates.
- **PASS**: All 10 cycles complete. `final_dl == baseline_dl`.
  `extra_rows == 0`. Cluster has 3 streaming peers at end.
- **FAIL**: Replication slot leak after one of the cycles. Or peer
  fails to rejoin and stays absent.
- **Recovery**: §0.6 reset.
- **Duration**: ~5 min.
- **Priority**: P1

---

## §7 Control plane / coordination (CTRL-*)

### CTRL-01 — Embedded NATS server restart on the primary (via SIGHUP-incompatible config force)

- **REQ / AC**: REQ-COORD-04 / AC-COORD-04b
- **Already covered?**: NO.
- **Precondition**: Fresh rig.
- **Action**: Send SIGHUP after changing a startup-only key (e.g., add
  `PGMAN_PROXY_CLUSTER_NAME=different-name` env override and reload).
  This requires modifying the container env mid-run, which docker
  doesn't allow — instead, write a sentinel file the SIGHUP loader can
  read, OR exercise via a config-file based deployment.
  **Practical execution**: SIGHUP with `cluster.peers` unchanged AND a
  password rotation (allow-list key). Verify the embedded NATS server
  does NOT restart — only `ReloadOptions` is called.
- **Observable**: `embedded_nats.server_started` / `server_stopped`
  events should NOT fire on SIGHUP. `embedded_nats.reload_applied`
  SHOULD fire.
- **PASS**: One `reload_applied` event; zero new `server_started` or
  `server_stopped` events. Routes stay up throughout (`P-MESH`
  stays at 2 on each peer).
- **FAIL**: SIGHUP causes a full embedded-server restart (routes drop,
  leadership flaps).
- **Recovery**: §0.6 reset.
- **Duration**: ~2 min.
- **Priority**: P1

### CTRL-02 — Peer isolated from coordination but local PG reachable

- **REQ / AC**: REQ-DL-05 / AC-DL-05a
- **Already covered?**: NO (alias of NET-05; included here under
  the control-plane category for cross-reference).
- See NET-05.

### CTRL-03 — Rapid leader-elect churn (kill leader repeatedly)

- **REQ / AC**: REQ-AVAIL-01 / AC-AVAIL-01a
- **Already covered?**: Partially (chaos loop's rapid flap).
- **Precondition**: Fresh rig.
- **Action**: Loop 5 times: kill current primary (`docker kill -s
  KILL`), wait 15 s for failover and rejoin, repeat. Each cycle picks
  a different node as the kill target (whichever is leader at the
  time).
- **Observable**: Each cycle: one leader election. Mesh stable. No
  duplicate or "fork" leaders.
- **PASS**: 5 leader changes observed; for each, time-to-first-write
  on the new primary is < 5 s. `final_dl == baseline_dl`.
  `extra_rows == 0`.
- **FAIL**: A cycle produces two simultaneous leaders (split-brain),
  OR `data_loss_total` does not settle, OR election doesn't converge
  within 5 s on more than 1 cycle.
- **Recovery**: §0.6 reset.
- **Duration**: ~7 min.
- **Priority**: P0

### CTRL-04 — SIGHUP password rotation across all peers under load

- **REQ / AC**: REQ-COORD-05 / AC-COORD-05a, AC-COORD-05b
- **Already covered?**: NO.
- **Precondition**: Fresh rig.
- **Action**:
  1. Generate new password (in shell):
     `NEW_PW=$(./bin/pgman-proxy cluster-secret-gen 2>/dev/null | awk '/password:/{print $2}')`.
     Workaround for the rig: choose a deterministic 32-byte base32
     string for reproducibility.
  2. Update env in each container. Since env can't be changed
     mid-container, this requires `docker exec` to write into a file
     SecretRef target, OR rebuilding the env via process-compose
     restart. Simplest: bake a file SecretRef into the rig (qa
     prerequisite) and `docker exec ... sh -c 'echo "$NEW_PW" >
     /etc/secret/cluster-password'`.
  3. SIGHUP each container in turn:
     `docker exec pgman-pc-node-a kill -HUP 1` (PID 1 is pgman-proxy).
- **Observable**:
  - On each peer: `embedded_nats.reload_applied{password_rotated=true,
    password_old_prefix=..., password_new_prefix=...}`.
  - `P-MESH` stays ≥ 1 on every peer throughout the rotation
    (quorum never lost).
  - Subsequent `embedded_nats.route_up` events carry the NEW
    `password_prefix`.
- **PASS**: All three peers log `reload_applied{password_rotated=true}`
  with consistent old/new prefixes. P-MESH stays ≥ 1 on each peer.
  After rotation completes, every new `route_up` event carries the new
  password_prefix.
- **FAIL**: P-MESH drops to 0 on any peer at any point. Or
  `reload_applied` missing `password_rotated=true` despite the secret
  changing. Or audit log doesn't carry the prefix.
- **Recovery**: §0.6 reset.
- **Duration**: ~4 min.
- **Priority**: P0

### CTRL-05 — SIGHUP attempted with non-allow-list key changed

- **REQ / AC**: REQ-COORD-04 / AC-COORD-04b
- **Already covered?**: NO.
- **Precondition**: Fresh rig.
- **Action**: Edit a non-allow-list value (e.g., `cluster.cluster_name`)
  in the config source SecretRef target. SIGHUP one peer.
- **Observable**: `embedded_nats.reload_applied{skipped_keys=
  ["cluster.cluster_name"], skipped_reason=...}`. The in-memory
  config does NOT advance the cluster_name (verify by inspecting
  `/v1/status`).
- **PASS**: Event emitted with the skipped key listed; in-memory
  config unchanged; metric
  `pgman_proxy_embedded_nats_sighup_reload_outcomes_total{result="partial_skipped"}`
  increments by 1.
- **FAIL**: Silent acceptance of the change (no skipped_keys
  warning). Or in-memory config advances the non-reloadable key. Or
  embedded server restarts.
- **Recovery**: §0.6 reset.
- **Duration**: ~2 min.
- **Priority**: P1

---

## §8 Resource pressure (RES-*)

### RES-01 — Disk-full on JetStream storage path

- **REQ / AC**: REQ-DL-04 / AC-DL-04a, AC-DL-04b, AC-DL-04c
- **Already covered?**: NO — has never fired in the rig.
- **Precondition**: Fresh rig. Identify a non-leader peer to target.
- **Action**: Fill the JetStream dir on a single peer. The container's
  `jetstream_dir` is `/var/lib/postgresql/jetstream` (inside the
  pgdata volume). Fill it via:
  ```sh
  docker exec pgman-pc-node-c sh -c \
    'fallocate -l $(df -B1 /var/lib/postgresql/jetstream | awk "NR==2 {print \$4}") \
     /var/lib/postgresql/jetstream/.filler'
  ```
  (Or use `dd if=/dev/zero ... bs=1M count=...`.) Trigger a JetStream
  write by, e.g., sending SIGHUP with a password rotation that needs
  to persist.
- **Observable**:
  - `embedded_nats.storage_degraded{kind="disk_full"}` event on the
    affected peer.
  - `pgman_proxy_embedded_nats_storage_degraded{kind="disk_full"}` gauge → 1.
  - Self-fence: leadership-state → `not-leader` (if it was leader,
    election triggers); local-PG writes refused.
- **PASS**: storage_degraded event fires within 30 s. Affected peer
  self-fences. Workload reroutes via libpq multi-host fallthrough;
  `final_dl == baseline_dl`.
- **FAIL**: No storage_degraded event despite disk full. Or peer
  continues to serve writes after the event.
- **Recovery**: Delete filler; §0.6 reset.
- **Duration**: ~5 min.
- **Priority**: P0

### RES-02 — File-descriptor exhaustion (open many connections)

- **REQ / AC**: REQ-AVAIL-02 / —
- **Already covered?**: NO.
- **Precondition**: Fresh rig.
- **Action**: Drive 5000 idle pgbench connections from the host
  against one proxy peer:
  ```sh
  for i in $(seq 1 5000); do
    psql 'host=127.0.0.1 port=16432 ...' -c 'SELECT pg_sleep(60);' &
  done
  ```
  (Use `ulimit -n 8192` first.) While saturated, kill the primary.
- **Observable**: Whether the proxy honours `max_open_files` gracefully
  or crashes; whether failover still happens.
- **PASS**: Failover completes within REQ-AVAIL-01 budget even under
  load. Excess connections fail-fast with a structured error.
- **FAIL**: Proxy panics or hangs on the connection storm.
- **Recovery**: Kill the host `psql` storm; §0.6 reset.
- **Duration**: ~5 min.
- **Priority**: P2

---

## §9 Boot / fail-closed validation (BOOT-*)

These are quick sanity scenarios; mostly covered already by integration
tests but worth re-checking against the deployment rig.

### BOOT-01 — Legacy `nats.url` rejected at validation

- **REQ / AC**: REQ-COORD-01 / AC-COORD-01b
- **Already covered?**: Yes in integration tests; re-verify in the rig.
- **Action**: Add `-e PGMAN_PROXY_NATS_URL=nats://stale:4222` to the
  node-a command. Start.
- **PASS**: Container exits with code 78; log says nats.url no longer
  supported. **FAIL**: Container starts.
- **Priority**: P0

### BOOT-02 — Replicas factor derivation visible

- **REQ / AC**: REQ-COORD-02 / AC-COORD-02a
- **Action**: With current rig (declared_size=3), curl /metrics on each
  peer and verify `pgman_proxy_embedded_nats_replicas_factor 3`.
- **PASS**: 3 on each peer. **FAIL**: any other value.
- **Priority**: P0

### BOOT-03 — Wrong cluster password on one peer

- **REQ / AC**: REQ-COORD-07 / AC-COORD-07a
- **Action**: Restart node-c with `PGMAN_CLUSTER_PASSWORD=wrong-value`.
- **PASS**: node-c's NATS server starts; route handshake to node-a/b
  fails with `cluster_route.auth_failed{kind="invalid_credential"}`;
  `pgman_proxy_embedded_nats_route_auth_failures_total{kind="invalid_credential"}`
  increments on node-a / node-b. node-c does NOT mesh.
- **FAIL**: Silent fall-back to unauthenticated; or route succeeds
  despite wrong password.
- **Priority**: P0

### BOOT-04 — Cluster-name mismatch

- **REQ / AC**: REQ-COORD-07 / AC-COORD-07a
- **Action**: Restart node-c with `PGMAN_PROXY_CLUSTER_NAME=other-cluster`.
- **PASS**: `cluster_route.auth_failed{kind="cluster_name_mismatch"}`
  on node-a / node-b. node-c does NOT mesh.
- **Priority**: P0

### BOOT-05 — Non-loopback routes_listen without TLS and without explicit_ack

- **REQ / AC**: REQ-CON-05 / AC-COORD-06c
- **Action**: Remove `PGMAN_PROXY_CLUSTER_TLS_PLAINTEXT_EXPLICIT_ACK=true`
  from node-a. Restart.
- **PASS**: Exit code 78 (CONFIG) with FR-010b error.
- **Priority**: P0

### BOOT-06 — Port collision

- **REQ / AC**: REQ-COORD-06 / AC-COORD-06a
- **Action**: Pre-bind :6222 inside the container, then start
  pgman-proxy.
- **PASS**: Exit 78 naming the port and (if determinable) the
  competing process.
- **Priority**: P1

---

## §10 Observability shape (OBS-*)

### OBS-01 — All required structured-log events fire in a 3-peer healthy run

- **REQ / AC**: Constitution V; observability.md events list / —
- **Action**: Boot fresh rig, let it run 5 min, grep logs for each of:
  `embedded_nats.server_started`, `…server_ready`, `…route_up`,
  `cluster_route.auth_failed` (should be absent), `…reload_applied`
  (absent unless SIGHUP'd).
- **PASS**: server_started + server_ready + route_up (≥ 4 across 3
  peers, since each peer accepts inbound + opens outbound) all present
  on every peer.
- **FAIL**: Any event missing on any peer.
- **Priority**: P1

### OBS-02 — route_up carries peer_node_id and password_prefix

- **REQ / AC**: REQ-AUDIT-03 / AC-COORD-05b
- **Action**: Grep `route_up` events. Each MUST have non-empty
  `peer_node_id` (one of node-a/b/c) AND non-empty 8-char
  `password_prefix`.
- **PASS**: Every event passes. **FAIL**: Any event missing or empty.
- **Priority**: P1

### OBS-03 — `pgman_proxy_embedded_nats_lifecycle_events_total` counters match log counts

- **REQ / AC**: REQ-COORD-* metrics / —
- **Action**: After OBS-01: count
  `event=server_ready` log lines on a peer and compare to the
  Prometheus counter value with the same label.
- **PASS**: Equal.
- **Priority**: P2

---

## §11 Long soak (SOAK-*)

### SOAK-01 — 10-minute sustained-load periodic-kill soak

- **REQ / AC**: REQ-DL-01, REQ-HEAL-05 / AC-DL-01a, AC-HEAL-05a
- **Already covered?**: NO — chaos loop scenarios are short.
- **Precondition**: Fresh rig.
- **Action**:
  - Workload at 50 RPS throughout.
  - Every 90 s, kill the current primary (`docker kill -s KILL`),
    let it restart, wait 30 s for re-mesh.
  - 6 kills total over 10 min.
- **Observable**: Counters at minute boundaries, leader churn pattern,
  any orphan slot accumulation, JS storage growth.
- **PASS**:
  - At end: `final_dl == baseline_dl`, `extra_rows == 0`.
  - PG primary key count == `writes_ok` count on every peer.
  - No replication-slot leak (count == 2 on the primary at end).
  - JS storage growth bounded (< 10 MB total per peer).
- **FAIL**: Any of the above breaks.
- **Recovery**: §0.6 reset.
- **Duration**: 12 min (includes settle).
- **Priority**: P0

### SOAK-02 — 30-minute steady-state no-chaos run (memory/CPU baseline assertion)

- **REQ / AC**: SC-005 from spec (memory ≤ 60 MB RSS p95, CPU < 0.5%) / —
- **Already covered?**: Not in chaos rig.
- **Precondition**: Fresh rig.
- **Action**: No chaos. Workload at 5 RPS for 30 min. Sample
  `docker stats pgman-pc-node-a` every 10 s.
- **PASS**: Embedded-NATS portion of memory (proxy total minus
  pg-manager baseline) stays under 60 MB RSS p95. CPU p95 < 0.5% of a
  core when idle (chaos workload at 5 RPS is near idle).
- **FAIL**: Memory or CPU exceeds caps.
- **Priority**: P2

### SOAK-03 — 4-hour soak (overnight pre-ship gate)

- **REQ / AC**: REQ-DL-01 / —
- **Action**: Run SOAK-01 pattern for 4 hours (≈ 160 kills).
- **PASS**: Same as SOAK-01.
- **Priority**: P1 (run once per release candidate)

---

## §12 Cross-references

- COVERAGE-REQUIREMENTS.md (this directory) — REQ/AC mapping.
- `process-compose.yaml` — chaos rig.
- `cmd/chaos-workload/main.go` — workload internals + counter
  semantics.
- `internal/runtime/start.go::ensureStandbySignalIfInitialized` —
  the f1d67d7 fix point exercised by REC-01.
- `../pg-manager/types.go` — `Policy`, `AutoDemotePolicy`,
  `AutoRebootstrapPolicy`, `OnSyncStandbyLoss`.
- `specs/002-embedded-nats-cluster/contracts/{observability,lifecycle,
  cluster-credentials,config}.md` — events, exit codes, SIGHUP
  semantics, fail-closed validation rules.

## §13 Scenarios this plan deliberately omits (deferred)

- mTLS-as-identity tests (out of scope per RD-001a).
- Multi-region latency-injection tests (R=3 cap makes them informational).
- Kubernetes-specific chaos (kubelet death, pod eviction) — out of
  project scope.
- PITR / restore correctness — feature 003 (not yet specified).
- Multi-cluster mixing (cluster_name collision across two unrelated
  clusters) — handled by `cluster_route.auth_failed{kind="cluster_name_mismatch"}`
  in BOOT-04; further multi-cluster coexistence is out of scope.
