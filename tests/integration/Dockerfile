# Integration-test image for pgman-proxy.
#
# Each peer in the test topology must run pgman-proxy *and* its colocated
# Postgres in the same container — pg-manager (which pgman-proxy embeds)
# drives initdb / pg_basebackup / pg_ctl directly against the local
# PGDATA, so a separate "postgres" container is structurally wrong.
#
# Production ships the distroless image (deploy/docker/Dockerfile). This
# is purposely a fatter image: integration tests need the full Postgres
# bin set, plus a writable PGDATA owned by the postgres user.
#
# IMPORTANT: do NOT pre-run initdb in this image and do NOT start
# Postgres before pgman-proxy. The first call into pg-manager observes
# an empty PGDATA and bootstraps via initdb (on the elected leader) or
# pg_basebackup (on followers). Pre-running initdb short-circuits that
# state machine.

# Global ARG so ${POSTGRES_VERSION} substitutes at the second FROM.
# Stage-scoped ARGs aren't visible to subsequent FROM lines; only
# ARGs declared before the first FROM are. The integration suite
# inherits the default (17); the process-compose dev cluster overrides
# to 18 via `--build-arg POSTGRES_VERSION=18`.
ARG POSTGRES_VERSION=17

# Go toolchain pinned; MUST be kept in lockstep with go.mod's `go`
# directive, exactly as deploy/docker/Dockerfile.bundle is. The official
# golang images set GOTOOLCHAIN=local, so a base image older than the
# directive does not self-upgrade — it fails this build with "go.mod
# requires go >= X (running Y)". A floating `golang:1.26-bookworm` tag
# survived only as long as the tag happened to track the directive: it
# breaks on the first directive bump made before docker-library
# publishes that patch, and the nightly suite is where that lands.
FROM golang:1.26.10-bookworm AS build
WORKDIR /src

# Pull both modules into the build context. go.mod pins pg-manager to a
# tagged (private) release; for local integration/chaos builds we redirect
# it at the copied sibling source with a build-stage-only `replace`, so the
# build needs no module token. GOWORK=off keeps a developer's stray go.work
# from interfering. (CI's bundled image instead fetches the pinned module
# via a secret mount — see deploy/docker/Dockerfile.bundle.)
COPY pg-manager /src/pg-manager
COPY pgman-proxy /src/pgman-proxy

WORKDIR /src/pgman-proxy
ENV GOWORK=off
RUN go mod edit -replace github.com/f1bonacc1/pg-manager=../pg-manager
RUN go mod download
RUN CGO_ENABLED=0 go build \
        -trimpath \
        -ldflags="-s -w -X main.version=integration-test" \
        -o /out/pgman-proxy ./cmd/pgman-proxy
# pgmctl rides along so scenarios can drive the operator CLI inside a
# node (scale-out S11: pgmctl promote).
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/pgmctl ./cmd/pgmctl

FROM postgres:${POSTGRES_VERSION}-bookworm
ARG POSTGRES_VERSION
COPY --from=build /out/pgman-proxy /usr/local/bin/pgman-proxy
COPY --from=build /out/pgmctl /usr/local/bin/pgmctl

ENV PGDATA=/var/lib/postgresql/data
ENV POSTGRES_BIN_DIR=/usr/lib/postgresql/${POSTGRES_VERSION}/bin

# The postgres image declares VOLUME /var/lib/postgresql/data and the
# default entrypoint expects an empty dir on first boot. We bypass that
# entrypoint, so we must guarantee an empty + correctly-owned PGDATA
# ourselves before dropping privileges.
USER root
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
         curl \
         iptables \
         iproute2 \
         procps \
         libcap2-bin \
         openssl \
    && rm -rf /var/lib/apt/lists/*
# Chaos-testing convenience: grant the non-root `postgres` user the
# capabilities needed to manipulate iptables inside the container, so
# integration / chaos rigs can inject network partitions without
# `docker exec -u root`. The container must already be started with
# `--cap-add=NET_ADMIN` (NET_RAW is in the default capability set);
# setcap simply exposes those caps to the unprivileged binary invocation.
# Targets the xtables multi-call binaries that the iptables / ip6tables
# / iptables-{nft,legacy} symlinks ultimately dispatch to. Either backend
# may be selected via update-alternatives, so we cover both. INTEGRATION
# / DEV IMAGE ONLY — never apply to production images.
RUN setcap CAP_NET_ADMIN,CAP_NET_RAW+ep /usr/sbin/xtables-nft-multi \
 && setcap CAP_NET_ADMIN,CAP_NET_RAW+ep /usr/sbin/xtables-legacy-multi
RUN install -d -o postgres -g postgres -m 0700 "$PGDATA"
# Peer TLS (spec 001 FR-018). A throwaway CA and one server certificate
# naming every compose peer, minted at build time so all peers built
# from this image share the CA. The integration rig runs Postgres with
# ssl=on and pgman-proxy's default postgres.tls_mode=verify-full against
# this CA, so replication between peers is verified TLS. The CA key is
# discarded. TEST IMAGE ONLY.
RUN install -d -o postgres -g postgres -m 0700 /etc/pgman-tls \
 && cd /etc/pgman-tls \
 && openssl req -x509 -newkey rsa:2048 -nodes -days 36500 -subj "/CN=pgman-proxy-it-ca" \
      -keyout ca.key -out ca.crt \
 && openssl req -newkey rsa:2048 -nodes -subj "/CN=pgman-proxy-it-peer" \
      -keyout server.key -out server.csr \
 && printf 'subjectAltName=DNS:node-a,DNS:node-b,DNS:node-c,DNS:localhost,IP:127.0.0.1\n' > san.ext \
 && openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 36500 \
      -extfile san.ext -out server.crt \
 && rm -f ca.key ca.srl server.csr san.ext \
 && chown postgres:postgres ca.crt server.crt server.key \
 && chmod 0600 server.key

USER postgres
ENTRYPOINT ["/usr/local/bin/pgman-proxy"]
