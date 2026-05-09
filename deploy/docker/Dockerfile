# Distroless static image for pgman-proxy.
# The binary is built outside the image (by goreleaser) and copied in.
# This image MUST run as a non-root UID (FR-013).

FROM gcr.io/distroless/static:nonroot

LABEL org.opencontainers.image.source=https://github.com/f1bonacc1/pgman-proxy
LABEL org.opencontainers.image.licenses=Apache-2.0

# goreleaser places the binary at the build root.
COPY pgman-proxy /usr/local/bin/pgman-proxy

USER nonroot:nonroot

# Data-plane (PostgreSQL wire) and observability + control-plane HTTP ports.
EXPOSE 6432
EXPOSE 9090
EXPOSE 9091

ENTRYPOINT ["/usr/local/bin/pgman-proxy"]
