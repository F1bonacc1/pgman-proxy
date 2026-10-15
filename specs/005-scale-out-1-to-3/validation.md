# Validation evidence: feature 005

Runs recorded while implementing, newest last. Each entry names the build, the command and the observed result. Baselines (pre-005) are in `docs/scale-out-1-to-3/plan.md` sections 1 and 6.

## 2026-10-14: research R-003 premise (T009)

**Setup**: integration image, PostgreSQL 17. `pg_basebackup -X stream -c fast -r 20M` from a 991 MB primary, killed with SIGKILL after 15 s.

| File | Partial clone (311 MB) | Complete clone |
|---|---|---|
| `PG_VERSION` | present | present |
| `backup_label` | present | n/a |
| `global/pg_control` | **absent** | present |
| `standby.signal` | absent | n/a |

## 2026-10-14: US1 (WI-A), branch `005-scale-out-1-to-3`, pg-manager v0.6.1 sibling

`PGMAN_SCALEOUT_IMAGE=pgman-proxy-scaleout:dev go test -count=1 -tags=scaleout -run 'TestSingleNode|TestSemver' -v ./tests/scaleout/`

```text
--- PASS: TestSemverAtLeast (0.00s)
    single_node_test.go:27: first commit 78ms after primary
    single_node_test.go:45: writable 8.956s after restart
--- PASS: TestSingleNodeFreshWritesAndRestart (18.19s)
--- PASS: TestSingleNodeStaleSyncNamesUpgrade (33.39s)
```

S5b took the pre-015 branch:
- the write on the stale value failed;
- the documented `ALTER SYSTEM` step made the node writable.

This confirms research R-002 end to end.

**Rig** (`IMG=pgman-proxy-scaleout:dev`, `docs/scale-out-1-to-3/rig/scenarios.sh`):

| Scenario | Baseline (v0.6.1, pre-005) | This build |
|---|---|---|
| `single-writes` | `BLOCKED (>10s, check wait_event=SyncRep)` | `primary after: 6s`, `synchronous_standby_names=[]`, `W_OK` |
| `golden` | load hung without the rig's ALTER SYSTEM workaround | `rows=1000000` (the rig still applies its workaround, which is harmless here) |
| `single-restart` | `primary after: never` (`missing_quorum_snapshot`) | `primary after: 7s`, `W_OK` |

## 2026-10-15: US2/US3 pre-015 slice (WI-C1, WI-C2, WI-C3), pg-manager v0.6.1 sibling

**Regression, `make integration`** (fresh 3-node compose cluster with the boot-window gate, store archiving and boot-time managed blocks): `ok ... tests/integration 118.175s`, exit 0.

**Scaleout**, full suite: `PGMAN_SCALEOUT_IMAGE=pgman-proxy-scaleout:dev go test -count=1 -tags=scaleout -v ./tests/scaleout/`

```text
--- SKIP: TestExpandOneToThree (0.00s)                      needs pg-manager 015
--- PASS: TestExpandWithoutPriorReplicationHBA (12.58s)     node-a assertions only (015 absent)
--- PASS: TestSingleNodeFreshWritesAndRestart (18.42s)      first commit 86ms after primary; writable 11.2s after restart
--- PASS: TestSingleNodeStaleSyncNamesUpgrade (33.69s)
--- PASS: TestSoloInThreeNodeConfig (129.06s)               node-a exited 75 after 2m0s; golden data intact
--- PASS: TestFreshClusterBootstrapsOnce (12.65s)           exactly one `initdb: starting`, on the primary
--- SKIP: TestDataNodeDownEmptyNodesWait (134.85s)         first half passed (no initdb, no PGDATA, b/c exit 75); second half needs 015
--- SKIP: TestRollbackToSingle (0.01s)                      needs pg-manager 015
ok  	github.com/f1bonacc1/pgman-proxy/tests/scaleout	341.261s
```

**Gates**: lcm-discipline grep, scope-gate grep and `make grep-gates` are clean. `golangci-lint run ./...`: 0 issues.

## 2026-10-16: with pg-manager 015 (sibling checkout, branch `015-empty-peer-safety` @ 3517f2a)

`PGMAN_SCALEOUT_UPSTREAM_015=1`, image built from the sibling. pgman-proxy carries the coupled pgx v5.11.0 / nats.go v1.53.1 bump (8c4952f).

**First run (pg-manager f3ba7c0)**: S1 passed 3 of 5. The two failures were a clone race: a new node cloned from node-a while node-a still held the lease in recovery, node-a's promotion aborted the copy, and the error was not retried. Fixed upstream in c959dc3 and 3517f2a (see `../pg-manager/specs/015-empty-peer-safety/validation.md`). The first run's S1 outage readings were vacuous (0 probe commits); the probe fix is 8c4952f.

**S1, 10 iterations** (seed 1792097782582830307): 10/10 PASS. Outage 7.6-10.7 s, median about 8.3 s.

**Full suite**:

```text
--- PASS: TestExpandOneToThree (111.52s)          5/5; outage 7.6, 7.9, 8.8, 9.5, 10.4 s
--- PASS: TestExpandWithoutPriorReplicationHBA (17.70s)
--- PASS: TestSingleNodeFreshWritesAndRestart (11.77s)
--- PASS: TestSingleNodeStaleSyncNamesUpgrade (13.99s)   015 path: no manual step needed
--- PASS: TestSoloInThreeNodeConfig (128.98s)
--- PASS: TestFreshClusterBootstrapsOnce (14.85s)
--- PASS: TestDataNodeDownEmptyNodesWait (154.81s)        both halves
--- PASS: TestRollbackToSingle (39.88s)
ok  	github.com/f1bonacc1/pgman-proxy/tests/scaleout	493.496s
```

Across both runs, S1 passed 15/15 expansions in all three start orders. SC-002 and SC-003 are met against the sibling. They will be confirmed again on the tagged release after WI-C5.

**Rig acceptance at 1 GB scale** (`IMG=pgman-proxy-scaleout:015c`, task T029). The golden node holds 1,000,000 rows (sysid 7697004710384128072).

| Scenario | Baseline (v0.6.1) | 015 build |
|---|---|---|
| `race` (unrigged 1→3) | VERDICT FAIL in 3/3 (an empty node ran initdb and became primary) | **VERDICT: PASS**. node-a primary on tli 2; node-b and node-c standbys with the same sysid and 1,000,000 rows |
| `a-wins` | FAIL after 71 `promote: refused` (sync_peer_silent) | **VERDICT: PASS**, 0 promote refusals |
| `rollback` (node-a back to single) | primary never | primary after 6 s, `synchronous_standby_names=[]`, `W_OK` |

**Regression, `make integration` against the 015 sibling**: `ok ... tests/integration 142.090s`, exit 0. The suite runs a fresh 3-node compose cluster, failover, switchover, fencing and upgrade tests. A fresh cluster now waits for every peer's empty report before bootstrapping, and the suite stays well inside its budget (118 s before 015).

**Plan section 4 definition of done, status against the 015 sibling**: rig `race` and `a-wins` print PASS; `make scaleout` is green (S1 15/15); `make integration` is green and unchanged. Remaining: the runbook merges with the PR, and the same runs repeat on the tagged pg-manager release (T028).

## 2026-10-16: rerun on pg-manager 4204520 (release candidate for v0.7.0)

Since the runs above, pg-manager gained:
- the v0.8.0 gate move (65a16b0);
- a guard that refuses a switchover to a target with an empty or partial data directory (f8083c6);
- example-harness fixes (4204520).

pgman-proxy gained the `cluster.identity` doctor check and gauge, and adopted `ClassifyDataDir`; S1 now asserts the gauge is 0 on every node (06a45b3). Image rebuilt from the sibling at 4204520.

- **Gates**: `make test lint grep-gates`, exit 0 (golangci-lint 0 issues).
- **`make integration`**: `ok ... tests/integration 144.984s`. This includes the `lcm_switchover` tests the new guard could affect.
- **`make scaleout`** with `PGMAN_SCALEOUT_UPSTREAM_015=1`: green twice. The second run is verbose, with no skips:

```text
start-order seed 1792103394713490732
--- PASS: TestExpandOneToThree (120.72s)          5/5; gaps 8.8, 9.5, 8.4, 10.4, 10.8 s (18-25 commits each)
--- PASS: TestExpandWithoutPriorReplicationHBA (20.24s)
--- PASS: TestSingleNodeFreshWritesAndRestart (16.15s)   first commit 82ms after primary; writable 6.8s after restart
--- PASS: TestSingleNodeStaleSyncNamesUpgrade (11.90s)   writable 4.7s after restart with no manual step
--- PASS: TestSoloInThreeNodeConfig (129.02s)            node-a exited 75 after 2m0s
--- PASS: TestFreshClusterBootstrapsOnce (12.65s)
--- PASS: TestDataNodeDownEmptyNodesWait (150.67s)
--- PASS: TestRollbackToSingle (40.46s)                  writable 4.7s after rollback restart
ok  	github.com/f1bonacc1/pgman-proxy/tests/scaleout	521.663s
```

**S1 across all runs on the 015 code**: 20/20 expansions, all three start orders. Write outage through node-a 7.6-10.8 s, median about 8.8 s, against the 30 s budget (SC-003). Remaining for T028: repeat these runs on the published v0.7.0 tag once `go.mod` pins it.

## 2026-10-16: on the published tag (go.mod pins pg-manager v0.7.0, T028)

`go.mod` pins `github.com/f1bonacc1/pg-manager v0.7.0`. MVS also raised `golang.org/x/text` to v0.41.0 (GO-2026-6629) and `golang.org/x/sync` to v0.22.0. The local `go.work` is gone, and the sibling checkout is at `main` = v0.7.0, so the test image builds from the tag. `PGMAN_SCALEOUT_UPSTREAM_015` was **not** set: the suite detected v0.7.0 from `go.mod`.

- **Gates**: `make test lint grep-gates`, exit 0.
- **`make integration`**: `ok ... tests/integration 118.999s`.
- **`make scaleout`** (verbose, no skips):

```text
start-order seed 1792105101994306356
--- PASS: TestExpandOneToThree (125.75s)          5/5; gaps 8.5, 8.4, 10.4, 14.1, 9.6 s (18-30 commits each)
--- PASS: TestExpandWithoutPriorReplicationHBA (24.78s)
--- PASS: TestSingleNodeFreshWritesAndRestart (11.80s)   first commit 82ms after primary; writable 4.7s after restart
--- PASS: TestSingleNodeStaleSyncNamesUpgrade (13.99s)   writable 4.7s after restart with no manual step
--- PASS: TestSoloInThreeNodeConfig (131.14s)            node-a exited 75 after 2m0s
--- PASS: TestFreshClusterBootstrapsOnce (15.01s)
--- PASS: TestDataNodeDownEmptyNodesWait (153.53s)
--- PASS: TestRollbackToSingle (39.90s)                  writable 6.9s after rollback restart
ok  	github.com/f1bonacc1/pgman-proxy/tests/scaleout	516.843s
```

**S1 across all runs on the 015 code**: 25/25. Write outage 7.6-14.1 s, median about 8.8 s, against the 30 s budget (SC-003). The 14.1 s gap (iteration 4, a-last) is the first above 10.8 s. Its cause is not investigated: the budget holds, but treat the upper end as noisy.

- **`govulncheck`**: no module findings. Nine standard-library advisories in go1.26.7 are fixed in go1.26.9 (GO-2026-6603, 6605, 6607, 6608, 6610, 6611, 6612, 6613, 6617), and CI installs Go from the `go.mod` directive. Hence the toolchain bump that follows.

**After the go1.26.9 toolchain bump**: `govulncheck ./...` reports 0 affecting vulnerabilities. `make test lint grep-gates` is green and `make integration` passes (`ok 154.451s`). The test image's binary reports `go1.26.9`, `pg-manager v0.7.0` (sibling at the tag) and `golang.org/x/text v0.41.0`.
