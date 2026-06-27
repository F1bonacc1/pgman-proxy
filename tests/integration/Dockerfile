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

FROM golang:1.26-bookworm AS build
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

FROM postgres:${POSTGRES_VERSION}-bookworm
ARG POSTGRES_VERSION
COPY --from=build /out/pgman-proxy /usr/local/bin/pgman-proxy

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

USER postgres
ENTRYPOINT ["/usr/local/bin/pgman-proxy"]
