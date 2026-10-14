# Data Model: Safe 1→3 Scale-out and Working Single-Node Mode

This feature adds no tables or wire messages in pgman-proxy. The "data" is:

- derived runtime values;
- on-disk layouts pgman-proxy inspects or rewrites;
- one upstream key it reads indirectly through pg-manager.

## 1. ReplicationPolicy (derived, WI-A)

| Input | Output | Label (`replication_policy` log field) |
|---|---|---|
| `len(peers) == 1` | `pgmanager.AsyncOnly{}` | `async` |
| `len(peers) >= 2` | `pgmanager.QuorumSync{MinSync: policy.quorum_sync.min_sync, Pool: peers}` | `quorum(min_sync=<N>)` |

- **Validation**: unchanged. `len(peers) == 0` is already rejected by `config.Validate`.
- **Lifetime**: computed once per process start. Peers are not reloadable.

## 2. PGDATA completeness (WI-C1, WI-C3)

| Field | Source | Meaning |
|---|---|---|
| `PG_VERSION` | `<data_dir>/PG_VERSION` regular file | a cluster directory was started |
| `pg_control` | `<data_dir>/global/pg_control` regular file | the directory is complete (written last by a clone, see research R-003) |

State function: `pgdataComplete(dir) (bool, error)`.

| Condition | Result |
|---|---|
| `dir == ""` | `(false, nil)` |
| both files exist | `(true, nil)` |
| either file is not-exist | `(false, nil)` |
| any other stat error | `(false, err)`; callers treat it as not complete |

Consumers:
- **Boot-window campaign gate.** While `manager == nil`, campaign iff `pgdataComplete` is `(true, nil)`. Once the manager exists, campaign iff `manager.ShouldCampaign()`.
- **Boot-time managed-block apply.** Runs only when the result is `(true, nil)`. An empty node gets its files from `initdb` (PostInitDB hook) or from the clone (inherits the source's files; the node then re-applies on its next boot).

## 3. JetStream store layout (WI-C2)

```text
<jetstream_dir>/                      # configured dir, often a mount point: never renamed
├── jetstream/                        # live store (created by nats-server)
│   ├── $SYS/_js_/_meta_/             # present ONLY in a clustered store
│   └── <account>/streams/...         # KV bucket pgmgr_<cluster>, history stream, ...
├── jetstream.standalone-20261015T091500Z/   # archived when switching standalone → clustered
└── jetstream.clustered-20261016T120000Z/    # archived when switching clustered → standalone
```

| Layout | Detection |
|---|---|
| `none` | `<dir>/jetstream` does not exist (or `jetstream_dir` is empty: in-memory) |
| `clustered` | `<dir>/jetstream/$SYS/_js_/_meta_` exists |
| `standalone` | `<dir>/jetstream` exists without the meta dir |

| Desired mode | Predicate |
|---|---|
| `clustered` | `cluster.routes_listen.enabled && cluster.declared_size > 1` (the same predicate as `bootEmbeddedNATS`) |
| `standalone` | otherwise |

Transition table for `ArchiveIncompatibleStore(dir, clustered, now)`:

| actual \ desired | standalone | clustered |
|---|---|---|
| none | no-op | no-op |
| standalone | no-op | archive → `jetstream.standalone-<ts>` |
| clustered | archive → `jetstream.clustered-<ts>` | no-op |

Idempotent: after an archive, the actual layout is `none`, and the new
server creates a matching store.

## 4. Managed configuration block (WI-C3)

| Field | Value |
|---|---|
| file | `<data_dir>/pg_hba.conf` (block `postgres.hba_extras`), `<data_dir>/postgresql.conf` (block `postgres.conf_extras`) |
| begin marker | `# BEGIN pgman-proxy managed: <block>` |
| end marker | `# END pgman-proxy managed: <block>` |
| body | configured lines, verbatim, one per line |
| position | in place if present; otherwise appended at the end of the file (preceded by a newline if the file does not end in one) |

Apply outcomes (`action`):

| Condition | Action |
|---|---|
| no block, lines non-empty | `inserted` |
| block present, body differs | `updated` |
| block present, lines empty | `removed` |
| block present and body equal, or no block and lines empty | `unchanged` (no write) |
| BEGIN without END, or more than one BEGIN | error (`managed block malformed`) |

The legacy marker `# pgman-proxy: postgres.<block>` is never touched.

## 5. Upstream entities (pg-manager v0.7.0; read-only from pgman-proxy)

See [contracts/upstream-015.md](contracts/upstream-015.md).

- **PGDATA presence record**: key `pgmgr/<cluster>/pgdata/<node>`, value `{has_data, system_identifier, updated_at}`. pgman-proxy never reads or writes this key directly; it is pg-manager's state.
- **Identity-mismatch status**: a field on `Manager.Status()`, consumed by the doctor check and the gauge.

## 6. Expansion lifecycle (observable states, for tests and runbook)

```text
node-a: single(primary) ──restart in 3-node cfg──► booting ─► store archived ─► mesh wait ─► leader ─► primary(async)
                                                                                                   │
node-b/c: empty ─► booting ─► mesh wait ─► deferring (empty, peer has data) ─► bootstrapping ─► standby ─► has_data
                                                                                                   │
node-a: primary(async) ─────────── first data-bearing standby reported ──────────► primary(quorum: ANY 1 (b,c))
```

Rollback: stop b and c, then restart node-a with the single-node
config. The store is archived (clustered → standalone), the policy is
async, the stale sync setting is cleared (upstream B4), and node-a is
primary.
