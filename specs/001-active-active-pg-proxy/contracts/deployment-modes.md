# Contract: Deployment Modes

**Feature**: 001-active-active-pg-proxy · **Phase**: 1 · **Date**: 2026-05-09

The same binary supports three deployment topologies. Differentiation
is purely in **configuration** and **placement**, not in code paths.

---

## Mode matrix

| Property                            | Standalone                              | Microservice                                  | Sidecar                                       |
|-------------------------------------|-----------------------------------------|-----------------------------------------------|-----------------------------------------------|
| Peer count                          | 1 (single-node) or N (small HA cluster) | N (≥ 3 typical)                               | 1 per PostgreSQL host                         |
| Listener default (data-plane)       | `0.0.0.0:6432`                          | `0.0.0.0:6432`                                | `127.0.0.1:6432`                              |
| Listener default (control-plane LCM API) | `0.0.0.0:9091`                     | `0.0.0.0:9091`                                | `127.0.0.1:9091` (loopback-only by default)   |
| Co-located with PostgreSQL?         | yes (one host)                          | optionally (placement is operator's choice)   | yes (same supervisor / pod / VM)              |
| Supervisor                          | systemd (typical)                       | systemd / container runtime                   | systemd / s6 / tini / sidecar runtime         |
| Failure-domain shared with PG?      | yes                                     | no (typically separate hosts)                 | yes                                           |
| External traffic routing            | direct DNS / client multi-host string   | external L4 LB or DNS RR (operator's choice)  | local apps → loopback only                    |
| NATS connectivity                   | required for HA mode; optional single-node | required                                  | required                                      |
| Smoke-test scenario                 | `tests/smoke/standalone_test.go`        | `tests/smoke/microservice_test.go`            | `tests/smoke/sidecar_test.go`                 |

The matrix above is operator-facing documentation; **the binary contains
no `if mode == sidecar` code paths**. CI enforces this with a grep gate
similar to SC-006.

---

## Standalone

**Goal**: One PostgreSQL host, one `pgman-proxy` process, optional
NATS-backed coordination if the operator runs more than one peer for
HA.

**Reference recipe**: `deploy/systemd/pgman-proxy.service` ships as a
template under `deploy/`. NATS connectivity is required even for
single-node form because `pg-manager`'s NATS adapters expect it; an
operator running a truly single-node deployment may run a tiny NATS
instance on the same host.

**Smoke test acceptance**:
- Process exits `0` after `SIGTERM` from supervisor.
- `/readyz` reports `200` within 10s of process start.
- A `psql` connection through the proxy executes `SELECT 1` and returns
  `1` end-to-end.

---

## Microservice

**Goal**: N proxy peers behind operator-managed L4/L7 load balancing or
DNS round-robin, fronting an existing pg-manager-managed PostgreSQL
cluster.

**Reference recipe**: `deploy/compose/docker-compose.yml` shows three
peers + one NATS + three PG nodes for end-to-end demo.

**External traffic distribution is the operator's responsibility**.
This repository does NOT ship:
- a Kubernetes Service definition;
- a Helm chart;
- HAProxy / NGINX / Envoy templates (those are common but not
  required — operators may use any L4 LB or none).

**Smoke test acceptance**:
- Three proxy peers all reach `/readyz=200` within 30s of compose-up.
- A `psql` connection routed in turn through each peer executes a
  write transaction and the same row is readable through any peer
  afterwards.
- Killing the leader leaves the cluster recoverable: a freshly
  reconnected client through any peer reaches the new leader within 5s
  (SC-002).

---

## Sidecar

**Goal**: A `pgman-proxy` peer colocated with each PostgreSQL host,
sharing the same supervisor / pod / VM. The proxy is the local
application's stable endpoint; the proxy is the layer that knows where
the writer is.

**Reference recipe**: `deploy/systemd/pgman-proxy-sidecar.service`
ships as a template alongside `pgman-proxy.service`. The two units are
identical save for the `After=` and `Wants=` lines that bind them to
PostgreSQL's unit. For container-based sidecars the `deploy/docker/`
image is unchanged — operators set a container `restart: unless-stopped`
and bind it to the same network namespace as the PostgreSQL container.

**Sidecar-specific guarantees**:
- Listener defaults to `127.0.0.1:6432` so only same-host applications
  reach it.
- Local apps connect via `host=127.0.0.1 port=6432` regardless of
  which host currently holds the leader; the proxy routes outbound to
  whichever peer is leader. (US2 Acceptance #2 — when the colocated PG
  crashes, the sidecar still routes to a remote leader.)

**Smoke test acceptance**:
- `pgman-proxy` and PostgreSQL run under the same supervisor; killing
  one MUST NOT kill the other; restart of either MUST converge to
  `/readyz=200`.
- An app on the same host connecting via `127.0.0.1:6432` succeeds.
- An off-host client attempting `<host>:6432` is **refused** by default
  (the listener bind is loopback-only).

---

## Cross-mode invariants

These MUST hold for all three modes:

1. The same binary, same configuration schema, same observability
   surface (FR-001, US2 Acceptance #3).
2. CI runs all three smoke tests on every release; failure of any
   blocks the release (SC-005).
3. No mode-specific build tags, no mode-specific source files,
   no mode-specific Dockerfiles (Constitution VII).
4. No Kubernetes-aware code in any deployment recipe
   (Constitution VII; SC-006 grep gate).
