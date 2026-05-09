// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build integration

// US4 / T052b — FR-033 plaintext-ack. When the operator opts in via
// `control.tls.plaintext_explicit_ack=true`, validation passes and a
// startup WARN log line is emitted. The test runs the binary in
// `--print-config` mode (which goes through validation) and asserts
// no exit error.

package integration

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestLCM_PlaintextAck_PermitsNonLoopbackBind(t *testing.T) {
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
		"PGMAN_PROXY_CONTROL_TLS_PLAINTEXT_EXPLICIT_ACK=true",
		"PGMAN_PROXY_CONTROL_AUTH_TOKEN_ENV=TOK",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("--print-config with plaintext_explicit_ack should succeed: %v\noutput:\n%s", err, out)
	}
	// The redacted config should reflect the ack.
	if !strings.Contains(string(out), `"plaintext_explicit_ack": true`) {
		t.Errorf("redacted config missing plaintext_explicit_ack, got: %s", lastN(string(out), 500))
	}
}
