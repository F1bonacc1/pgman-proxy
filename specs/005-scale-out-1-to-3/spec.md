# Feature Specification: Safe 1→3 Scale-out and Working Single-Node Mode

**Feature Branch**: `005-scale-out-1-to-3`
**Created**: 2026-10-14
**Status**: Draft
**Input**: User description: "Safe 1→3 scale-out and working single-node mode for pgman-proxy (feature 005). Source of truth: docs/scale-out-1-to-3/plan.md (verified facts F1-F16, work items WI-A and WI-C1..C7; WI-B is the upstream pg-manager milestone 015-empty-peer-safety and is a prerequisite, not in scope here)."

## Overview

pgman-proxy promises two deployment shapes: a single self-contained
node, and a three-node highly available cluster. A docker reproduction
run on 2026-10-14 (documented in `docs/scale-out-1-to-3/plan.md`,
section 1) showed that neither the single-node shape nor the path
between the two shapes works today:

- **A single node cannot commit.** A freshly initialized single node
  requires a synchronous standby that can never exist, so every write
  waits forever (F1).
- **A single node never recovers from a restart.** After any restart
  it stays a read-only standby indefinitely, because promotion is
  refused for lack of a quorum record that a one-node cluster never
  publishes (F2).
- **Expanding to three nodes replaces the data.** Following the
  obvious procedure (push the 3-node configuration, start the two new
  nodes, restart the original) ended with a brand-new, empty database
  as primary in 3 of 3 trials (F4, F5). Clients were silently routed to
  it, and pgman-proxy logged nothing (F6).
- **Existing nodes miss config changes.** An existing single node
  never receives the replication access rules that the new nodes need
  to clone it, because configured access rules are only applied when a
  database is first created (F11).
- **The coordination store is reused unsafely.** A store created in
  single-node mode is reused in clustered mode (and the reverse on
  rollback) with undefined results (F13).
- **The published spec over-promises.** Spec 002 promises a
  reload-only scale-out that the coordination layer cannot perform.

This feature makes the single-node shape work, and makes 1→3 expansion
a config-and-restart procedure that keeps the original node's data,
with a short write outage. Most of the coordination safety logic
belongs to the wrapped engine (pg-manager milestone 015, a
prerequisite). This feature covers the proxy's side: the single-node
replication mode, the boot window before the engine starts, the
coordination store layout, managed access rules, surfacing identity
mismatches, the end-to-end test suite and the operator documentation.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A single-node deployment accepts writes and survives restarts (Priority: P1)

An operator runs pgman-proxy as one node for a small or development
deployment. They expect it to accept writes immediately and to come
back writable after a restart or host reboot, without any manual
database steps.

**Why this priority**: The single-node shape is broken today for every
user: commits hang on a fresh node, and any restart leaves it
read-only forever. This is also the starting point of every 1→3
expansion. The fix is small, independent of the upstream prerequisite,
and ships first.

**Independent Test**: Start one node with a single-peer configuration
on empty storage against a real database, commit a write, restart the
node, and commit another write. Delivers value alone: the single-node
shape becomes usable.

**Acceptance Scenarios**:

1. **Given** a single-peer configuration and an empty data directory,
   **When** the node becomes primary, **Then** a write commits within
   10 s.
2. **Given** a single node holding data, **When** it is restarted with
   the same configuration, **Then** it becomes primary and writable
   within 30 s, and all previously committed rows are present.
3. **Given** a single node initialized by an earlier release (whose
   database still carries the stale synchronous-standby setting),
   **When** it is restarted on this release, **Then** it becomes
   writable within 30 s. Until the upstream prerequisite ships, a
   documented one-time manual step is acceptable for this case; once it
   ships, no manual step is required.
4. **Given** any configuration, **When** a node starts, **Then** its
   startup log states the effective replication mode (asynchronous for
   one peer, quorum with its minimum synchronous count otherwise).
5. **Given** a configuration with two or more peers, **When** a node
   starts, **Then** the replication behavior is unchanged from the
   previous release.

---

### User Story 2 - Expand a single node to three without losing data (Priority: P1)

An operator outgrows a single node and expands it to three. They
provision two empty hosts, push the 3-node configuration to all three,
start the two new nodes and restart the original. They expect the
original node to stay the primary with all of its data, and the new
nodes to clone from it and follow it, with only a short write outage
and no manual database steps.

**Why this priority**: This is the feature's headline capability and
today it destroys the user's data from the clients' point of view (the
empty database becomes primary). It depends on the upstream engine
prerequisite, so it lands after User Story 1.

**Independent Test**: Create a single node with a known data set,
expand it to three following the documented procedure, and verify a
single database identity, all rows on all three nodes, the original
node as primary and both new nodes streaming. Repeat with the original
node started first, last and at the same time as the new nodes.

**Acceptance Scenarios**:

1. **Given** a single node holding data and two empty hosts, **When**
   the operator follows the expansion procedure in any start order,
   **Then** the original node ends as primary with all of its data, both
   new nodes end as standbys holding identical data, and no new
   database is created on any node.
2. **Given** an expansion in progress, **When** the original node has
   restarted but the new nodes have not finished cloning, **Then** the
   original node accepts writes (with the same durability it had as a
   single node), and the client-visible write outage across the whole
   procedure is under 30 s.
3. **Given** both new nodes have finished cloning and are streaming,
   **When** a client commits, **Then** the commit is confirmed only
   after at least one new node has acknowledged it (synchronous
   replication is restored).
4. **Given** a single node created without replication access rules,
   **When** the operator adds those rules to the 3-node configuration
   and expands, **Then** both new nodes clone successfully with no
   manual edit of the database's access files.
5. **Given** the original node's coordination store was created in
   single-node mode, **When** the original node restarts in the 3-node
   configuration, **Then** the old store is moved aside (kept, not
   deleted), a fresh clustered store is used, and a warning names the
   old and new locations.

---

### User Story 3 - Empty nodes never create a competing database (Priority: P2)

An operator needs two guarantees from the expansion safety
mechanism. A brand-new cluster of three empty nodes still bootstraps
exactly as before. And when the only node holding data is absent, the
empty nodes wait instead of creating a new database that clients would
be routed to.

**Why this priority**: These are the safety boundaries of User Story 2.
A mechanism that makes empty nodes defer must not deadlock a fresh
cluster, and must not let empty nodes take over when the data holder
is merely late or down.

**Independent Test**: (a) Start three empty nodes with a 3-node
configuration and verify exactly one database initialization and a
healthy cluster. (b) Start only the two empty nodes of an expansion,
with the data-holding node kept down, and verify that no database is
created and no node accepts writes for 3 minutes; then start the data
node and verify the expansion completes.

**Acceptance Scenarios**:

1. **Given** three empty nodes with a 3-node configuration, **When**
   they start in any order, **Then** exactly one node initializes a
   database, the other two clone from it, and the cluster becomes
   healthy.
2. **Given** an expansion where the data-holding node is down, **When**
   the two empty nodes run for 3 minutes, **Then** no database is
   created, no node accepts writes, and each empty node logs that it is
   deferring to peers that have data or have not reported.
3. **Given** the scenario above, **When** the data-holding node is then
   started, **Then** the expansion completes as in User Story 2.
4. **Given** a node that has not yet started its engine (the boot
   window), **When** its data directory is empty or cannot be
   inspected, **Then** it does not compete for leadership during that
   window.

---

### User Story 4 - Roll back to a single node by reverting the configuration (Priority: P2)

An operator whose expansion failed, or who wants to go back to a
single node, stops the new nodes, reverts the original node to its
single-node configuration and restarts it. They expect it to come back
as a writable single node with its data.

**Why this priority**: A safe escape hatch makes the expansion low-risk
to attempt. Today a reverted node stays read-only forever (F9).

**Independent Test**: After an expansion (successful or failed), stop
all nodes, revert the original node to the single-node configuration,
start it, and verify it is writable within 30 s with all of its data.

**Acceptance Scenarios**:

1. **Given** an expanded or partially expanded cluster with the new
   nodes stopped, **When** the original node is restarted with its
   single-node configuration, **Then** it is primary and writable within
   30 s with all of its data.
2. **Given** that rollback, **When** the node starts, **Then** the
   clustered coordination store is moved aside (kept, not deleted) and
   the stale synchronous-standby setting is cleared.

---

### User Story 5 - Identity mismatches are visible (Priority: P3)

An operator needs to know when a standby is following a primary whose
database identity differs from its own. That situation means two
unrelated databases exist in one cluster, and today nothing in
pgman-proxy reports it.

**Why this priority**: After User Stories 2 and 3 this should never
happen. It is the backstop that turns a silent split into an alert.

**Independent Test**: Force a standby to follow a primary with a
different database identity, and verify the diagnostics check fails,
the metric reports the mismatch and an event is recorded.

**Acceptance Scenarios**:

1. **Given** a standby whose database identity differs from the
   leader's, **When** the operator runs the diagnostics check, **Then**
   the identity check fails and names both identities.
2. **Given** the same state, **When** metrics are scraped, **Then** the
   identity-mismatch indicator reads 1, and it reads 0 when identities
   match.
3. **Given** the same state, **When** it is first detected, **Then** an
   event is recorded in the node's event history and logs. No automatic
   corrective action is taken.

---

### Edge Cases

- **Original node alone in the 3-node config** (the new nodes never
  start): it exits with the existing distinct "route mesh not formed"
  exit code after about 120 s, and its database is untouched.
- **No persistent coordination-store directory** (single peer, in
  memory): nothing is moved aside.
- **The store directory is a mount point**: only its contents are
  renamed, never the directory itself.
- **Moving the store aside fails**: startup is refused with an error
  naming the path. The node never falls back to reusing an
  incompatible store.
- **The node boots again after the store was moved aside**: nothing
  further is moved (idempotent).
- **The operator removes all configured access rules or settings**: the
  managed section is removed from the files, and everything outside it
  is preserved.
- **Files carry sections appended by earlier releases**: the old
  sections are left in place, and the managed section is authoritative
  from this release on. Duplicate access rules are harmless because the
  first match wins.
- **The database is already running at boot** (for example a container
  restart where the database survived): changed managed content is
  applied with a configuration reload, without a restart.
- **The data directory cannot be inspected during the boot window**:
  the node is treated as empty and does not compete for leadership
  (fail-closed).
- **A new node's clone fails part-way**: the new node must not be
  treated as data-bearing and must not compete for leadership. The
  original node keeps serving writes with single-node durability, and
  the new node retries.
  - An orderly clone failure removes the partial directory (the
    upstream executor does this today).
  - A process crash in the middle of a clone, or of database
    initialization, can leave a partial directory behind. That
    directory MUST NOT count as an initialized database, either in the
    boot-window check or in the upstream data-presence record.
- **Rollback while the new nodes are still running**: not supported,
  and the runbook must say so. Two streaming new nodes still form a
  two-of-three coordination majority, so one of them could be promoted
  while the reverted original also runs as a single node.
- **Coordination state lost in the rebuild**: fences, cooldown
  history and audit history from the single-node era are lost (the
  history store is moved aside with the rest). This is accepted, and
  the runbook must document it.

## Requirements *(mandatory)*

### Functional Requirements

**Single-node mode (User Story 1)**

- **FR-001**: A node configured with exactly one peer MUST run with
  asynchronous replication: commits MUST NOT wait for any standby
  acknowledgement, and promotion MUST NOT require a synchronous quorum
  record.
- **FR-002**: Nodes configured with two or more peers MUST keep the
  existing quorum-synchronous behavior, including the configured
  minimum synchronous count and the peer pool, unchanged.
- **FR-003**: The single-node replication mode MUST be derived from the
  configured peer count. No new configuration key is introduced, so
  every existing configuration keeps its meaning (Constitution VII).
- **FR-004**: Every node MUST log its effective replication mode at
  startup as a structured field.
- **FR-005**: The release notes MUST document the one-time manual step
  that clears the stale synchronous-standby setting on single nodes
  initialized by earlier releases, until the upstream prerequisite
  makes it unnecessary.

**Boot window and expansion safety (User Stories 2, 3)**

- **FR-006**: Until the engine is running, a node MUST compete for
  leadership only if its database data directory holds a complete
  database. A directory left behind by an interrupted clone or
  initialization does not count. Any error while inspecting the
  directory MUST be treated as "not initialized".
- **FR-007**: Once the engine is running, leadership competition and
  database initialization MUST follow the upstream empty-node
  deference rules. An empty node defers while any configured peer has
  data or has not reported, and empty peers are excluded from the
  synchronous set until they hold data. pgman-proxy MUST consume an
  upstream release that provides these rules.
- **FR-008**: Expansion MUST NOT create a second database identity:
  after a completed expansion, every node reports the same database
  identity as the original node.

**Coordination store layout (User Stories 2, 4)**

- **FR-009**: At boot, before starting its coordination server, a node
  MUST detect when its persistent coordination store was created in the
  other mode (single-node vs clustered). It MUST then move the store
  aside inside the configured store directory, under a name that
  records the old mode and a UTC timestamp. It MUST NOT delete the
  store, and MUST NOT rename the configured directory itself.
- **FR-010**: Moving the store aside MUST emit a warning-level
  structured event naming the old location, the new location and the
  reason.
- **FR-011**: If the store cannot be moved aside, startup MUST fail
  with an error naming the path (Constitution II).
- **FR-012**: When the store layout already matches the mode, or no
  persistent store directory is configured, nothing is moved.

**Managed access rules and settings (User Story 2)**

- **FR-013**: The configured extra access rules and extra settings
  MUST be written into the database's configuration files as clearly
  delimited, pgman-proxy-managed sections. This happens when a database
  is first created, and again on every boot of a node whose database
  already exists.
- **FR-014**: Applying a managed section MUST replace its previous
  content in place. The section MUST be removed when the corresponding
  configuration is empty. Content outside the section MUST be
  preserved. The file's permissions MUST be preserved, and the write
  MUST be atomic (no partially written file is ever visible).
- **FR-015**: When a managed section changes while the database is
  running, the node MUST trigger a configuration reload.

**Identity mismatch (User Story 5)**

- **FR-016**: The node MUST surface an upstream-reported database
  identity mismatch in three ways: a read-only diagnostics check, a 0/1
  metric, and a recorded event. It MUST NOT take automatic corrective
  action.

**Verification and documentation**

- **FR-017**: An end-to-end scale-out test suite MUST exercise, against
  real database and coordination servers:
  - expansion (5 iterations with randomized start order);
  - fresh 3-node bootstrap;
  - rollback;
  - the original node alone in the 3-node configuration;
  - single-node restart;
  - the data node kept down during expansion;
  - expansion of a node created without replication access rules.

  The suite MUST be runnable with one documented command, and MUST run
  in the nightly CI job.
- **FR-018**: The existing integration suite MUST pass unchanged. It
  starts every run from empty nodes, so it also proves the fresh-cluster
  bootstrap path still works.
- **FR-019**: An operator runbook MUST document:
  - the expansion procedure and the configuration changes from one
    node to three;
  - how to verify success;
  - rollback, including stopping the new nodes first;
  - which coordination state is lost, and the stores moved aside that
    the operator may delete after verification.
- **FR-020**: Spec 002 User Story 2 acceptance scenario 2 (reload-only
  scale-out) MUST be amended to the restart-based procedure, with the
  reason recorded.
- **FR-021**: The changelog MUST record:
  - the single-node fixes;
  - safe expansion;
  - the behavior changes: stores moved aside, and managed configuration
    sections.

### Key Entities

- **Replication mode**: the effective replication behavior of a node,
  either asynchronous (one peer) or quorum-synchronous with a minimum
  synchronous count and a peer pool (two or more peers). Derived from
  the configuration and reported at startup.
- **Peer data-presence record** *(upstream)*: per node, whether it
  holds a database, its database identity when known, and when this was
  last reported. Used by the engine to decide which nodes may compete
  for leadership or initialize, and which peers count toward the
  synchronous set.
- **Coordination store**: the persistent on-disk state of the embedded
  coordination server (leadership, cluster state and history). Its
  layout differs between single-node and clustered mode. A store moved
  aside keeps its contents and gains a mode-and-timestamp suffix.
- **Managed configuration section**: a delimited block in a database
  configuration file whose content pgman-proxy owns and rewrites on
  every boot from the configured extra access rules or extra settings.
- **Identity-mismatch status**: whether this node's database identity
  differs from the current leader's, with both identities, as reported
  by the engine.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A fresh single node commits its first write within 10 s
  of becoming primary, and a restarted single node is writable within
  30 s, in 100% of test runs.
- **SC-002**: 5 out of 5 consecutive expansion runs, with randomized
  start order, end with:
  - one database identity on all three nodes;
  - every row present before expansion on all three nodes;
  - the original node as primary;
  - both new nodes streaming.
- **SC-003**: The client-visible write outage during an expansion is
  under 30 s (about 9 s measured on a workstation with the default
  lease).
- **SC-004**: A fresh 3-node cluster started from empty storage
  performs exactly one database initialization in 100% of test runs,
  and the existing integration suite passes unchanged.
- **SC-005**: With the only data-holding node absent, zero databases
  are created and zero nodes accept writes over a 3-minute observation
  window.
- **SC-006**: After rollback, the original node is writable within
  30 s with all of its data.
- **SC-007**: An operator completes an expansion using only
  configuration changes and process restarts, with zero manual
  database commands (excluding the one-time interim step of FR-005 for
  nodes initialized by earlier releases).
- **SC-008**: An identity mismatch is visible in diagnostics, metrics
  and events within 30 s of the standby first following the mismatched
  primary.

## Assumptions

- **Upstream prerequisite.** pg-manager milestone `015-empty-peer-safety`
  (released as v0.7.0) provides:
  - the per-node data-presence record;
  - empty-node deference for leadership and initialization;
  - re-checking before initialization, and stepping down;
  - empty-peer exclusion from the synchronous set and the failover
    quorum, with an asynchronous fallback, and rendering an empty
    synchronous-standby setting instead of skipping it;
  - the initialization-time setting fix (B-024);
  - the identity-mismatch status.

  User Stories 2, 3, 4 and 5 cannot be completed until it ships. User
  Story 1 and the store-layout and managed-section requirements do not
  depend on it.
- **Join-window durability** (decision 3). While every pool peer is
  still empty, the primary runs asynchronously. That equals the
  single-node durability the cluster had before expansion. An empty
  peer cannot hold an acknowledged commit, so excluding it never drops
  a durable write.
- **Gate strictness** (decision 2). An empty node waits for every
  configured peer to report empty, not a majority. The existing
  route-mesh gate already requires every peer to be reachable before
  leadership is contested, so this costs nothing in pgman-proxy.
- **Store direction** (decision 4). The store is moved aside in both
  directions, which makes rollback a plain configuration revert.
- **Access-rule ownership** (decision 6). Managed access rules are
  implemented in pgman-proxy now. Under Constitution IV this is flagged
  in the pull request with a follow-up to move the capability upstream.
- **Accepted losses.** Restarts are acceptable, and losing non-crucial
  coordination state (fences, cooldown history, audit history) is
  accepted.
- **Targets.** The expansion target is three nodes. One-to-two and
  three-to-five expansions are not tested by this feature.
- **Timing budgets.** Budgets are set generously for shared CI runners.
  The measured local figures are in the plan (section 1, F8).

## Out of Scope

- Live (no-restart) expansion: a lone coordination server cannot be
  turned into a cluster member at runtime.
- Starting a single node without a persistent coordination store and
  adding one later.
- Carrying coordination state across the rebuild.
- Reload-driven changes to the peer list or declared cluster size.
- Shrinking from three nodes to one, beyond the rollback path in User
  Story 4.
- An operator override that authorizes an empty node to initialize
  when the only data holder is permanently lost (decision 7, recorded
  as a follow-up).
