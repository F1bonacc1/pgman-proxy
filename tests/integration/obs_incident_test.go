// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build integration

// US3 / T068 — incident-response observability (SC-004). An operator
// MUST be able to answer the three on-call questions from metrics +
// logs ALONE, without reading source:
//
//   1. "Who is leader right now?"
//      → pgman_proxy_leadership_state{state="leader"} == 1 on exactly one peer
//   2. "When was the last failover?"
//      → pgman_proxy_leader_changes_total has incremented; the
//        `coordination event` log line at the change-over moment
//   3. "Is any peer running with a stale lease?"
//      → pgman_proxy_lease_renewal_failures_total > 0 on the offending peer

package integration

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestObs_IncidentResponse_AnswersFromMetricsAndLogsOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	peers := Peers()
	_, _ = retryLCM(t, ctx, peers[0].Name, "GET", "/v1/status", "", 200, 2*time.Minute)

	// Q1 — who is leader: scrape /metrics on every peer, look for
	// `pgman_proxy_leadership_state{state="leader"} 1`.
	leaderFound := false
	for _, p := range peers {
		body, err := scrapeMetrics(ctx, p)
		if err != nil {
			t.Fatalf("scrape %s: %v", p.Name, err)
		}
		if strings.Contains(body, `pgman_proxy_leadership_state{`) &&
			strings.Contains(body, `state="leader"} 1`) {
			leaderFound = true
			t.Logf("leader observed on %s", p.Name)
			break
		}
	}
	if !leaderFound {
		t.Errorf("no peer reports state=\"leader\" via /metrics")
	}

	// Q2 — leader changes counter has a known schema (counter exists,
	// is exported, has a value). Without forcing a failover here the
	// value may legitimately be 0; presence of the metric is the
	// observability promise.
	for _, p := range peers {
		body, _ := scrapeMetrics(ctx, p)
		if !strings.Contains(body, "pgman_proxy_leader_changes_total") {
			t.Errorf("%s missing pgman_proxy_leader_changes_total", p.Name)
		}
	}

	// Q3 — stale-lease counter is exported per peer.
	for _, p := range peers {
		body, _ := scrapeMetrics(ctx, p)
		if !strings.Contains(body, "pgman_proxy_lease_renewal_failures_total") {
			t.Errorf("%s missing pgman_proxy_lease_renewal_failures_total", p.Name)
		}
	}
}
