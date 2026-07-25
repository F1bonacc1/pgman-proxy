// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// harnessConfig captures the shared compose-tier state populated once
// by TestMain. Read-only after TestMain returns so concurrent Test*
// readers are race-free.
type harnessConfig struct {
	composeFile string // absolute path to docker-compose.test.yml
	workdir     string // tests/integration/ — the compose CWD
	project     string // unique compose project name for this run
}

var harness harnessConfig

// TestMain brings the docker-compose topology up before any Test* runs
// and tears it down on exit. It only runs under `-tags=integration`;
// `make integration` is the canonical entrypoint.
//
// The harness deliberately does NOT call goleak.VerifyTestMain — these
// tests exercise external processes via TCP so transient connections
// will be visible to a leak detector.
func TestMain(m *testing.M) {
	if err := setupHarness(); err != nil {
		fmt.Fprintf(os.Stderr, "integration harness setup failed: %v\n", err)
		os.Exit(2)
	}
	code := m.Run()
	teardownHarness(code != 0)
	os.Exit(code)
}

func setupHarness() error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker not on PATH: %w", err)
	}

	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}
	harness.workdir = wd
	harness.composeFile = filepath.Join(wd, "docker-compose.test.yml")
	if _, err := os.Stat(harness.composeFile); err != nil {
		return fmt.Errorf("compose file missing: %w", err)
	}
	harness.project = fmt.Sprintf("pgman-proxy-it-%d", time.Now().UnixNano())

	if err := verifyComposeV2(context.Background()); err != nil {
		return fmt.Errorf("docker compose v2 required: %w", err)
	}

	upCtx, cancelUp := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancelUp()
	if err := runCompose(upCtx, "up", "-d", "--build", "--wait"); err != nil {
		return fmt.Errorf("compose up: %w", err)
	}

	readyCtx, cancelReady := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelReady()
	if err := waitReady(readyCtx, Peers(), 5*time.Minute); err != nil {
		return fmt.Errorf("readiness gate: %w", err)
	}
	return nil
}

// verifyComposeV2 confirms the Docker Compose v2 plugin is present.
// `docker compose version` is a local, daemon-free call that normally
// returns in milliseconds, but the FIRST docker CLI invocation on a
// cold or heavily-loaded CI runner can stall (plugin discovery, config
// / credential-helper init) long enough to blow a tight deadline — the
// nightly suite has been SIGKILLed here at exactly the old 10s mark.
// Retry a few times with a generous per-attempt timeout so a single
// transient stall doesn't fail the whole run; a genuinely absent plugin
// fails fast on each attempt, so the retries stay cheap.
func verifyComposeV2(ctx context.Context) error {
	const attempts = 3
	var err error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
		attempt, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = exec.CommandContext(attempt, "docker", "compose", "version").Run() //nolint:gosec
		cancel()
		if err == nil {
			return nil
		}
	}
	return err
}

func teardownHarness(failed bool) {
	if harness.project == "" {
		return
	}
	if failed {
		dumpComposeLogs()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := runCompose(ctx, "down", "-v", "--remove-orphans"); err != nil {
		fmt.Fprintf(os.Stderr, "compose down: %v\n", err)
	}
}

// dumpComposeLogs streams every container's recent log tail to
// stderr. Called only on a failed run, BEFORE `down -v` erases the
// topology — the containers hold the only record of which
// engine/reconciler action produced an unexpected state (the
// 2026-07-25 nightly flake left no trace without this). runCompose is
// unsuitable here: it buffers output and discards it on success.
func dumpComposeLogs() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fmt.Fprintf(os.Stderr, "=== suite failed; compose logs for %s ===\n", harness.project)
	cmd := exec.CommandContext(ctx, "docker",
		composeArgs("logs", "--no-color", "--timestamps", "--tail", "5000")...) //nolint:gosec
	cmd.Dir = harness.workdir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "compose logs: %v\n", err)
	}
	fmt.Fprintln(os.Stderr, "=== end compose logs ===")
}
