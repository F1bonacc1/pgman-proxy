# Contract: `tests/scaleout` end-to-end suite (WI-C6)

**Feature**: `005-scale-out-1-to-3` | FR-017, FR-018 | research R-009, R-010, R-012

## Entry points

```bash
make scaleout       # go test -trimpath -count=1 -timeout=40m -tags=scaleout ./tests/scaleout/...
```

| Env var | Effect |
|---|---|
| `PGMAN_SCALEOUT_IMAGE=<tag>` | skip the image build and use this tag |
| `PGMAN_SCALEOUT_UPSTREAM_015=1` | run scenarios that need pg-manager 015 even though `go.mod` pins an older version (sibling checkout carries 015) |
| `PGMAN_SCALEOUT_KEEP=1` | keep containers, volumes and network after each test (debugging) |
| `PGMAN_SCALEOUT_ITERATIONS=<n>` | S1 iteration count (default 5) |

## Harness guarantees

- **Isolation**: every test uses a unique prefix `pgso-<scenario>-<6 hex>` for its docker network, volumes (`<prefix>-<node>-data`, `<prefix>-<node>-js`) and containers (`<prefix>-node-<x>`). Containers get the network alias `node-<x>`, so peer ids resolve as DNS names (the same convention as compose).
- **Cleanup**: `t.Cleanup` removes everything unless `PGMAN_SCALEOUT_KEEP=1`. On failure, every container's logs are attached to the test output before removal.
- **Image**: `TestMain` builds `tests/integration/Dockerfile` with the parent directory of the repo as context (same as compose), once per run.
- **Node configs**: env presets equivalent to `docs/scale-out-1-to-3/rig/{common,single-a,three-x}.env`. Credentials are test-only literals (the same pattern as the compose file).

## Probes (all via `docker exec`, local socket)

| Probe | Implementation | Returns |
|---|---|---|
| `role(node)` | `select pg_is_in_recovery()` | `primary` / `standby` / `down` |
| `sysid(node)` | `select system_identifier from pg_control_system()` | decimal string |
| `rows(node, table)` | `select count(*)` | int |
| `ssn(node)` | `show synchronous_standby_names` | string |
| `write(node)` | `insert ... returning 1` with a 10 s statement timeout | ok / error / timeout |
| `streaming(node)` | `select count(*) from pg_stat_replication where state='streaming'` on primary | int |
| `logs(node)` | `docker logs` | string |
| `exitCode(node)` | `docker wait` | int |

## Scenarios

| ID | Needs 015 | Setup | Must hold | Budget |
|---|---|---|---|---|
| S5 | no | fresh single node | write commits ≤ 10 s after primary; `ssn = ''`; `replication_policy=async` logged; restart → primary + write ≤ 30 s; all rows kept | 2 min |
| S5b | flips | single node with the old initdb `ssn` line appended to `postgresql.conf`, restarted | **pre-015**: after the documented ALTER SYSTEM step, write ≤ 30 s. **015**: write ≤ 30 s with no step | 2 min |
| S4 | no | golden node-a alone with the 3-node config | exit code 75 after 110-180 s; restored golden sysid and rows unchanged | 4 min |
| S7 | partial | golden created **without** replication HBA; 3-node config **with** it | node-a `pg_hba.conf` has the managed block; `postgres.managed_block_applied` logged on node-a. **With 015**, also: b and c cloned (`bootstrap: starting basebackup source=node-a`) and streaming | 6 min |
| S1 | yes | golden node-a (50 k rows) + empty b, c; 3-node config; start order per iteration ∈ {a-first, a-last, together} (round-robin, shuffled) | every node same sysid as golden; rows == golden on all; node-a primary; `streaming(node-a) == 2`; final `ssn` names node-b and node-c; no `initdb: starting` in b/c logs; `embedded_nats.store_archived` on node-a; longest write gap through node-a < 30 s | 5 × 4 min |
| S2 | yes (for "exactly once") | three empty nodes, 3-node config | exactly one node logs `initdb: starting`; the others clone from it; one sysid; writes succeed | 4 min |
| S3 | yes | after S1-style expansion: stop b, c; node-a single-node config | node-a primary and writable ≤ 30 s; `ssn = ''`; `embedded_nats.store_archived` (clustered) logged; rows intact | 3 min |
| S6 | partial | golden volume for node-a but node-a **stopped**; b, c empty, 3-node config | over 180 s: no `initdb: starting` on b/c, no `PG_VERSION` on b/c, no node accepts writes; b and c exit 75 (route mesh needs every peer, F15; this part runs today). **With 015**: start node-a, restart b and c (as a supervisor would) → S1 assertions hold | 8 min |

`make integration` (unchanged) remains the regression test that a fresh
3-node bootstrap from empty volumes works (FR-018). S2 adds the
explicit "exactly once" assertion.
