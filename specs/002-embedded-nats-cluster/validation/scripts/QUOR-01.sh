#!/usr/bin/env bash
# QUOR-01 - Primary alone, both standbys down, sync_commit=on, ANY 1 (...)
# REQ-DL-01 (sync-block)
#
# Stops both standbys via `process-compose stop` (no docker auto-restart),
# then attempts an INSERT against the primary with a hard 8s timeout.
# Expected behaviour:
#   PASS if the INSERT (a) hangs / times out waiting for sync standby
#         OR returns an error indicating "no sync replicas"
#   FAIL if the INSERT silently succeeds (== fake durability)
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
source "$HERE/lib.sh"

LOG="$HERE/logs/QUOR-01-$(date +%Y%m%dT%H%M%S).log"
exec > >(tee -a "$LOG") 2>&1

banner "QUOR-01 — pre-state"
snapshot
P=$(current_primary)
echo "primary: $P"
sync_standby_names "$P"
sync_commit "$P"
repl_state "$P"

# Determine the two standby letters.
STANDBYS=()
for n in a b c; do
  if [ "$n" != "$P" ]; then STANDBYS+=("$n"); fi
done
echo "standbys: ${STANDBYS[*]}"

banner "QUOR-01 — stop both standbys (process-compose stop) so docker won't auto-restart"
process-compose process stop "node-${STANDBYS[0]}" 2>&1 || true
process-compose process stop "node-${STANDBYS[1]}" 2>&1 || true

# Wait for both containers to disappear.
sleep 5
docker ps --format '{{.Names}} {{.Status}}' | grep pgman-pc-node
echo "still-running primaries:"
for n in a b c; do echo "  node-$n is_primary=$(is_primary $n 2>/dev/null)"; done

banner "QUOR-01 — repl state on primary (should be empty)"
repl_state "$P"
echo "synchronous_standby_names: $(sync_standby_names "$P")"

banner "QUOR-01 — attempt INSERT with 8s server-side statement_timeout"
T0=$(date -Ins)
# Use psql -c with a wrapping query that sets statement_timeout. If
# block-on-sync-loss is honoured, the COMMIT should fail/hang.
INSERT_OUT=$(timeout 12 docker exec "pgman-pc-node-$P" psql -h /var/run/postgresql -U postgres -tAc \
  "SET statement_timeout = '8s'; INSERT INTO chaos_events (writer_id, seq, payload) VALUES ('quor-01', 1, '\x01');" 2>&1; echo "RC=$?")
T1=$(date -Ins)
echo "$INSERT_OUT"
echo "wall T0: $T0"
echo "wall T1: $T1"

banner "QUOR-01 — verify whether row was committed despite no sync standby"
# If sync-block is honoured the row should NOT be visible on COMMIT
# even after we cancel.  Check it.
ROW=$(docker exec "pgman-pc-node-$P" psql -h /var/run/postgresql -U postgres -tAc \
  "SELECT writer_id, seq FROM chaos_events WHERE writer_id='quor-01';" 2>&1)
echo "post-row probe: $ROW"

banner "QUOR-01 — bring standbys back"
process-compose process start "node-${STANDBYS[0]}" 2>&1 || true
process-compose process start "node-${STANDBYS[1]}" 2>&1 || true

# Wait for streaming
for i in $(seq 1 60); do
  sleep 2
  RS=$(repl_state "$P" 2>/dev/null | grep -c streaming || true)
  if [ "$RS" -ge 2 ]; then
    echo "both standbys streaming after ${i}*2s"
    break
  fi
done

banner "QUOR-01 — post-state"
snapshot
repl_state "$P"

# Cleanup
docker exec "pgman-pc-node-$P" psql -h /var/run/postgresql -U postgres -c \
  "DELETE FROM chaos_events WHERE writer_id='quor-01';" >/dev/null 2>&1 || true

# Verdict logic:
#  - if INSERT_OUT contains "canceling statement due to statement timeout"
#    => block honoured => PASS
#  - if INSERT_OUT shows INSERT 0 1 with RC=0  => silent success => FAIL
#  - any other => INCONCLUSIVE
VERDICT="INCONCLUSIVE"
if echo "$INSERT_OUT" | grep -q "statement timeout"; then
  VERDICT="PASS (commit blocked - statement_timeout fired waiting for sync)"
elif echo "$INSERT_OUT" | grep -q "RC=124"; then
  VERDICT="PASS (commit blocked - timeout 12s outer fired)"
elif echo "$INSERT_OUT" | grep -q "INSERT 0 1" && echo "$INSERT_OUT" | grep -q "RC=0"; then
  if echo "$ROW" | grep -q "quor-01|1"; then
    VERDICT="FAIL (silent commit success with zero sync replicas)"
  else
    VERDICT="INCONCLUSIVE (RC=0 reported but row not present)"
  fi
fi
banner "QUOR-01 — VERDICT $VERDICT"
echo "log: $LOG"
