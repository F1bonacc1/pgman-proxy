// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build integration

// US1 fail-closed startup: every documented startup-gate failure in
// contracts/lifecycle.md MUST exit with the matching code rather than
// limp forward in a degraded state. This test exercises the binary
// directly (no compose) so we can observe exit codes deterministically.

package integration

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStartupFailClose_ExitCodes verifies every documented exit code
// from contracts/lifecycle.md § Exit codes that we can drive from a
// pure-config failure path. Per Constitution II (fail-closed safety),
// the binary must exit non-zero rather than start with broken
// invariants.
func TestStartupFailClose_ExitCodes(t *testing.T) {
	bin := pgmanProxyBin(t)

	type tc struct {
		name     string
		args     []string
		env      map[string]string
		wantCode int
		wantStr  string // substring expected on stderr (case-insensitive)
	}

	cases := []tc{
		{
			name:     "unknown_flag_is_EX_CONFIG",
			args:     []string{"--this-flag-does-not-exist=1"},
			wantCode: 78,
		},
		{
			name:     "unexpected_positional_is_EX_CONFIG",
			args:     []string{"unexpected-positional"},
			wantCode: 78,
		},
		{
			name:     "missing_required_keys_is_EX_CONFIG",
			args:     []string{"--print-config"},
			wantCode: 78,
			wantStr:  "is required",
		},
		{
			name: "version_and_print_config_mutually_exclusive_is_EX_CONFIG",
			args: []string{"--version", "--print-config"},
			env: map[string]string{
				"PGMAN_PROXY_CLUSTER_ID": "demo",
				"PGMAN_PROXY_NODE_ID":    "node-a",
				"PGMAN_PROXY_PEERS":      "node-a",
				// Feature 002: external NATS removed; embedded coordination plane.
				"PGMAN_PROXY_CLUSTER_DECLARED_SIZE":  "1",
				"PGMAN_PROXY_PROXY_LISTEN_ADDR":      "127.0.0.1:6432",
				"PGMAN_PROXY_POSTGRES_BIN_DIR":       "/usr/lib/postgresql/17/bin",
				"PGMAN_PROXY_POSTGRES_DATA_DIR":      "/var/lib/postgresql/data",
				"PGMAN_PROXY_POSTGRES_LOCAL_DSN_ENV": "LOCAL_DSN",
				"PGMAN_PROXY_CONTROL_AUTH_TOKEN_ENV": "TOK",
			},
			wantCode: 78,
			wantStr:  "mutually exclusive",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			cmd := exec.CommandContext(ctx, bin, c.args...) //nolint:gosec
			env := []string{
				"PATH=" + getEnv("PATH"),
				"HOME=" + getEnv("HOME"),
			}
			for k, v := range c.env {
				env = append(env, k+"="+v)
			}
			cmd.Env = env

			out, err := cmd.CombinedOutput()
			gotCode := exitCode(err)
			if gotCode != c.wantCode {
				t.Errorf("exit code = %d, want %d\nstderr+stdout:\n%s",
					gotCode, c.wantCode, out)
			}
			if c.wantStr != "" && !strings.Contains(strings.ToLower(string(out)), strings.ToLower(c.wantStr)) {
				t.Errorf("output missing %q\ngot:\n%s", c.wantStr, out)
			}
		})
	}
}

// pgmanProxyBin builds the binary under test once and returns its
// path. Builds into a per-test tempdir so the test is hermetic.
func pgmanProxyBin(t *testing.T) string {
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

// exitCode extracts the OS exit code from an *exec.ExitError. Returns 0
// for nil and -1 for unrecognised error types.
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

func getEnv(key string) string {
	return os.Getenv(key)
}
