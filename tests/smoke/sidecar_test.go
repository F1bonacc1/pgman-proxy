// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build smoke

// US2 sidecar-mode smoke: when DEPLOYMENT_MODE=sidecar is set AND the
// listener address is at its default (0.0.0.0:* / :*), the binary
// rewrites it to 127.0.0.1:* so off-host clients can't reach the
// proxy or observability surface (FR-013).
//
// Operator-pinned addresses (e.g. 127.0.0.1:6432 explicitly, or a
// hostname) MUST pass through unchanged.

package smoke

import (
	"strings"
	"testing"
)

// TestSmoke_SidecarMode_RewriteToLoopback checks the documented
// auto-rewrite path: a 0.0.0.0:6432 supplied via env is rewritten to
// 127.0.0.1:6432 once mode=sidecar is set.
func TestSmoke_SidecarMode_RewriteToLoopback(t *testing.T) {
	bin := buildBinary(t)
	env := minimalEnv()
	env["PGMAN_PROXY_DEPLOYMENT_MODE"] = "sidecar"
	env["PGMAN_PROXY_PROXY_LISTEN_ADDR"] = "0.0.0.0:6432"
	env["PGMAN_PROXY_OBS_HEALTH_ADDR"] = ":9090"

	stdout, stderr, code := runPrintConfig(t, bin, env)
	if code != 0 {
		t.Fatalf("--print-config exited %d\nstderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, `"listen_addr": "127.0.0.1:6432"`) {
		t.Errorf("sidecar must rewrite 0.0.0.0:6432 → 127.0.0.1:6432; got:\n%s", stdout)
	}
	// Bare ":port" → "127.0.0.1:port" too.
	if !strings.Contains(stdout, `"health_addr": "127.0.0.1:9090"`) {
		t.Errorf("sidecar must rewrite :9090 → 127.0.0.1:9090; got:\n%s", stdout)
	}
}

// TestSmoke_SidecarMode_OperatorPinPreserved: an operator who supplies
// a non-default address (e.g. a specific interface IP) wins.
func TestSmoke_SidecarMode_OperatorPinPreserved(t *testing.T) {
	bin := buildBinary(t)
	env := minimalEnv()
	env["PGMAN_PROXY_DEPLOYMENT_MODE"] = "sidecar"
	env["PGMAN_PROXY_PROXY_LISTEN_ADDR"] = "10.0.0.5:6432"

	stdout, stderr, code := runPrintConfig(t, bin, env)
	if code != 0 {
		t.Fatalf("--print-config exited %d\nstderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, `"listen_addr": "10.0.0.5:6432"`) {
		t.Errorf("sidecar must not rewrite operator-pinned 10.0.0.5:6432; got:\n%s", stdout)
	}
}
