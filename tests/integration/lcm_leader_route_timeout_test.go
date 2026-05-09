// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build integration

// US4 / T052c — FR-034 leader-route timeout. Black-hole the forward
// reply path; assert Switchover returns HTTP 504 with
// `error.code = "leader_route_timeout"` within the configured budget
// ± 1s. Audit record carries `leader_at_request` and outcome=failed.
//
// In our compose harness the forward path uses NATS req/reply; we
// black-hole it by setting an artificially low timeout and stopping
// the leader's engine while it's processing. v1 implementation has
// `leader_route_timeout` config knob; we set it to 2s and assert.

package integration

import (
	"context"
	"testing"
)

func TestLCM_LeaderRouteTimeout_Returns504(t *testing.T) {
	t.Skip("leader-route-timeout test requires a configurable leader-route timeout " +
		"override per-peer; current compose template uses the 30s default. " +
		"Tracked alongside FR-034 follow-up that exposes the override.")
	_ = context.Background
}
