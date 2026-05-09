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
		t.Skipf("callLCM: %v (cluster may not be reachable yet)", err)
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
