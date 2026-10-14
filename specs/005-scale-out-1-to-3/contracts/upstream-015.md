# Contract: What pgman-proxy consumes from pg-manager milestone 015 (v0.7.0)

**Feature**: `005-scale-out-1-to-3`
**Upstream**: `../pg-manager`, milestone `015-empty-peer-safety`. Input: `docs/scale-out-1-to-3/plan.md` section WI-B (B1-B7).
**Status**: requirements on upstream. Names marked *(proposed)* are pinned when 015's own contracts are written; this file is then updated to match, and the tests that assert them change in the same commit.

## Behavior pgman-proxy relies on (no API calls needed)

| # | Upstream item | pgman-proxy dependency |
|---|---|---|
| U1 | B1: per-node PGDATA presence record (`pgmgr/<cluster>/pgdata/<node>`) | none directly; enables U2/U4 |
| U2 | B2: empty node does not campaign while any configured peer has data or has not reported (fresh records only) | US2, US3. Combined with the boot-window gate (WI-C1), which covers the window before `manager.New` |
| U3 | B3: initdb re-checks peer records, refuses and resigns the lease when unsafe | US2 AS-1, US3 AS-2 |
| U4 | B4: empty peers are excluded from the sync set and the failover quorum; async snapshot when none remain; `''` is rendered through ALTER SYSTEM instead of returning early | US2 AS-2/AS-3, US4 (stale value cleared), FR-005 interim step retired |
| U5 | B5 (B-024): initdb-time conf renders `''` for sync names | US1 on fresh single nodes is already fixed by WI-A; U5 keeps fresh multi-peer initdb sane |
| U6 | B6: identity-mismatch detection | US5 |
| U7 | B7 (optional): leader publishes its snapshot before the first promote preflight | SC-003 margin only |

### Data-presence predicate (request)

- **U1's `has_data`** must not count a partially written data directory as data.
- **The case**: a crash in the middle of a clone or initdb leaves such a directory behind (spec edge case; research R-003). Basing `has_data` on `PG_VERSION` alone is insufficient.
- **The request**: export the predicate *(proposed: `pgmanager.PGDataComplete(dir string) (bool, error)`)*, so pgman-proxy's boot-window gate uses the same rule. pgman-proxy then deletes its local `pgdataComplete`.

## API surface pgman-proxy calls

| Use | Upstream symbol | Notes |
|---|---|---|
| Identity mismatch, doctor and gauge | `Manager.Status()` → *(proposed)* `Status.IdentityMismatch *IdentityMismatch{LocalSystemIdentifier, LeaderSystemIdentifier string; LeaderNodeID NodeID}` (nil when matching or unknown) | Read-only |
| Identity mismatch, event | *(proposed)* topic `cluster.identity_mismatch`, payload = the struct above + `EventHeader` | Forwarded by `cluster.SubscribeCoordinationEvents` |
| Complete-database predicate | *(proposed)* `pgmanager.PGDataComplete` | Optional; local fallback until exported |

## Log lines asserted by `tests/scaleout`

| Scenario | Pattern *(proposed; pin when 015 lands)* |
|---|---|
| S6 | `campaign: empty-node deference armed` with `peers_with_data` or `peers_unreported` |
| S1, S6 (negative) | no `initdb: starting` on node-b or node-c (exists today, `reconciler/act.go:330`) |
| S1 (positive) | `bootstrap: starting basebackup` with `source=node-a` on node-b and node-c (exists today, `reconciler/act.go`) |

## Release coupling

- WI-C5 bumps `go.mod` to the 015 release (provisionally `v0.7.0`; see research R-013 for the collision with the upgrade gate reserved for v0.7.0). If 015 moves `pgx`, the coupled `pgx` bump lands in the same commit (see the go.mod comment).
- Until then, development uses the sibling checkout. The integration image always builds against it via `replace`. Local Go builds that need 015 symbols use a temporary, commented `replace` in `go.mod`, which is removed in the WI-C5 commit.
