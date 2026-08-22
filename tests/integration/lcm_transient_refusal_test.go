// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build integration

// US4 / T052d — FR-029 transient-refusal contract. Submit a mutating
// op before /readyz=200 and observe `cluster_bootstrapping`. Submit
// during a leadership transition and observe `leadership_in_transition`.
//
// The pre-readyz path is exercised opportunistically via the existing
// retryLCM helper — every LCM test that runs before bootstrap finishes
// observes `cluster_bootstrapping`. This test asserts the error code
// surfaces explicitly.

package integration

import (
	"context"
	"testing"
	"time"
)

func TestLCM_TransientRefusal_ClusterBootstrapping(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	peers := Peers()
	// Don't wait for the cluster — issue the request immediately and
	// hope to catch the bootstrap window. The pre-flight check fires
	// only when the engine reports it's still bootstrapping.
	code, body, err := callLCM(ctx, peers[0].Name, "POST", "/v1/switchover",
		integrationToken, `{"target":"node-b"}`)
	if err != nil {
		// NOT a skip. TestMain has already run `compose up --wait`, so
		// every peer is up before any Test* runs and a transport error
		// here is a real defect, not an unmet precondition.
		//
		// This was `t.Skipf` until 2026-08-22, and it masked exactly the
		// kind of regression it should have caught. An upstream
		// pg-manager change demoted the ex-primary read-only on every
		// planned switchover, opening a leaderless window in which this
		// call blocked for the full 30s leader-route timeout. The suite
		// stayed green: 32 PASS / 0 FAIL / 9 SKIP either side of the
		// bisect, with only the per-test duration moving (0.08s -> 30.0s).
		// The defect was found by diffing timings, which is not a thing
		// anyone should have to do. See pg-manager B-019.
		t.Fatalf("callLCM: %v\n"+
			"The control plane did not answer. A timeout here usually means a "+
			"leader-routed call had no leader to route to — check for a "+
			"leaderless window during the switchover this suite performs "+
			"a few tests earlier (pg-manager B-019).", err)
	}
	if code == 200 {
		t.Skipf("cluster already past bootstrap; outcome=accepted (this is the happy path)")
	}
	if code == 409 {
		env := expectLCM(t, code, body, 409, "rejected")
		if env.Error == nil || env.Error.Code != "cluster_bootstrapping" {
			t.Errorf("expected cluster_bootstrapping, got %+v", env.Error)
		}
	}
}
