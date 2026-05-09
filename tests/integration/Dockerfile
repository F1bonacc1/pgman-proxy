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

FROM golang:1.25-bookworm AS build
WORKDIR /src

# Pull both modules into the build context. The replace directive in
# pgman-proxy/go.mod points at ../pg-manager, so both must be present.
COPY pg-manager /src/pg-manager
COPY pgman-proxy /src/pgman-proxy

WORKDIR /src/pgman-proxy
RUN go mod download
RUN CGO_ENABLED=0 go build \
        -trimpath \
        -ldflags="-s -w -X main.version=integration-test" \
        -o /out/pgman-proxy ./cmd/pgman-proxy

FROM postgres:17-bookworm
COPY --from=build /out/pgman-proxy /usr/local/bin/pgman-proxy

ENV PGDATA=/var/lib/postgresql/data

# The postgres image declares VOLUME /var/lib/postgresql/data and the
# default entrypoint expects an empty dir on first boot. We bypass that
# entrypoint, so we must guarantee an empty + correctly-owned PGDATA
# ourselves before dropping privileges.
USER root
RUN install -d -o postgres -g postgres -m 0700 "$PGDATA"

USER postgres
ENTRYPOINT ["/usr/local/bin/pgman-proxy"]
