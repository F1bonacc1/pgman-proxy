# Research: Safe 1→3 Scale-out and Working Single-Node Mode

Facts F1-F16 come from `docs/scale-out-1-to-3/plan.md` section 1 (docker rig, 2026-10-14). This file records the design decisions made on top of them. Each is tagged by how it was established:

- **[Verified]**: reproduced or read in code at the baseline commits.
- **[Decided]**: chosen among alternatives.
- **[To verify]**: an assumption with a named verification task.

---

## R-001: Single-node replication policy is derived, not configured [Decided]

**Decision**: `replicationPolicy(cfg)` returns `pgmanager.AsyncOnly{}` when `len(cfg.Peers) == 1`. Otherwise it returns `pgmanager.QuorumSync{MinSync: cfg.Policy.QuorumSync.MinSync, Pool: nodeIDs(cfg.Peers)}`, which is today's behavior byte for byte.

**Rationale**:
- With one peer, `QuorumSync` cannot be satisfied: `MinSync ≥ 1` is enforced by `replication.Render` (`sync_standby_names.go:71`), and the only pool member is self (F1).
- `AsyncOnly` is pg-manager's supported async mode. It renders `''` and publishes an async snapshot, so promotion takes the LSN gate (F3, verified in an experiment build).
- Deriving the mode mirrors `DeriveReplicas(declared_size)`.

**Alternatives considered**:
- **A `policy.replication: async|quorum` key**: rejected (Constitution VII). It adds a key whose only valid value for one peer is forced anyway, and whose `quorum` value for one peer is a known hang.
- **Keying off `declared_size`**: rejected. Replication pools are built from `peers`, so the policy must agree with the pool. `declared_size` keeps governing NATS only.

**Note**: validation does not cross-check `len(peers)` against `declared_size` today. That is out of scope here; it is recorded as a follow-up in the runbook's "config sanity" list.

## R-002: Data dirs initialized by earlier releases keep a stale sync setting [Verified in code / To verify end to end]

**Decision**: document a one-time `ALTER SYSTEM SET synchronous_standby_names = ''` plus `SELECT pg_reload_conf()` for single nodes initialized before this release (FR-005). It stays until pg-manager 015's B4 renders `''` instead of returning early (`act.go:1434`). `tests/scaleout` S5b reproduces the stale state cheaply: it appends the old `initdb` value to `postgresql.conf` on a single node. It then asserts:

- **before 015**: the documented step makes the node writable;
- **after 015**: no step is needed (the assertion flips with the version gate, R-009).

**Rationale**: pgman-proxy must not render `synchronous_standby_names` itself. That is a library-mandated key (pg-manager `manager.go` `Custom` docs), and writing it here would violate Constitution IV.

**Alternatives considered**:
- **Writing `synchronous_standby_names = ''` into the managed `conf_extras` block**: rejected. It fights the engine's ownership of that key, and for multi-peer clusters it would be actively wrong.

## R-003: "Complete database" predicate for the boot-window gate [Decided / To verify]

**Decision**: `pgdataComplete(dir)` is true only when both `<dir>/PG_VERSION` and `<dir>/global/pg_control` exist as regular files. Any stat error other than not-exist is reported as an error, and the caller treats it as not complete (fail-closed, FR-006).

**Rationale**:
- `PG_VERSION` alone (today's `ensureStandbySignalIfInitialized` check) is present early in an interrupted clone.
- PostgreSQL's base backup sends `global/pg_control` last, so its absence marks an incomplete clone. This is from PostgreSQL's `basebackup.c`, which skips pg_control in the main directory walk and appends it at the end.
- Orderly clone failures already delete the target directory (pg-manager `internal/pgproto/pgexec.go` BaseBackup cleanup).

**[Verified 2026-10-14]** (tasks T009). In the integration image, PostgreSQL 17:
- **Setup**: a 991 MB primary; `pg_basebackup -X stream -c fast -r 20M` killed with SIGKILL after 15 s.
- **Partial clone (311 MB)**: `PG_VERSION` present, `backup_label` present, `global/pg_control` **absent**, `standby.signal` absent.
- **Uninterrupted clone of the same primary**: `global/pg_control` present.

The predicate therefore separates the two cases. `backup_label` would not work: PostgreSQL 17 writes it at the start of a clone, and a completed but not yet started clone also carries it.

**Constitution IV**: pg-manager has an equivalent internal check (`hasPGData`, `observe.go` `PGDataEmpty`) that is not exported. [contracts/upstream-015.md](contracts/upstream-015.md) asks 015 to export one predicate (for example `pgmanager.PGDataComplete(dir)`) shared by its data-presence record and this gate. Once exported, pgman-proxy calls it, and the local function is deleted.

**Alternatives considered**:
- **Asking pg-manager's Manager**: not possible. The boot window is precisely the time before `manager.New` returns (F12).
- **Changing pg-manager's nil-receiver `ShouldCampaign` to false**: rejected. Other embedders rely on today's semantics (plan WI-C1).

## R-004: JetStream store layout detection and archive naming [Verified / Decided]

**Decision**:
- **Layout detection** for `<dir>/jetstream`:
  - absent → `none`;
  - contains `$SYS/_js_/_meta_` → `clustered` (F13);
  - otherwise → `standalone`.
- **Desired mode**: `clustered` iff the server will start with routes. This is the exact predicate `bootEmbeddedNATS` uses, `cc.RoutesListen.Enabled && cc.DeclaredSize > 1`, not `declared_size` alone.
- **Archiving**: when the actual layout is not `none` and differs from the desired mode, rename `<dir>/jetstream` to `<dir>/jetstream.<actual>-<UTC 20060102T150405Z>`.
  - On a name collision, append `-1`, `-2` and so on, up to 100, then fail.
  - Rename within `<dir>`, never `<dir>` itself; it is often a mount point.
- **Store directory not configured** (in-memory): no-op.

**Rationale**: reusing the other mode's store gave undefined results, including half-adopted streams (F13). Archiving in both directions makes rollback a plain config revert (decision 4). Keeping the old store preserves evidence; the runbook tells operators to delete it after verification.

**Alternatives considered**:
- **Deleting the store**: rejected. It is irreversible and destroys forensics.
- **Refusing to start on a mismatch**: rejected for the expansion direction, because it defeats the automation goal. Considered for rollback (decision 4) and rejected for symmetry.
- **Migrating KV keys**: out of scope (accepted loss).

## R-005: Where store archiving runs and how it reports [Decided]

**Decision**: in `bootEmbeddedNATS`, after `BuildOptions` and before `embedded.NewServer` (`start.go:791`). Archiving emits `embedded_nats.store_archived` at Warn through `logger.Event`, with fields `from`, `to`, `reason`, `actual_layout` and `desired_layout`.

The history stream is not available yet. It is created later in the same boot, inside the new store, so the event lands only in logs. The history of the archived era lives in the archived store.

A rename failure returns `StartupError{Code: ExitConfig}` with the path (FR-011). `ExitConfig` is used because the operator must fix the directory (permissions, read-only mount), and retrying the boot will not help.

## R-006: Managed block format and semantics [Decided]

**Decision**: [contracts/managed-conf-blocks.md](contracts/managed-conf-blocks.md) has the full contract. In summary:
- Markers are `# BEGIN pgman-proxy managed: postgres.hba_extras` and `# END pgman-proxy managed: postgres.hba_extras`, with the same pair for `postgres.conf_extras` in `postgresql.conf`.
- The block is replaced in place when present, appended at the end when absent, and removed when the configured list is empty.
- Content outside the markers is preserved byte for byte.
- Writes are atomic: temp file in the same directory, fsync, rename. The original file mode is preserved.
- A malformed state (BEGIN without END, or duplicate BEGIN) is an error. Startup is refused, and nothing is guessed.

**Legacy**: blocks written by earlier releases (`# pgman-proxy: postgres.hba_extras` followed by lines) are left untouched. Duplicate HBA lines are harmless, because the first match wins and the legacy lines were the same rules. For `postgresql.conf`, the later assignment wins, and the managed block is appended after the legacy one. This is documented in the CHANGELOG.

**Rationale**: the block is idempotent across boots and lets operators withdraw rules. It does not fight pg-manager, which changes `synchronous_standby_names` through `ALTER SYSTEM` into `postgresql.auto.conf` (F10) and writes `postgresql.conf` only at `initdb`.

## R-007: Applying managed blocks on boot and reloading [Verified in code / Decided]

**Decision**:
- **PostInitDB**: the hook calls `applyManagedConf` instead of `appendLines`. No reload is needed, because the postmaster starts afterwards.
- **Every boot**: after `ensureStandbySignalIfInitialized` (`start.go:272`), when `pgdataComplete(dataDir)` holds, call `applyManagedConf`. If anything changed and `pg.IsRunning(ctx)` reports a live postmaster, call `pg.Reload(ctx)`. `pg` is pg-manager's `PostgresExecutor`, already built at gate #6 (`start.go:256`); its `Reload` runs the engine's own reload command (`internal/pgproto/pgexec.go:208`), so no engine mechanics are named in this repo.
- **Failures**: a write or reload failure returns `StartupError{Code: ExitDeps}` (fail-closed). Silently running with stale replication HBA would only fail later, inside a new peer's clone.
- **Events**: `postgres.managed_block_applied` at Info, fields `file`, `block`, `action` (`inserted|updated|removed|unchanged`, `unchanged` not logged), `lines`, `reloaded`.

**Rationale**: under systemd the postmaster can outlive the proxy (F16), so a boot can find Postgres running.

## R-008: Identity mismatch consumption [Decided; shape depends on upstream]

**Decision**: pgman-proxy consumes three things from pg-manager v0.7.0 (exact names pinned in [contracts/upstream-015.md](contracts/upstream-015.md) and adjusted when 015 lands):
1. a `Status` field reporting the mismatch (local and leader identities);
2. a coordination event topic `identity_mismatch`;
3. no new mutating API.

pgman-proxy then exposes it in three places:
- doctor check `cluster.identity`, read-only, failing with both identities as evidence;
- gauge `pgman_proxy_cluster_identity_mismatch` (0/1), set by the existing observability gauge loop (`startObservabilityGauges`);
- the event, passed through the existing coordination-event subscription into logs and history.

**Alternatives considered**:
- **Computing the mismatch in pgman-proxy** by reading `pg_control_system()` locally and from the leader: rejected. It duplicates engine logic (Constitution IV) and needs the leader's DSN.

## R-009: Testing before pg-manager 015 exists [Decided]

**Decision**: `tests/scaleout` gates each scenario on the capability it needs:
- **S4, S5, S5b-with-manual-step, S7**: run on today's engine.
- **S1, S2-exactly-once, S3, S6**: call `requireUpstream015(t)`.

`requireUpstream015` passes when either:
- `go list -m github.com/f1bonacc1/pg-manager` reports ≥ v0.7.0; or
- `PGMAN_SCALEOUT_UPSTREAM_015=1` is set.

The env var is for local development against a sibling checkout that already carries 015, since the image always builds from the sibling via `replace`. Otherwise the test calls `t.Skip` with the reason.

**Rationale**: nightly stays honest. A skip names the missing prerequisite instead of failing red, or worse passing vacuously.

**Alternatives considered**:
- **A build tag per capability**: rejected; easy to forget.
- **Not landing tests until 015**: rejected. S5 is WI-A's required real-server test (Constitution VI).

## R-010: Scaleout suite mechanics [Decided]

- **Driver**: the docker CLI via `os/exec`, mirroring `docs/scale-out-1-to-3/rig/lib.sh`. Each test gets a unique prefix (`pgso-<scenario>-<rand>`) for network, volumes and containers, and `t.Cleanup` removes them (unless `PGMAN_SCALEOUT_KEEP=1`).
- **Image**: `TestMain` builds `tests/integration/Dockerfile` once, with the parent directory as build context, exactly as compose does. The tag is `pgman-proxy-scaleout:<unix-nanos>`. `PGMAN_SCALEOUT_IMAGE=<tag>` skips the build.
- **Volume ownership**: separate named volumes for PGDATA and the store directory. They are chowned to `postgres` with a root one-shot container, because the postgres image declares a `VOLUME` on its data dir.
- **Dataset**: a golden single node with 50,000 rows of about 1 KB (about 50 MB). This keeps basebackup and CI time small. The 1 GB figure from F8 stays a manual rig exercise.
- **Write-outage measurement**: a probe container (or `docker exec` loop) commits `INSERT ... RETURNING` every 250 ms through node-a's proxy port and records the longest gap. Budget: 30 s (SC-003).
- **Lease timing**: defaults, with no lease skew. The rig's `lease-fast/slow.env` trick is exactly what the gate must make unnecessary, so the suite must not use it.
- **Log assertions**: read with `docker logs`. Examples: "no initdb on node-b/node-c" checks that the pg-manager `initdb` dispatch message is absent; "deference armed" checks for the 015 log line.

## R-011: Rollback procedure ordering [Verified reasoning]

**Decision**: the runbook and S3 stop node-b and node-c **before** restarting node-a with the single-node config.

**Rationale**: two streaming standbys with the 3-node config still hold 2 of 3 NATS servers and can elect a leader among themselves. That leader would be promoted while node-a also runs standalone: a split brain, with two writers on diverging timelines.

## R-012: Nightly CI placement [Decided]

**Decision**: a separate job `scaleout` in `.github/workflows/nightly-integration.yml`, with the same sibling-checkout steps as the integration job, `timeout-minutes: 60`, running `make scaleout`. It is not part of per-PR CI.

**Rationale**: the suite runs for about 20-25 minutes (S1 runs 5 iterations, S4 waits 120 s, S6 waits 180 s), and Constitution VI's real-server requirement is satisfied nightly, as for `make integration`.

## R-013: The v0.7.0 version number is already spoken for upstream [Verified / Open]

**Finding**: pg-manager already earmarks v0.7.0 in three places:
- **The major-upgrade gate**: `manager/backup_upgrade.go:104` returns `"upgrade: major strategies wired but gated on v0.7.0"`.
- **`docs/upgrade.md`**: calls v0.7.0 the "release-hardening pass" that lifts that gate.
- **The B-017 removal checklist** (`specs/BACKLOG.md`): targets v0.7.0 as the earliest legal removal.

pgman-proxy's feature 004 pins the gate string as a sentinel (`tests/integration/lcm_upgrade_test.go`, `internal/control/handlers_upgrade_test.go`).

**Consequence**: if milestone 015 ships as v0.7.0 without lifting the gate, the running engine says it is gated on the version it already is. pg-manager will likely reword the message, which trips the 004 sentinel (by design).

**Decision (provisional)**: keep decision 5, so 015 is a minor bump, because it adds a state key and changes behavior. The exact tag is pg-manager's call, made when 015 is cut. WI-C5's task list includes:
- re-pinning the 004 sentinel if the gate message changes;
- updating `specs/004-upgrade-tests` references.

**Raise with the maintainer before tagging 015**: either 015 takes v0.7.0 and the hardening pass moves to v0.8.0 (reword the gate and docs), or 015 waits to ship inside the hardening release.
