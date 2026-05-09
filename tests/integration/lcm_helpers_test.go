// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build integration

// LCM-tier helpers shared by every Phase 5 integration test.
// docker-compose.test.yml does NOT bind the control-plane port to the
// host (only the data plane and observability surface are exposed).
// LCM tests reach the control plane via `docker compose exec`, which
// keeps the trust boundary inside the per-project Docker network.

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// lcmResponse mirrors internal/control's response envelope. Tests
// decode into this shape rather than the real envelope so the test
// package doesn't have to depend on the internal/control package.
type lcmResponse struct {
	Operation    string          `json:"operation"`
	RequestID    string          `json:"request_id"`
	Outcome      string          `json:"outcome"`
	EngineResult json.RawMessage `json:"engine_result,omitempty"`
	Error        *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// callLCM POSTs a JSON body to a peer's control plane via curl
// running inside the peer's container. Returns the HTTP status code,
// response body, and any transport error. The container has curl
// installed via the postgres:17-bookworm base image.
func callLCM(ctx context.Context, peerName, method, path, token, body string) (int, []byte, error) {
	args := []string{
		"compose", "-p", harness.project, "-f", harness.composeFile,
		"exec", "-T", peerName,
		"curl", "-sS",
		"-X", method,
		"-H", "Authorization: Bearer " + token,
		"-H", "Content-Type: application/json",
		"-w", "\n%{http_code}",
		"http://127.0.0.1:9091" + path,
	}
	if body != "" {
		args = append(args, "-d", body)
	}
	cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec
	cmd.Dir = harness.workdir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return 0, stdout.Bytes(), fmt.Errorf("docker compose exec curl: %w (stderr: %s)", err, stderr.String())
	}
	out := stdout.Bytes()
	// Last line is the HTTP status; everything before is the body.
	idx := bytes.LastIndexByte(out, '\n')
	if idx < 0 {
		return 0, out, fmt.Errorf("malformed curl output: %s", out)
	}
	body0 := out[:idx]
	statusStr := strings.TrimSpace(string(out[idx+1:]))
	var code int
	if _, err := fmt.Sscanf(statusStr, "%d", &code); err != nil {
		return 0, body0, fmt.Errorf("parse status %q: %w", statusStr, err)
	}
	return code, body0, nil
}

// callLCMUnauth issues an LCM request without an Authorization header.
func callLCMUnauth(ctx context.Context, peerName, method, path, body string) (int, []byte, error) {
	args := []string{
		"compose", "-p", harness.project, "-f", harness.composeFile,
		"exec", "-T", peerName,
		"curl", "-sS",
		"-X", method,
		"-H", "Content-Type: application/json",
		"-w", "\n%{http_code}",
		"http://127.0.0.1:9091" + path,
	}
	if body != "" {
		args = append(args, "-d", body)
	}
	cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec
	cmd.Dir = harness.workdir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return 0, stdout.Bytes(), fmt.Errorf("docker compose exec curl: %w (stderr: %s)", err, stderr.String())
	}
	out := stdout.Bytes()
	idx := bytes.LastIndexByte(out, '\n')
	if idx < 0 {
		return 0, out, fmt.Errorf("malformed curl output: %s", out)
	}
	body0 := out[:idx]
	statusStr := strings.TrimSpace(string(out[idx+1:]))
	var code int
	if _, err := fmt.Sscanf(statusStr, "%d", &code); err != nil {
		return 0, body0, fmt.Errorf("parse status %q: %w", statusStr, err)
	}
	return code, body0, nil
}

// integrationToken is the bearer token wired into docker-compose.test.yml.
const integrationToken = "integration-test-token"

// expectLCM is a small helper that decodes the response and asserts
// outcome + status. Returns the decoded envelope so tests can drill
// into engine_result.
func expectLCM(t *testing.T, code int, body []byte, wantStatus int, wantOutcome string) lcmResponse {
	t.Helper()
	if code != wantStatus {
		t.Fatalf("HTTP status: got %d, want %d (body: %s)", code, wantStatus, body)
	}
	var env lcmResponse
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v (body: %s)", err, body)
	}
	if env.Outcome != wantOutcome {
		t.Errorf("outcome: got %q, want %q (body: %s)", env.Outcome, wantOutcome, body)
	}
	return env
}

// retryLCM polls op until it returns wantStatus or budget expires.
// Used to ride out transient `cluster_bootstrapping` /
// `leadership_in_transition` rejections that the spec marks as
// retryable (FR-029).
func retryLCM(t *testing.T, ctx context.Context, peer string, method, path, body string, wantStatus int, budget time.Duration) (int, []byte) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		code, raw, err := callLCM(ctx, peer, method, path, integrationToken, body)
		if err == nil && code == wantStatus {
			return code, raw
		}
		if time.Now().After(deadline) {
			if err != nil {
				t.Fatalf("retryLCM exhausted: %v", err)
			}
			return code, raw
		}
		time.Sleep(500 * time.Millisecond)
	}
}
