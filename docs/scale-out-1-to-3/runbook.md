# Runbook: expand a single pgman-proxy node to three

**Applies to**: pgman-proxy with feature 005. The safe expansion also requires pg-manager v0.7.0 or later (milestone 015, empty-peer safety); check `go list -m github.com/f1bonacc1/pg-manager` or the release notes.
**Background**: [plan.md](plan.md) (verified facts F1-F16), [specs/005-scale-out-1-to-3/spec.md](../../specs/005-scale-out-1-to-3/spec.md).

> **Do not expand on a release without pg-manager v0.7.0.** On earlier releases an empty new node can win the leadership race and initialize a **new, empty database**, and clients are routed to it (F4, F5). Your data stays on node-a, but the cluster no longer serves it.

## What changes, what is lost

- **Changes**: node-a keeps its database and stays primary. node-b and node-c clone it.
- **Restarts**: node-a restarts once. The write outage is measured in seconds: 7.6 to 14.1 s (median about 8.8 s) across 25 test expansions on a workstation with the default 5 s lease. The CI budget is 30 s.
- **Lost**: everything in node-a's embedded coordination store (JetStream):
  - divergence and identity fences;
  - auto-demote and auto-rebootstrap cooldown history;
  - the cluster event/audit history stream (`pgmctl history`).

  The old store is **moved aside, not deleted**, to `<cluster.jetstream_dir>/jetstream.standalone-<UTC timestamp>`. Delete it once the expanded cluster is verified.

## Prerequisites

1. **Hosts**: two new hosts with PostgreSQL binaries at the same `postgres.bin_dir`, an **empty** `postgres.data_dir`, and an **empty**, durable `cluster.jetstream_dir`.
2. **Network**: every host reaches every other on:
   - the cluster routes port (default 6222);
   - PostgreSQL (5432 or your `topology.port`);
   - the control plane, if you use it.
3. **Single node initialized before feature 005**: no manual step. Its stale `synchronous_standby_names` value is cleared on its first start as primary on this release.

## Configuration: one node → three

Push the same cluster settings to all three hosts. Per-node values differ only where noted.

| Key | 1 node | 3 nodes |
|---|---|---|
| `peers` | `[node-a]` | `[node-a, node-b, node-c]` |
| `cluster.declared_size` | `1` | `3` (keep equal to the number of `peers`) |
| `cluster.route_peers` | `[]` | per node: the other two `host:6222` |
| `cluster.routes_listen` | ignored at size 1 | enabled, reachable address, port 6222 |
| `cluster.username`, `cluster.password_env` or `cluster.password_file` | unused | required, identical on all nodes |
| `cluster.tls.*` or `cluster.tls.plaintext_explicit_ack` | unused | required for non-loopback routes |
| `cluster.jetstream_dir` | optional | required, durable |
| `postgres.replication_addr` | n/a | per node, unless peer ids resolve through DNS |
| `postgres.hba_extras` | anything | must allow `replication` connections from the other nodes' addresses |
| unchanged | `cluster.id`, `cluster.name`, node-a's `node.id`, `postgres.data_dir`, `postgres.bin_dir` | |

`postgres.hba_extras` and `postgres.conf_extras` are applied on **every boot** as marked blocks:

```text
# BEGIN pgman-proxy managed: postgres.hba_extras
...
# END pgman-proxy managed: postgres.hba_extras
```

These blocks are written into `pg_hba.conf` and `postgresql.conf`.
- Adding the replication rule to node-a's config is enough; you do not need to edit its `pg_hba.conf` by hand.
- Removing every line from the config removes the block.
- Edit outside the markers only.
- Blocks appended by earlier releases (`# pgman-proxy: postgres.hba_extras`) are left in place.
- A changed `conf_extras` setting that needs a PostgreSQL restart takes effect at the next restart. A reload is issued automatically when Postgres is already running.

## Procedure

1. Write the 3-node config to node-a's host. **Do not restart node-a yet.**
2. Start node-b and node-c. Each waits up to 120 s for the full route mesh, then exits 75 if it does not form. Let your supervisor restart them, or start them close to step 3.
3. Restart node-a.

Start order between steps 2 and 3 does not matter for correctness. Starting node-a last keeps its outage shortest.

### What you will see in the logs

- **node-a**:
  - `embedded_nats.store_archived` (Warn; `actual_layout=standalone`, `from`, `to`);
  - `postgres.managed_block_applied` when the HBA block changed;
  - promotion;
  - `replication: empty peers excluded from synchronous set` (`excluded=[node-b node-c]`, `join_window=true`), logged again with `excluded=[]` once both stream.
- **node-b and node-c**:
  - `campaign: boot-window deferral reason=pgdata_empty` while their engine starts;
  - `campaign: empty-node deference armed`;
  - then `bootstrap: starting basebackup source=node-a`. One `bootstrap: basebackup transient error; retrying` with `the standby was promoted during online backup` is expected when the clone starts before node-a's promotion completes.
- **node-b and node-c must never log `initdb: starting`.** If one does, stop the cluster and see Troubleshooting.

## Verify

Check each of the following:

```bash
pgmctl status            # node-a primary, node-b and node-c streaming standbys
pgmctl doctor            # all checks pass, including cluster.identity (pg-manager v0.7.0)
```

- **Metrics**, on every node:
  - `pgman_proxy_embedded_nats_replicas_factor` = 3;
  - `pgman_proxy_embedded_nats_routes_meshed` = 2;
  - `pgman_proxy_cluster_identity_mismatch` = 0 (pg-manager v0.7.0).
- **Database identity**: on every node,

  ```sql
  SELECT system_identifier FROM pg_control_system();
  ```

  returns node-a's value from before the expansion.
- **Sync set**: on node-a, `SHOW synchronous_standby_names;` names node-b and node-c once both stream. Until then, node-a runs asynchronously, with the same durability it had as a single node.
- **Failover readiness**: until the sync set names at least one of node-b and node-c, the cluster cannot fail over. If node-a is lost in that window, neither new node promotes on its own, even one that has finished cloning and is streaming (pg-manager's join window). "Both standbys streaming" is not enough: do not stop, replace or decommission node-a, and do not let automation proceed, until this check passes. Recovering from a lost node-a before then needs an operator: see [node-a lost during the join window](#node-a-lost-during-the-join-window).

Then delete `jetstream.standalone-*` under node-a's `cluster.jetstream_dir`.

## Rollback to one node

1. **Stop node-b and node-c first.** Two running new nodes still hold two of three coordination votes and can elect a leader between themselves. One of them would then be promoted while node-a also runs alone: two writers, two timelines.
2. Restore node-a's single-node config and restart node-a.

What happens next:
- node-a moves the clustered store aside (`jetstream.clustered-<ts>`);
- it runs asynchronous replication (single peer);
- it becomes writable within 30 s;
- the stale sync value naming node-b and node-c is cleared automatically.

node-b and node-c can then be wiped and reused.

## node-a lost during the join window

Until node-a names node-b or node-c in its sync set, it is the only node allowed to promote (the join window). If node-a dies in that window and comes back, it resumes as primary: wait for it. If it is lost for good (disk, volume or host gone), no node promotes on its own, and an operator must decide to abandon the writes only node-a acknowledged.

**Diagnose.** On any surviving node:

```bash
pgmctl doctor
```

`cluster.join-window` fails once node-a has been silent for 60 s, and names the best candidate and the exact command. `pgmctl status -o json` shows the `JoinWindow` block: the publisher, how long it has been silent, and each node's data state, database identity, LSN and fence.

**Promote.** On any surviving node (no leader is needed):

```bash
pgmctl promote node-b --accept-data-loss --reason "node-a volume lost, INC-1234"
```

pgmctl prints what is lost (the WAL node-a published that node-b lacks, plus every write node-a acknowledged after its last published position, which nobody can count), asks for the typed cluster name, records the authorization, and waits until node-b is primary. pg-manager refuses, and changes nothing, when:

- node-a holds the lease or published anything in the last 60 s: it may only be partitioned;
- node-b holds a different database, or has no fresh complete copy;
- another live node is ahead of node-b: the refusal names it; promote that one instead.

node-a is fenced in the same step. node-c follows node-b like after any failover.

**Verify.** node-b answers `SELECT pg_is_in_recovery();` with `f` and takes writes, `pgmctl doctor` passes `cluster.join-window`, and node-c streams from node-b.

**node-a returns.** It is fenced: it neither promotes nor starts Postgres. Its data directory holds the abandoned writes; copy it aside if they matter, then:

```bash
pgmctl unfence node-a
```

With `policy.auto_rebootstrap.enabled`, it then re-clones from node-b; otherwise move its data directory aside and restart it.

The older override, removing node-a from the topology of every remaining node and restarting them, still works, but needs one edit per node and records no acknowledgment.

## Troubleshooting

| Symptom | Meaning | Action |
|---|---|---|
| A node exits 75 after about 120 s | The route mesh did not form: not every peer was reachable | Check routes port reachability, `cluster.route_peers`, credentials and TLS on all three hosts |
| node-b or node-c logged `initdb: starting` | A new database was created (engine older than v0.7.0, or a gate failure) | Stop all three. Do not let clients write. node-a's data is intact. Roll back (above), then retry on pg-manager v0.7.0 or later |
| `pgmctl doctor` fails `cluster.join-window` | node-a was lost before naming a new node; no node will promote | [node-a lost during the join window](#node-a-lost-during-the-join-window) |
| `pgmctl doctor` fails `cluster.identity` | A standby follows a primary with a different database identity | Do not wipe either node before identifying which one holds the acknowledged writes |
| node-a refuses to start: `archive incompatible jetstream store` | The store could not be moved aside (permissions or a read-only mount) | Fix permissions on `cluster.jetstream_dir`; the directory itself is never renamed, only `jetstream/` inside it |
| node-a refuses to start: `managed postgres config blocks` | A managed block could not be written, or the markers are malformed | Fix the file. Each BEGIN needs exactly one matching END |

## Config sanity

- Keep `cluster.declared_size` equal to the number of `peers`. Replication is derived from `peers` (one peer → asynchronous). The coordination layer is sized from `declared_size`.
- One peer must not have a non-empty `cluster.route_peers`.
