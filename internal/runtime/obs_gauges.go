// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

package runtime

import (
	"context"
	"time"

	"github.com/f1bonacc1/pgman-proxy/internal/embedded"
	"github.com/f1bonacc1/pgman-proxy/internal/obs"
)

// startObservabilityGauges keeps the feature-002 gauge surface
// (contracts/observability.md FR-013) current from cheap in-process
// state: embedded-NATS readiness and meshed-route count, the KV
// replica-factor decision, and this peer's leadership state. The
// gauges were registered by obs.NewMetrics since feature 002 but had
// no writer — /metrics served zeros for the embedded pair and nothing
// at all for the two GaugeVecs (surfaced by the integration suite's
// FR-013 tests once the full suite ran end-to-end).
//
// Values are set once synchronously so a scrape immediately after
// startup sees them, then refreshed on the same 2 s cadence the
// RouteWatcher uses. Snapshot() and IsLeader() are in-process reads —
// no I/O per tick.
func startObservabilityGauges(ctx context.Context, m *obs.MetricSet, emb *embedded.Server,
	leader interface{ IsLeader() bool }, replicas int, overridden bool,
) {
	if m == nil || emb == nil || leader == nil {
		return
	}
	ov := "false"
	if overridden {
		ov = "true"
	}
	update := func() {
		snap := emb.Snapshot()
		m.EmbeddedNATSUp.Set(boolGauge(snap.Ready))
		m.EmbeddedNATSRoutesMeshed.Set(float64(snap.RoutesMeshed))
		m.EmbeddedNATSReplicasFactor.WithLabelValues(ov).Set(float64(replicas))
		isLeader := leader.IsLeader()
		m.LeadershipState.WithLabelValues("leader").Set(boolGauge(isLeader))
		m.LeadershipState.WithLabelValues("follower").Set(boolGauge(!isLeader))
	}
	update()
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				update()
			}
		}
	}()
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
