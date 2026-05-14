#!/usr/bin/env bash
# NET-04 - Long partition (> AutoDemote.Cooldown 30s) of the current
# leader. Survivors must elect a new leader; reconnected ex-primary
# must self-demote (PGDATA wipe + re-basebackup).
# REQ-HEAL-01 / REQ-HEAL-03 / AC-HEAL-01c
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
source "$HERE/lib.sh"

LOG="$HERE/logs/NET-04-$(date +%Y%m%dT%H%M%S).log"
exec > >(tee -a "$LOG") 2>&1

banner "NET-04 — pre-state"
snapshot
P=$(current_primary)
echo "leader: $P"
BASELINE_DL=$(chaos_field data_loss_total)
BASELINE_ER=$(chaos_field extra_rows)
echo "baseline data_loss_total=$BASELINE_DL extra_rows=$BASELINE_ER"

banner "NET-04 — disconnect leader from pgman-pc-net"
DISC_TS=$(date -Ins)
docker network disconnect pgman-pc-net "pgman-pc-node-$P"
echo "disconnected at $DISC_TS"

banner "NET-04 — observe survivors elect new leader (poll every 2s for 60s)"
NEW_PRIMARY=""
for i in $(seq 1 30); do
  sleep 2
  CUR=$(current_primary)
  if [ -n "$CUR" ] && [ "$CUR" != "$P" ] && [ "${CUR:0:9}" != "MULTIPLE:" ]; then
    NEW_PRIMARY="$CUR"
    echo "t+$((i*2))s: new primary = $NEW_PRIMARY"
    break
  else
    echo "t+$((i*2))s: primary=$CUR (still old or none yet)"
  fi
done

# Continue running workload for some time while partitioned.
banner "NET-04 — partition held 45s total, monitor counters"
HOLD_TARGET=45
START_HOLD=$(date +%s)
while [ "$(($(date +%s) - START_HOLD))" -lt $HOLD_TARGET ]; do
  sleep 5
  snapshot
done

banner "NET-04 — reconnect leader"
docker network connect --alias "node-$P" pgman-pc-net "pgman-pc-node-$P"
RECON_TS=$(date -Ins)
echo "reconnected at $RECON_TS"

banner "NET-04 — wait 90s for auto_demote / rebootstrap"
for i in $(seq 1 18); do
  sleep 5
  snapshot
done

banner "NET-04 — inspect logs for divergence/auto-demote events"
for n in a b c; do
  echo "==== node-$n ===="
  docker logs "pgman-pc-node-$n" 2>&1 \
    | grep -iE "divergen|auto_demote|stale leader|rebootstrap|standby.signal|state transition" \
    | tail -20
done

banner "NET-04 — final state"
P2=$(current_primary)
echo "final primary: $P2 (was: $P)"
echo "is_primary on each: a=$(is_primary a) b=$(is_primary b) c=$(is_primary c)"
if [ -n "$P2" ]; then repl_state "$P2"; fi

# Verdict
FINAL_DL=$(chaos_field data_loss_total)
FINAL_ER=$(chaos_field extra_rows)
echo "final data_loss_total=$FINAL_DL extra_rows=$FINAL_ER (baseline dl=$BASELINE_DL er=$BASELINE_ER)"

# Multiple-primary check
MULT=$(current_primary 2>&1)
SPLIT="no"
if [ "${MULT:0:9}" = "MULTIPLE:" ]; then SPLIT="YES"; fi

VERDICT="UNKNOWN"
if [ "$SPLIT" = "YES" ]; then
  VERDICT="FAIL (split-brain: $MULT)"
elif [ -z "$P2" ]; then
  VERDICT="FAIL (no primary after recovery)"
elif [ "$P2" = "$P" ] && [ -n "$NEW_PRIMARY" ] && [ "$NEW_PRIMARY" != "$P" ]; then
  VERDICT="FAIL (ex-primary still primary; election did not stick)"
elif [ -z "$NEW_PRIMARY" ]; then
  VERDICT="FAIL (no new leader elected during partition)"
elif [ "$FINAL_DL" != "$BASELINE_DL" ]; then
  VERDICT="FAIL (data_loss_total moved $BASELINE_DL -> $FINAL_DL)"
else
  VERDICT="PASS (new leader=$NEW_PRIMARY, final=$P2, dl stable at $FINAL_DL)"
fi
banner "NET-04 — VERDICT $VERDICT"
echo "log: $LOG"
