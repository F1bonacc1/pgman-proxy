#!/usr/bin/env bash
# SOAK-01 — 10-minute sustained-load periodic-kill soak.
#
# Strategy:
#   - 10 min wall clock.
#   - Every 2 min, kill a random NON-PRIMARY peer (docker kill -s KILL).
#     restart:always on the docker run wrapper brings the container back
#     automatically.
#   - At the 5-min mark, kill the PRIMARY once to force failover.
#   - Poll cluster state every 10s: counters, primary identity, slot list,
#     replication lag.
#   - 60s post-settle, then verdict.

set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
source "$HERE/lib.sh"

LOG="$HERE/logs/SOAK-01-$(date -u +%Y%m%dT%H%M%SZ).log"
exec > >(tee -a "$LOG") 2>&1

banner "SOAK-01 — 10-min soak with periodic kills"
PRE_PRIMARY="$(current_primary)"
echo "pre-state primary=$PRE_PRIMARY"
slot_list "$PRE_PRIMARY"
PRE_DL=$(chaos_field data_loss_total); PRE_DL=${PRE_DL:-0}
PRE_ER=$(chaos_field extra_rows);      PRE_ER=${PRE_ER:-0}
PRE_WO=$(chaos_field writes_ok);       PRE_WO=${PRE_WO:-0}
PRE_WF=$(chaos_field writes_failed);   PRE_WF=${PRE_WF:-0}
echo "PRE counters: writes_ok=$PRE_WO writes_failed=$PRE_WF data_loss_total=$PRE_DL extra_rows=$PRE_ER"

START=$(date +%s)
END=$((START + 600))            # 10 min
NEXT_KILL_NONPRIMARY=$((START + 120))  # every 2 min
KILLED_PRIMARY_AT=$((START + 300))     # one primary kill at 5min mark
PRIMARY_KILLED=0

banner "Soak loop start at $(date -Is)"
while [ "$(date +%s)" -lt "$END" ]; do
  NOW=$(date +%s)
  ELAPSED=$((NOW - START))

  # Periodic non-primary kill
  if [ "$NOW" -ge "$NEXT_KILL_NONPRIMARY" ]; then
    P=$(current_primary)
    # pick a non-primary that's currently running
    VICTIM=""
    for n in a b c; do
      if [ "$n" != "$P" ] && docker ps --format '{{.Names}}' | grep -q "pgman-pc-node-$n"; then
        VICTIM="$n"; break
      fi
    done
    if [ -n "$VICTIM" ]; then
      echo "[t+${ELAPSED}s] KILL non-primary node-$VICTIM (primary was $P)"
      docker kill -s KILL "pgman-pc-node-$VICTIM" 2>&1 | head -1
    fi
    NEXT_KILL_NONPRIMARY=$((NOW + 120))
  fi

  # One-shot primary kill at 5-min mark
  if [ "$PRIMARY_KILLED" -eq 0 ] && [ "$NOW" -ge "$KILLED_PRIMARY_AT" ]; then
    P=$(current_primary)
    if [ -n "$P" ] && [ "${P:0:9}" != "MULTIPLE:" ]; then
      echo "[t+${ELAPSED}s] KILL PRIMARY node-$P"
      docker kill -s KILL "pgman-pc-node-$P" 2>&1 | head -1
      PRIMARY_KILLED=1
    fi
  fi

  # 10s snapshot
  P=$(current_primary)
  CTRS=$(counters)
  SLOTC=$(pq "${P:-b}" "SELECT count(*) FROM pg_replication_slots" 2>/dev/null)
  SLOTL=$(pq "${P:-b}" "SELECT count(*) FROM pg_replication_slots WHERE wal_status='lost'" 2>/dev/null)
  STREAM=$(pq "${P:-b}" "SELECT count(*) FROM pg_stat_replication" 2>/dev/null)
  RUN_CT=$(docker ps --format '{{.Names}}' | grep -c "pgman-pc-node-")
  printf "[t+%4ds] primary=%s running=%s/3 streaming=%s slots=%s slots_lost=%s ctrs=%s\n" \
    "$ELAPSED" "${P:-NONE}" "$RUN_CT" "${STREAM:-NA}" "${SLOTC:-NA}" "${SLOTL:-NA}" "$CTRS"

  sleep 10
done

banner "Soak window ended at $(date -Is) — 60s settle"
T=0
while [ "$T" -lt 60 ]; do
  P=$(current_primary)
  CTRS=$(counters)
  RUN_CT=$(docker ps --format '{{.Names}}' | grep -c "pgman-pc-node-")
  STREAM=$(pq "${P:-b}" "SELECT count(*) FROM pg_stat_replication" 2>/dev/null)
  printf "[settle+%2ds] primary=%s running=%s/3 streaming=%s ctrs=%s\n" \
    "$T" "${P:-NONE}" "$RUN_CT" "${STREAM:-NA}" "$CTRS"
  sleep 10
  T=$((T + 10))
done

banner "Final assessment"
PRIMARY=$(current_primary)
echo "final primary=$PRIMARY"
echo "final slot list:"
slot_list "$PRIMARY"
pq "$PRIMARY" "SELECT slot_name, active, wal_status, pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn) AS behind FROM pg_replication_slots ORDER BY slot_name;"
echo "repl_state:"
repl_state "$PRIMARY"

POST_DL=$(chaos_field data_loss_total); POST_DL=${POST_DL:-0}
POST_ER=$(chaos_field extra_rows);      POST_ER=${POST_ER:-0}
POST_WO=$(chaos_field writes_ok);       POST_WO=${POST_WO:-0}
POST_WF=$(chaos_field writes_failed);   POST_WF=${POST_WF:-0}

echo "POST counters: writes_ok=$POST_WO writes_failed=$POST_WF data_loss_total=$POST_DL extra_rows=$POST_ER"
DL_DELTA=$((POST_DL - PRE_DL))
ER_DELTA=$((POST_ER - PRE_ER))
WO_DELTA=$((POST_WO - PRE_WO))
echo "deltas: writes_ok+$WO_DELTA writes_failed+$((POST_WF - PRE_WF)) data_loss+$DL_DELTA extra_rows+$ER_DELTA"

STREAM=$(pq "$PRIMARY" "SELECT count(*) FROM pg_stat_replication")
SLOTL=$(pq "$PRIMARY" "SELECT count(*) FROM pg_replication_slots WHERE wal_status='lost'")
RUN_CT=$(docker ps --format '{{.Names}}' | grep -c "pgman-pc-node-")

VERDICT="PASS"
REASONS=""
[ "$DL_DELTA" -ne 0 ] && { VERDICT="FAIL"; REASONS="$REASONS data_loss_delta=$DL_DELTA;"; }
[ "$ER_DELTA" -ne 0 ] && { VERDICT="FAIL"; REASONS="$REASONS extra_rows_delta=$ER_DELTA;"; }
[ "$RUN_CT" -ne 3 ] && { VERDICT="FAIL"; REASONS="$REASONS running_containers=$RUN_CT;"; }
[ "$STREAM" -ne 2 ] && { VERDICT="FAIL"; REASONS="$REASONS streaming=$STREAM (want 2);"; }
[ "${SLOTL:-0}" -ne 0 ] && { REASONS="$REASONS slots_lost=$SLOTL (rebootstrap needed);"; }

banner "VERDICT SOAK-01: $VERDICT"
echo "reasons: ${REASONS:-none}"
echo "log: $LOG"
