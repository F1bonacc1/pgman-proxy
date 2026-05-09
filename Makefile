# pgman-proxy build & test targets
#
# Targets:
#   build         - go build cmd/pgman-proxy
#   test          - go test ./internal/...  (unit tests, no docker)
#   lint          - go vet + staticcheck + golangci-lint
#   integration   - go test ./tests/integration/...  (requires docker compose)
#   smoke         - go test ./tests/smoke/...        (requires docker compose)
#   release       - goreleaser dry-run (use 'release-publish' to actually publish)
#   clean         - remove build artefacts
#
# CI uses these targets in lockstep with .github/workflows/.

GO          ?= go
GOFLAGS     ?= -trimpath
LDFLAGS     ?= -s -w -X main.version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BIN         ?= bin/pgman-proxy
PKG         := github.com/f1bonacc1/pgman-proxy/cmd/pgman-proxy

.PHONY: all build test lint integration smoke release clean

all: lint test build

build:
	mkdir -p bin
	$(GO) build $(GOFLAGS) -ldflags='$(LDFLAGS)' -o $(BIN) $(PKG)

test:
	$(GO) test $(GOFLAGS) -race ./internal/...

lint:
	$(GO) vet ./...
	staticcheck ./...
	golangci-lint run ./...

integration:
	$(GO) test $(GOFLAGS) -timeout=15m -tags=integration ./tests/integration/...

smoke:
	$(GO) test $(GOFLAGS) -timeout=10m -tags=smoke ./tests/smoke/...

release:
	goreleaser release --snapshot --clean

clean:
	rm -rf bin/ dist/
