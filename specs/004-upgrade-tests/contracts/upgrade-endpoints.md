# Contract: Upgrade Endpoints (as pinned by feature 004 tests)

**Status**: This documents EXISTING behavior (001 contract surface,
implemented in `internal/control/handlers_upgrade.go`). Feature 004
adds no new surface; it pins this contract with tests. Any deviation
discovered during implementation is a bug in either this document or
the code — resolve explicitly, don't paper over.

## Common properties (both endpoints)

- **Auth**: mutating ⇒ bearer token always required. Missing token →
  401 `auth_required`; invalid → 403 `auth_invalid`.
- **Audit gate**: audit pipeline unhealthy → 503 `audit_unavailable`
  (fail-closed, FR-028 of 001).
- **Leader routing**: both routes are leader-only. On a non-leader
  peer: forward mode publishes the raw body to the leader via the
  LeaderRouter and relays the leader's envelope verbatim; timeout →
  504 `leader_route_timeout`; redirect mode → `not_leader` semantics.
- **Envelope**: every response is the standard LCM envelope
  (`operation`, `request_id`, `outcome`, optional `engine_result`,
  optional `error{code,message}`); `X-Request-Id` header set.
- **Forwarded-response shape** (verified against the live topology):
  when the receiving peer is NOT the leader and mode is `forward`,
  the HTTP response is the FORWARDER's envelope — `200` /
  `outcome:"accepted"` (the forward itself succeeded) — with the
  LEADER's envelope spliced verbatim into `engine_result`. The
  operator-visible outcome of the upgrade is the INNER envelope; the
  status/outcome tables below describe that effective envelope (which
  is also the literal HTTP response when the receiving peer IS the
  leader). Clients (and tests) must unwrap before interpreting.
- **Request body** (both):

```json
{
  "plan": {
    "Strategy": 0,
    "TargetMajor": 17,
    "TargetMinor": 5,
    "NodeOrder": ["node-a"],
    "PerNodeBudget": 60000000000
  },
  "request_id": "optional-client-ulid (accepted, ignored)"
}
```

Strategy: `0` = minor rolling, `1` = major in-place, `2` = major
logical-bridge. Field names are Go-exported names (no json tags);
decoding is case-insensitive.

### Version semantics (pinned engine quirk — flagged upstream)

The engine (pg-manager v0.3.0, unchanged through v0.4.1;
`pgproto.Executor.Version`) decomposes
`server_version_num` with the pre-PostgreSQL-10 scheme:
`major = num/10000`, `minor = (num/100)%100`, `patch = num%100`. For
PostgreSQL ≥ 10 this reports **Minor = 0 always** — the real minor
release (e.g. the `10` in 17.10) lands in `Patch`. Consequences for
plans, discovered and pinned by the feature-004 integration tests:

- A same-version minor plan for 17.x MUST be `{TargetMajor: 17,
  TargetMinor: 0}`; targeting the human-readable minor (e.g.
  `TargetMinor: 10`) fails the post-swap confirm with
  `upgrade: expected minor 10, got 0`.
- The downgrade guard operates at major granularity only.

This belongs in pg-manager (thin-scaffold rule: fix upstream, not
here). Until then, plans submitted through this surface are
interpreted in the engine's scheme, and the tests encode it via a
shared `engineVersion` helper. A pg-manager release that fixes the
scheme will break the happy-path test — treat that like a sentinel
firing and re-pin deliberately.

## POST /v1/upgrade/prepare

Dry run. Delegates to `engine.PrepareUpgrade(ctx, plan)`. Never
changes cluster state.

| Case | Response |
|---|---|
| Plan valid (minor same-major, or major strictly-higher) | 200, `outcome: "accepted"` |
| Downgrade / cross-major minor / non-higher major | 500, `outcome: "failed"`, `error.code: "engine_error"`, message = engine's validation reason |
| Undecodable body | 400, `outcome: "rejected"`, `error.code: "invalid_argument"`; engine NOT invoked |

## POST /v1/upgrade/execute

Runs local-node steps. Internally re-validates (engine calls
`PrepareUpgrade` first), then:

| Case | Response |
|---|---|
| Minor plan, sequence succeeds (stop → pre-swap → start → version probe) | 200, `outcome: "accepted"` |
| Minor plan, any step fails (swap error, version mismatch, lifecycle error) | 500, `outcome: "failed"`, `error.code: "engine_error"`, message preserved |
| Major plan (either strategy), validation passed | 500, `outcome: "failed"`, `error.code: "engine_error"`, message contains exactly: `upgrade: major strategies wired but gated on v0.7.0` — **pinned sentinel** |
| Invalid plan | as prepare (engine re-validates) |
| Undecodable body | 400 `rejected` / `invalid_argument`; engine NOT invoked |

**Pre-swap**: the HTTP surface always supplies a no-op pre-swap
callback (v1; real binary swaps are a host concern outside HTTP —
Constitution VII trust boundary). Therefore the maximal real upgrade the
HTTP surface performs is a same-version minor run; tests exploit this
for the real-server happy path.

## Sentinel obligations

When a pg-manager version bump changes any of:
- the gate error string above,
- major-strategy execution becoming available,
- validation reasons' wording relied on by assertions,

the corresponding test MUST fail. The failing test is the designed
notification; the follow-up is a conscious contract revision (new
spec/PR), never an in-place assertion loosening without review.
