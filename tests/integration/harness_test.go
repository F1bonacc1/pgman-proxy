// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build integration

// Package integration holds the docker-compose-driven test harness for
// the pgman-proxy data-plane and coordination integration suite.
//
// Every Test* function in this directory uses the shared harness from
// compose_main_test.go: a single `docker compose up --build --wait`
// brings the topology online once per `go test` invocation; per-test
// cleanup belongs in t.Cleanup. The harness itself is responsible for
// `compose down -v` on TestMain teardown.
package integration

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Peer represents one of the three pgman-proxy nodes in the test
// topology. Host ports below match docker-compose.test.yml exactly.
type Peer struct {
	Name      string // compose service name (e.g. "node-a")
	PsqlPort  int    // host port mapped to the peer's :6432
	HealthURL string // "http://127.0.0.1:<port>/" — append /healthz, /readyz, /metrics
}

// Peers returns the three pgman-proxy peers exposed by the test
// compose file. The slice order is stable so tests can index by role.
func Peers() []Peer {
	return []Peer{
		{Name: "node-a", PsqlPort: 16432, HealthURL: "http://127.0.0.1:19090"},
		{Name: "node-b", PsqlPort: 16433, HealthURL: "http://127.0.0.1:19091"},
		{Name: "node-c", PsqlPort: 16434, HealthURL: "http://127.0.0.1:19092"},
	}
}

// DSN returns a libpq connection string targeting peer's host-port-mapped
// proxy listener as the "postgres" superuser, with sslmode=disable so
// tests don't depend on TLS material.
func (p Peer) DSN() string {
	return fmt.Sprintf(
		"host=127.0.0.1 port=%d user=postgres dbname=postgres sslmode=disable connect_timeout=5",
		p.PsqlPort,
	)
}

// composeArgs prefixes the standard `docker compose -p <project> -f <file>`
// arguments to the supplied command. Used by every harness operation so
// the test project name stays consistent across invocations.
func composeArgs(extra ...string) []string {
	args := []string{
		"compose",
		"-p", harness.project,
		"-f", harness.composeFile,
	}
	return append(args, extra...)
}

// runCompose executes a `docker compose ...` invocation bound to the
// harness project. Stdout/stderr are streamed to the test output to
// surface compose errors immediately. The supplied ctx bounds runtime.
func runCompose(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", composeArgs(args...)...) //nolint:gosec
	cmd.Dir = harness.workdir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker compose %s: %w\noutput:\n%s",
			strings.Join(args, " "), err, buf.String())
	}
	return nil
}

// waitReady polls every peer until it satisfies BOTH gates:
//  1. /readyz returns 200 (NATS-up + listener-up + manager-past-singleton)
//  2. a `SELECT 1` round-trip through the proxy succeeds (the leader's
//     Postgres has finished initdb / pg_ctl start)
//
// /readyz alone is necessary but not sufficient: the proxy listener
// can be open before the leader's Postgres has finished bootstrap, in
// which case the data-plane EOFs the client. The SQL probe closes
// that race.
func waitReady(ctx context.Context, peers []Peer, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	hc := &http.Client{Timeout: 2 * time.Second}
	for _, p := range peers {
		url := p.HealthURL + "/readyz"
		for {
			if time.Now().After(deadline) {
				return fmt.Errorf("peer %s never reached /readyz=200 (last URL: %s)", p.Name, url)
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			resp, err := hc.Do(req)
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					break
				}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
	// Once every peer's /readyz is green, verify a SQL handshake succeeds
	// on at least one peer — that proves the leader's Postgres is live
	// and the proxy is forwarding to it.
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("no peer accepted SQL within deadline")
		}
		for _, p := range peers {
			if pingSQL(ctx, p.DSN()) == nil {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
}

// pingSQL opens a fresh libpq session and runs `SELECT 1`. Returns nil
// on success.
func pingSQL(ctx context.Context, dsn string) error {
	connCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	conn, err := pgx.Connect(connCtx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	var n int
	return conn.QueryRow(connCtx, "SELECT 1").Scan(&n)
}

// scrapeMetrics fetches a peer's Prometheus exposition format from
// /metrics. Used by obs_incident_test.go to verify named metrics.
func scrapeMetrics(ctx context.Context, p Peer) (string, error) {
	hc := &http.Client{Timeout: 5 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.HealthURL+"/metrics", nil)
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck
	buf := bytes.NewBuffer(nil)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// dockerComposeOutput runs `docker compose ... <args>` in the harness's
// project and returns combined stdout. Errors include captured output.
func dockerComposeOutput(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", composeArgs(args...)...) //nolint:gosec
	cmd.Dir = harness.workdir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return buf.Bytes(), fmt.Errorf("docker compose %s: %w\noutput:\n%s",
			strings.Join(args, " "), err, buf.String())
	}
	return buf.Bytes(), nil
}

// buildPgmanProxy compiles cmd/pgman-proxy into a tempdir and returns
// the binary path. Used by Phase 5 tests that need to exercise the
// validator on its own (without the compose harness).
func buildPgmanProxy(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := dir + "/pgman-proxy"
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	build := exec.CommandContext(ctx, //nolint:gosec
		"go", "build", "-o", bin,
		"github.com/f1bonacc1/pgman-proxy/cmd/pgman-proxy",
	)
	out, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("go build pgman-proxy: %v\n%s", err, out)
	}
	return bin
}

// portReachable reports whether something is accepting TCP at addr
// within the supplied timeout. Used by smoke probes that don't need a
// full SQL handshake.
func portReachable(addr string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// execDocker runs `docker <args...>` with stdout/stderr captured. Used
// for raw operations the compose CLI doesn't expose (network attach
// /detach, container kill -s SIGSTOP, image inspection).
func execDocker(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker %s: %w\noutput:\n%s",
			strings.Join(args, " "), err, buf.String())
	}
	return nil
}
