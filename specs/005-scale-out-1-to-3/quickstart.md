# Quickstart: verifying feature 005

All commands are run from the directory that contains both `pg-manager/` and `pgman-proxy/` unless noted.

## 0. Prerequisites

- Docker with the compose v2 plugin.
- The `pg-manager` sibling checkout. Use the `015-empty-peer-safety` branch (or v0.7.0) for the US2-US5 checks, or v0.6.1 for US1 only.

```bash
docker build -f pgman-proxy/tests/integration/Dockerfile -t pgman-proxy-it:latest .
```

## 1. Unit tier (no docker)

```bash
cd pgman-proxy && make test
```

It covers:
- `replicationPolicy` (WI-A);
- `campaignGate` and `pgdataComplete` (WI-C1);
- `DetectStoreLayout` and `ArchiveIncompatibleStore` (WI-C2);
- `applyManagedBlock` (WI-C3);
- the `cluster.identity` doctor check with `fakeEngine` (WI-C4).

## 2. US1: single node works (no upstream dependency)

```bash
cd pgman-proxy && PGMAN_SCALEOUT_IMAGE=pgman-proxy-it:latest \
  go test -count=1 -tags=scaleout -run 'TestSingleNode' ./tests/scaleout/...
```

Manual cross-check with the rig (baseline outputs in `docs/scale-out-1-to-3/plan.md` section 6):

```bash
cd pgman-proxy/docs/scale-out-1-to-3/rig
./scenarios.sh single-writes    # expect: W_OK
./scenarios.sh golden
./scenarios.sh single-restart   # expect: primary in < 30 s, W_OK
```

## 3. US2-US4: expansion, safety, rollback (needs pg-manager 015 in the sibling)

```bash
cd pgman-proxy && PGMAN_SCALEOUT_UPSTREAM_015=1 make scaleout
```

The rig's unrigged race must now pass:

```bash
cd pgman-proxy/docs/scale-out-1-to-3/rig
./scenarios.sh golden && ./scenarios.sh race       # expect: VERDICT: PASS
./scenarios.sh rollback                            # expect: primary, W_OK
./scenarios.sh clean
```

## 4. Regression: fresh 3-node bootstrap unchanged

```bash
cd pgman-proxy && make integration
```

## 5. Operator procedure

`docs/scale-out-1-to-3/runbook.md` (WI-C7) is the operator-facing version of section 2 of the technical plan.

**Verification after an expansion**:
- `pgmctl status` shows node-a as primary and node-b and node-c as streaming standbys;
- `pgman_proxy_embedded_nats_replicas_factor == 3`;
- `pgman_proxy_embedded_nats_routes_meshed == 2` on every node;
- `pgman_proxy_cluster_identity_mismatch == 0` on every node;
- `pgmctl doctor` passes `cluster.identity`.
