#!/usr/bin/env bash
# NET-01 - One non-leader peer partitioned from siblings for 60s
# REQ-HEAL-03 / AC-HEAL-03a, AC-HEAL-03b
#
# Picks a non-leader peer; docker network disconnect; holds 60s while
# workload runs; reconnect; verify final mesh, counters, and no split
# brain.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
source "$HERE/lib.sh"

LOG="$HERE/logs/NET-01-$(date +%Y%m%dT%H%M%S).log"
exec > >(tee -a "$LOG") 2>&1

banner "NET-01 — pre-state"
snapshot
P=$(current_primary)
echo "leader: $P"
TARGET=""
for n in a b c; do
  if [ "$n" != "$P" ]; then TARGET="$n"; break; fi
done
echo "partition target (non-leader): node-$TARGET"
BL_OK=$(chaos_field writes_ok)
BL_DL=$(chaos_field data_loss_total)
BL_ER=$(chaos_field extra_rows)
echo "baseline writes_ok=$BL_OK dl=$BL_DL er=$BL_ER"

banner "NET-01 — disconnect node-$TARGET from pgman-pc-net"
docker network disconnect pgman-pc-net "pgman-pc-node-$TARGET"
DC_TS=$(date -Ins)
echo "disconnected at $DC_TS"

banner "NET-01 — observe survivors for 60s"
T_END=$(( $(date +%s) + 60 ))
while [ "$(date +%s)" -lt $T_END ]; do
  sleep 5
  # Probe primary (survivor) — partitioned node will time out.
  echo "t=$(date -Ins)  primary-survivor probe:"
  for n in a b c; do
    [ "$n" = "$TARGET" ] && continue
    p=$(timeout 2 docker exec "pgman-pc-node-$n" psql -h /var/run/postgresql -U postgres -tAc "SELECT NOT pg_is_in_recovery();" 2>/dev/null || echo "TIMEOUT")
    echo "  node-$n is_primary=$p"
  done
  # Counters
  echo "  ctrs=$(counters)"
done

banner "NET-01 — reconnect node-$TARGET"
docker network connect --alias "node-$TARGET" pgman-pc-net "pgman-pc-node-$TARGET"
RC_TS=$(date -Ins)
echo "reconnected at $RC_TS"

banner "NET-01 — settle 60s post-reconnect"
T_END=$(( $(date +%s) + 60 ))
while [ "$(date +%s)" -lt $T_END ]; do
  sleep 10
  snapshot
done

banner "NET-01 — final state"
snapshot
P2=$(current_primary)
echo "final primary: $P2 (was: $P)"
if [ -n "$P2" ]; then repl_state "$P2"; fi

# Event greps
banner "NET-01 — route_down / route_up event counts"
for n in a b c; do
  RD=$(docker logs "pgman-pc-node-$n" 2>&1 | grep -c '"event":"embedded_nats.route_down"' || true)
  RU=$(docker logs "pgman-pc-node-$n" 2>&1 | grep -c '"event":"embedded_nats.route_up"' || true)
  echo "node-$n: route_down=$RD route_up=$RU"
done

FINAL_DL=$(chaos_field data_loss_total)
FINAL_ER=$(chaos_field extra_rows)
FINAL_OK=$(chaos_field writes_ok)

VERDICT="UNKNOWN"
if [ -z "$P2" ]; then
  VERDICT="FAIL (no primary)"
elif [ "$P2" != "$P" ]; then
  VERDICT="UNUSUAL (leader changed during non-leader partition? $P -> $P2)"
elif [ "$FINAL_DL" != "$BL_DL" ]; then
  VERDICT="FAIL (data_loss_total moved: $BL_DL -> $FINAL_DL)"
elif [ "$FINAL_OK" -le "$BL_OK" ]; then
  VERDICT="FAIL (writes_ok did not grow: $BL_OK -> $FINAL_OK)"
else
  VERDICT="PASS (leader stable=$P2, writes_ok $BL_OK->$FINAL_OK, dl stable=$FINAL_DL)"
fi
banner "NET-01 — VERDICT $VERDICT"
echo "log: $LOG"
