#!/bin/bash
# Bind-mounted into the pgman-pc-pgsql:dev containers under
# /docker-entrypoint-initdb.d/. The postgres image's official entrypoint
# runs *.sh files in this directory exactly once after `initdb`, before
# the server is exposed for client connections.
#
# Why this is needed: the postgres image's POSTGRES_HOST_AUTH_METHOD=trust
# env var only writes `host all all all trust` to pg_hba.conf. The
# `replication` connection class has its own pg_hba match-list and is
# refused by default — so pg_basebackup from a sibling pgman-proxy peer
# fails with "no pg_hba.conf entry for replication connection from
# host …". Adding a permissive replication line unblocks dev-cluster
# follower bootstrap.
#
# DEV ONLY: 0.0.0.0/0 trust is acceptable here because the listener is
# bound to 127.0.0.1 on the host and only reachable from the docker
# bridge gateway. NEVER use this in production.
set -euo pipefail
echo "host  replication  all  0.0.0.0/0  trust" >> "${PGDATA}/pg_hba.conf"
echo "host  replication  all  ::/0       trust" >> "${PGDATA}/pg_hba.conf"
