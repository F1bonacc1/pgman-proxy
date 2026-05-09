// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build integration

// US3 / T070 — every required log-event name from
// contracts/observability.md § Required event names MUST be emitted
// at the documented level with the documented fields.

package integration

import (
	"context"
	"strings"
	"testing"
	"time"
)

// requiredStartupEvents is the subset of log events the test asserts
// after a clean cluster bring-up. Runtime-only events (`leader changed`,
// `lease renewal failed`) are exercised by their own integration tests
// when those conditions arise; this test focuses on what every healthy
// cold start MUST emit.
var requiredStartupEvents = []struct {
	msg    string
	level  string
	fields []string // substrings expected in the log JSON
}{
	{"config loaded", "INFO", []string{`"version":`}},
	{"nats connected", "INFO", []string{`"url":`}},
	{"manager started", "INFO", []string{`"singleton_claim_attempts":`}},
	{"control_plane started", "INFO", []string{`"addr":`, `"auth_source":`}},
	{"proxy listener bound", "INFO", []string{`"addr":`}},
}

func TestObs_LogSchema_RequiredEventsPresent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	peers := Peers()
	_, _ = retryLCM(t, ctx, peers[0].Name, "GET", "/v1/status", "", 200, 2*time.Minute)

	logs, err := dumpLogs(ctx, peers[0].Name)
	if err != nil {
		t.Fatalf("dumpLogs: %v", err)
	}

	for _, e := range requiredStartupEvents {
		t.Run(e.msg, func(t *testing.T) {
			marker := `"msg":"` + e.msg + `"`
			if !strings.Contains(logs, marker) {
				t.Errorf("missing event %q (looking for %q in container logs)", e.msg, marker)
				return
			}
			levelMarker := `"level":"` + e.level + `"`
			if !strings.Contains(logs, levelMarker) {
				t.Errorf("event %q missing level %q", e.msg, e.level)
			}
			for _, f := range e.fields {
				if !strings.Contains(logs, f) {
					t.Errorf("event %q missing field %q", e.msg, f)
				}
			}
		})
	}

	// Every record carries the documented core fields.
	t.Run("core_fields_present", func(t *testing.T) {
		for _, want := range []string{
			`"cluster_id":"pgman-proxy-it"`,
			`"node_id":"node-a"`,
			`"component":"runtime"`,
		} {
			if !strings.Contains(logs, want) {
				t.Errorf("logs missing core field %q", want)
			}
		}
	})
}
