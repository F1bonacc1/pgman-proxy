// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build smoke

// US2 microservice-mode smoke: multiple peers running on separate
// hosts, all-interfaces binds so cross-host clients reach the listener.
// This mode is functionally equivalent to standalone w.r.t. listener
// binding policy; the difference is a coordination concern (peer count
// > 1) not a network-binding one.

package smoke

import (
	"strings"
	"testing"
)

// TestSmoke_MicroserviceMode validates that microservice mode behaves
// like standalone for binding purposes — sidecar is the special case.
func TestSmoke_MicroserviceMode(t *testing.T) {
	bin := buildBinary(t)
	env := minimalEnv()
	env["PGMAN_PROXY_DEPLOYMENT_MODE"] = "microservice"
	env["PGMAN_PROXY_PEERS"] = "node-a,node-b,node-c"

	stdout, stderr, code := runPrintConfig(t, bin, env)
	if code != 0 {
		t.Fatalf("--print-config exited %d\nstderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, `"listen_addr": "0.0.0.0:6432"`) {
		t.Errorf("microservice should keep 0.0.0.0:6432; got:\n%s", stdout)
	}
}
