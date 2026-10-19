// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

package runtime

import (
	"context"
	"testing"
)

// Issue #11: an operator restart must go through main's drain, which
// stops Postgres and releases the lease, instead of exiting on the
// spot.
func TestSelfTerminate_StartsGracefulShutdown(t *testing.T) {
	var shutdowns, exits int
	s := &selfTerminator{localNodeID: "node-a", presence: "tini", shutdown: func() { shutdowns++ }}
	s.SetExitFn(func(int) { exits++ })

	s.SelfTerminate(context.Background(), "operator_restart")

	if shutdowns != 1 {
		t.Errorf("shutdown called %d times, want 1", shutdowns)
	}
	if exits != 0 {
		t.Errorf("exit called %d times, want 0: the drain must run first", exits)
	}
}

func TestSelfTerminate_ExitsWithoutShutdownHook(t *testing.T) {
	code := -1
	s := &selfTerminator{localNodeID: "node-a", presence: "tini"}
	s.SetExitFn(func(c int) { code = c })

	s.SelfTerminate(context.Background(), "operator_restart")

	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}
