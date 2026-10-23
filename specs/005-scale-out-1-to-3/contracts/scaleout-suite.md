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
| S5 | no | fresh single node | write commits ≤ 10 s after primary; `ssn = ''`; `replication_policy=async` logged; control file `shut down` after the stop (issue #11); restart → primary + write ≤ 30 s; all rows kept | 2 min |
| S5b | flips | single node with the old initdb `ssn` line appended to `postgresql.conf`, restarted | **pre-015**: after the documented ALTER SYSTEM step, write ≤ 30 s. **015**: write ≤ 30 s with no step | 2 min |
| S4 | no | golden node-a alone with the 3-node config | exit code 75 after 110-180 s; restored golden sysid and rows unchanged | 4 min |
| S7 | partial | golden created **without** replication HBA; 3-node config **with** it | node-a `pg_hba.conf` has the managed block; `postgres.managed_block_applied` logged on node-a. **With 015**, also: b and c cloned (`bootstrap: starting basebackup source=node-a`) and streaming | 6 min |
| S1 | yes | golden node-a (50 k rows) + empty b, c; 3-node config; start order per iteration ∈ {a-first, a-last, together} (round-robin, shuffled) | every node same sysid as golden; rows == golden on all; node-a primary; `streaming(node-a) == 2`; final `ssn` names node-b and node-c; no `initdb: starting` in b/c logs; `embedded_nats.store_archived` on node-a; longest write gap through node-a < 30 s | 5 × 4 min |
| S1r | yes | S1 with HBA extras that give peers `replication` access only (`host replication all 0.0.0.0/0 trust`, the probe role on database `postgres`, every other TCP session rejected); one iteration per start order (issue #9, pg-manager ≥ v0.7.2) | the S1 assertions; a SQL session from node-b to node-a is rejected by `pg_hba.conf`; no `drop pre-existing slot from leader failed` in b/c logs | 3 × 4 min |
| S2 | yes (for "exactly once") | three empty nodes, 3-node config | exactly one node logs `initdb: starting`; the others clone from it; one sysid; writes succeed | 4 min |
| S3 | yes | after S1-style expansion: stop b, c; node-a single-node config | node-a primary and writable ≤ 30 s; `ssn = ''`; `embedded_nats.store_archived` (clustered) logged; rows intact | 3 min |
| S9a | no | three empty nodes, 3-node config; `docker stop -t 45` one streaming standby (pgman-proxy issue #11) | the standby's control file reads `shut down in recovery`; its log has `postgres stopped` with `mode=fast`; restarted, it streams again | 5 min |
| S9b | no | three empty nodes, 3-node config; a writer commits to the primary's port 5432 directly every 50 ms; `docker stop -t 45` the primary (issue #11) | old primary's control file reads `shut down`; `postgres stopped` with `mode=fast` logged; a standby promotes and holds every commit the writer saw acknowledged; restarted, the old primary is a standby with WAL receiver `streaming` and the new primary streams to 2; no `bootstrap: starting basebackup` and no `"to":"rewinding"` in its log | 6 min |
| S9c | no | three empty nodes, 3-node config; a standby restarted with `PC_PROC_NAME` set (so supervisor detection passes); `POST /v1/restart` target=proxy on it (issue #11) | 200 response; the process exits 0; its control file reads `shut down in recovery`; `postgres stopped` with `mode=fast` logged | 5 min |
| S10a | no | three empty nodes, 3-node config; every node's last checkpoint is the same one and the primary writes 100 more rows; `docker network disconnect` the primary, commit 100 rows on it with `synchronous_commit = local`, `docker kill -s KILL` it; `pg_waldump` must find a record past the promoted node's history-file switch point (pgman-proxy issue #12, pg-manager ≥ v0.7.3). Restart the old primary with auto-rebootstrap off | its `/v1/status` `TimelineDivergence` reaches evidence `source` from the promoted node, with the history file's timelines and switch LSN; its `pgman_proxy_timeline_diverged` = 1 and the standbys' = 0; `replication.timeline-consistent` fails on the promoted node naming it; `timeline.diverged` in the promoted node's log; not streaming and no `bootstrap: starting basebackup` | 4 min |
| S10b | no | the S10a divergence; restart the old primary with auto-rebootstrap on (window 15 s), as in the issue | standby with WAL receiver `streaming`, the promoted node streams to 2; `timeline.diverged` and `bootstrap: starting basebackup` logged; the last `auto_rebootstrap.decided` has `detection_source` `local_lag_persisted` (the divergence-confirmed stale-WAL regime, not a failed Postgres start); gauge 0, no `TimelineDivergence`, the check passes; both nodes hold the 200 replicated rows and none of the 100 local ones | 5 min |
| S6 | partial | golden volume for node-a but node-a **stopped**; b, c empty, 3-node config | over 180 s: no `initdb: starting` on b/c, no `PG_VERSION` on b/c, no node accepts writes; b and c exit 75 (route mesh needs every peer, F15; this part runs today). **With 015**: start node-a, restart b and c (as a supervisor would) → S1 assertions hold | 8 min |

`make integration` (unchanged) remains the regression test that a fresh
3-node bootstrap from empty volumes works (FR-018). S2 adds the
explicit "exactly once" assertion.
