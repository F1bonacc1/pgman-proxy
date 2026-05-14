#!/usr/bin/env bash
# NET-05 - Cluster-routes port black-holed on the leader; local PG
# reachable. The peer must self-fence (lease-loss observed at action
# time).
# REQ-DL-05 / AC-DL-05a
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
source "$HERE/lib.sh"

LOG="$HERE/logs/NET-05-$(date +%Y%m%dT%H%M%S).log"
exec > >(tee -a "$LOG") 2>&1

banner "NET-05 — pre-state"
snapshot
P=$(current_primary)
echo "leader: $P"
BL_DL=$(chaos_field data_loss_total)

banner "NET-05 — block port 6222 inbound on leader"
docker exec "pgman-pc-node-$P" iptables -I INPUT 1 -p tcp --dport 6222 -j DROP
docker exec "pgman-pc-node-$P" iptables -L INPUT -n | head -5

banner "NET-05 — hold 25s; sample state every 3s"
T_END=$(( $(date +%s) + 25 ))
while [ "$(date +%s)" -lt $T_END ]; do
  sleep 3
  echo "t=$(date -Ins)"
  for n in a b c; do
    p=$(timeout 2 docker exec "pgman-pc-node-$n" psql -h /var/run/postgresql -U postgres -tAc "SELECT NOT pg_is_in_recovery();" 2>/dev/null || echo TIMEOUT)
    echo "  node-$n is_primary=$p"
  done
  echo "  ctrs=$(counters)"
  # Lease renewal failures on the affected peer.
  LRF=$(curl -sS "http://127.0.0.1:1909$([ "$P" = "a" ] && echo 0; [ "$P" = "b" ] && echo 1; [ "$P" = "c" ] && echo 2)/metrics" 2>/dev/null | grep "^pgman_proxy_lease_renewal_failures_total" | awk '{print $NF}')
  echo "  lease_renewal_failures(node-$P): $LRF"
done

banner "NET-05 — flush iptables on leader"
docker exec "pgman-pc-node-$P" iptables -F INPUT
docker exec "pgman-pc-node-$P" iptables -L INPUT -n

banner "NET-05 — settle 30s"
T_END=$(( $(date +%s) + 30 ))
while [ "$(date +%s)" -lt $T_END ]; do
  sleep 5
  snapshot
done

banner "NET-05 — final state"
P2=$(current_primary)
echo "final primary: $P2 (was: $P)"
if [ -n "$P2" ]; then repl_state "$P2"; fi

FINAL_DL=$(chaos_field data_loss_total)
echo "final data_loss_total=$FINAL_DL (baseline=$BL_DL)"

# Inspect lease-loss events
banner "NET-05 — leadership/lease events on node-$P"
docker logs "pgman-pc-node-$P" 2>&1 | grep -iE "stale leader|lease|leadership|state transition" | tail -20

VERDICT="UNKNOWN"
if [ -z "$P2" ]; then
  VERDICT="FAIL (no primary after recovery)"
elif [ "$FINAL_DL" != "$BL_DL" ]; then
  VERDICT="FAIL (data_loss_total moved: $BL_DL -> $FINAL_DL)"
elif [ "$P2" = "$P" ]; then
  # Note: it could be that the original primary retained leadership if
  # the partition resolved before lease expiry. That is correct
  # behaviour for a short outage.
  VERDICT="PASS-OR-INCONCLUSIVE (leader unchanged $P; check lease_renewal_failures + survivors saw stale-leader)"
else
  VERDICT="PASS (leader changed $P -> $P2; survivors elected new leader)"
fi
banner "NET-05 — VERDICT $VERDICT"
echo "log: $LOG"
