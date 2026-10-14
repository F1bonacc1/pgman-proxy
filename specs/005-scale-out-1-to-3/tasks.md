---

description: "Task list for feature 005: safe 1→3 scale-out and working single-node mode"
---

# Tasks: Safe 1→3 Scale-out and Working Single-Node Mode

**Input**: Design documents from `/specs/005-scale-out-1-to-3/`
**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/, quickstart.md
**Technical source**: `docs/scale-out-1-to-3/plan.md` (facts F1-F16, work items WI-A, WI-C1..C7)

**Tests**: REQUIRED. Constitution VI mandates real-server integration tests for every behavior that crosses the proxy boundary, and FR-017 specifies the scaleout suite. Unit tables accompany every new function.

**Upstream gate**: tasks marked **(needs 015)** depend on pg-manager milestone `015-empty-peer-safety` (contracts/upstream-015.md). They can be written against the sibling checkout but cannot be verified green until 015 exists there. Scenario tests call `requireUpstream015(t)` (research R-009).

## Format: `[ID] [P?] [Story] Description`

- **[P]**: can run in parallel (different files, no dependency on an incomplete task)
- **[Story]**: US1..US5 from spec.md

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: the `tests/scaleout` harness and its CI entry points, shared by every story.

- [X] T001 Add a `scaleout` target to `Makefile`: `$(GO) test $(GOFLAGS) -count=1 -timeout=40m -tags=scaleout ./tests/scaleout/...`. Add it to `.PHONY` and to the header target list, with a comment that it needs docker and the sibling `../pg-manager` checkout (contracts/scaleout-suite.md).
- [X] T002 Create `tests/scaleout/main_test.go` (`//go:build scaleout`, package `scaleout`, Apache header like `tests/integration/compose_main_test.go`).
  - `TestMain` checks that `docker` is on PATH.
  - Unless `PGMAN_SCALEOUT_IMAGE` is set, it builds `tests/integration/Dockerfile` with context `<repo>/..` and tag `pgman-proxy-scaleout:<unix-nanos>`. It stores the tag in a package var and exits 2 on build failure.
  - Add `requireUpstream015(t)`. It skips with the reason "needs pg-manager 015-empty-peer-safety (set PGMAN_SCALEOUT_UPSTREAM_015=1 to run against a sibling checkout that has it)" unless the env var is `1`, or `go list -m -f {{.Version}} github.com/f1bonacc1/pg-manager` (run from the repo root) is semver ≥ v0.7.0.
  - Add `upstream015()`, a bool helper for assertions that flip (S5b).
- [X] T003 [P] Create `tests/scaleout/docker_test.go`, docker helpers ported from `docs/scale-out-1-to-3/rig/lib.sh`.
  - Type `topo` with fields `prefix`, `net` and `t`, built by `newTopo(t, scenario)`. The prefix is `pgso-<scenario>-<6 hex>`. It creates the network and registers `t.Cleanup`, which on failure dumps the logs of every container with the prefix into `t.Log`. Unless `PGMAN_SCALEOUT_KEEP=1`, the cleanup then removes the containers, volumes and network.
  - Volume helpers: `vol(name)` creates a volume and chowns it to `postgres:postgres` via `docker run --rm -u root --entrypoint chown`. `copyVol(src, dst)` uses `cp -a` via a root one-shot container.
  - Node helpers:
    - `run(node, env map[string]string)` runs `docker run -d --name <prefix>-node-<x> --hostname node-<x> --network <net> --network-alias node-<x>` with `-e` per env entry and the `-data` and `-js` volumes mounted;
    - `stop(node, timeout)`, `rm(node)`, `logs(node)`, `wait(node) (exitCode)`;
    - `exec(node, args...)`, using an `exec.CommandContext` with a timeout.
- [X] T004 [P] Create `tests/scaleout/node_test.go`.
  - Env presets as Go maps: `commonEnv()` (copy of `rig/common.env`), `singleEnv("a")`, `threeEnv(x)` (peers a,b,c, declared size 3, route peers = the other two `node-y:6222`).
  - `withoutReplHBA(env)` replaces `PGMAN_PROXY_POSTGRES_HBA_EXTRAS` with `host all all 0.0.0.0/0 trust` only.
  - Probes per contracts/scaleout-suite.md, via `psql -h /var/run/postgresql -U postgres -XAtq` in `docker exec`: `role`, `sysid`, `rows`, `ssn`, `write` (10 s `statement_timeout`), `streaming`.
  - `waitFor(t, budget, interval, cond)` returns the elapsed time or fails with the last observed value.
- [X] T005 [P] Create `tests/scaleout/golden_test.go` with `buildGolden(tp *topo, rows int, env map[string]string) golden{sysid, rows}`.
  - Starts a single node-a and waits for primary (≤ 90 s).
  - Loads `bulk(id bigint primary key, pad text)` with `rows` rows of 900-char pad.
  - Leaves node-a **running** as a writable primary. Expansions start from a live single node, so the measured write outage includes the operator's restart of node-a.
  - On builds without WI-A, it applies and then resets the rig's `ALTER SYSTEM` workaround, so the helper works for baselines.
  - Default rows: 50,000 (research R-010).
- [X] T006 Add a `scaleout` job to `.github/workflows/nightly-integration.yml`.
  - Copy the integration job's checkout, auth, setup-go, "Resolve pinned pg-manager version" and sibling-checkout steps.
  - Set `timeout-minutes: 60` and run `make scaleout` in `pgman-proxy`.
  - Add an `if: always()` sweep step: `docker ps -aq --filter name=pgso- | xargs -r docker rm -f` and `docker volume ls -q --filter name=pgso- | xargs -r docker volume rm -f`.

**Checkpoint**: `make scaleout` compiles and runs; with no scenarios yet, it passes trivially.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: the shared "complete database" predicate used by the boot-window gate (US2/US3) and the boot-time managed blocks (US2), plus verification of its premise.

- [X] T007 [P] Write table tests in `internal/runtime/campaign_gate_test.go` for `pgdataComplete(dir)`:
  - `""` → false;
  - empty dir → false;
  - only `PG_VERSION` → false;
  - `PG_VERSION` + `global/pg_control` → true;
  - `global/pg_control` is a directory → false;
  - unreadable `global/` (chmod 000; skip when root) → error.
- [X] T008 Implement `pgdataComplete(dir string) (bool, error)` in `internal/runtime/campaign_gate.go`, per data-model.md §2.
  - Both files must be regular files.
  - not-exist → `(false, nil)`; other stat errors → `(false, err)`.
  - The doc comment cites research R-003 and the upstream request in contracts/upstream-015.md.
  - Do not write the lowercase engine command name in code outside comments (lcm-discipline-gate).
- [X] T009 Verify the R-003 premise with a throwaway script; no code ships.
  - In a container from the integration image:
    - create a primary with about 1 GB;
    - run a clone into an empty dir and kill it at about 50%;
    - list `PG_VERSION` and `global/pg_control`.
  - Record the result (command and listing) in research.md under R-003 as "[Verified 2026-10-xx]".
  - If `pg_control` is present in the partial dir, switch `pgdataComplete` to the documented fallback (no `backup_label`), and update T007/T008 and data-model.md §2.

**Checkpoint**: predicate merged and its premise verified.

---

## Phase 3: User Story 1: A single-node deployment accepts writes and survives restarts (Priority: P1) 🎯 MVP

**Goal**: a single peer commits immediately and is writable again after a restart (F1, F2). No upstream dependency. Ships first as PR-1.

**Independent Test**: `go test -count=1 -tags=scaleout -run TestSingleNode ./tests/scaleout/...` (S5, S5b), plus the rig's `single-writes` and `single-restart`.

### Tests for User Story 1

- [X] T010 [P] [US1] Write a table test in `internal/runtime/replication_policy_test.go` for `replicationPolicy(cfg)`:
  - 1 peer → `pgmanager.AsyncOnly{}`;
  - 2 peers, min_sync 1 → `QuorumSync{MinSync:1, Pool:[a,b]}`;
  - 3 peers, min_sync 2 → `QuorumSync{2,[a,b,c]}`.

  Also test `policyLabel`: `async`, `quorum(min_sync=1)`.
- [X] T011 [P] [US1] Write `TestSingleNodeFreshWritesAndRestart` (S5) in `tests/scaleout/single_node_test.go`.
  - Fresh single node-a.
  - Assert, after primary:
    - `write` ok within 10 s;
    - `ssn == ""`;
    - the logs contain `"replication_policy":"async"`.
  - Insert 100 rows, then `docker stop -t 30` and start again.
  - Assert primary within 30 s, `write` ok, and 100 rows kept.
- [X] T012 [P] [US1] Write `TestSingleNodeStaleSyncNamesUpgrade` (S5b) in `tests/scaleout/single_node_test.go`.
  - Setup:
    - fresh single node, primary;
    - stop it;
    - append `synchronous_standby_names = 'ANY 1 ("node-a")'` to `postgresql.conf` via a root one-shot container on the data volume (reproduces a pre-005 initdb);
    - start it, and wait for primary (≤ 30 s).
  - If `!upstream015()`:
    - assert `write` times out;
    - run the documented step (`ALTER SYSTEM SET synchronous_standby_names = ''` + `SELECT pg_reload_conf()`);
    - assert `write` ok within 30 s.
  - Else assert `write` ok within 30 s with no step.

### Implementation for User Story 1

- [X] T013 [US1] Create `internal/runtime/replication_policy.go`.
  - `replicationPolicy(cfg config.Config) pgmanager.ReplicationPolicy`, per research R-001; use the doc comment from `docs/scale-out-1-to-3/plan.md` WI-A.
  - `policyLabel(p pgmanager.ReplicationPolicy) string`.
  - In `internal/runtime/start.go` `policyFromConfig` (line ~648), replace the inline `pgmanager.QuorumSync{...}` literal with `replicationPolicy(cfg)`.
- [X] T014 [US1] In `internal/runtime/start.go`, add `pgmanager.Field{Key: "replication_policy", Value: policyLabel(mgrCfg.Policy.Replication)}` to the `"manager started"` Info log (line ~492). Capture the label after `manager.New` so the field reflects what was handed to the engine (contracts/observability.md).
- [X] T015 [US1] Make `make test` and the US1 scaleout tests pass. Also run `docs/scale-out-1-to-3/rig/scenarios.sh single-writes` (expect `W_OK`) and `golden` + `single-restart` (expect primary in under 30 s and `W_OK`) against a freshly built `pgman-proxy-it:latest`. Paste the outputs into the PR description.
- [X] T016 [US1] Add a `CHANGELOG.md` entry under Unreleased (FR-005, FR-021).
  - **Fixed**: single-node commits hung (F1) and single nodes stayed read-only after any restart (F2).
  - **Upgrade note**: the exact one-time `ALTER SYSTEM SET synchronous_standby_names = ''; SELECT pg_reload_conf();` step for single nodes initialized by earlier releases. It is needed until the pg-manager 015 bump.

**Checkpoint**: US1 is shippable alone (PR-1).

---

## Phase 4: User Story 2: Expand a single node to three without losing data (Priority: P1)

**Goal**: config push + start b, c + restart a ⇒ node-a stays primary with its data, b and c clone and stream, write outage under 30 s.

**Independent Test**: `PGMAN_SCALEOUT_UPSTREAM_015=1 go test -tags=scaleout -run 'TestExpand' ./tests/scaleout/...` (S1, S7).

### Tests for User Story 2

- [ ] T017 [P] [US2] Write unit tests in `internal/embedded/store_layout_test.go`, covering every case listed in contracts/jetstream-store-layout.md "Tests". Use `t.TempDir()` fixtures: create `jetstream/$SYS/_js_/_meta_` for clustered, and `jetstream/<acct>/streams` for standalone. Assert that the parent dir inode (`os.SameFile`) and mode are unchanged after an archive.
- [ ] T018 [P] [US2] Write unit tests in `internal/runtime/managed_block_test.go`, covering every row of the contracts/managed-conf-blocks.md semantics table, plus:
  - CRLF input;
  - no trailing newline;
  - modes 0600 and 0640 preserved;
  - legacy-marker coexistence;
  - idempotence (the second apply is `unchanged`, with no write; mtime unchanged);
  - a read-only parent dir → error, with the original content intact.
- [ ] T019 [P] [US2] Extend `internal/runtime/campaign_gate_test.go` with a table for `campaignGate(m, dataDir)`, with the manager abstracted behind a tiny interface so the test needs no real Manager:
  - nil manager + complete PGDATA → true;
  - nil manager + empty → false;
  - nil manager + stat error → false;
  - non-nil manager → delegates (true/false).
- [ ] T020 [P] [US2] Write `TestExpandOneToThree` (S1) in `tests/scaleout/expand_test.go`. **(needs 015)**
  - Iterations: `PGMAN_SCALEOUT_ITERATIONS` (default 5).
  - Start order: round-robin over {a-first, a-last, together}, shuffled with a logged seed.
  - Per iteration:
    - build golden (50,000 rows) in a fresh topo;
    - start a background writer goroutine that runs `write(node-a)` every 250 ms and records the longest gap;
    - apply the 3-node env to all three nodes and start them in the chosen order.
  - Assert, per contracts/scaleout-suite.md S1, within 4 min:
    - sysid equal on a, b, c and equal to golden;
    - `rows` equals golden on all three nodes;
    - node-a is primary and `streaming(a) == 2`;
    - `ssn(a)` contains `"node-b"` and `"node-c"`;
    - b/c logs lack `initdb: starting` and contain `bootstrap: starting basebackup` with `node-a`;
    - node-a logs contain `embedded_nats.store_archived` with `"actual_layout":"standalone"`;
    - the longest write gap is under 30 s.
- [ ] T021 [P] [US2] Write `TestExpandWithoutPriorReplicationHBA` (S7) in `tests/scaleout/expand_test.go`.
  - Build golden with `withoutReplHBA(env)`.
  - Expand with the full HBA extras.
  - Always assert:
    - node-a's `pg_hba.conf` (read via `docker exec cat`) contains the `# BEGIN pgman-proxy managed: postgres.hba_extras` block with the replication line;
    - node-a's logs contain `postgres.managed_block_applied` with `"phase":"boot"`.
  - If `upstream015()`, also assert that b and c reach streaming within 4 min.

### Implementation for User Story 2

- [ ] T022 [US2] Create `internal/embedded/store_layout.go` with `StoreLayout`, `DetectStoreLayout` and `ArchiveIncompatibleStore`, exactly per contracts/jetstream-store-layout.md (rules 1-6).
- [ ] T023 [US2] Wire store archiving in `internal/runtime/start.go` `bootEmbeddedNATS`, after `embedded.BuildOptions` and before `embedded.NewServer` (line ~791).
  - Call `embedded.ArchiveIncompatibleStore(cc.JetStreamDir, routesEnabled, time.Now())`, reusing the existing `routesEnabled` variable.
  - On error, return `&StartupError{Code: ExitConfig, Err: fmt.Errorf("archive incompatible jetstream store in %q: %w", cc.JetStreamDir, err)}`.
  - On a non-empty `to`, emit `logger.Event("embedded_nats.store_archived", ...)` at Warn with the fields from contracts/observability.md.
  - Check whether `obs.Logger.Event` supports a level. If it does not, use `logger.Warn` with an `event` field, matching how other Warn-level events are emitted in `start.go` (for example `embedded_nats.replica_advisory`).
- [ ] T024 [US2] Create `internal/runtime/managed_block.go` with `BlockAction`, `applyManagedBlock` and `applyManagedConf`, per contracts/managed-conf-blocks.md.
  - The atomic write uses `os.CreateTemp(filepath.Dir(path), ".pgman-proxy-*")`, then `Chmod` to the original mode, `Sync`, `Close` and `os.Rename`.
  - `applyManagedConf` emits `postgres.managed_block_applied` per changed block, and returns `changed`.
- [ ] T025 [US2] Rewire `postInitDBHook` in `internal/runtime/start.go` (line ~845).
  - Replace both `appendLines` calls with `applyManagedConf(cfg, dataDir, "post_initdb", logger)`.
  - Keep returning nil when both lists are empty.
  - Delete `appendLines` (now unused).
  - Keep the two `"postgresql.conf"` / `"pg_hba.conf"` string literals, which the lcm-discipline-gate allow-lists.
- [ ] T026 [US2] Add the boot-time apply in `internal/runtime/start.go`, right after the `ensureStandbySignalIfInitialized` call (line ~272).
  - If `ok, err := pgdataComplete(cfg.Postgres.DataDir)` returns an error, fail with `ExitDeps`.
  - If `ok`, call `applyManagedConf(cfg, cfg.Postgres.DataDir, "boot", res.Logger)`.
  - If `changed`, `running, rerr := pg.IsRunning(ctx)`; when `running`, call `pg.Reload(ctx)`. Include `reloaded` in a follow-up Info log.
  - Any error → `&StartupError{Code: ExitDeps, Err: ...}`.
  - `pg` is the executor built at gate #6. Move this block after gate #6 if it is not already after it.
- [ ] T027 [US2] Implement the boot-window campaign gate in `internal/runtime/campaign_gate.go`.
  - Define `type campaigner interface{ ShouldCampaign() bool }` and `func campaignGate(m campaigner, dataDir string) bool`.
  - It handles a typed-nil `*manager.Manager` correctly: check `m == nil` via a helper that takes `*manager.Manager`, or pass an explicit nil interface.
  - Replace the closure at `start.go:174` with `func() bool { if mm := mgrRef.Load(); mm != nil { return mm.ShouldCampaign() }; return bootWindowCampaign(cfg.Postgres.DataDir, res.Logger) }`.
  - `bootWindowCampaign` logs `campaign: boot-window deferral` once (edge-triggered via `sync.Once`) when it returns false, with field `reason` = `pgdata_empty` or `pgdata_unreadable`.
  - Update the comment above `mgrRef` (it currently says the boot window "campaigns normally").
- [ ] T028 [US2] **(needs 015)** Bump pg-manager (WI-C5).
  - Run `go get github.com/f1bonacc1/pg-manager@<015 tag>`, apply the coupled pgx bump if pg-manager moved pgx (update the go.mod comment), then `go mod tidy`.
  - Remove any temporary development `replace`.
  - Run `make test`, `make integration` and `make scaleout`.
  - If the upstream major-upgrade gate message changed, re-pin the 004 sentinel in `tests/integration/lcm_upgrade_test.go` and `internal/control/handlers_upgrade_test.go`, and the reference in CLAUDE.md (research R-013).
- [ ] T029 [US2] **(needs 015)** Run the rig acceptance against the 015 build: `docs/scale-out-1-to-3/rig/scenarios.sh golden && scenarios.sh race` must print `VERDICT: PASS`, and `a-wins` must print PASS. Record the outputs in the PR.

**Checkpoint**: expansion works end to end (S1 5/5, S7).

---

## Phase 5: User Story 3: Empty nodes never create a competing database (Priority: P2)

**Goal**: fresh clusters still bootstrap exactly once; with the data node absent, empty nodes wait.

**Independent Test**: `go test -tags=scaleout -run 'TestFreshCluster|TestDataNodeDown|TestSoloInThreeNodeConfig' ./tests/scaleout/...`

- [ ] T030 [P] [US3] Write `TestSoloInThreeNodeConfig` (S4) in `tests/scaleout/solo_test.go`.
  - Build golden, then start node-a alone with `threeEnv("a")`.
  - Assert that `wait(node-a)` returns 75 after 110-180 s.
  - Restart with `singleEnv("a")`, wait until Postgres answers queries (primary or standby, either is fine for reads), and assert that the golden sysid and row count are unchanged.
  - The golden was built by a WI-A build, so its `initdb` conf already has an empty sync-names value, and no S5b step is needed.
- [ ] T031 [P] [US3] Write `TestFreshClusterBootstrapsOnce` (S2) in `tests/scaleout/fresh_test.go`.
  - Start three empty nodes with the 3-node env, together.
  - Within 4 min, assert:
    - exactly one node's logs contain `initdb: starting`;
    - the other two contain `bootstrap: starting basebackup`;
    - the sysid is equal on all three;
    - `write` succeeds on the primary.
  - Call `requireUpstream015(t)` only for the exactly-once assertion; the rest runs on today's engine.
- [ ] T032 [P] [US3] Write `TestDataNodeDownEmptyNodesWait` (S6) in `tests/scaleout/data_node_down_test.go`. **(needs 015)**
  - Build golden for node-a but leave node-a stopped. Start b and c with the 3-node env.
  - For 180 s, poll every 10 s and assert:
    - no `initdb: starting` in b/c logs;
    - `docker exec ls /var/lib/postgresql/data/PG_VERSION` fails on b and c;
    - no node answers `write`.
  - Assert b/c logs contain the deference-armed line pinned in contracts/upstream-015.md.
  - Then start node-a with the 3-node env and reuse the S1 assertion helper.
- [ ] T033 [US3] Extract the shared S1 assertion helper `assertExpanded(t, tp, goldenSysid, goldenRows)` into `tests/scaleout/expand_test.go`, and use it in T020 and T032.

**Checkpoint**: the safety boundaries hold.

---

## Phase 6: User Story 4: Roll back to a single node by reverting the configuration (Priority: P2)

**Goal**: stop b and c, then revert node-a ⇒ writable single node within 30 s with its data.

**Independent Test**: `go test -tags=scaleout -run TestRollbackToSingle ./tests/scaleout/...`

- [ ] T034 [US4] Write `TestRollbackToSingle` (S3) in `tests/scaleout/rollback_test.go`. **(needs 015)**
  - Expand (reuse T020's setup with one iteration, a-last).
  - Stop b and c (`docker stop -t 30`), then stop node-a.
  - Start node-a with `singleEnv("a")`.
  - Assert:
    - primary and `write` ok within 30 s;
    - `ssn == ""`;
    - rows ≥ golden;
    - node-a logs contain `embedded_nats.store_archived` with `"actual_layout":"clustered"`.
  - Then restart node-a again with the same config and assert that no second archive event occurs (idempotence).

**Checkpoint**: rollback is a plain config revert.

---

## Phase 7: User Story 5: Identity mismatches are visible (Priority: P3)

**Goal**: doctor check, gauge and event for an upstream-reported identity mismatch.

**Independent Test**: `go test -race -run 'Identity' ./internal/control/... ./internal/runtime/...` (fault injection), plus the S1/S6 real-server runs, which must report no mismatch.

- [ ] T035 [P] [US5] Write a contract test in `internal/control/handlers_doctor_test.go`. **(needs 015 for the Status field)**
  - Extend `fakeEngine` to return a `Status` with the upstream identity-mismatch field set and unset, and unknown.
  - Assert that `cluster.identity` returns fail / pass / warn with evidence `doctor.evidence.cluster-identity/v1` and both identifiers.
  - Assert that the check stays in the read-only test's coverage (it calls `Status` only).
- [ ] T036 [US5] Add the `cluster.identity` check to `DefaultDoctorChecks` in `internal/control/doctor_checks.go`. **(needs 015)**
  - Append it at the end, keeping the catalogue order stable for pgmctl.
  - Use the suggested-fix text from contracts/observability.md.
- [ ] T037 [US5] Add the gauge `IdentityMismatch prometheus.Gauge` (`pgman_proxy_cluster_identity_mismatch`) to `internal/obs/metrics.go` and register it. Set it from the observability gauge loop in `startObservabilityGauges` (`internal/runtime`), reading `Manager.Status()`. **(needs 015)**
- [ ] T038 [US5] Make sure the upstream `cluster.identity_mismatch` topic reaches logs and history. **(needs 015)**
  - Check whether `internal/cluster` `SubscribeCoordinationEvents` subscribes by wildcard or by an explicit topic list.
  - Add the topic if it is a list.
  - Add a unit test in the cluster package's existing event-subscription test file.
- [ ] T039 [US5] In `tests/scaleout/expand_test.go` `assertExpanded`, also assert that the `pgman_proxy_cluster_identity_mismatch` gauge reads 0 on all three nodes. Read it by `docker exec` scraping of `:9090/metrics` with `wget -qO-` or `curl` (whichever the image has; else use the control-plane doctor endpoint). **(needs 015)**

**Checkpoint**: a silent split becomes an alert.

---

## Phase 8: Polish & Cross-Cutting Concerns

- [ ] T040 [P] Amend `specs/002-embedded-nats-cluster/spec.md` User Story 2 acceptance scenario 2 (FR-020).
  - Replace the SIGHUP scale-out promise with the restart-based procedure.
  - Add a dated note citing 005 and the reason:
    - size 1 has no routes listener;
    - `declared_size` and `routes_listen` are startup-only;
    - a lone NATS server cannot become a cluster member (`raft.go:364`).
- [ ] T041 [P] Write `docs/scale-out-1-to-3/runbook.md` (FR-019). Cover:
  - prerequisites;
  - the config delta table from `docs/scale-out-1-to-3/plan.md` §2;
  - the procedure (push config, start b and c, restart a);
  - what happens;
  - verification (quickstart.md §5);
  - rollback (stop b and c FIRST; research R-011);
  - what is lost (KV fences, cooldown and audit history; the archived store paths and when to delete them);
  - the one-time step for pre-005 single nodes (until 015);
  - a troubleshooting section: exit 75 after 120 s, deference logs, the identity-mismatch doctor check;
  - "config sanity", noting that `len(peers)` should equal `declared_size` (research R-001 note).
- [ ] T042 [P] Extend the `CHANGELOG.md` Unreleased section (FR-021).
  - **Added**: safe 1→3 expansion, the scaleout suite, the identity check and gauge.
  - **Changed**: stores are moved aside at boot (both directions); `hba_extras`/`conf_extras` are now managed blocks applied on every boot, legacy blocks are untouched, and reload happens when Postgres is running.
  - **Upgrade note**: list the new log events.
- [ ] T043 Move `docs/scale-out-1-to-3/plan.md` status line from "plan only" to "implemented in feature 005", and replace the rig README pointer in `docs/scale-out-1-to-3/rig/scenarios.sh` (header comment "Replace with tests/scaleout once WI-7 lands") with "superseded by tests/scaleout (make scaleout)".
- [ ] T044 Run the gates locally:
  - `make lint`;
  - `make test`;
  - `make grep-gates`;
  - the lcm-discipline-gate grep from `.github/workflows/lcm-discipline-gate.yml`, run by hand against `cmd internal`;
  - the scope-gate grep from `.github/workflows/scope-gate.yml`.

  Fix any hit. In particular, no lowercase engine command names in new non-comment code.
- [ ] T045 Draft the PR description(s) per plan.md "Delivery slices".
  - Include the Constitution IV flag for WI-C3, with a link to a newly filed pg-manager follow-up issue ("own HBA/conf extras rendering"). Draft its text in the PR; filing it is a user action.
  - Include the R-013 version-collision note.
  - Include the rig before/after outputs.
- [ ] T046 Run quickstart.md end to end (sections 1-4) on a clean docker host and record any drift.

---

## Dependencies & Execution Order

### Phase Dependencies

- **Phase 1 (Setup)**: no dependencies.
- **Phase 2 (Foundational)**: depends on Phase 1 only for T009's image; T007/T008 can start immediately.
- **US1 (Phase 3)**: depends on Phase 1 (harness). It does not depend on Phase 2.
- **US2 (Phase 4)**: depends on Phase 2 (T008 is used by T026/T027).
  - T022-T027 can be built and unit-tested without 015.
  - T020, T028 and T029 need 015.
- **US3 (Phase 5)**: depends on US2's T027 (boot-window gate) and on 015. T030 (S4) runs on today's engine.
- **US4 (Phase 6)**: depends on US2 (T022/T023) and 015.
- **US5 (Phase 7)**: depends on 015 (the Status field and topic) and on T033 for T039.
- **Polish**: T040 is independent and can land any time. T041/T042 come after behavior is final.

### User Story Dependencies

- US1 is independent: it is the MVP and PR-1.
- US2 ← Foundational. Its full green run ← 015.
- US3, US4 ← US2 code + 015.
- US5 ← 015.

### Parallel Opportunities

- **Phase 1**: T003, T004 and T005 are separate files.
- **Phase 2**: T007 can be written while T009 runs.
- **US1**: T010, T011 and T012.
- **US2**: T017, T018, T019, T020 and T021 are separate files. The implementation pairs (T022 with T023, and T024 with T025/T026) can be split between two developers.
- **US3**: T030, T031 and T032.
- **Polish**: T040, T041 and T042.

### Parallel Example: after Phase 2

```text
Developer A: T017 → T022 → T023            (store layout)
Developer B: T018 → T024 → T025 → T026     (managed blocks)
Developer C: T019 → T027                   (boot-window gate)
```

---

## Implementation Strategy

### MVP First (User Story 1 only)

1. Phase 1 (harness) → Phase 3 (US1).
2. Validate S5/S5b and the rig `single-writes` / `single-restart` outputs.
3. Ship PR-1. Single-node mode is fixed for every user, independent of upstream.

### Incremental Delivery

1. **PR-1**: Setup + US1.
2. **PR-2**: T040 (spec 002 amendment).
3. **PR-3**: Foundational + T017/T022/T023 (store layout).
4. **PR-4**: T018/T024-T026 (managed blocks) + T021 (S7, partial pre-015).
5. **In parallel**, pg-manager 015 through its own speckit flow (input: `docs/scale-out-1-to-3/plan.md` WI-B + contracts/upstream-015.md).
6. **PR-5**: T019/T027 (boot-window gate) + T028 (bump) + US3 + US4 + US5.
7. **PR-6**: Polish (runbook, CHANGELOG, plan status).

### Definition of done (from the technical plan §4)

- Every rig scenario passes on the fixed build; `race` and `a-wins` print `VERDICT: PASS`.
- `make scaleout` is green, with 5/5 S1 iterations.
- `make integration` is green and unchanged.
- The runbook is merged.
