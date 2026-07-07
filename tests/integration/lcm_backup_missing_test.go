// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build integration

// US4 / T048 — TriggerBackup with no operator-supplied executor MUST
// return `backup_executor_missing` (FR-030, HTTP 412). The harness
// deliberately leaves backup.driver empty.

package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestLCM_TriggerBackup_NoExecutor_Returns412(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()

	peers := Peers()
	// Wait for the cluster to be ready so we don't get
	// `cluster_bootstrapping` instead.
	_, _ = retryLCM(t, ctx, peers[0].Name, "GET", "/v1/status", "", 200, 2*time.Minute)

	// The literal 412 is only visible on the LEADER's direct response;
	// a non-leader peer in forward mode relays the leader's envelope
	// inside engine_result under an outer 200. Which peer is leader
	// depends on the boot-time election, so probe every peer: the
	// direct answer carries the 412 contract, forwarded answers must
	// relay the same failure.
	sawDirect := false
	for _, p := range peers {
		code, body, err := callLCM(ctx, p.Name, "POST", "/v1/backup", integrationToken, "")
		if err != nil {
			t.Fatalf("callLCM via %s: %v", p.Name, err)
		}
		var outer lcmResponse
		if jerr := json.Unmarshal(body, &outer); jerr != nil {
			t.Fatalf("decode via %s: %v (body: %s)", p.Name, jerr, body)
		}
		if isForwardRelay(outer) {
			inner := unwrapForwarded(outer)
			if inner.Outcome != "failed" || inner.Error == nil || inner.Error.Code != "backup_executor_missing" {
				t.Errorf("forwarded via %s: relayed envelope %+v, want failed/backup_executor_missing",
					p.Name, inner.Error)
			}
			continue
		}
		sawDirect = true
		env := expectLCM(t, code, body, 412, "failed")
		if env.Error == nil || env.Error.Code != "backup_executor_missing" {
			t.Errorf("error: got %+v, want backup_executor_missing", env.Error)
		}
	}
	if !sawDirect {
		t.Error("no peer answered the backup probe directly — leader not found among the peers")
	}
}
