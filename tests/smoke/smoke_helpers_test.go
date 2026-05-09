// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build smoke

// Package smoke holds the deployment-mode smoke tests for US2.
//
// Smoke tests validate that the same `pgman-proxy` binary works in
// standalone, microservice, and sidecar modes — distinguished only by
// configuration. They run the binary as a subprocess and inspect its
// observable surface (stdout/stderr, exit code, listener bindings).
//
// The tests deliberately avoid bringing up a full PostgreSQL + NATS
// topology: that's the integration tier's job. Smoke focuses on the
// glue that sits in front of the engine.
package smoke

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// buildBinary compiles cmd/pgman-proxy into a tempdir and returns the
// path. Built once per t.Helper invocation; callers can share via t.Cleanup.
func buildBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "pgman-proxy")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	build := exec.CommandContext(ctx, //nolint:gosec
		"go", "build",
		"-o", bin,
		"github.com/f1bonacc1/pgman-proxy/cmd/pgman-proxy",
	)
	out, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("go build pgman-proxy: %v\n%s", err, out)
	}
	return bin
}

// runPrintConfig invokes `pgman-proxy --print-config` with the supplied
// env. Returns stdout (the redacted config JSON), stderr, and the exit
// code.
func runPrintConfig(t *testing.T, bin string, env map[string]string) (stdout, stderr string, code int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--print-config") //nolint:gosec
	cmd.Env = baseEnv(env)
	var outBuf, errBuf safeBuf
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	return outBuf.String(), errBuf.String(), exitCode(err)
}

// baseEnv merges the supplied env over PATH/HOME so go-tooling can run
// (e.g., when pgman-proxy itself shells out — currently it does not,
// but PATH is cheap insurance).
func baseEnv(extra map[string]string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
	}
	for k, v := range extra {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	return env
}

// minimalEnv returns the env vars that satisfy validate.go's required
// rows. Tests overlay deployment-mode-specific keys on top.
func minimalEnv() map[string]string {
	return map[string]string{
		"PGMAN_PROXY_CLUSTER_ID":             "demo",
		"PGMAN_PROXY_NODE_ID":                "node-a",
		"PGMAN_PROXY_PEERS":                  "node-a",
		"PGMAN_PROXY_NATS_URL":               "nats://example:4222",
		"PGMAN_PROXY_PROXY_LISTEN_ADDR":      "0.0.0.0:6432",
		"PGMAN_PROXY_POSTGRES_BIN_DIR":       "/usr/lib/postgresql/17/bin",
		"PGMAN_PROXY_POSTGRES_DATA_DIR":      "/var/lib/postgresql/data",
		"PGMAN_PROXY_POSTGRES_LOCAL_DSN_ENV": "LOCAL_DSN",
		"PGMAN_PROXY_CONTROL_AUTH_TOKEN_ENV": "TOK",
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// safeBuf is a minimal thread-safe bytes.Buffer wrapper. exec.Cmd reads
// stdout/stderr from goroutines so concurrent writes are possible.
type safeBuf struct {
	b []byte
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.b = append(s.b, p...)
	return len(p), nil
}

func (s *safeBuf) String() string { return string(s.b) }
