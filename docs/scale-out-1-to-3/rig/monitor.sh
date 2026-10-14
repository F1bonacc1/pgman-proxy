#!/usr/bin/env bash
# usage: monitor.sh <outfile> <seconds>
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"
out=$1; dur=$2; end=$((SECONDS+dur))
while [ $SECONDS -lt $end ]; do
  line="$(ts)"
  for n in a b c; do
    st=$(docker inspect -f '{{.State.Status}}' pgv-node-$n 2>/dev/null || echo none)
    if [ "$st" = running ]; then
      rec=$(timeout 3 docker exec pgv-node-$n psql -h /var/run/postgresql -U postgres -XAtqc "select case when pg_is_in_recovery() then 'standby' else 'primary' end" 2>/dev/null || echo down)
      cnt=$(timeout 3 docker exec pgv-node-$n psql -h /var/run/postgresql -U postgres -XAtqc "select count(*) from bulk" 2>/dev/null || echo -)
      ssn=$(timeout 3 docker exec pgv-node-$n psql -h /var/run/postgresql -U postgres -XAtqc "show synchronous_standby_names" 2>/dev/null || echo -)
      sid=$(sysid $n)
    else rec=$st; cnt=-; ssn=-; sid=-; fi
    line="$line | $n:$rec rows=$cnt sid=[$sid] ssn=[$ssn]"
  done
  # write probe through node-a's local socket (3s budget)
  w=$(timeout 3 docker exec pgv-node-a psql -h /var/run/postgresql -U postgres -XAtqc "insert into probe(src) values ('p2') returning 'W_OK'" 2>&1 | tr '\n' ' ' | cut -c1-60)
  [ -z "$w" ] && w="W_TIMEOUT"
  echo "$line | write_a=$w" >> $out
  sleep 2
done
