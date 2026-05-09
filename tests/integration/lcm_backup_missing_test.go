// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build integration

// US4 / T048 — TriggerBackup with no operator-supplied executor MUST
// return `backup_executor_missing` (FR-030, HTTP 412). The harness
// deliberately leaves backup.driver empty.

package integration

import (
	"context"
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

	code, body, err := callLCM(ctx, peers[0].Name, "POST", "/v1/backup", integrationToken, "")
	if err != nil {
		t.Fatalf("callLCM: %v", err)
	}
	env := expectLCM(t, code, body, 412, "failed")
	if env.Error == nil || env.Error.Code != "backup_executor_missing" {
		t.Errorf("error: got %+v, want backup_executor_missing", env.Error)
	}
}
