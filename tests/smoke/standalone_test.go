// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build smoke

// US2 standalone-mode smoke: a single peer in front of one PostgreSQL
// instance with no sidecar coupling. The data-plane and observability
// listeners bind all-interfaces; --print-config round-trips a coherent
// configuration; the binary exits 0.

package smoke

import (
	"strings"
	"testing"
)

// TestSmoke_StandaloneMode verifies the standalone deployment mode
// keeps the data-plane listener at the operator-supplied (or default)
// all-interfaces bind.
func TestSmoke_StandaloneMode(t *testing.T) {
	bin := buildBinary(t)
	env := minimalEnv()
	env["PGMAN_PROXY_DEPLOYMENT_MODE"] = "standalone"

	stdout, stderr, code := runPrintConfig(t, bin, env)
	if code != 0 {
		t.Fatalf("--print-config exited %d\nstderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, `"listen_addr": "0.0.0.0:6432"`) {
		t.Errorf("standalone should preserve operator-supplied 0.0.0.0:6432; got:\n%s", stdout)
	}
	if strings.Contains(stdout, `"listen_addr": "127.0.0.1:6432"`) {
		t.Errorf("standalone must NOT downgrade to loopback; got:\n%s", stdout)
	}
}
