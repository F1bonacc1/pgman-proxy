#!/usr/bin/env bash
# REPL-03 — Replication-slot leak after permanent peer removal.
#
# Strategy:
#   1. Capture pre-state: pg_replication_slots on primary.
#   2. Permanently remove a non-primary peer (process-compose stop, also
#      docker rm -f to make sure restart:always cannot bring it back).
#   3. Wait > 2 x AutoDemote.Cooldown (~ 60s) and observe.
#   4. Drive sustained writes for 5 min. With max_slot_wal_keep_size=16MB
#      the slot should be invalidated to wal_status=lost (or be dropped
#      entirely by pg-manager's slot-cleanup grace).
#   5. PASS if either: slot dropped, OR slot wal_status=lost and WAL is
#      bounded. FAIL if slot persists with growing WAL pinned.
#   6. Restore: bring the peer back.

set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
source "$HERE/lib.sh"

LOG="$HERE/logs/REPL-03-$(date -u +%Y%m%dT%H%M%SZ).log"
exec > >(tee -a "$LOG") 2>&1

banner "REPL-03 — slot leak after permanent peer removal"
PRIMARY="$(current_primary)"
echo "primary=$PRIMARY"
[ -z "$PRIMARY" ] && { echo "no primary"; exit 2; }

TARGET=""
for n in a b c; do
  if [ "$n" != "$PRIMARY" ]; then TARGET="$n"; break; fi
done
echo "target peer to remove=$TARGET"

banner "Step 1: pre-state"
slot_list "$PRIMARY"
echo "ctrs: $(counters)"

banner "Step 2: permanently remove node-$TARGET"
process-compose process stop "node-$TARGET" 2>&1 | tail -3
# Force-kill the container too just in case.
docker rm -f "pgman-pc-node-$TARGET" 2>/dev/null
until ! docker ps --format '{{.Names}}' | grep -q "pgman-pc-node-$TARGET"; do sleep 1; done
echo "node-$TARGET gone at $(date -Is)"

banner "Step 3: 60s observation window (> 2x AutoDemote.Cooldown)"
T0=$(date +%s)
END=$((T0 + 60))
while [ "$(date +%s)" -lt "$END" ]; do
  NOW=$(date +%s)
  ELAPSED=$((NOW - T0))
  SLOT_LINE=$(pq "$PRIMARY" "SELECT slot_name||'|'||active||'|'||wal_status||'|'||pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn) FROM pg_replication_slots WHERE slot_name='pgmgr_node_$TARGET';")
  CTRS=$(counters)
  printf "t+%2ds slot=%s ctrs=%s\n" "$ELAPSED" "$SLOT_LINE" "$CTRS"
  sleep 5
done

banner "Step 4: sustained-write window — drive WAL past 16MB threshold"
# Force WAL: this is on top of chaos workload's normal writes.
pq "$PRIMARY" "DROP TABLE IF EXISTS repl03_burner; CREATE TABLE repl03_burner (id bigserial PRIMARY KEY, payload bytea);"
T0=$(date +%s)
END=$((T0 + 300))   # 5 min
LAST_PRINT=0
while [ "$(date +%s)" -lt "$END" ]; do
  pq "$PRIMARY" "INSERT INTO repl03_burner (payload) SELECT repeat('y',2000)::bytea FROM generate_series(1,1000);" >/dev/null
  pq "$PRIMARY" "SELECT pg_switch_wal();" >/dev/null
  NOW=$(date +%s)
  if [ $((NOW - LAST_PRINT)) -ge 30 ]; then
    LAST_PRINT=$NOW
    ELAPSED=$((NOW - T0))
    SLOT_LINE=$(pq "$PRIMARY" "SELECT slot_name||'|'||active||'|'||wal_status||'|'||pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn) FROM pg_replication_slots WHERE slot_name='pgmgr_node_$TARGET';")
    SLOT_PRESENT=$(pq "$PRIMARY" "SELECT count(*) FROM pg_replication_slots WHERE slot_name='pgmgr_node_$TARGET'")
    CTRS=$(counters)
    printf "t+%3ds slot_present=%s slot=%s ctrs=%s\n" "$ELAPSED" "$SLOT_PRESENT" "$SLOT_LINE" "$CTRS"
    if [ "$SLOT_PRESENT" = "0" ]; then
      echo "slot dropped at t+${ELAPSED}s"
      break
    fi
  fi
done

# Drop burner data.
pq "$PRIMARY" "DROP TABLE repl03_burner;" >/dev/null
pq "$PRIMARY" "CHECKPOINT;" >/dev/null

banner "Step 5: final slot state assessment"
echo "all slots on primary:"
slot_list "$PRIMARY"
pq "$PRIMARY" "SELECT slot_name, active, wal_status, pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn) AS behind_bytes FROM pg_replication_slots ORDER BY slot_name;"

SLOT_PRESENT=$(pq "$PRIMARY" "SELECT count(*) FROM pg_replication_slots WHERE slot_name='pgmgr_node_$TARGET'")
SLOT_STATUS=$(pq "$PRIMARY" "SELECT wal_status FROM pg_replication_slots WHERE slot_name='pgmgr_node_$TARGET';")

if [ "$SLOT_PRESENT" = "0" ]; then
  VERDICT="PASS (slot dropped)"
elif [ "$SLOT_STATUS" = "lost" ] || [ "$SLOT_STATUS" = "unreserved" ]; then
  VERDICT="PASS (slot invalidated, WAL recyclable)"
else
  VERDICT="FAIL (slot retained, WAL accumulating)"
fi

banner "VERDICT REPL-03: $VERDICT"

banner "Step 6: restore node-$TARGET"
process-compose process start "node-$TARGET" 2>&1 | tail -3
T=0
while [ "$T" -lt 180 ]; do
  if docker ps --format '{{.Names}}' | grep -q "pgman-pc-node-$TARGET"; then
    STATE=$(pq "$TARGET" "SELECT pg_is_in_recovery()" 2>/dev/null)
    REPL=$(pq "$PRIMARY" "SELECT count(*) FROM pg_stat_replication WHERE application_name='node-$TARGET'" 2>/dev/null)
    echo "T+${T}s state=$STATE streaming=$REPL"
    if [ "$STATE" = "t" ] && [ "$REPL" = "1" ]; then break; fi
  fi
  sleep 5
  T=$((T + 5))
done

echo "log: $LOG"
