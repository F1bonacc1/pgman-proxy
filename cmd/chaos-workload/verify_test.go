// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

package main

import (
	"slices"
	"sync"
	"testing"
)

// TestFinalizeVerifyDiff_AbsorbsInFlightCommit pins the fix for the
// false-extras race observed under sustained chaos load: the writer
// flow is INSERT -> Store(confirmedSeqs), so a row that commits
// between the verifier's initial Range and its SELECT lands in the
// DB result but not in the pre-SELECT snapshot. finalizeVerifyDiff
// re-Ranges confirmedSeqs after the SELECT to fold those in-flight
// commits back into expected, eliminating the false extras=1 spam.
func TestFinalizeVerifyDiff_AbsorbsInFlightCommit(t *testing.T) {
	var confirmed sync.Map
	confirmed.Store(int64(1), struct{}{})
	confirmed.Store(int64(2), struct{}{})

	// Initial snapshot taken before the writer Stored seq 3.
	expected := map[int64]struct{}{1: {}, 2: {}}

	// Between the snapshot and the SELECT, the writer committed seq 3
	// (INSERT durable, then Store into confirmedSeqs).
	confirmed.Store(int64(3), struct{}{})
	present := map[int64]struct{}{1: {}, 2: {}, 3: {}}

	missing, extras := finalizeVerifyDiff(&confirmed, expected, present)

	if len(missing) != 0 {
		t.Errorf("missing=%v, want []", missing)
	}
	if extras != 0 {
		t.Errorf("extras=%d, want 0 (in-flight commit should be absorbed)", extras)
	}
}

// TestFinalizeVerifyDiff_ReportsRealExtras guards against the fix
// over-correcting: a row that is in the DB but NEVER in confirmedSeqs
// (e.g., a duplicate INSERT, or a row from a different test run that
// was not cleaned up) must still surface as extras > 0.
func TestFinalizeVerifyDiff_ReportsRealExtras(t *testing.T) {
	var confirmed sync.Map
	confirmed.Store(int64(1), struct{}{})
	confirmed.Store(int64(2), struct{}{})

	expected := map[int64]struct{}{1: {}, 2: {}}
	// seq 3 is in the DB but the writer never Stored it.
	present := map[int64]struct{}{1: {}, 2: {}, 3: {}}

	missing, extras := finalizeVerifyDiff(&confirmed, expected, present)

	if len(missing) != 0 {
		t.Errorf("missing=%v, want []", missing)
	}
	if extras != 1 {
		t.Errorf("extras=%d, want 1 (genuine extra row must still be reported)", extras)
	}
}

// TestFinalizeVerifyDiff_ReportsDataLoss is the smoke test for the
// missing-rows path: a seq the writer Stored as confirmed must be
// flagged as DATA LOSS if the DB no longer returns it.
func TestFinalizeVerifyDiff_ReportsDataLoss(t *testing.T) {
	var confirmed sync.Map
	confirmed.Store(int64(1), struct{}{})
	confirmed.Store(int64(2), struct{}{})
	confirmed.Store(int64(3), struct{}{})

	expected := map[int64]struct{}{1: {}, 2: {}, 3: {}}
	present := map[int64]struct{}{1: {}, 2: {}} // seq 3 vanished from DB

	missing, extras := finalizeVerifyDiff(&confirmed, expected, present)

	slices.Sort(missing)
	if !slices.Equal(missing, []int64{3}) {
		t.Errorf("missing=%v, want [3]", missing)
	}
	if extras != 0 {
		t.Errorf("extras=%d, want 0", extras)
	}
}

// TestFinalizeVerifyDiff_CleanState is the baseline: no race, no loss,
// no extras. Pins that the helper does nothing surprising when the
// snapshot and the DB agree.
func TestFinalizeVerifyDiff_CleanState(t *testing.T) {
	var confirmed sync.Map
	confirmed.Store(int64(1), struct{}{})
	confirmed.Store(int64(2), struct{}{})
	confirmed.Store(int64(3), struct{}{})

	expected := map[int64]struct{}{1: {}, 2: {}, 3: {}}
	present := map[int64]struct{}{1: {}, 2: {}, 3: {}}

	missing, extras := finalizeVerifyDiff(&confirmed, expected, present)

	if len(missing) != 0 {
		t.Errorf("missing=%v, want []", missing)
	}
	if extras != 0 {
		t.Errorf("extras=%d, want 0", extras)
	}
}
