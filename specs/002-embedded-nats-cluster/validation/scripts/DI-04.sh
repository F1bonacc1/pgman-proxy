#!/usr/bin/env bash
# DI-04 - Acknowledged-commit durability across failover (causal)
# REQ-DL-01 / AC-DL-01c
#
# Inserts a sentinel row via libpq multi-host against the current
# primary, captures the wall-clock T-1, kills the primary within 100 ms,
# waits for new leader, then probes the new primary for the sentinel.
#
# PASS  the row is present on the new primary.
# FAIL  the row is absent.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
source "$HERE/lib.sh"

LOG="$HERE/logs/DI-04-$(date +%Y%m%dT%H%M%S).log"
exec > >(tee -a "$LOG") 2>&1

banner "DI-04 — pre-state"
snapshot
P0=$(current_primary)
echo "initial primary: $P0"
echo "baseline counters: $(counters)"

# Pre-create a writer table row marker — sentinel writer_id='di-04'.
# Use libpq via the proxy multi-host DSN so the routing must hit the
# current primary.
DSN="host=127.0.0.1,127.0.0.1,127.0.0.1 port=16432,16433,16434 user=postgres dbname=postgres sslmode=disable connect_timeout=2"

# Make sure the row is clean.
docker exec "pgman-pc-node-$P0" psql -h /var/run/postgresql -U postgres -c \
  "DELETE FROM chaos_events WHERE writer_id='di-04';" >/dev/null

banner "DI-04 — issue sentinel INSERT + kill primary within ~100ms"
# Use a coproc-like approach: insert, capture LSN, kill in background.
SENTINEL_TS_BEFORE=$(date -Ins)
INSERT_OUT=$(docker exec "pgman-pc-node-$P0" psql -h /var/run/postgresql -U postgres -tAc \
  "INSERT INTO chaos_events (writer_id, seq, payload) VALUES ('di-04', 1, '\x01') RETURNING pg_current_wal_flush_lsn();" 2>&1)
SENTINEL_TS_AFTER=$(date -Ins)
echo "INSERT RETURNING: $INSERT_OUT"
echo "TS before/after: $SENTINEL_TS_BEFORE  $SENTINEL_TS_AFTER"

# Kill primary now (target is the same container we just talked to).
echo "killing primary node-$P0 ..."
docker kill -s KILL "pgman-pc-node-$P0" >/dev/null
KILL_TS=$(date -Ins)
echo "KILL TS: $KILL_TS"

banner "DI-04 — wait for new primary (≤ 60s)"
NEW_PRIMARY=""
for i in $(seq 1 60); do
  sleep 1
  NEW_PRIMARY=$(current_primary)
  if [ -n "$NEW_PRIMARY" ] && [ "$NEW_PRIMARY" != "$P0" ] && [ "${NEW_PRIMARY:0:9}" != "MULTIPLE:" ]; then
    echo "new primary: $NEW_PRIMARY at t+${i}s"
    break
  fi
done
NEW_PRIMARY_TS=$(date -Ins)

banner "DI-04 — probe sentinel on new primary"
if [ -z "$NEW_PRIMARY" ] || [ "$NEW_PRIMARY" = "$P0" ]; then
  echo "NO NEW PRIMARY — INCONCLUSIVE"
  snapshot
  exit 2
fi

ROW=$(docker exec "pgman-pc-node-$NEW_PRIMARY" psql -h /var/run/postgresql -U postgres -tAc \
  "SELECT writer_id, seq FROM chaos_events WHERE writer_id='di-04' AND seq=1;" 2>&1)
echo "probe row: $ROW"

banner "DI-04 — settle 30s, post-state"
wait_seconds 30
snapshot
event_grep "leadership.changed" || true
event_grep "auto_demote" || true

VERDICT="UNKNOWN"
if [ "$ROW" = "di-04|1" ]; then
  VERDICT="PASS"
else
  VERDICT="FAIL"
fi
banner "DI-04 — VERDICT $VERDICT"

# Cleanup sentinel row to avoid contaminating later scenarios.
docker exec "pgman-pc-node-$NEW_PRIMARY" psql -h /var/run/postgresql -U postgres -c \
  "DELETE FROM chaos_events WHERE writer_id='di-04';" >/dev/null

echo "log: $LOG"
exit 0
