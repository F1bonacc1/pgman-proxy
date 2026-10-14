#!/usr/bin/env bash
# Reproduction scenarios for docs/scale-out-1-to-3/plan.md (section 6).
#
# Throwaway verification tooling, not a test suite: it drives the
# integration image (tests/integration/Dockerfile) with plain `docker run`
# so a single node can be restarted into a 3-node config, which the
# compose harness cannot do. Superseded for CI by tests/scaleout (make
# scaleout); kept for manual runs at the 1 GB scale of the original evidence.
#
# Build the image first (from the directory that contains BOTH
# pg-manager/ and pgman-proxy/):
#   docker build -f pgman-proxy/tests/integration/Dockerfile -t pgman-proxy-it:latest .
# Override with IMG=<tag> to test another build.
#
# Usage: scenarios.sh <single-writes|golden|single-restart|race|a-wins|rollback|solo|clean>
set -u
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"
: > "$D/none.env"

wait_primary() { # $1=node $2=budget seconds; echoes seconds taken or "never"
  local s=$SECONDS
  while [ $((SECONDS - s)) -lt "$2" ]; do
    [ "$(psqlx "$1" -c 'select pg_is_in_recovery()')" = "f" ] && { echo $((SECONDS - s)); return; }
    sleep 1
  done
  echo never
}

try_write() { # $1=node; prints W_OK or BLOCKED/ERROR
  local w
  w=$(timeout 10 docker exec "pgv-node-$1" psql -h /var/run/postgresql -U postgres -XAtqc \
    "create table if not exists probe(t timestamptz default now(), src text); insert into probe(src) values ('rig') returning 'W_OK'" 2>&1)
  echo "${w:-BLOCKED (>10s, check wait_event=SyncRep)}"
}

verdict() { # compares every node against node-a's system identifier and row count
  local want rows ok=1
  want=$(sysid a | awk '{print $1}')
  for n in a b c; do
    rows=$(psqlx "$n" -c 'select count(*) from bulk' 2>/dev/null)
    echo "node-$n: role=$(psqlx "$n" -c "select case when pg_is_in_recovery() then 'standby' else 'primary' end") sysid=$(sysid "$n") rows=$rows"
    [ "$(sysid "$n" | awk '{print $1}')" = "$want" ] && [ "$rows" = "1000000" ] || ok=0
  done
  [ "$(psqlx a -c 'select pg_is_in_recovery()')" = "f" ] || ok=0
  [ $ok = 1 ] && echo "VERDICT: PASS (one database, node-a primary, all rows everywhere)" \
              || echo "VERDICT: FAIL (diverged system identifiers, missing rows, or node-a not primary)"
}

case "${1:-}" in
  single-writes) # D1: fresh single node, can it commit at all?
    reset_all; start_node a single-a.env
    echo "primary after: $(wait_primary a 90)s"
    echo "synchronous_standby_names=[$(psqlx a -c 'show synchronous_standby_names')]"
    echo "write: $(try_write a)"
    ;;
  golden) # single node with 1M rows (~1 GB), snapshotted to pgv-golden-{data,js}
    reset_all; start_node a single-a.env
    echo "primary after: $(wait_primary a 90)s"
    # Workaround for D1 on unfixed builds; harmless on fixed ones. Reset
    # below so it does not leak into later scenarios.
    psqlx a -c "alter system set synchronous_standby_names=''" -c "select pg_reload_conf()" >/dev/null; sleep 1
    psqlx a -c "create table bulk(id bigint primary key, pad text); insert into bulk select g, repeat('x',900) from generate_series(1,1000000) g; create table probe(t timestamptz default now(), src text);"
    psqlx a -c "alter system reset synchronous_standby_names" -c "select pg_reload_conf()" >/dev/null
    echo "rows=$(psqlx a -c 'select count(*) from bulk') sysid=$(sysid a)"
    docker stop -t 60 pgv-node-a >/dev/null
    for v in data js; do
      docker volume rm -f "pgv-golden-$v" >/dev/null 2>&1; docker volume create "pgv-golden-$v" >/dev/null
      docker run --rm -u root --entrypoint sh -v "pgv-a-$v:/src" -v "pgv-golden-$v:/dst" "$IMG" -c 'cp -a /src/. /dst/'
    done
    echo "golden saved"
    ;;
  single-restart) # D2: restart the golden single node with the same config
    restore_golden; start_node a single-a.env
    echo "primary after: $(wait_primary a 90)s (baseline: never, missing_quorum_snapshot)"
    echo "write: $(try_write a)"
    ;;
  race) # unrigged lease race, today's procedure: start b,c then restart a in 3-node config
    run_trial race none.env none.env 150; verdict
    ;;
  a-wins) # rig node-a to win the lease (fast TTL) to isolate the promote gate
    run_trial a-wins lease-fast.env lease-slow.env 150; verdict
    grep -c 'promote: refused' "$D/a-wins-a.log" | sed 's/^/node-a promote refusals: /'
    ;;
  rollback) # after race/a-wins: revert node-a to the single-node config, wipe its JetStream dir
    docker rm -f pgv-node-a pgv-node-b pgv-node-c >/dev/null 2>&1
    docker volume rm -f pgv-a-js >/dev/null; mkvol pgv-a-js
    start_node a single-a.env
    echo "primary after: $(wait_primary a 90)s (baseline: never)"
    echo "synchronous_standby_names=[$(psqlx a -c 'show synchronous_standby_names')]"
    echo "write: $(try_write a)"
    ;;
  solo) # node-a alone in a 3-node config: expect exit code 75 after 120s, data untouched
    restore_golden; s=$SECONDS; start_node a three-a.env
    echo "exit code $(docker wait pgv-node-a) after $((SECONDS - s))s"
    ;;
  clean)
    docker rm -f pgv-node-a pgv-node-b pgv-node-c >/dev/null 2>&1
    for n in a b c golden; do docker volume rm -f "pgv-$n-data" "pgv-$n-js" >/dev/null 2>&1; done
    docker network rm "$NET" >/dev/null 2>&1; echo cleaned
    ;;
  *) sed -n '2,15p' "$0"; exit 2 ;;
esac
