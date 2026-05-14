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
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS     ?= -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)
BIN         ?= bin/pgman-proxy
PKG         := github.com/f1bonacc1/pgman-proxy/cmd/pgman-proxy

# pgmctl: feature 003 operator CLI.
PGMCTL_BIN  ?= bin/pgmctl
PGMCTL_PKG  := github.com/f1bonacc1/pgman-proxy/cmd/pgmctl

.PHONY: all build pgmctl test lint integration smoke release pgmctl-release clean grep-gates

all: lint test build pgmctl grep-gates

build:
	mkdir -p bin
	$(GO) build $(GOFLAGS) -ldflags='$(LDFLAGS)' -o $(BIN) $(PKG)

pgmctl:
	mkdir -p bin
	$(GO) build $(GOFLAGS) -ldflags='$(LDFLAGS)' -o $(PGMCTL_BIN) $(PGMCTL_PKG)

# Cross-compile pgmctl for the v1 release matrix per research.md RD-012.
pgmctl-release:
	mkdir -p dist
	@for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
	    os=$$(echo $$target | cut -d/ -f1); \
	    arch=$$(echo $$target | cut -d/ -f2); \
	    out=dist/pgmctl-$(VERSION)-$$os-$$arch; \
	    echo "  -> $$out"; \
	    CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build $(GOFLAGS) -ldflags='$(LDFLAGS)' -o $$out $(PGMCTL_PKG); \
	    (cd dist && tar czf $$(basename $$out).tar.gz $$(basename $$out) && sha256sum $$(basename $$out).tar.gz > $$(basename $$out).tar.gz.sha256); \
	done

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

# grep-gates: enforce constitutional / spec invariants that lint can't catch.
#   * 001 SC-006: no Kubernetes / Helm artefacts in the source tree.
#   * 002 SC-007: no external `nats-server` process references in deploy /
#     quickstart / README. The exception list mirrors the audit at the end
#     of /speckit-implement Phase 2 — historical breadcrumbs documenting
#     the removal are allowed; live legacy code is not.
grep-gates:
	@echo "=== grep-gate 001 SC-006 (no Kubernetes/Helm) ==="
	@! grep -rE 'apiVersion: (apps|batch|networking)|kind: (Deployment|StatefulSet|DaemonSet|Service|ConfigMap|Secret|Ingress|Pod|CronJob|Job|HorizontalPodAutoscaler|RoleBinding|ClusterRole)|helm-chart|chart\.yaml|Chart\.yaml|kustomization\.ya?ml|operator-bundle' \
	    --include='*.go' --include='*.ya?ml' --include='*.md' \
	    --exclude-dir=.git --exclude-dir=dist --exclude-dir=.specify --exclude-dir=specs \
	    . 2>/dev/null && echo "  PASS — no k8s/Helm artefacts" || (echo "  FAIL — Kubernetes/Helm artefact detected"; exit 1)
	@echo "=== grep-gate 002 SC-007 (no external nats-server references) ==="
	@! grep -rE 'image: nats:|nats:[0-9]+\.[0-9]+|run --rm --name pgman-pc-nats|docker pull nats|/usr/local/bin/nats-server|service nats start' \
	    --include='*.go' --include='*.ya?ml' --include='*.md' \
	    --exclude-dir=.git --exclude-dir=dist --exclude-dir=.specify --exclude-dir=specs \
	    . 2>/dev/null && echo "  PASS — no external nats-server references" || (echo "  FAIL — external nats-server reference detected"; exit 1)
