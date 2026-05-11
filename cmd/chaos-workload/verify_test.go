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

// TestFinalizeVerifyDiff_StoreAfterSelectIsNotFalseMissing pins the
// regression that the first version of this helper hit: a seq the
// writer Stored AFTER the SELECT was sent (whose postgres commit may
// have happened after the SELECT's MVCC snapshot, so the row is
// legitimately not visible to that SELECT) MUST NOT be flagged as
// missing. The original implementation re-Ranged confirmedSeqs and
// mutated `expected`, then iterated to find missing — which incorrectly
// reported the in-flight seq as data loss every verify tick, exactly
// matching the chaos-run pattern where each reported missing equaled
// max_seq (the very latest write).
//
// Now: missing detection ignores the post-SELECT Store; extras
// detection still absorbs it.
func TestFinalizeVerifyDiff_StoreAfterSelectIsNotFalseMissing(t *testing.T) {
	var confirmed sync.Map
	confirmed.Store(int64(1), struct{}{})
	confirmed.Store(int64(2), struct{}{})

	// Initial snapshot — taken BEFORE the SELECT — captures only the
	// seqs whose Store happened-before this moment.
	expected := map[int64]struct{}{1: {}, 2: {}}

	// SELECT returns {1, 2} because seq 3's commit happened after the
	// SELECT's MVCC snapshot.
	present := map[int64]struct{}{1: {}, 2: {}}

	// After the SELECT returns, the writer's INSERT for seq 3 commits
	// and Store(3) fires. confirmedSeqs now has 3.
	confirmed.Store(int64(3), struct{}{})

	missing, extras := finalizeVerifyDiff(&confirmed, expected, present)

	if len(missing) != 0 {
		t.Errorf("missing=%v, want [] — post-SELECT Store must NOT trigger false data-loss", missing)
	}
	if extras != 0 {
		t.Errorf("extras=%d, want 0", extras)
	}
}

// TestUpdateMissingSet_NewSeqDetectedOnce pins that a seq missing in
// this tick is reported as newly detected once and added to the set.
func TestUpdateMissingSet_NewSeqDetectedOnce(t *testing.T) {
	set := map[int64]struct{}{}
	thisTick := []int64{42}
	present := map[int64]struct{}{}

	newly, resolved := updateMissingSet(set, thisTick, present)

	if !slices.Equal(newly, []int64{42}) {
		t.Errorf("newly=%v, want [42]", newly)
	}
	if len(resolved) != 0 {
		t.Errorf("resolved=%v, want []", resolved)
	}
	if _, ok := set[42]; !ok {
		t.Errorf("set missing seq 42 after add: %v", set)
	}
}

// TestUpdateMissingSet_PersistentMissingNotReReported pins the key
// behavior that fixes the runaway cumulative counter: a seq that was
// missing in a previous tick and is STILL missing must NOT show up
// again as newly detected — otherwise the operator gets one error
// per tick per missing seq instead of one error per distinct loss.
func TestUpdateMissingSet_PersistentMissingNotReReported(t *testing.T) {
	set := map[int64]struct{}{42: {}}
	thisTick := []int64{42}
	present := map[int64]struct{}{}

	newly, resolved := updateMissingSet(set, thisTick, present)

	if len(newly) != 0 {
		t.Errorf("newly=%v, want [] (already tracked)", newly)
	}
	if len(resolved) != 0 {
		t.Errorf("resolved=%v, want []", resolved)
	}
	if got := len(set); got != 1 {
		t.Errorf("set size=%d, want 1 (still just seq 42)", got)
	}
}

// TestUpdateMissingSet_ReappearanceResolvesAndRemoves pins the recovery
// path: a previously-reported missing seq that now appears in `present`
// is reported as resolved and removed from the rolling set. This is
// what eliminates the 28k false-data-loss accumulation observed in the
// chaos run — when the proxy stabilizes back on the current primary
// after a failover window, transient stale-reads heal.
func TestUpdateMissingSet_ReappearanceResolvesAndRemoves(t *testing.T) {
	set := map[int64]struct{}{42: {}, 99: {}}
	thisTick := []int64{99} // seq 42 reappeared; seq 99 still missing
	present := map[int64]struct{}{42: {}, 100: {}}

	newly, resolved := updateMissingSet(set, thisTick, present)

	if len(newly) != 0 {
		t.Errorf("newly=%v, want [] (99 was already tracked)", newly)
	}
	if !slices.Equal(resolved, []int64{42}) {
		t.Errorf("resolved=%v, want [42]", resolved)
	}
	if _, ok := set[42]; ok {
		t.Errorf("set still contains resolved seq 42: %v", set)
	}
	if _, ok := set[99]; !ok {
		t.Errorf("set lost still-missing seq 99: %v", set)
	}
}

// TestUpdateMissingSet_CountReflectsDistinctUnresolved pins the
// reported "data_loss_total" semantics: it's len(set), not a running
// sum. Across 3 ticks where seqs come and go, the set size tracks the
// instantaneous number of distinct unresolved losses, with no
// accumulation of resolved sightings.
func TestUpdateMissingSet_CountReflectsDistinctUnresolved(t *testing.T) {
	set := map[int64]struct{}{}

	// Tick 1: seqs 1, 2, 3 missing.
	updateMissingSet(set, []int64{1, 2, 3}, map[int64]struct{}{})
	if got := len(set); got != 3 {
		t.Errorf("after tick 1: size=%d, want 3", got)
	}

	// Tick 2: same 3 seqs still missing — set should not grow.
	updateMissingSet(set, []int64{1, 2, 3}, map[int64]struct{}{})
	if got := len(set); got != 3 {
		t.Errorf("after tick 2 (no change): size=%d, want 3 (NOT 6 — would be the regression)", got)
	}

	// Tick 3: seqs 1, 3 reappear; seq 2 still missing; seq 5 newly missing.
	updateMissingSet(set, []int64{2, 5}, map[int64]struct{}{1: {}, 3: {}, 99: {}})
	if got := len(set); got != 2 {
		t.Errorf("after tick 3: size=%d, want 2 (seqs 2 and 5)", got)
	}
	if _, ok := set[1]; ok {
		t.Errorf("seq 1 should be resolved")
	}
	if _, ok := set[3]; ok {
		t.Errorf("seq 3 should be resolved")
	}
	if _, ok := set[5]; !ok {
		t.Errorf("seq 5 should be tracked")
	}
}
