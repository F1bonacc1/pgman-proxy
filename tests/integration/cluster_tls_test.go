// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build integration

// US1 (feature 002 / FR-010b) — cluster-routes TLS gating. Spec
// coverage:
//   * Non-loopback bind without TLS material → fail-closed at startup
//     with exit code 78 (CONFIG).
//   * `cluster.tls.plaintext_explicit_ack=true` + non-loopback bind
//     succeeds with an audit-logged warning at every startup.
//   * Cert/key half-set is rejected.
//
// The test does NOT exercise sibling-presented-cert mismatch — that
// requires a live multi-peer mesh and is tracked under T028's
// follow-up integration test once a TLS-enabled compose harness
// lands.

package integration

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestClusterTLS_NonLoopbackWithoutTLS_FailsClosed asserts the
// FR-010b gate: a non-loopback `cluster.routes_listen` bind without
// TLS material AND without `plaintext_explicit_ack` is rejected at
// validation with exit code 78.
func TestClusterTLS_NonLoopbackWithoutTLS_FailsClosed(t *testing.T) {
	bin := buildPgmanProxy(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "--print-config") //nolint:gosec
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"PGMAN_PROXY_CLUSTER_ID=demo",
		"PGMAN_PROXY_CLUSTER_DECLARED_SIZE=3",
		"PGMAN_PROXY_CLUSTER_USERNAME=demo-cluster",
		"PGMAN_PROXY_CLUSTER_PASSWORD_ENV=PW",
		"PW=longenoughpassword12345",
		"PGMAN_PROXY_CLUSTER_ROUTE_PEERS=node-b:6222,node-c:6222",
		"PGMAN_PROXY_CLUSTER_JETSTREAM_DIR=/tmp/pgman-jetstream",
		"PGMAN_PROXY_NODE_ID=node-a",
		"PGMAN_PROXY_PEERS=node-a,node-b,node-c",
		"PGMAN_PROXY_PROXY_LISTEN_ADDR=127.0.0.1:6432",
		"PGMAN_PROXY_POSTGRES_BIN_DIR=/usr/lib/postgresql/17/bin",
		"PGMAN_PROXY_POSTGRES_DATA_DIR=/var/lib/postgresql/data",
		"PGMAN_PROXY_POSTGRES_LOCAL_DSN_ENV=LOCAL_DSN",
		"PGMAN_PROXY_CONTROL_LISTEN_ADDR=127.0.0.1:9091",
		"PGMAN_PROXY_CONTROL_AUTH_TOKEN_ENV=TOK",
		// Notably absent: cluster.tls.{cert_file,key_file,plaintext_explicit_ack}.
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("--print-config should have failed; output: %s", out)
	}
	if !strings.Contains(string(out), "FR-010b") {
		t.Errorf("expected FR-010b error message, got: %s", out)
	}
	// Exit code 78 (EX_CONFIG) is the documented failure for validation
	// errors per contracts/lifecycle.md.
	if exitCode := cmdExitCode(cmd); exitCode != 78 {
		t.Errorf("exit code = %d, want 78 (EX_CONFIG)", exitCode)
	}
}

// TestClusterTLS_NonLoopbackPlaintextExplicitAck asserts the named
// opt-in path: setting `cluster.tls.plaintext_explicit_ack=true`
// permits a non-loopback bind without TLS material to validate.
func TestClusterTLS_NonLoopbackPlaintextExplicitAck(t *testing.T) {
	bin := buildPgmanProxy(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "--print-config") //nolint:gosec
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"PGMAN_PROXY_CLUSTER_ID=demo",
		"PGMAN_PROXY_CLUSTER_DECLARED_SIZE=3",
		"PGMAN_PROXY_CLUSTER_USERNAME=demo-cluster",
		"PGMAN_PROXY_CLUSTER_PASSWORD_ENV=PW",
		"PW=longenoughpassword12345",
		"PGMAN_PROXY_CLUSTER_ROUTE_PEERS=node-b:6222,node-c:6222",
		"PGMAN_PROXY_CLUSTER_JETSTREAM_DIR=/tmp/pgman-jetstream",
		"PGMAN_PROXY_CLUSTER_TLS_PLAINTEXT_EXPLICIT_ACK=true",
		"PGMAN_PROXY_NODE_ID=node-a",
		"PGMAN_PROXY_PEERS=node-a,node-b,node-c",
		"PGMAN_PROXY_PROXY_LISTEN_ADDR=127.0.0.1:6432",
		"PGMAN_PROXY_POSTGRES_BIN_DIR=/usr/lib/postgresql/17/bin",
		"PGMAN_PROXY_POSTGRES_DATA_DIR=/var/lib/postgresql/data",
		"PGMAN_PROXY_POSTGRES_LOCAL_DSN_ENV=LOCAL_DSN",
		"PGMAN_PROXY_CONTROL_LISTEN_ADDR=127.0.0.1:9091",
		"PGMAN_PROXY_CONTROL_AUTH_TOKEN_ENV=TOK",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("plaintext_explicit_ack should permit non-loopback bind; output: %s", out)
	}
	if !strings.Contains(string(out), `"plaintext_explicit_ack": true`) {
		t.Errorf("expected ack flag in printed config; got: %s", out)
	}
}

// TestClusterTLS_HalfSetCertAndKey_FailsClosed asserts the validator
// rejects a configuration where only one of `cluster.tls.cert_file`
// and `cluster.tls.key_file` is set.
func TestClusterTLS_HalfSetCertAndKey_FailsClosed(t *testing.T) {
	bin := buildPgmanProxy(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "--print-config") //nolint:gosec
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"PGMAN_PROXY_CLUSTER_ID=demo",
		"PGMAN_PROXY_CLUSTER_DECLARED_SIZE=3",
		"PGMAN_PROXY_CLUSTER_USERNAME=demo-cluster",
		"PGMAN_PROXY_CLUSTER_PASSWORD_ENV=PW",
		"PW=longenoughpassword12345",
		"PGMAN_PROXY_CLUSTER_ROUTE_PEERS=node-b:6222,node-c:6222",
		"PGMAN_PROXY_CLUSTER_JETSTREAM_DIR=/tmp/pgman-jetstream",
		"PGMAN_PROXY_CLUSTER_TLS_CERT_FILE=/etc/pgman-proxy/cluster-cert.pem",
		// Note: KEY_FILE not set — half-configured TLS.
		"PGMAN_PROXY_NODE_ID=node-a",
		"PGMAN_PROXY_PEERS=node-a,node-b,node-c",
		"PGMAN_PROXY_PROXY_LISTEN_ADDR=127.0.0.1:6432",
		"PGMAN_PROXY_POSTGRES_BIN_DIR=/usr/lib/postgresql/17/bin",
		"PGMAN_PROXY_POSTGRES_DATA_DIR=/var/lib/postgresql/data",
		"PGMAN_PROXY_POSTGRES_LOCAL_DSN_ENV=LOCAL_DSN",
		"PGMAN_PROXY_CONTROL_LISTEN_ADDR=127.0.0.1:9091",
		"PGMAN_PROXY_CONTROL_AUTH_TOKEN_ENV=TOK",
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("--print-config should have failed on half-set TLS; output: %s", out)
	}
	if !strings.Contains(string(out), "must both be set") {
		t.Errorf("expected 'must both be set' error, got: %s", out)
	}
}

// cmdExitCode extracts the exit code from a finished os/exec.Cmd.
// Returns -1 when no exit code is available (signal-killed processes,
// etc.). Mirrors the conversion done by cmd.ProcessState.ExitCode().
func cmdExitCode(cmd *exec.Cmd) int {
	if cmd == nil || cmd.ProcessState == nil {
		return -1
	}
	return cmd.ProcessState.ExitCode()
}
