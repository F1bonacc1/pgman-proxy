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
	teardownHarness()
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

	verify, cancelV := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelV()
	if err := exec.CommandContext(verify, "docker", "compose", "version").Run(); err != nil { //nolint:gosec
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

func teardownHarness() {
	if harness.project == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := runCompose(ctx, "down", "-v", "--remove-orphans"); err != nil {
		fmt.Fprintf(os.Stderr, "compose down: %v\n", err)
	}
}
