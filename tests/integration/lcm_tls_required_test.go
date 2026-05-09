// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build integration

// US4 / T052a — FR-033 control-plane TLS required. The validator MUST
// reject a non-loopback control-plane bind without TLS material AND
// without `plaintext_explicit_ack`. We exercise this by running the
// binary directly — no compose harness needed; the failure surfaces
// during config validation BEFORE any listener binds.

package integration

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

var _ time.Duration // keep "time" referenced; deadline is below

func TestLCM_NonLoopbackBind_WithoutTLS_ExitsConfigError(t *testing.T) {
	bin := buildPgmanProxy(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--print-config") //nolint:gosec
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"PGMAN_PROXY_CLUSTER_ID=demo",
		"PGMAN_PROXY_NODE_ID=node-a",
		"PGMAN_PROXY_PEERS=node-a",
		"PGMAN_PROXY_NATS_URL=nats://example:4222",
		"PGMAN_PROXY_PROXY_LISTEN_ADDR=127.0.0.1:6432",
		"PGMAN_PROXY_POSTGRES_BIN_DIR=/usr/lib/postgresql/17/bin",
		"PGMAN_PROXY_POSTGRES_DATA_DIR=/var/lib/postgresql/data",
		"PGMAN_PROXY_POSTGRES_LOCAL_DSN_ENV=LOCAL_DSN",
		"PGMAN_PROXY_CONTROL_LISTEN_ADDR=0.0.0.0:9091",
		"PGMAN_PROXY_CONTROL_AUTH_TOKEN_ENV=TOK",
	}
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected ExitError, got %v (output: %s)", err, out)
	}
	if ee.ExitCode() != 78 {
		t.Errorf("exit code: got %d, want 78 (EX_CONFIG)", ee.ExitCode())
	}
	if !strings.Contains(string(out), "plaintext bind on non-loopback") {
		t.Errorf("error message missing FR-033 hint, got: %s", out)
	}
}
