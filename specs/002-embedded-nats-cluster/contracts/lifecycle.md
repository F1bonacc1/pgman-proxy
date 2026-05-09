# Contract — Process lifecycle (delta from feature 001)

**Feature**: `002-embedded-nats-cluster`
**Phase**: 1

001 already specified SIGINT/SIGTERM with a configurable drain budget.
Feature 002 adds **SIGHUP** with a tightly-scoped reload surface, and
amends the startup and shutdown sequences to bracket the embedded NATS
server.

## Startup sequence

```
1. Parse flags + load config file + read env vars
2. Validate config (FR-002, FR-008, FR-009, FR-010b, FR-011 gates)
   ↳ ANY failure → exit code 78 (CONFIG)
3. Build embedded NATS server.Options from cluster config
4. Pre-create the JetStream KV bucket with derived Replicas (RD-002 path)
   - if pg-manager exposes WithReplicas: skip, defer to adapter
   - else: open a temporary nats.go connection AFTER server ready, create bucket, close connection
5. server.NewServer(opts); go s.Start()
6. Wait for s.ReadyForConnections(connect_timeout)
   ↳ on timeout → log embedded_nats.server_stopped(startup_timeout); exit code 75 (TEMPFAIL)
7. Emit embedded_nats.server_started + ...server_ready events
8. Construct pg-manager NATS adapters (Leadership / StateStore / EventBus)
   dialing nats://<cluster.client_listen.host>:<port>
9. Start data-plane proxy listener (001 unchanged path)
10. Start control-plane HTTP listener (001 unchanged path)
11. Mark process ready (/readyz returns 200)
```

Exit codes (extending 001's set):

| Code | Meaning |
|---|---|
| 0 | Clean shutdown |
| 64 (USAGE) | Bad flags |
| 78 (CONFIG) | Validation failure (incl. legacy `nats.url`, missing cluster username/password, password < 16 bytes, missing TLS on non-loopback) |
| 75 (TEMPFAIL) | Embedded server failed to reach ready within `connect_timeout` |
| 73 (CANTCREAT) | JetStream storage path unwritable / disk full at startup |
| 1 | Unhandled error |

## Shutdown sequence (SIGTERM / SIGINT)

```
1. Stop accepting new client connections (data-plane listener closed)
2. Drain in-flight queries up to FR-014 budget (001)
3. Stop control-plane listener (no new LCM requests)
4. Stop pg-manager adapters (leadership released cleanly via NATS)
5. s.Shutdown() on the embedded NATS server
   ↳ blocks up to half of the remaining shutdown budget
   ↳ on hang: forced server.Stop() and log embedded_nats.server_stopped(reason="forced")
6. Flush logs and metrics
7. Exit 0
```

The order matters: pg-manager adapters must release the leadership lease
*before* the embedded server stops, otherwise the lease remains held in
the cluster's view until lease expiry.

## SIGHUP — hot reload

**Allow-list** (FR-014a):

- `cluster.peers`
- `cluster.password` (the SecretRef target value, re-read from source on SIGHUP — RD-001a amendment to FR-014a)

**Sequence**:

```
1. Re-read config from the same source(s) that were used at startup
2. Compute a ReloadDiff vs current in-memory config
3. If non-allow-list keys differ:
     emit embedded_nats.reload_applied with skipped_keys + skipped_reason
     do NOT advance them in memory
4. If allow-list keys differ:
     build new server.Options from the new values (preserving everything else)
     call s.ReloadOptions(newOpts)
     ↳ NATS handles route add/remove + auth-list update without restart
     emit embedded_nats.reload_applied with routes_added/removed, keys_added/removed
5. Update in-memory config to reflect applied changes only
6. Bump pgman_proxy_embedded_nats_sighup_reload_outcomes_total{result="applied"|"partial_skipped"|"error"}
```

**Failure modes**:

- Re-read fails → log error; do NOT mutate in-memory config; result="error".
- ReloadOptions returns error → log error; revert in-memory config to
  pre-reload state if possible; result="error".
- New `peers` URL is unreachable → not a SIGHUP failure; NATS will retry
  in the background and emit `route_up` when the route comes up.

**Reload latency target**: 1 s p99 on a 3-peer cluster (RD-006).

## Other signals

001's behaviours are preserved:

- `SIGUSR1` / `SIGUSR2`: not handled; reserved for future use.
- `SIGPIPE`: ignored (network code handles peer disconnects directly).
- `SIGHUP` previously had no defined behaviour in 001; **this contract
  defines it**.
