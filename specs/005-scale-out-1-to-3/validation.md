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
