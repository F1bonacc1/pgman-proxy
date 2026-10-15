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
