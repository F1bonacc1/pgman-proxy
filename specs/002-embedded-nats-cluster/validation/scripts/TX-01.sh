#!/usr/bin/env bash
# TX-01 - In-flight 10s BEGIN/COMMIT spans a failover
# REQ-DL-03 / AC-DL-03a, AC-DL-03b
#
# Opens a single dedicated psql session through the proxy (one host
# only, so libpq cannot silently fail over). Issues BEGIN; INSERT;
# pg_sleep(10); INSERT; COMMIT. While the sleep is in flight, kill the
# primary container. Expected: client receives a connection-error, NO
# row is committed.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
source "$HERE/lib.sh"

LOG="$HERE/logs/TX-01-$(date +%Y%m%dT%H%M%S).log"
exec > >(tee -a "$LOG") 2>&1

banner "TX-01 — pre-state"
snapshot
P=$(current_primary)
echo "primary: $P"

# Pick the host port for this peer's proxy listener.
case "$P" in
  a) PORT=16432 ;;
  b) PORT=16433 ;;
  c) PORT=16434 ;;
esac
echo "proxy host port: 127.0.0.1:$PORT"

# Clean any prior sentinel.
docker exec "pgman-pc-node-$P" psql -h /var/run/postgresql -U postgres -c \
  "DELETE FROM chaos_events WHERE writer_id='tx-01';" >/dev/null 2>&1 || true

banner "TX-01 — start long psql session in background"
SESSION_LOG="$HERE/logs/TX-01-session-$(date +%Y%m%dT%H%M%S).log"
# Run psql from a SURVIVING peer (not the one we'll kill). Pick another
# peer container to host the psql command — it talks to the target
# proxy by docker-network alias on its 6432.
HOST_CONTAINER=""
for n in a b c; do
  if [ "$n" != "$P" ]; then HOST_CONTAINER="$n"; break; fi
done
echo "session runs from: pgman-pc-node-$HOST_CONTAINER (single-host DSN to node-$P:6432)"
( docker exec -i "pgman-pc-node-$HOST_CONTAINER" psql "host=node-$P port=6432 user=postgres dbname=postgres sslmode=disable connect_timeout=2" \
    -tA -c "BEGIN; INSERT INTO chaos_events (writer_id, seq, payload) VALUES ('tx-01',1,'\x01'); SELECT pg_sleep(10); INSERT INTO chaos_events (writer_id, seq, payload) VALUES ('tx-01',2,'\x02'); COMMIT;" > "$SESSION_LOG" 2>&1; echo "session exit: $?" >> "$SESSION_LOG" ) &
SESSION_PID=$!
echo "background pid: $SESSION_PID"

banner "TX-01 — kill primary 3s after session start (mid-sleep)"
sleep 3
docker kill -s KILL "pgman-pc-node-$P" >/dev/null
KILL_TS=$(date -Ins)
echo "KILL TS: $KILL_TS"

banner "TX-01 — wait for session to finish (it should error out)"
wait $SESSION_PID || true
echo "---- session output ----"
cat "$SESSION_LOG"
echo "---- end ----"

banner "TX-01 — wait for new primary"
NEW_P=""
for i in $(seq 1 60); do
  sleep 1
  C=$(current_primary)
  if [ -n "$C" ] && [ "$C" != "$P" ] && [ "${C:0:9}" != "MULTIPLE:" ]; then
    NEW_P="$C"
    echo "new primary at t+${i}s: $NEW_P"
    break
  fi
done

banner "TX-01 — probe tx-01 rows on new primary"
ROW=$(docker exec "pgman-pc-node-$NEW_P" psql -h /var/run/postgresql -U postgres -tAc \
  "SELECT writer_id, seq FROM chaos_events WHERE writer_id='tx-01' ORDER BY seq;" 2>&1)
echo "probe rows: ----"
echo "$ROW"
echo "-----"
COUNT=$(echo "$ROW" | grep -c "tx-01|" || true)
echo "count: $COUNT"

banner "TX-01 — final state (settle 20s)"
sleep 20
snapshot

# Verdict
SESSION_OK=$(grep -c "INSERT 0 1" "$SESSION_LOG" || true)
SESSION_HAS_ERR=$(grep -ciE "FATAL|connection|terminat|reset|closed" "$SESSION_LOG" || true)

VERDICT="UNKNOWN"
if [ "$COUNT" = "0" ] && [ "$SESSION_HAS_ERR" -gt 0 ]; then
  VERDICT="PASS (no tx-01 rows on new primary; client saw connection error)"
elif [ "$COUNT" = "2" ]; then
  VERDICT="FAIL? (both rows present - client may have silently been routed)"
elif [ "$COUNT" = "1" ]; then
  VERDICT="FAIL (partial commit - 1 of 2 rows present)"
elif [ "$COUNT" = "0" ] && [ "$SESSION_HAS_ERR" -eq 0 ]; then
  VERDICT="INCONCLUSIVE (no rows but no client error - was the session ever live?)"
fi
banner "TX-01 — VERDICT $VERDICT"
echo "session log: $SESSION_LOG"
echo "scenario log: $LOG"
