IMG=${IMG:-pgman-proxy-it:latest}
NET=pgv-net
D="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ts() { date +%H:%M:%S.%3N; }
mkvol() { docker volume create "$1" >/dev/null; docker run --rm -u root --entrypoint chown -v "$1":/v $IMG -R postgres:postgres /v; }
start_node() { # $1=node letter $2=envfile
  docker rm -f pgv-node-$1 >/dev/null 2>&1
  docker run -d --name pgv-node-$1 --hostname node-$1 --network $NET --network-alias node-$1 \
    --env-file $D/common.env --env-file $D/$2 \
    -v pgv-$1-data:/var/lib/postgresql/data -v pgv-$1-js:/var/lib/postgresql/js $IMG >/dev/null
}
psqlx() { local n=$1; shift; docker exec pgv-node-$n psql -h /var/run/postgresql -U postgres -XAtq "$@" 2>&1; }
sysid() { docker exec pgv-node-$1 /usr/lib/postgresql/17/bin/pg_controldata -D /var/lib/postgresql/data 2>/dev/null | awk -F: '/system identifier/{gsub(/ /,"",$2);print $2} /TimeLineID/ && /Latest checkpoint/{gsub(/ /,"",$2);print "tli=" $2}' | paste -sd' '; }
reset_all() {
  docker rm -f pgv-node-a pgv-node-b pgv-node-c >/dev/null 2>&1
  for n in a b c; do docker volume rm -f pgv-$n-data pgv-$n-js >/dev/null 2>&1; done
  docker network rm $NET >/dev/null 2>&1
  docker network create $NET >/dev/null
  for n in a b c; do mkvol pgv-$n-data; mkvol pgv-$n-js; done
}
restore_golden() {
  docker network create $NET >/dev/null 2>&1
  docker rm -f pgv-node-a pgv-node-b pgv-node-c >/dev/null 2>&1
  for n in a b c; do docker volume rm -f pgv-$n-data pgv-$n-js >/dev/null 2>&1; done
  for n in a b c; do mkvol pgv-$n-data; mkvol pgv-$n-js; done
  for v in data js; do docker run --rm -u root --entrypoint sh -v pgv-golden-$v:/src -v pgv-a-$v:/dst $IMG -c 'cp -a /src/. /dst/'; done
}
start_node_x() { # $1=node $2=envfile $3=extra envfile
  docker rm -f pgv-node-$1 >/dev/null 2>&1
  docker run -d --name pgv-node-$1 --hostname node-$1 --network $NET --network-alias node-$1 \
    --env-file $D/common.env --env-file $D/$2 --env-file $D/$3 \
    -v pgv-$1-data:/var/lib/postgresql/data -v pgv-$1-js:/var/lib/postgresql/js $IMG >/dev/null
}
run_trial() { # $1=name $2=extra-env for a $3=extra-env for b,c $4=monitor seconds
  restore_golden
  rm -f $D/$1.mon; $D/monitor.sh $D/$1.mon $4 &
  echo "$(ts) start b,c"; start_node_x b three-b.env $3; start_node_x c three-c.env $3
  sleep 1; echo "$(ts) start a"; start_node_x a three-a.env $2
  wait
  for n in a b c; do docker logs pgv-node-$n > $D/$1-$n.log 2>&1; done
}
