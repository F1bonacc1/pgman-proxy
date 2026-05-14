# FIXES — pgman-proxy v1 (feature 002 embedded NATS cluster)

**Audience**: SW Engineer (implements), Architect / on-call SRE (reviews
ship gate).
**Status**: Design only. No code lands from this document; the user
selects what to merge.
**Inputs**: `TEST-RESULTS.md` §0–§3 findings, `COVERAGE-REQUIREMENTS.md`
§4 GO/NO-GO bar, `TEST-PLAN.md` scenarios, `contracts/observability.md`.

Each entry traces the defect to specific source lines, proposes the
smallest viable change, names what other surfaces could ripple, and
identifies the regression-locking test.

---

## FIX-01 — Wire the embedded-NATS gauges (`_up`, `_routes_meshed`)

- **Tied to**: REQ-COORD-03 (MUST-PASS) / AC-COORD-03a; REQ-COORD-02
  indirectly via the observability contract.
- **Symptom** (TEST-RESULTS §0 Top-3 #1, F-1): `pgman_proxy_embedded_nats_up`
  and `pgman_proxy_embedded_nats_routes_meshed` report 0 on every peer's
  `/metrics` despite a healthy 3-route mesh, and the `coordination_events_total`
  counter is non-zero (so the NATS substrate is genuinely working).

### Root-cause analysis

The gauges are **registered** but **never set anywhere in the
codebase**:

- `internal/obs/metrics.go:157-188` — `EmbeddedNATSUp`,
  `EmbeddedNATSRoutesMeshed`, `EmbeddedNATSReplicasFactor`,
  `EmbeddedNATSStorageBytes`, `EmbeddedNATSStorageDegraded`,
  `EmbeddedNATSLifecycleEventsTotal`, `EmbeddedNATSRouteAuthFailures`,
  `EmbeddedNATSReloadOutcomes` are all constructed and registered.
- `grep -rn "EmbeddedNATSUp\|EmbeddedNATSRoutesMeshed\|EmbeddedNATSReplicasFactor"
  --include="*.go"` returns ONLY the definitions in `metrics.go`. There
  are zero call sites for `.Set(...)` / `.Inc(...)` / `.WithLabelValues(...).Set(...)`.

In other words: every embedded-NATS gauge and counter that the contract
documents is dead. The control-plane Status response (which uses
`embedded.Server.Snapshot()` in `internal/embedded/snapshot.go:33-47`)
populates `RoutesMeshed` from `s.srv.NumRoutes()` — but the equivalent
write into the Prometheus gauge does not exist.

The two correct write sites are:

1. **`_up`**: flip to 1 inside `embedded.Server.Start` after the ready
   signal closes (`internal/embedded/server.go:110-122`); flip back to 0
   inside `embedded.Server.Shutdown` (`server.go:188-223`).
2. **`_routes_meshed`**: set on every poll inside
   `RouteWatcher.tick()` at `internal/embedded/route_watcher.go:74-115`,
   using `len(current)` (or equivalently `w.srv.NumRoutes()`). This is
   already the canonical "current mesh count" surface in the code.

The `embedded` package cannot import `internal/obs` (circular import
documented at `server.go:30-31`), so the gauge updates must arrive via a
callback the host supplies — same pattern already used for the
`EventEmitter` (`server.go:31`) and `StorageMonitor` fence callback
(`storage.go:62-65`).

### Fix design

1. Add a `MetricsSink interface` (or function-typed field) to the
   `embedded` package, e.g.:

   ```
   type MetricsSink struct {
       SetUp          func(float64)
       SetRoutesMeshed func(float64)
       // (additional setters for FIX-03 and lifecycle counter)
   }
   ```

   Plumb a `MetricsSink` parameter through `embedded.NewServer`
   (`server.go:51`) — keep `nil` as legal for tests, just like the
   `emit` callback.

2. In `embedded.Server.Start` (`server.go:110-122`), after the ready
   signal closes:

   ```
   if s.metrics != nil && s.metrics.SetUp != nil { s.metrics.SetUp(1) }
   ```

   In `Shutdown` (`server.go:208-220`), in both the clean-shutdown and
   forced-shutdown branches:

   ```
   if s.metrics != nil && s.metrics.SetUp != nil { s.metrics.SetUp(0) }
   ```

3. Extend `NewRouteWatcher` (`route_watcher.go:34`) to take the same
   `MetricsSink`. At the end of `tick()` (`route_watcher.go:115`) call
   `m.SetRoutesMeshed(float64(len(current)))`. This piggybacks on the
   existing 2 s poll cadence — no new goroutine, no new lock.

4. At the call site `internal/runtime/start.go:90-94` (boot embedded
   NATS) and `:203` (start the RouteWatcher), construct the
   `MetricsSink` from `res.Metrics.EmbeddedNATSUp.Set` and
   `res.Metrics.EmbeddedNATSRoutesMeshed.Set` and pass it through.

### Blast radius

- Touches `internal/embedded/server.go`, `internal/embedded/route_watcher.go`,
  `internal/runtime/start.go`. Three files, all internal.
- Adds one new optional argument to two constructors. Tests that
  construct `embedded.NewServer` without an obs layer can keep passing
  `nil`.
- No metric label changes, no new label cardinality.
- `embedded.Server.Snapshot()` (used by Status LCM) is independent of
  this fix — Snapshot already reads `NumRoutes` directly, so the Status
  endpoint becomes self-consistent with `/metrics` for free.

### Test to lock the regression

Reuse `BOOT-09` (3-peer mesh converge). New assertion:

```
for port in 19090 19091 19092; do
  up=$(curl -s "http://127.0.0.1:$port/metrics" \
         | awk '/^pgman_proxy_embedded_nats_up /{print $NF}')
  meshed=$(curl -s "http://127.0.0.1:$port/metrics" \
             | awk '/^pgman_proxy_embedded_nats_routes_meshed /{print $NF}')
  [ "$up" = "1" ]      || die "$port: _up=$up want 1"
  [ "$meshed" = "2" ]  || die "$port: _routes_meshed=$meshed want 2"
done
```

Add as `BOOT-09b` in TEST-PLAN. Existing `validation/scripts/lib.sh`
already has a `routes_meshed` probe (TEST-RESULTS §4) — wire it into
the smoke-test assertion path. Should also fire on `NET-01` after
recovery (mesh count returns to 2 on every peer).

### Priority

**P0** — direct MUST-PASS violation per COVERAGE §4
(`AC-COORD-03a`, `AC-AVAIL-04b` relies on the same gauge).

---

## FIX-02 — Wire and populate the `_replicas_factor` gauge

- **Tied to**: REQ-COORD-02 (MUST-PASS) / AC-COORD-02a, AC-COORD-02c.
- **Symptom** (TEST-RESULTS §0 Top-3 #1, F-2): `pgman_proxy_embedded_nats_replicas_factor`
  is **absent** from `/metrics`. (Technically it IS registered — see
  below — but never had a sample written, so Prometheus' text exposition
  omits the line entirely. The contract treats absent vs `0` as equally
  bad: there's nothing to assert against.)

### Root-cause analysis

- `internal/obs/metrics.go:165-168` registers the GaugeVec correctly
  with label `overridden`.
- The replica decision is computed in `internal/runtime/start.go:143-150`
  via `embedded.DecideReplicas(cfg.Cluster.DeclaredSize, cfg.Cluster.ReplicationFactorOverride)`
  and a warning is logged.
- The decision is then handed to `embedded.PreCreateClusterKV`
  (`start.go:151`) and that's where the value's role ends. There is no
  `res.Metrics.EmbeddedNATSReplicasFactor.WithLabelValues(...).Set(...)`
  call.
- Same story for `embedded.Server.Snapshot()`
  (`internal/embedded/snapshot.go:16,33-47`): `ReplicasFactor int` is a
  declared field but `Snapshot()` returns a zero-value `StatusSnapshot`
  for it. So `Status.cluster.embedded_nats.replicas_factor` also lies.

### Fix design

Two surfaces, one logical state, no need to recompute:

1. **Metric**: at `internal/runtime/start.go:151` (right after
   `PreCreateClusterKV` succeeds), call:

   ```
   overridden := "false"
   if replicaDecision.Overridden() { overridden = "true" }
   res.Metrics.EmbeddedNATSReplicasFactor.
       WithLabelValues(overridden).
       Set(float64(replicaDecision.Effective()))
   ```

   This is a once-at-startup write — the value is immutable for a given
   process. No need to thread anything through the embedded package.

2. **Snapshot**: store `replicaDecision.Effective()` and
   `replicaDecision.Overridden()` on the `embedded.Server` struct at
   `NewServer` time (extend `OptionsInput` in
   `internal/embedded/options.go:25` with `ReplicasFactor int`,
   `ReplicasOverridden bool`; pass them at `start.go:595-611`), then
   surface them in `Snapshot()` at `internal/embedded/snapshot.go:37-47`:

   ```
   snap.ReplicasFactor     = s.replicasFactor
   snap.ReplicasOverridden = s.replicasOverridden
   ```

   (Trivial: just struct-field plumbing.)

### Blast radius

- Touches `internal/runtime/start.go`, `internal/embedded/options.go`,
  `internal/embedded/server.go`, `internal/embedded/snapshot.go`.
- No callers outside the proxy depend on these structs.
- Existing test `tests/integration/replica_factor_test.go` is the
  natural home for an assertion extension.

### Test to lock the regression

Add `BOOT-07` (replicas-factor derivation) per TEST-PLAN. Spin a 1-peer,
2-peer, and 3-peer cluster; assert from `/metrics`:

```
pgman_proxy_embedded_nats_replicas_factor{overridden="false"} == declared_size when 1≤n≤3
pgman_proxy_embedded_nats_replicas_factor{overridden="false"} == 3            when n≥3
```

And `BOOT-08` (override loud-log): boot with
`PGMAN_PROXY_CLUSTER_REPLICATION_FACTOR_OVERRIDE=2` on a 3-peer cluster,
assert label `overridden="true"` and value `2`, and assert the warning
log line is emitted on each peer.

`tests/integration/replica_factor_test.go` covers PreCreateClusterKV
already; extend it with a `/metrics` scrape and a `Status` probe.

### Priority

**P0** — direct MUST-PASS violation (`AC-COORD-02a`).

---

## FIX-03 — Populate `embedded_nats.route_up.password_prefix`

- **Tied to**: REQ-AUDIT-03 / AC-COORD-05b / AC-AUDIT-03a (MUST-PASS in
  strict reading of §4; SHOULD-PASS in lenient reading).
- **Symptom** (TEST-RESULTS §0 Top-3 #2, F-3): every emitted
  `embedded_nats.route_up` event has `"password_prefix":""`.

### Root-cause analysis

Direct, in-source TODO:

- `internal/embedded/route_watcher.go:100` literally has
  `"password_prefix": "", // populated by future pp work`. The field is
  emitted as empty string by design — the watcher does not know the
  cluster password.
- The proxy DOES know it: `runtime/start.go:573-576` resolves the
  password via `resolveClusterPassword` and constructs an
  `embedded.ClusterCredential` at `start.go:582-588`.
- The 8-char prefix helper already exists at
  `internal/embedded/reload.go:163-168` (`passwordPrefix`) and is used by
  `ComputeDiff` for the rotation-audit case (`reload.go:67-72`).

So we have a known prefix on the host side and a known empty hole on
the emit side. The fix is plumbing.

### Fix design

1. Extend `embedded.Server` to hold the current 8-char password prefix:

   ```
   type Server struct {
       ...
       passwordPrefix atomic.Value // string
   }
   ```

   Set it in `NewServer` from the resolved credential. Update it on
   SIGHUP-driven rotation (`internal/runtime/reload.go` — after a
   successful `ComputeDiff` whose `PasswordRotated == true`, call
   `s.SetPasswordPrefix(diff.PasswordNewPrefix)`).

2. In `route_watcher.go:94-101`, replace the hardcoded `""` with:

   ```
   "password_prefix": w.srv.PasswordPrefix(),
   ```

   `PasswordPrefix()` returns the atomic load.

3. As a consequence, the `EmitReloadApplied` helper
   (`server.go:242-259`) already carries `password_old_prefix` /
   `password_new_prefix` — no change needed there.

### Blast radius

- Touches `internal/embedded/server.go` (one accessor, one setter, one
  field), `internal/embedded/route_watcher.go` (one line), and
  `internal/runtime/reload.go` (call the setter when a rotation lands).
- Does NOT touch metric labels — `password_prefix` is a log field only
  (REQ-AUDIT-03), never a label (the cardinality budget in
  `contracts/observability.md` § Prometheus metrics forbids it).
- Single peer mode (no cluster password) returns empty string; that's
  expected and the validator should special-case `declared_size == 1`.

### Test to lock the regression

Add `SEC-03` (password rotation) per TEST-PLAN. Steady-state assertion:

```
docker logs pgman-pc-node-b 2>&1 \
  | jq -c 'select(.msg=="embedded_nats.route_up")' \
  | jq -r '.password_prefix' \
  | while read p; do
      [ "${#p}" = "8" ] || die "route_up emitted password_prefix=$p (len=${#p}, want 8)"
    done
```

That assertion fires for both the boot mesh and post-SIGHUP rotation;
plus assert that after rotation `password_old_prefix` and
`password_new_prefix` on `embedded_nats.reload_applied` differ. (The
rotation path is blocked separately by FIX-05 — file-based SecretRef.)

### Priority

**P0** — MUST-PASS in the strict reading of §4 (`AC-COORD-05b` says
"non-empty 8 chars"). Even under the lenient reading, the audit trail
is the only forensic surface for credential rotation, so the cost of
shipping with this empty is permanent forensic blindness for a
rotation. The fix is minimal (~30 LOC). No reason to defer.

---

## FIX-04 — Cut failover RTO below the 5 s p99 budget

- **Tied to**: REQ-AVAIL-01 / AC-AVAIL-01a (MUST-PASS).
- **Symptom** (TEST-RESULTS §0 Top-3 #3, F-4): observed RTO 8–16 s wall
  clock from `docker kill` to "new primary serving writes" (DI-04: 12 s;
  TX-01: 8 s; NET-04: 6–8 s). Sample-of-3, but every observation
  exceeds the 5 s p99 budget.

### Root-cause analysis

The long pole is **stale-leader eviction in the NATS leadership
adapter**, not pg-manager's failover machinery and not
`Policy.LivenessInterval`. Trace:

- `pg-manager/adapters/nats/leadership.go:146` —
  `leaseTTL: 5 * time.Second` is the renewal tick interval. The leader
  renews every 5 s; survivors observe a non-self leader at the same
  cadence.
- `pg-manager/adapters/nats/leadership.go:150` — `staleThreshold: 3`.
  A survivor must observe the **same `(value, rev)` pair on three
  consecutive ticks** before deleting the leader key
  (`leadership.go:319-323`). Worst case: 3 × 5 s = **15 s** just for
  the eviction.
- After eviction, the next campaign winner takes the lease, transitions
  to Promoting, runs pg_promote (~1–2 s), and posts the leadership
  change. The verdict script
  (`validation/scripts/DI-04.sh:42-50`) polls once per second and counts
  `current_primary()` returning a non-old letter — so its quantisation
  alone adds up to 1 s of measurement noise.

`Policy.LivenessInterval = 2s` (default since commit 42984f7) and
`LivenessFailures = 3` give a 6 s detection floor for in-PG liveness,
but that path runs the reconciler tick — NOT the NATS-adapter eviction.
The two clocks are independent; the SLOWER one wins. Today the slower
one is the 15-s NATS eviction.

`pgman-proxy/internal/cluster/cluster.go:110-113` calls
`natsadapter.NewLeadership(..., WithLogger(logger))` — note: NO
`WithLeaseTTL` and NO `WithStaleThreshold` are passed. So we inherit
the upstream production defaults (5 s × 3). The chaos rig has the
two options it needs to tighten this but the proxy currently doesn't
even surface them as a knob.

`pg-manager/adapters/nats/leadership.go:97-114` documents both options
as the explicit tuning knobs for this exact problem ("Milestone 008 /
B-006: this knob exists so test scenarios can drive eviction quickly").

### Fix design

Two-part fix; both parts cheap.

**Part A — surface the knobs in proxy config (no behaviour change at
default):**

1. Add `Policy.LeaseTTL time.Duration` and
   `Policy.LeaseStaleThreshold int` (or, since these are NATS-substrate
   knobs, put them under a new `Cluster.Leadership` subblock in
   `internal/config/config.go`) with env aliases
   `PGMAN_PROXY_POLICY_LEASE_TTL` and
   `PGMAN_PROXY_POLICY_LEASE_STALE_THRESHOLD` in
   `internal/config/loader.go` next to the existing
   `PGMAN_PROXY_POLICY_LIVENESS_INTERVAL` (line 131).
2. Plumb through to `cluster.BuildHandles` (signature change) so it can
   pass `natsadapter.WithLeaseTTL(...)` and
   `natsadapter.WithStaleThreshold(...)` at `cluster.go:110-113`.
3. Defaults: keep the 5 s × 3 production defaults at the
   `config.Defaults()` level (`config.go:240`) so production posture
   doesn't change without an opt-in.

**Part B — set chaos-rig defaults that fit the 5 s p99 budget:**

In `process-compose.yaml` add env vars to each of the three nodes
(lines ~190–194, ~247–251, ~306–310):

```
-e PGMAN_PROXY_POLICY_LEASE_TTL=1s \
-e PGMAN_PROXY_POLICY_LEASE_STALE_THRESHOLD=2 \
```

That gives a 2-second eviction worst-case + ~1.5 s pg_promote + ~1 s
verdict-polling = **~4.5 s** new-primary visibility. Comfortably under
the 5 s budget for the wall-clock signal, and tight against the
`leadership.changed` event signal (which fires immediately after CAS
delete).

**Open question to coordinate with the architect** (referenced in
COVERAGE §5 Q-3): the 5 s p99 budget was specified in
`REQ-AVAIL-01` against the **`leadership.changed` event** per 001
SC-002 wording, not against the first successful client write. If the
architect endorses the COVERAGE Q-1 proposal (measure against first
write), then the Part-A knobs let chaos and production set different
thresholds; if they require event-only-budget, Part B is sufficient at
the rig level and the production default is also re-tunable.

### Blast radius

- `internal/cluster/cluster.go:109` signature changes (add one or two
  parameters). All call sites are inside `internal/runtime/start.go`.
- `internal/config/config.go` adds two fields with documented defaults.
- `internal/config/loader.go` adds two env-var bindings.
- `process-compose.yaml` adds two env vars per peer.
- **Does NOT** modify pg-manager — the knobs already exist upstream
  (`WithLeaseTTL`, `WithStaleThreshold` at `leadership.go:99-114`).
- **Does NOT** change production posture — Part B is rig-only.
- Constitution: keep `WALRetentionDays`, `BaseBackupRetention`, etc.
  untouched; this is leadership-adapter tuning, not pg lifecycle.

### Test to lock the regression

Add `SLO-01` per TEST-PLAN: 50-iteration loop. Each iteration:

1. Capture `current_primary()` letter as `P0`.
2. Record `T_kill` = `date -Ins` at the moment of
   `docker kill -s KILL pgman-pc-node-$P0`.
3. Poll `current_primary()` at **100 ms granularity** (not 1 s as DI-04
   does) until it returns a non-`P0`, non-`MULTIPLE:` letter; record
   `T_promoted` = `date -Ins`.
4. Emit `T_promoted - T_kill` in ms.
5. Restart killed peer; wait `wait_for_mesh`.

Compute p50 / p95 / p99 from the 50 samples; assert p99 ≤ 5000 ms.
Optionally also capture `leadership.changed` event timestamps from
`docker logs` for the dual-clock breakdown (see COVERAGE §5 Q-3).

### Priority

**P0** — MUST-PASS in COVERAGE §4. The lease-tuning Part B alone (no
code, just rig config) might pass the bar; Part A is then load-bearing
for the production knob and for the chaos rig to look like production.

---

## FIX-05 — Process-compose rig: file-based SecretRef + CAP_NET_ADMIN + slot-WAL knob

- **Tied to**: REQ-COORD-05 (SHOULD-PASS), REQ-DL-05 / NET-02 / NET-05 /
  NET-06 (MUST-PASS for partition scenarios), REQ-HEAL-02 / REPL-03 /
  REPL-05 / REC-04 (SHOULD-PASS).
- **Symptom** (TEST-RESULTS §1 NET-05 SKIPPED-blocked; CTRL-04
  SKIPPED-blocked; REC-04 INCONCLUSIVE):
  - Containers lack `--cap-add=NET_ADMIN`, so `iptables` inside the
    container returns "Permission denied" and all partition tests fail
    to even start.
  - The cluster password is supplied via env var; SIGHUP cannot pick up
    a new password from env (envs are immutable mid-container), so
    password rotation under load cannot be exercised.
  - `wal_keep_size = 128MB` + active replication slots make stale-WAL
    impossible to induce, so AutoRebootstrap scenarios are inconclusive.

### Root-cause analysis

All three are rig-config gaps; the proxy code already supports the
fixed posture:

1. **CAP_NET_ADMIN**: the integration Dockerfile already installs
   `iptables`/`iproute2`/`procps` (`tests/integration/Dockerfile:52-57`,
   landed in this validation round). What is missing is the run-time
   capability grant. `process-compose.yaml:159-195`, `:215-252`,
   `:273-311` (each peer's `docker run` invocation) does NOT pass
   `--cap-add=NET_ADMIN`.

2. **SecretRef file**: `internal/config/loader.go:74` already binds
   `PGMAN_PROXY_CLUSTER_PASSWORD_FILE`, and `start.go:651-661`
   (`resolveClusterPassword`) reads from a file path with newline
   trimming and SIGHUP-safe re-resolution. The rig
   (`process-compose.yaml:172-173`, `:229-230`, `:288-289`) instead uses
   `PGMAN_PROXY_CLUSTER_PASSWORD_ENV=PGMAN_CLUSTER_PASSWORD` with the
   value baked into the docker-run env.

3. **`max_slot_wal_keep_size`**: `process-compose.yaml:190`, `:247`,
   `:306` set `PGMAN_PROXY_POSTGRES_CONF_EXTRAS=wal_keep_size = 128MB`
   but never set `max_slot_wal_keep_size`. PG's default for that is `-1`
   (unlimited). With unlimited slot-pinned WAL, no amount of write load
   recycles the WAL the disconnected standby needs. The proxy code
   already passes `PGMAN_PROXY_POSTGRES_CONF_EXTRAS` through to PG via
   `postInitDBHook` at `internal/runtime/start.go:682-708` so this is
   pure rig-config work.

### Fix design

**5a. CAP_NET_ADMIN in `process-compose.yaml`**

Add `--cap-add=NET_ADMIN \` to the `docker run` invocation for each of
node-a, node-b, node-c. The exact lines:

- `process-compose.yaml:160` (node-a, after `--rm \`),
- `process-compose.yaml:216` (node-b),
- `process-compose.yaml:275` (node-c).

Three identical one-line additions. No proxy-code change.

**5b. File-based SecretRef + writable secret volume**

For each of the three nodes, replace lines 172-173 (and equivalents in
node-b and node-c) with:

```
-e PGMAN_PROXY_CLUSTER_PASSWORD_FILE=/etc/pgman/cluster-password \
-v pgman-pc-cluster-secret:/etc/pgman \
```

Then add a `bootstrap-secret` process to `process-compose.yaml` that runs
once before the nodes start, writes the initial password into the shared
named volume, and exits successfully (the named-volume mount makes the
file visible to all three peer containers). Sequence:

```
processes:
  bootstrap-secret:
    command: |
      docker volume create pgman-pc-cluster-secret >/dev/null
      docker run --rm -v pgman-pc-cluster-secret:/etc/pgman alpine \
        sh -c 'echo -n "process-compose-dev-cluster-secret" > /etc/pgman/cluster-password'
    is_daemon: false
    availability:
      restart: "no"
  node-a:
    depends_on:
      bootstrap-secret:
        condition: process_completed_successfully
      ...
```

Then a SEC-03 / CTRL-04 scenario rotates the password by re-writing the
file and SIGHUP-ing each peer:

```
docker run --rm -v pgman-pc-cluster-secret:/etc/pgman alpine \
  sh -c 'echo -n "$NEW_PW" > /etc/pgman/cluster-password'
for c in pgman-pc-node-{a,b,c}; do docker kill -s HUP "$c"; done
```

**5c. Tight `max_slot_wal_keep_size` for REPL/REC scenarios**

Augment `PGMAN_PROXY_POSTGRES_CONF_EXTRAS` on each node to include:

```
max_slot_wal_keep_size = 16MB
```

(Note: the env var carries multi-line content via the `\n` separator
inside YAML quoting — see line 189's pattern with `PGMAN_PROXY_POSTGRES_HBA_EXTRAS`.)

This is opt-in for the rig profile; production deployments tune this
per their WAL economics. The 16 MB ceiling forces slot invalidation
under modest write pressure, which is exactly what REPL-03 / REC-04
need to induce stale-WAL.

Alternative (not preferred): create a second compose profile
(`process-compose.repl.yaml`) with the tighter setting, so the
default rig doesn't change steady-state behaviour. The COVERAGE §5
Q-5 proposal recommends the second-profile approach; either is fine.

### Blast radius

- `process-compose.yaml` only (and a new helper compose for the
  bootstrap-secret).
- No proxy or pg-manager code change.
- 5b may surface latent bugs in the SIGHUP reload path under load — but
  that's the point of CTRL-04 / SEC-03.

### Test to lock the regression

- For 5a: rerun NET-05; assert `iptables -L` works inside the
  container and the partition scenario runs to completion.
- For 5b: run CTRL-04/SEC-03; assert
  `embedded_nats.reload_applied{password_rotated=true}` on every peer
  and `routes_meshed=2` maintained throughout (per `AC-COORD-05a`).
- For 5c: rerun REC-04; assert `auto_rebootstrap.detected` event fires
  within the expected window and `restart_lsn` jumps to a recent value
  (or that the slot is shown as `wal_status=lost` / invalid).

### Priority

- 5a: **P1** but elevated — gates 3 MUST-PASS scenarios (NET-01 *is*
  covered by docker-network-disconnect, but NET-04 / NET-05 / NET-06
  rely on intra-container iptables; without CAP_NET_ADMIN three
  MUST-PASS items remain unverifiable in chaos).
- 5b: **P1**.
- 5c: **P1** (REPL-* are SHOULD-PASS in §4; this only blocks SHOULD).

---

## FIX-06 — (Cosmetic) Inactive `pgmgr_<self>` slot after auto-demote reseed

- **Tied to**: TEST-RESULTS F-5 (no MUST-PASS REQ; tangential to
  REQ-HEAL-02).
- **Symptom**: After NET-04, `pgmgr_node_a` shows `active=f` with a
  stale `restart_lsn` while node-a streams successfully against node-c.

### Root-cause analysis

This is **pg-manager territory**, and Constitution §IV / REQ-CON-02
forbids the proxy from running slot-creation SQL. The mechanism:

- `pg-manager/replication/conninfo.go:319-334`
  (`RenderExpectedPrimaryConninfo`) renders the standby's
  `primary_conninfo` from `Topology.PeerDSNs[leader]` plus
  `application_name=<self>`. **It does NOT inject `primary_slot_name`**
  into the conninfo.
- Grep confirms: no occurrence of `primary_slot_name` anywhere in
  `pg-manager/{replication,reconciler}/*.go` (only in conninfo.go's docs
  and in the slot-naming helper). `pg_basebackup` is also invoked
  without `-S <slot>` / `--slot=<name>` (no occurrences of those flags
  in `pg-manager/reconciler/`).
- Net effect: standbys reconnect without referencing the per-peer slot;
  `ensurePeerSlots` (`pg-manager/reconciler/peer_slots.go:53-130`)
  creates the slot on the primary side for WAL retention but no one
  uses it. After auto-demote, the slot from the previous primary's
  reign carries an obsolete `restart_lsn`; the new primary
  (`ensurePeerSlots` post-promotion) hits `42710 duplicate_object`,
  classifies the slot as "existing", and never advances `restart_lsn`
  because no walreceiver is bound.

### Fix design

**This belongs in pg-manager, not pgman-proxy.** The two reasonable
upstream options (in increasing order of behaviour change):

A. **Drop-and-recreate on each promotion**: in `ensurePeerSlots`
   (`pg-manager/reconciler/peer_slots.go:79-104`), if the slot exists
   AND is inactive AND its `restart_lsn` is older than the current
   primary's most recent checkpoint, drop and recreate. Or:

B. **Bind the slot at the standby**: extend
   `RenderExpectedPrimaryConninfo` to append
   `&primary_slot_name=pgmgr_<self>` (or at least to write the slot
   reference into postgresql.auto.conf alongside primary_conninfo).
   That's the standard PG idiom and gives the slot a real consumer,
   making `restart_lsn` advance naturally. The trade-off is a new
   failure mode: if the slot is missing on the primary the walreceiver
   refuses to start until ensurePeerSlots catches up.

**Proxy action**: file a pg-manager ticket pointing at the trace above.
**Do not patch the proxy.** The Constitution gate (REQ-CON-02) would
reject any slot-manipulation SQL landing in this repo.

### Blast radius

- None on the proxy side. Upstream fix is bounded to pg-manager's
  reconciler/replication packages.

### Test to lock the regression

In pg-manager: an integration test that does promote → assert
`pg_replication_slots.active=t AND restart_lsn != initial_lsn` for the
target peer's slot within 30 s.

In pgman-proxy: passive check in any `KILL-*` / `NET-*` scenario —
extend `lib.sh` with `slot_active` probe and warn (not fail) when a
streaming standby has `active=f`. Keeps visibility without blocking
ship.

### Priority

**P2** — cosmetic. Streaming works without the slot; slot-less standby
is a documented pg-manager mode. AutoRebootstrap (REQ-HEAL-02) is the
safety net when WAL would otherwise be lost. Defer to a pg-manager
follow-up.

---

## FIX-07 — (Workload semantics, P1) Reconcile `extra_rows` with sync-block semantics

- **Tied to**: REQ-DL-01 strict reading (`AC-DL-01b: extras == 0`);
  TEST-RESULTS F-6.
- **Symptom**: chaos-workload `extra_rows` jumped 0 → 6 during QUOR-01
  (sync-block window, both standbys dead) and stayed at 6 across the
  rest of the run.

### Root-cause analysis

Hypothesis from TEST-RESULTS F-6 (well-supported, not yet confirmed
against the offending seqs): when both standbys were down and
`synchronous_commit=on` / `ANY 1 (...)` blocked the COMMIT, the
chaos-workload client-side `connect_timeout=1` killed the libpq
connection BEFORE the server-side COMMIT acked the sync wait. When the
standbys came back, the queued commit's heap rows were flushed and
became visible — the workload had no in-memory record of those seqs,
so the verifier marks them as "extras".

This is **correct PG behaviour** (lost ACK on a successful COMMIT) and
the workload's `extra_rows` semantics need to widen to absorb it. The
verifier sits at `cmd/chaos-workload/main.go` (per
`COVERAGE-REQUIREMENTS.md:18`).

### Fix design

- Option A (preferred): when a libpq write fails with
  `connection lost` (SQLSTATE 08006 / 08003 / connection-time-out),
  record the seq as **`lost_ack`** rather than `failed`. At verifier
  time, rows present in PG whose seq is in `lost_ack` are NOT counted
  as extras. This matches the existing distinct-unresolved semantics
  in CHANGELOG d239a6f.
- Option B: rename `extra_rows` to `unattributed_rows` and explicitly
  document the lost-ACK class as a non-failure.
- Either way: add a CLI knob `--tolerate-lost-acks=N` (default 0 for
  CI, override in chaos rig).

Before any rename: actually inspect the 6 offending seqs from the run.
`SELECT seq, payload, xmin FROM chaos_events WHERE writer_id LIKE '01KR%'
ORDER BY seq DESC LIMIT 20` and correlate with proxy slog around the
22:35:09–22:35:24 window in QUOR-01's docker logs. Confirm or refute the
hypothesis before changing semantics.

### Blast radius

- `cmd/chaos-workload/main.go` only.
- Risks: a too-aggressive lost-ACK absorber masks a real fake-success.
  Mitigated by keeping `data_loss_total` (distinct-unresolved) as the
  primary integrity signal — REQ-DL-01's load-bearing assertion is
  AC-DL-01a/c, not AC-DL-01b.

### Test to lock the regression

Re-run QUOR-01 after the fix; assert:

- `data_loss_total(t_end) == data_loss_total(t_start)` (unchanged
  guarantee).
- `extra_rows(t_end) == extra_rows(t_start)` (the new guarantee for
  this scenario).
- The 6 "lost-ACK" seqs appear in a new `lost_acks_total` counter
  whose value increases by exactly 6.

### Priority

**P1** — the cluster genuinely did NOT lose data in QUOR-01 (per F-6's
hypothesis). The counter mis-categorisation can pass under a lenient
reading of AC-DL-01b (the architect calls F-6 out as "workload
instrumentation gap, not REQ-DL-01 violation"). Confirm the hypothesis
before changing the verifier.

---

## §Summary table

| FIX | Priority | Files touched | Effort | Blocks REQ |
|---|---|---|---|---|
| FIX-01 — wire `_up` / `_routes_meshed` gauges | **P0** | `internal/embedded/server.go`, `route_watcher.go`, `internal/runtime/start.go` | S | REQ-COORD-03 (MUST-PASS) |
| FIX-02 — wire `_replicas_factor` gauge + Snapshot field | **P0** | `internal/runtime/start.go`, `internal/embedded/options.go`, `server.go`, `snapshot.go` | S | REQ-COORD-02 (MUST-PASS) |
| FIX-03 — populate `route_up.password_prefix` | **P0** | `internal/embedded/server.go`, `route_watcher.go`, `internal/runtime/reload.go` | S | REQ-AUDIT-03 / AC-COORD-05b |
| FIX-04 — cut failover RTO via `WithLeaseTTL`+`WithStaleThreshold` knobs | **P0** | `internal/cluster/cluster.go`, `internal/config/{config,loader}.go`, `process-compose.yaml` | M | REQ-AVAIL-01 (MUST-PASS) |
| FIX-05a — `--cap-add=NET_ADMIN` in rig | **P1** | `process-compose.yaml` | S | NET-02/05/06 unblock; REQ-DL-05 verifiability |
| FIX-05b — file-based SecretRef in rig | **P1** | `process-compose.yaml` (+ helper bootstrap process) | M | REQ-COORD-05 (SHOULD-PASS) verifiability |
| FIX-05c — `max_slot_wal_keep_size=16MB` in rig | **P1** | `process-compose.yaml` | S | REQ-HEAL-02 / REPL-03 (SHOULD-PASS) verifiability |
| FIX-06 — slot lifecycle on reseed (UPSTREAM pg-manager) | **P2** | none in pgman-proxy; pg-manager `replication/conninfo.go` and/or `reconciler/peer_slots.go` | M | none (cosmetic) |
| FIX-07 — chaos-workload `extra_rows` semantics | **P1** | `cmd/chaos-workload/main.go` | S | AC-DL-01b strict reading |

Effort scale: S = ≤ 0.5 day, M = 0.5–2 days, L = > 2 days.

---

## §Ship gate

Of the P0 fixes, **all four MUST land before the architect's
GO/NO-GO bar is met** (each maps directly onto a MUST-PASS row in
COVERAGE §4). Recommended order — driven by code dependence and by
which fix unblocks the most validation:

1. **FIX-04 (RTO via lease knobs)** — first, because:
   - The 3 observability fixes (FIX-01/02/03) can all be re-verified in
     ~5 minutes once they ship. The RTO fix needs a fresh 50-trial
     `SLO-01` run that takes 10–15 minutes to produce a p99 figure.
   - It's also the only one whose default value choice is up to the
     architect (the rig-vs-production split; see COVERAGE §5 Q-3). Get
     that decision in early so the architect can sign off on the knob
     value before the other fixes start landing.
2. **FIX-01 (`_up` + `_routes_meshed` gauges)** — second, because:
   - This is the smallest patch on the critical path (one new callback
     plumbed into two files).
   - Once the gauges are live, the existing chaos rig's `routes_meshed`
     probe (already in `lib.sh`) can re-confirm `AC-AVAIL-04b` and
     `AC-COORD-03a` automatically. Every subsequent fix benefits from
     having a working observability surface.
3. **FIX-02 (`_replicas_factor` gauge)** — third, alongside FIX-01.
   - Same idiom (single Set() call after replica decision), but
     mechanically independent. Can land in the same PR as FIX-01.
4. **FIX-03 (`route_up.password_prefix`)** — fourth.
   - Independent of FIX-01/02/04; deferable to the next PR if reviewer
     bandwidth is the constraint.
   - The deeper test (`SEC-03` rotation) requires FIX-05b's file-based
     SecretRef before it can fire under load; but the steady-state
     assertion (`AC-AUDIT-03a` — 8-char prefix on every emitted route_up)
     does NOT require FIX-05b.

After these four land, re-run the QA pilot (the 9 scenarios that
were attempted). Expected outcomes:

- `BOOT-09b` (new) PASS — gauges populated.
- `SLO-01` (new 50-trial harness) — p99 against the 5 s budget; if PASS,
  the GO/NO-GO bar's REQ-AVAIL-01 is met.
- `DI-04`, `TX-01`, `NET-01`, `NET-04`, `REC-01`, `QUOR-01` — re-run
  and confirm no regression introduced by the lease-TTL change.
- `route_up` slog assertion — every emitted event has an 8-char
  `password_prefix`.

Only then take the P1 rig fixes (FIX-05a/b/c) to unblock the
SHOULD-PASS scenarios (`NET-02/05/06`, `SEC-03`, `REPL-03`).
FIX-06 (slot lifecycle) and FIX-07 (workload semantics) can ship after
v1; neither blocks the architect's bar.

---

## Document control

- Cross-reference scenario IDs and REQ IDs are stable (see
  COVERAGE-REQUIREMENTS § Document control).
- Code-line citations are pinned to the commits visible at validation
  time: HEAD = 42984f7. Re-locate against `grep` if line numbers drift.
- This document does NOT decide what ships. It supplies the smallest
  change-set per defect and the order in which the P0 set unlocks the
  GO/NO-GO bar.
