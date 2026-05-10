// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

package main

import (
	"testing"
	"time"
)

// TestWriteTimeoutBudgetInvariant pins the relationship between
// defaultWriteTimeout and the libpq DSN's per-host connect_timeout.
// The writer pumps INSERTs through a pgxpool whose multi-host DSN
// fails over via libpq fall-through; each dead host costs up to its
// connect_timeout to be skipped. If defaultWriteTimeout <=
// peerCount * dsnConnectTimeout, a single dead peer eats the entire
// per-Exec budget before pgxpool can reach a healthy one — exactly
// the regression Gap L: with the original 2s budget and 2s
// connect_timeout, killing one peer froze writes_ok at 0
// successful writes for the entire duration of the outage.
//
// libpq's connect_timeout has integer-second granularity with a
// floor of 1s, so the smallest workable connect_timeout is 1s.
// With three peers that means worst-case fall-through is 3s; the
// remaining defaultWriteTimeout - 3s is the time pgxpool gets to
// actually run the INSERT once it reaches a live peer.
func TestWriteTimeoutBudgetInvariant(t *testing.T) {
	const (
		peerCount         = 3
		dsnConnectTimeout = 1 * time.Second // libpq minimum
		queryHeadroom     = 1 * time.Second // INSERT execution slack
	)
	minBudget := peerCount*dsnConnectTimeout + queryHeadroom

	if defaultWriteTimeout < minBudget {
		t.Fatalf(
			"defaultWriteTimeout=%v is too tight for %d-peer fall-through "+
				"at connect_timeout=%v + %v query slack (need >= %v); "+
				"a single dead peer would consume the entire per-Exec budget",
			defaultWriteTimeout, peerCount, dsnConnectTimeout, queryHeadroom, minBudget,
		)
	}
}
