#!/usr/bin/env bash
# REC-01 - standby.signal pre-emption: force the bug, verify f1d67d7 catches it.
# REQ-HEAL-01 / AC-HEAL-01a (the standby.signal load-bearing fix)
#
# Stop a standby, delete standby.signal from its PGDATA volume, start it.
# PASS  the proxy's ensureStandbySignalIfInitialized recreates the file
#       before pg_ctl start; node comes up as standby; no split-brain.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
source "$HERE/lib.sh"

LOG="$HERE/logs/REC-01-$(date +%Y%m%dT%H%M%S).log"
exec > >(tee -a "$LOG") 2>&1

banner "REC-01 — pre-state"
snapshot
P=$(current_primary)
echo "primary: $P"
# Pick a standby (NOT the primary).
TARGET=""
for n in a b c; do
  if [ "$n" != "$P" ]; then TARGET="$n"; break; fi
done
echo "target standby (to delete standby.signal on): node-$TARGET"

banner "REC-01 — stop target via process-compose"
process-compose process stop "node-$TARGET" 2>&1
# Wait until container gone.
for i in $(seq 1 30); do
  if ! docker ps --format '{{.Names}}' | grep -q "pgman-pc-node-$TARGET"; then break; fi
  sleep 1
done
docker ps --format '{{.Names}}\t{{.Status}}' | grep pgman-pc-node || true

banner "REC-01 — delete standby.signal from PGDATA volume"
docker run --rm -v "pgman-pc-node-$TARGET-data":/data alpine sh -c \
  'echo "before:"; ls -la /data/standby.signal 2>&1 || true; rm -f /data/standby.signal; echo "after:"; ls -la /data/standby.signal 2>&1 || true; echo "data top:"; ls /data | head'

banner "REC-01 — start target again"
process-compose process start "node-$TARGET" 2>&1

# Wait until container appears.
for i in $(seq 1 60); do
  if docker ps --format '{{.Names}}' | grep -q "pgman-pc-node-$TARGET"; then break; fi
  sleep 1
done

banner "REC-01 — wait 30s for boot, then inspect standby.signal"
for i in $(seq 1 30); do
  sleep 1
  if docker exec "pgman-pc-node-$TARGET" sh -c 'ls /var/lib/postgresql/data/standby.signal 2>/dev/null' >/dev/null 2>&1; then
    echo "standby.signal present at t+${i}s"
    break
  fi
done
docker exec "pgman-pc-node-$TARGET" sh -c 'ls -la /var/lib/postgresql/data/standby.signal 2>&1' || true
docker exec "pgman-pc-node-$TARGET" sh -c 'ls -la /var/lib/postgresql/data/PG_VERSION 2>&1' || true

banner "REC-01 — check for primary status on target"
sleep 10
echo "is_primary on each: a=$(is_primary a) b=$(is_primary b) c=$(is_primary c)"
CP=$(current_primary)
echo "current_primary -> $CP"

banner "REC-01 — grep startup_with_pgdata logs"
docker logs "pgman-pc-node-$TARGET" 2>&1 | grep -i "standby.signal\|startup_with_pgdata" | tail -10

banner "REC-01 — wait for streaming to resume"
for i in $(seq 1 60); do
  sleep 2
  if [ -n "$CP" ]; then
    RS=$(repl_state "$CP" 2>/dev/null | grep -c streaming || true)
    if [ "$RS" -ge 2 ]; then echo "two streaming standbys after ${i}*2s"; break; fi
  fi
done

banner "REC-01 — final state"
snapshot
if [ -n "$CP" ]; then repl_state "$CP"; fi

# Verdict
PRIM_T=$(is_primary "$TARGET")
SPLIT="no"
M=$(current_primary 2>&1)
if [ "${M:0:9}" = "MULTIPLE:" ]; then SPLIT="YES ($M)"; fi

VERDICT="UNKNOWN"
if [ "$SPLIT" != "no" ]; then
  VERDICT="FAIL (split-brain: $SPLIT)"
elif [ "$PRIM_T" = "t" ]; then
  VERDICT="FAIL (target node-$TARGET came up as primary - f1d67d7 fix didn't trigger)"
elif [ "$PRIM_T" = "f" ]; then
  VERDICT="PASS (target node-$TARGET came up as standby; standby.signal re-created)"
fi
banner "REC-01 — VERDICT $VERDICT"
echo "log: $LOG"
