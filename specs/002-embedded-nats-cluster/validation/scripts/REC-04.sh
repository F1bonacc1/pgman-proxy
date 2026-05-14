#!/usr/bin/env bash
# REC-04 — AutoRebootstrap byte-for-byte correctness.
#
# Strategy (post-rig-prep): with max_slot_wal_keep_size=16MB,
#   1) Stop node-X (a standby).
#   2) On primary, write > 16MB of WAL until pgmgr_node_X slot is `lost`.
#   3) Start node-X. Expect pg-manager to detect stale-WAL, wipe PGDATA,
#      pg_basebackup from primary, resume streaming.
#   4) Once standby streaming caught up, fingerprint row data on primary
#      and standby. md5 must match.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
source "$HERE/lib.sh"

LOG="$HERE/logs/REC-04-$(date -u +%Y%m%dT%H%M%SZ).log"
exec > >(tee -a "$LOG") 2>&1

banner "REC-04 — rebootstrap correctness"
PRIMARY="$(current_primary)"
echo "primary=$PRIMARY"
[ -z "$PRIMARY" ] && { echo "no primary"; exit 2; }

TARGET=""
for n in a b c; do
  if [ "$n" != "$PRIMARY" ]; then TARGET="$n"; break; fi
done
echo "target standby=$TARGET"

echo "--- pre slots on $PRIMARY ---"
slot_list "$PRIMARY"
echo "--- pre ctrs $(counters)"

banner "Step 1: stop node-$TARGET"
process-compose process stop "node-$TARGET" 2>&1 | tail -3
until ! docker ps --format '{{.Names}}' | grep -q "pgman-pc-node-$TARGET"; do sleep 1; done
echo "node-$TARGET container down at $(date -Is)"
sleep 3
echo "slots after stop:"
slot_list "$PRIMARY"

banner "Step 2: burn WAL on primary $PRIMARY until slot wal_status=lost"
pq "$PRIMARY" "DROP TABLE IF EXISTS rec04_burner; CREATE TABLE rec04_burner (id bigserial PRIMARY KEY, payload bytea);"
ITER=0
MAX_ITER=80
while [ "$ITER" -lt "$MAX_ITER" ]; do
  ITER=$((ITER + 1))
  pq "$PRIMARY" "INSERT INTO rec04_burner (payload) SELECT repeat('x',4000)::bytea FROM generate_series(1,1500);" >/dev/null
  pq "$PRIMARY" "SELECT pg_switch_wal();" >/dev/null
  if [ $((ITER % 3)) -eq 0 ]; then
    pq "$PRIMARY" "CHECKPOINT;" >/dev/null 2>&1
    WAL_STATUS=$(pq "$PRIMARY" "SELECT wal_status FROM pg_replication_slots WHERE slot_name='pgmgr_node_$TARGET';")
    DIFF=$(pq "$PRIMARY" "SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn) FROM pg_replication_slots WHERE slot_name='pgmgr_node_$TARGET';")
    echo "iter=$ITER wal_status=$WAL_STATUS wal_behind=$DIFF"
    if [ "$WAL_STATUS" = "lost" ] || [ "$WAL_STATUS" = "unreserved" ]; then
      echo "slot $WAL_STATUS at iter=$ITER"
      break
    fi
  fi
done

echo "--- post-burn slot state ---"
pq "$PRIMARY" "SELECT slot_name, active, wal_status, pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn) AS behind FROM pg_replication_slots;"

pq "$PRIMARY" "DROP TABLE rec04_burner;" >/dev/null
pq "$PRIMARY" "CHECKPOINT;" >/dev/null 2>&1

banner "Step 3: restart node-$TARGET"
process-compose process start "node-$TARGET" 2>&1 | tail -3
echo "start invoked at $(date -Is)"

banner "Step 4: wait for AutoRebootstrap or catch-up"
T=0; TIMEOUT=240
while [ "$T" -lt "$TIMEOUT" ]; do
  if docker ps --format '{{.Names}}' | grep -q "pgman-pc-node-$TARGET"; then
    STATE=$(pq "$TARGET" "SELECT pg_is_in_recovery()" 2>/dev/null)
    STREAMING=$(pq "$PRIMARY" "SELECT count(*) FROM pg_stat_replication WHERE application_name='node-$TARGET'" 2>/dev/null)
    echo "T+${T}s state=$STATE streaming=$STREAMING"
    if [ "$STATE" = "t" ] && [ "$STREAMING" = "1" ]; then
      break
    fi
  else
    echo "T+${T}s container not running"
  fi
  sleep 5
  T=$((T + 5))
done

echo "--- rebootstrap-related events on node-$TARGET ---"
docker logs "pgman-pc-node-$TARGET" 2>&1 | grep -iE "rebootstrap|basebackup|auto_demote|wipe|streaming.resumed|stale" | tail -40

banner "Step 5: fingerprint comparison"
pq "$PRIMARY" "CHECKPOINT;" >/dev/null 2>&1
# wait for replay
T=0
while [ "$T" -lt 60 ]; do
  PLSN=$(pq "$PRIMARY" "SELECT pg_current_wal_insert_lsn();")
  RLSN=$(pq "$TARGET" "SELECT pg_last_wal_replay_lsn();")
  echo "T+${T}s primary_lsn=$PLSN replay_lsn=$RLSN"
  if [ "$PLSN" = "$RLSN" ]; then
    break
  fi
  sleep 3
  T=$((T + 3))
done

H_QUERY="SELECT count(*) || '|' || coalesce(sum(length(payload))::text,'NIL') || '|' || coalesce(md5(string_agg(writer_id||'|'||seq, ',' ORDER BY writer_id, seq)),'NIL') || '|' || coalesce(md5(string_agg(encode(payload,'hex'), ',' ORDER BY writer_id, seq)),'NIL') FROM chaos_events;"

HP=$(pq "$PRIMARY" "$H_QUERY")
HS=$(pq "$TARGET" "$H_QUERY")
echo "primary[$PRIMARY]: $HP"
echo "standby[$TARGET]: $HS"

if [ "$HP" = "$HS" ]; then
  VERDICT="PASS"
else
  sleep 6
  HP2=$(pq "$PRIMARY" "$H_QUERY")
  HS2=$(pq "$TARGET" "$H_QUERY")
  echo "retry primary: $HP2"
  echo "retry standby: $HS2"
  if [ "$HP2" = "$HS2" ]; then VERDICT="PASS"; else VERDICT="FAIL"; fi
fi

banner "VERDICT REC-04: $VERDICT"
echo "post ctrs: $(counters)"
echo "post slots:"; slot_list "$PRIMARY"
echo "log written to $LOG"
