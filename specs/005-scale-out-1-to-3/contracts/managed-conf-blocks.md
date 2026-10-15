# Contract: Managed configuration blocks (WI-C3)

**Feature**: `005-scale-out-1-to-3` | FR-013..FR-015 | research R-006, R-007

## API (internal/runtime/managed_block.go)

```go
// BlockAction reports what applyManagedBlock did.
type BlockAction string // "inserted" | "updated" | "removed" | "unchanged"

// applyManagedBlock rewrites the marker-delimited block `name` in the file
// at path so that its body is exactly lines. Atomic (temp file in the same
// directory + fsync + rename), preserves the original file mode, and leaves
// every byte outside the markers untouched.
func applyManagedBlock(path, name string, lines []string) (BlockAction, error)

// applyManagedConf applies both blocks for cfg.Postgres into dataDir and
// returns the blocks that changed (unchanged blocks are omitted).
func applyManagedConf(cfg config.Config, dataDir string) ([]blockChange, error)

// logManagedChanges emits postgres.managed_block_applied per change with
// phase "post_init" | "boot" and whether a reload was issued.
func logManagedChanges(logger *obs.Logger, nodeID, phase string, changes []blockChange, reloaded bool)

// applyManagedConfAtBoot: boot-time call site (below).
func applyManagedConfAtBoot(ctx context.Context, cfg config.Config, pg pgmanager.PostgresExecutor, logger *obs.Logger) error
```

## Block format

```text
# BEGIN pgman-proxy managed: postgres.hba_extras
host replication all 10.0.0.0/24 scram-sha-256
host all all 10.0.0.0/24 scram-sha-256
# END pgman-proxy managed: postgres.hba_extras
```

| Block | File |
|---|---|
| `postgres.hba_extras` | `<data_dir>/pg_hba.conf` |
| `postgres.conf_extras` | `<data_dir>/postgresql.conf` |

## Semantics

| File state | `lines` | Result |
|---|---|---|
| no block | non-empty | append the block at the end of the file. If the file does not end in a newline, add one first. → `inserted` |
| block, different body | non-empty | replace the body between the markers → `updated` |
| block | empty | delete the markers and body (and nothing else) → `removed` |
| block, same body | non-empty | no write → `unchanged` |
| no block | empty | no write → `unchanged` |
| BEGIN without END, END without BEGIN, or two BEGINs | any | error `managed block <name> malformed in <path>`; no write |

**Marker matching**: on whole lines, after trimming trailing `\r` and spaces. A body line is never interpreted as a marker unless it is an exact marker line.

**Legacy markers**: `# pgman-proxy: postgres.<block>` (written by releases ≤ v0.x before 005) are never recognized or modified.

## Call sites

1. **`postInitDBHook`** (`start.go`): `applyManagedConf(cfg, dataDir)`, then `logManagedChanges(..., "post_init", changes, false)`. It replaces both former `appendLines` calls, and the hook is still nil when both lists are empty (unchanged behavior). No reload: the postmaster starts afterwards.
2. **Boot**: gate #6.6 in `start.go`, right after `ensureStandbySignalIfInitialized`, calls `applyManagedConfAtBoot`. When `pgdataComplete(dataDir)` is `(true, nil)`:
   - `changes, err := applyManagedConf(cfg, dataDir)`;
   - if anything changed and `pg.IsRunning(ctx)` returns `(true, nil)`, call `pg.Reload(ctx)`;
   - log with phase `boot`;
   - any error → `StartupError{Code: ExitDeps}`.

   This runs even when both lists are empty, so previously managed blocks are removed. The phase value is `post_init` rather than naming the engine command, because the lcm-discipline gate rejects that token outside comments.

## Tests

**Unit** (`managed_block_test.go`):
- one case per row of the semantics table;
- CRLF and no-trailing-newline files;
- file mode 0600 and 0640 preserved;
- legacy block coexistence: the legacy block is untouched, and the managed block is appended after it;
- idempotence (a second apply is `unchanged`, with no mtime change);
- write into a read-only directory → error, and the original file is intact.

**Real server**: `tests/scaleout` S7.
- The golden node is created **without** replication HBA lines.
- It is then expanded with them configured.
- Assert that the block is present in node-a's `pg_hba.conf`, that b and c cloned, and that a reload was logged (or that Postgres was started fresh).
