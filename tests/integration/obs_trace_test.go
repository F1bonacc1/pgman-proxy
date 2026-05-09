// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build integration

// US3 / T069 — W3C trace-context propagation. Every documented HTTP
// surface (/healthz, /readyz, /metrics, /v1/*) MUST echo the inbound
// `traceparent` header back on the response. Coordination events
// observed on NATS MUST surface the trace fields on their log lines
// when the upstream publisher attaches a `traceparent`.

package integration

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestObs_HTTPTraceparent_EchoesOnResponse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()

	peers := Peers()
	_, _ = retryLCM(t, ctx, peers[0].Name, "GET", "/v1/status", "", 200, 2*time.Minute)

	const tp = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	hc := &http.Client{Timeout: 5 * time.Second}

	// Obs surface — direct on host port.
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
			peers[0].HealthURL+path, nil)
		req.Header.Set("traceparent", tp)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if got := resp.Header.Get("traceparent"); got != tp {
			t.Errorf("%s should echo traceparent, got %q want %q", path, got, tp)
		}
	}

	// Control plane is reached via docker compose exec; curl in-
	// container has the leverage to echo headers.
	args := []string{
		"compose", "-p", harness.project, "-f", harness.composeFile,
		"exec", "-T", peers[0].Name,
		"curl", "-sS", "-D", "-",
		"-H", "traceparent: " + tp,
		"-H", "Authorization: Bearer " + integrationToken,
		"http://127.0.0.1:9091/v1/status",
	}
	out, err := dockerComposeOutput(ctx, args[1:]...)
	if err != nil {
		t.Fatalf("control plane curl: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("traceparent: "+tp)) {
		t.Errorf("control plane should echo traceparent on /v1/status, got:\n%s", out)
	}
}

// TestObs_NATSTraceparent_PropagatesToLogLine asserts that when a
// coordination event arrives with a `traceparent` header, the
// emitted log line carries `trace_id` + `span_id` fields.
//
// We can't easily inject a NATS message with headers into the live
// pg-manager flow from a black-box test; flag this as scaffold-only
// until we have a NATS test client wired into the harness.
func TestObs_NATSTraceparent_PropagatesToLogLine(t *testing.T) {
	t.Skip("requires a NATS publish hook into the running harness; tracked alongside FR-006 follow-up")
	_ = strings.Contains
}
