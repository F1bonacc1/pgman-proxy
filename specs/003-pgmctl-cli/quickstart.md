# Quickstart — `pgmctl`

**Feature**: `003-pgmctl-cli` · **Phase**: 1 · **Date**: 2026-05-14

This quickstart is the executable form of US1 (`pgmctl status`), US2
(`pgmctl dump`), US3 (`pgmctl doctor`), and US4 (`pgmctl watch`) from
spec.md. It assumes a 3-peer `pgman-proxy` cluster is already running
(see `specs/002-embedded-nats-cluster/quickstart.md` for that).

---

## Prerequisites

- A pgman-proxy peer reachable at an HTTPS endpoint (default `:9091`).
- A bearer token for that peer's control plane (per 001 FR-024). The
  token may live in an env var, a file, or be emitted by a command —
  see [`contracts/cli-commands.md § config`](./contracts/cli-commands.md).

For development against a local 3-peer cluster brought up via the
existing `process-compose.yaml`:

```bash
# From repo root, with a 3-peer dev cluster running:
export PGMCTL_ENDPOINT="https://127.0.0.1:9091"
export PGMCTL_TOKEN="$(cat ~/.cache/pgman-proxy/dev-token)"
```

---

## 1. Install `pgmctl`

From repo root (development build):

```bash
go build -o ./bin/pgmctl ./cmd/pgmctl
./bin/pgmctl version
```

Release binaries (linux/amd64, linux/arm64, darwin/amd64, darwin/arm64)
are published as `pgmctl-<version>-<os>-<arch>.tar.gz` per RD-012.

---

## 2. Configure a context

```bash
mkdir -p "${XDG_CONFIG_HOME:-$HOME/.config}/pgmctl"
chmod 700 "${XDG_CONFIG_HOME:-$HOME/.config}/pgmctl"

cat > "${XDG_CONFIG_HOME:-$HOME/.config}/pgmctl/config.yaml" <<'YAML'
apiVersion: pgmctl/v1
kind: Config
current-context: dev
contexts:
  - name: dev
    endpoint: https://127.0.0.1:9091
    expected_cluster: dev-cluster
    token_file: ~/.cache/pgman-proxy/dev-token
    tls:
      insecure_skip_tls_verify: true
YAML

chmod 600 "${XDG_CONFIG_HOME:-$HOME/.config}/pgmctl/config.yaml"

pgmctl config view
```

`pgmctl config view` redacts secrets and tells you which context is
active. The `chmod 600` is required — pgmctl REFUSES to load the file
otherwise (RD-007).

For a second cluster (e.g., production-east):

```bash
pgmctl config set-context prod-east \
  --endpoint https://pgman-proxy.prod-east.example.com:9091 \
  --expected-cluster prod-east \
  --token-file /var/run/secrets/pgmctl/prod-east.token \
  --tls-ca-file /etc/pki/ca-trust/source/anchors/pgmctl-prod.pem

pgmctl config use-context prod-east
```

---

## 3. One-glance status (US1, P1)

```bash
pgmctl status
```

Expected output (healthy 3-peer cluster):

```text
Cluster: dev-cluster                              Snapshot: 13:42:09Z
Leader:  node-1   Primary: node-1   Peers: 3/3 reachable
Mesh:    3 routes meshed  ·  embedded NATS: OK on every peer

NODE     ROLE       FENCE   LAG       LAST TRANSITION
node-1   primary    -       -         13:00:11Z  start
node-2   standby    -       8 KB      12:59:47Z  attach
node-3   standby    -       6.4 MB    12:59:55Z  attach
```

All lines render in green. Exit code `0`. Same view as JSON for
automation:

```bash
pgmctl status -o json | jq '.cluster.leader.node_id'
```

---

## 4. Full dump (US2, P1)

```bash
# Write a tar.gz next to the failing engineer:
pgmctl dump --output ./post-mortem-$(date +%Y%m%d-%H%M%S).tar.gz

# Or pipe directly into S3 / Slack / a ticket attachment:
pgmctl dump --output - --redact-level strict | \
    gh issue create --title "post-mortem 2026-05-14" --body-file -
```

The artifact contains everything from `data-model.md § DumpArtifact`:
status snapshot, topology, redacted config, events / audit from the
history stream, doctor results, clock-skew measurements, per-peer
slices via fan-out, and a manifest naming every slice's outcome and
duration.

Run it during an incident; the dump completes in p95 ≤ 15s on a
healthy 3-peer cluster (SC-003).

---

## 5. Interactive doctor (US3, P1)

Run the full battery, render only the highlights:

```bash
pgmctl doctor
```

Each check renders one line — green PASS, yellow INFO / WARN, red FAIL,
yellow UNKNOWN with bracket markers when stdout is not a TTY.

Run one check:

```bash
pgmctl doctor --check replication.lag-acceptable
```

Walk failing checks with fix prompts:

```bash
pgmctl doctor --fix
```

Each failing check with a `single-resource` `suggested_fix` prompts
`[y/N]`. Cluster-affecting fixes require typed cluster-name
confirmation; `advisory` fixes are never auto-applied. `-y` bypasses
single-resource prompts.

Automation form:

```bash
pgmctl doctor -o json | jq '.summary'
```

---

## 6. Live watch during an incident (US4, P2)

In one terminal, leave a status watch up:

```bash
pgmctl watch status
```

The screen updates within 1s of any server-observed state change
(SC-009); unchanged cells are not redrawn (no flicker).

In a second terminal, tail transitions:

```bash
pgmctl watch transitions
```

Or tail every event the cluster emits:

```bash
pgmctl watch events
```

Or focus on one node:

```bash
pgmctl watch node node-3
```

Ctrl-C exits with code `0`.

---

## 7. Mutating operations (US6, P2)

Single-resource:

```bash
# Interactive (default):
pgmctl fence node-2

# CI / pipeline (still prompts on non-TTY without -y):
pgmctl fence node-2 --yes
```

Cluster-affecting (require typed cluster-name OR `--force --cluster`):

```bash
# Interactive — pgmctl asks you to type the cluster name:
pgmctl failover --target node-3

# Automation form:
pgmctl failover --target node-3 --force --cluster dev-cluster
```

Restart targets:

```bash
# Default — restarts pg-manager's managed PostgreSQL on node-2:
pgmctl restart node-2

# Restarts the pgman-proxy peer process on node-2 (requires a detected supervisor):
pgmctl restart node-2 --target proxy
```

Decommission a peer from the topology (does NOT touch the host):

```bash
pgmctl delete node-3
```

After accepting a mutating operation, pgmctl prints the `request_id`
to stdout so you can correlate with the server audit log:

```bash
pgmctl fence node-2 --yes
# accepted: request_id=01H8XYZABC... operation=Fence target=node-2

pgmctl get audit --since 5m | jq 'select(.request_id == "01H8XYZABC...")'
```

---

## 8. Explain mode

Ask the cluster to diagnose itself:

```bash
pgmctl explain failover-stuck
pgmctl explain node-not-promoting node-3
pgmctl explain replication-broken node-3
pgmctl explain leader-election
```

Each emits a Diagnosis / Evidence / Suggested-next-steps narrative
grounded in the cluster's own history and doctor output. The
suggested-next-steps section names concrete `pgmctl` invocations you
can copy-paste.

---

## 9. Common operator-friendly invocations

```bash
# Health rollup for an automated monitor:
pgmctl health -o json

# Replication lag with thresholds:
pgmctl lag --warn 100MB --fail 1GB

# Topology as a tree:
pgmctl topology

# Tail events from the last 10 minutes:
pgmctl events --since 10m

# Audit decisions made by the cluster today:
pgmctl get audit --since 24h

# Effective configuration (redacted) for the connected peer:
pgmctl get config

# Skew between this pgmctl and the server:
pgmctl version
```

---

## Acceptance — run-through

The quickstart is "complete" for v1 when:

1. `pgmctl status` against a healthy 3-peer cluster renders green and
   completes inside 1.5s p95 (SC-002).
2. `pgmctl dump` produces a `.tar.gz` whose `manifest.json` lists
   every slice from §4 above with `outcome: ok` (SC-003).
3. `pgmctl doctor` returns `PASS` or `INFO` on every check against a
   healthy cluster, and `--fix` walks induced failures back to PASS
   within one prompt each (SC-004, SC-010).
4. `pgmctl watch status` redraws on a fixture-triggered state
   transition within 1s p95 (SC-009).
5. Every mutating subcommand has a corresponding integration test
   asserting prompt wording, `--yes` / `--force --cluster <name>`
   override behaviour, refusal to escalate `-y` to cluster-affecting,
   and the audit record on the server side (SC-006).
6. `pgmctl status -o json | jq` succeeds; the JSON schema's
   `apiVersion` is `pgmctl/v1` (SC-007).
7. `--no-color`, `NO_COLOR=1`, and a non-TTY stdout each suppress
   ANSI escapes (SC-008).
