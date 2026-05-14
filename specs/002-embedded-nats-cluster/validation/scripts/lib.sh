#!/usr/bin/env bash
# Shared probe library for v1 chaos validation scenarios.
# Re-source from each scenario script: `source "$(dirname "$0")/lib.sh"`
#
# All probes are idempotent. They never mutate cluster state. They print
# either a scalar value (single line) or a small structured record.

set -uo pipefail

NODES=(a b c)
NODE_NAMES=(node-a node-b node-c)
HOST_PORTS=(16432 16433 16434)
OBS_PORTS=(19090 19091 19092)

# pq via in-container unix socket. $1 is the node id letter (a/b/c).
pq() {
  local n="$1"; shift
  docker exec "pgman-pc-node-$n" psql -h /var/run/postgresql -U postgres -tAc "$*"
}

# is_primary <a|b|c> → "t" or "f" (or empty on container error)
is_primary() {
  pq "$1" "SELECT NOT pg_is_in_recovery();" 2>/dev/null
}

# current_primary → letter (a|b|c) or empty if none found / multiple found.
current_primary() {
  local found=""
  for n in "${NODES[@]}"; do
    local r
    r=$(is_primary "$n")
    if [ "$r" = "t" ]; then
      if [ -n "$found" ]; then
        echo "MULTIPLE:$found,$n" >&2
        return 1
      fi
      found="$n"
    fi
  done
  echo "$found"
}

# pg_flush_lsn <a|b|c>  current_wal_flush_lsn on primary (semantic on standby varies)
pg_flush_lsn() {
  pq "$1" "SELECT pg_current_wal_flush_lsn();" 2>/dev/null
}

pg_replay_lsn() {
  pq "$1" "SELECT pg_last_wal_replay_lsn();" 2>/dev/null
}

# Returns the rows of pg_stat_replication on the primary.
repl_state() {
  pq "$1" "SELECT application_name, state, sync_state, write_lag, flush_lag, replay_lag FROM pg_stat_replication ORDER BY application_name;" 2>/dev/null
}

sync_standby_names() {
  pq "$1" "SHOW synchronous_standby_names;" 2>/dev/null
}

sync_commit() {
  pq "$1" "SHOW synchronous_commit;" 2>/dev/null
}

slot_list() {
  pq "$1" "SELECT slot_name, active, restart_lsn FROM pg_replication_slots ORDER BY slot_name;" 2>/dev/null
}

# chaos counters: print latest workload JSON line.
chaos_latest() {
  process-compose process logs chaos-workload --tail 1 2>&1 \
    | tail -1
}

# Extract a numeric field from the latest chaos line, e.g. data_loss_total.
chaos_field() {
  local field="$1"
  chaos_latest | grep -oE "\"${field}\":[0-9]+" | head -1 | cut -d: -f2
}

# counters → JSON snapshot of writes_ok/writes_failed/data_loss_total/extra_rows.
counters() {
  local line wo wf dl er
  line="$(chaos_latest)"
  wo=$(echo "$line" | grep -oE '"writes_ok":[0-9]+' | head -1 | cut -d: -f2)
  wf=$(echo "$line" | grep -oE '"writes_failed":[0-9]+' | head -1 | cut -d: -f2)
  dl=$(echo "$line" | grep -oE '"data_loss_total":[0-9]+' | head -1 | cut -d: -f2)
  er=$(echo "$line" | grep -oE '"extra_rows":[0-9]+' | head -1 | cut -d: -f2)
  printf '{"writes_ok":%s,"writes_failed":%s,"data_loss_total":%s,"extra_rows":%s}\n' \
    "${wo:-NA}" "${wf:-NA}" "${dl:-NA}" "${er:-NA}"
}

# Routes-meshed value (0..2) for peer letter.
routes_meshed() {
  local n="$1"
  local p
  case "$n" in
    a) p=19090 ;;
    b) p=19091 ;;
    c) p=19092 ;;
  esac
  curl -sS "http://127.0.0.1:$p/metrics" 2>/dev/null \
    | grep -E '^pgman_proxy_embedded_nats_routes_meshed[ {]' \
    | head -1 \
    | awk '{print $NF}'
}

# all probes: print compact one-liner snapshot of the whole cluster.
snapshot() {
  printf "wall=%s " "$(date -Is)"
  local p
  p=$(current_primary)
  printf "primary=%s " "${p:-NONE}"
  for n in "${NODES[@]}"; do
    local m
    m=$(routes_meshed "$n")
    printf "mesh-%s=%s " "$n" "${m:-NA}"
  done
  printf "ctrs=%s\n" "$(counters)"
}

# Sleep helpers
wait_seconds() {
  local s="$1"
  for i in $(seq 1 "$s"); do
    sleep 1
  done
}

# Wait until current_primary returns a non-empty value or timeout.
wait_for_primary() {
  local timeout="${1:-30}"
  local t=0
  while [ "$t" -lt "$timeout" ]; do
    local p
    p=$(current_primary)
    if [ -n "$p" ] && [ "${p:0:9}" != "MULTIPLE:" ]; then
      echo "$p"
      return 0
    fi
    sleep 1
    t=$((t + 1))
  done
  return 1
}

# Wait until a routes_meshed reaches a particular value on a node.
wait_for_mesh() {
  local n="$1" want="$2" timeout="${3:-30}"
  local t=0
  while [ "$t" -lt "$timeout" ]; do
    local m
    m=$(routes_meshed "$n")
    if [ "$m" = "$want" ]; then return 0; fi
    sleep 1
    t=$((t + 1))
  done
  return 1
}

# Emit a section banner.
banner() {
  echo "=================================================================="
  echo "$*"
  echo "=================================================================="
}

# Pull recent slog events of a given event name from all containers.
event_grep() {
  local needle="$1"
  for n in "${NODES[@]}"; do
    local container="pgman-pc-node-$n"
    local count
    count=$(docker logs "$container" 2>&1 | grep -c "\"event\":\"$needle\"" || true)
    printf "%s:%s " "$container" "$count"
  done
  printf "\n"
}

# Note: functions are defined directly; no `export -f` (zsh-incompat).
