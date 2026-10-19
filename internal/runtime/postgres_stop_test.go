// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	pgmanager "github.com/f1bonacc1/pg-manager"

	"github.com/f1bonacc1/pgman-proxy/internal/obs"
)

// stopCall records one Stop invocation and how long its ctx had left.
type stopCall struct {
	mode pgmanager.StopMode
	left time.Duration // time to the ctx deadline at call time; 0 = none
}

// fakePostgres implements postgresStopper. stop decides each Stop
// call's outcome; nil means success.
type fakePostgres struct {
	running    bool
	runningErr error
	stop       func(ctx context.Context, mode pgmanager.StopMode) error

	mu    sync.Mutex
	calls []stopCall
}

func (f *fakePostgres) IsRunning(context.Context) (bool, error) { return f.running, f.runningErr }

func (f *fakePostgres) Stop(ctx context.Context, mode pgmanager.StopMode) error {
	c := stopCall{mode: mode}
	if d, ok := ctx.Deadline(); ok {
		c.left = time.Until(d)
	}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	if f.stop == nil {
		return nil
	}
	return f.stop(ctx, mode)
}

func (f *fakePostgres) modes() []pgmanager.StopMode {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]pgmanager.StopMode, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.mode
	}
	return out
}

// blockFast makes fast mode run until its ctx ends, as pg_ctl does when
// a walsender waits on a stalled standby, while immediate succeeds.
func blockFast(ctx context.Context, mode pgmanager.StopMode) error {
	if mode == pgmanager.StopFast {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func testLogger(buf *obs.SafeBuffer) *obs.Logger {
	return obs.NewLogger(buf, "info", "demo", "node-a", "test")
}

func equalModes(a, b []pgmanager.StopMode) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestStopPostgres_NotRunningSkipsStop(t *testing.T) {
	buf := &obs.SafeBuffer{}
	pg := &fakePostgres{running: false}
	if err := StopPostgres(context.Background(), pg, testLogger(buf)); err != nil {
		t.Fatalf("StopPostgres = %v, want nil", err)
	}
	if got := pg.modes(); len(got) != 0 {
		t.Fatalf("Stop called %v, want no calls", got)
	}
	if !strings.Contains(buf.String(), "postgres already stopped") {
		t.Errorf("missing 'postgres already stopped' log:\n%s", buf.String())
	}
}

func TestStopPostgres_FastSucceeds(t *testing.T) {
	buf := &obs.SafeBuffer{}
	pg := &fakePostgres{running: true}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := StopPostgres(ctx, pg, testLogger(buf)); err != nil {
		t.Fatalf("StopPostgres = %v, want nil", err)
	}
	if got := pg.modes(); !equalModes(got, []pgmanager.StopMode{pgmanager.StopFast}) {
		t.Fatalf("Stop modes = %v, want [fast]", got)
	}
	// Fast mode must leave the immediate window and the tail reserve.
	maxLeft := 30*time.Second - postgresStopImmediateWindow - postgresStopTailReserve
	if left := pg.calls[0].left; left <= 0 || left > maxLeft {
		t.Errorf("fast window = %v, want (0, %v]", left, maxLeft)
	}
	if !strings.Contains(buf.String(), `"mode":"fast"`) {
		t.Errorf("missing fast-mode 'postgres stopped' log:\n%s", buf.String())
	}
}

// An IsRunning error means unknown, which must not skip the stop.
func TestStopPostgres_UnknownStateStillStops(t *testing.T) {
	pg := &fakePostgres{runningErr: errors.New("probe failed")}
	if err := StopPostgres(context.Background(), pg, testLogger(&obs.SafeBuffer{})); err != nil {
		t.Fatalf("StopPostgres = %v, want nil", err)
	}
	if got := pg.modes(); !equalModes(got, []pgmanager.StopMode{pgmanager.StopFast}) {
		t.Fatalf("Stop modes = %v, want [fast]", got)
	}
}

func TestStopPostgres_EscalatesToImmediate(t *testing.T) {
	buf := &obs.SafeBuffer{}
	pg := &fakePostgres{running: true, stop: blockFast}
	// Budget so small the fast floor applies: fast gets 1 s, then
	// immediate runs with time to spare.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	start := time.Now()
	err := StopPostgres(ctx, pg, testLogger(buf))
	if !errors.Is(err, ErrDrainDegraded) {
		t.Fatalf("StopPostgres = %v, want ErrDrainDegraded", err)
	}
	if got := pg.modes(); !equalModes(got, []pgmanager.StopMode{pgmanager.StopFast, pgmanager.StopImmediate}) {
		t.Fatalf("Stop modes = %v, want [fast immediate]", got)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("escalation took %v, want about the 1 s fast floor", took)
	}
	if left := pg.calls[1].left; left <= 0 || left > postgresStopImmediateWindow {
		t.Errorf("immediate window = %v, want (0, %v]", left, postgresStopImmediateWindow)
	}
	if !strings.Contains(buf.String(), "postgres stop escalated") {
		t.Errorf("missing 'postgres stop escalated' log:\n%s", buf.String())
	}
}

// When neither mode stops Postgres the step must report a real error,
// not a context error that Drain would swallow.
func TestStopPostgres_BothModesFail(t *testing.T) {
	pg := &fakePostgres{running: true, stop: func(ctx context.Context, mode pgmanager.StopMode) error {
		if mode == pgmanager.StopFast {
			return blockFast(ctx, mode)
		}
		return errors.New("pg_ctl exit 1")
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := StopPostgres(ctx, pg, testLogger(&obs.SafeBuffer{}))
	if err == nil {
		t.Fatal("StopPostgres = nil, want an error")
	}
	if errors.Is(err, ErrDrainDegraded) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("StopPostgres = %v, want a plain error Drain records", err)
	}
}

func TestFastStopWindow(t *testing.T) {
	if _, ok := fastStopWindow(context.Background()); ok {
		t.Error("fastStopWindow without a deadline reported a window")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	want := time.Hour - postgresStopImmediateWindow - postgresStopTailReserve
	if got, _ := fastStopWindow(ctx); got > want || got < want-time.Second {
		t.Errorf("fastStopWindow(1h) = %v, want about %v", got, want)
	}
	short, cancelShort := context.WithTimeout(context.Background(), time.Second)
	defer cancelShort()
	if got, _ := fastStopWindow(short); got != postgresStopFastFloor {
		t.Errorf("fastStopWindow(1s) = %v, want the %v floor", got, postgresStopFastFloor)
	}
}

// A degraded step makes the drain report a timeout and does not stop
// the steps after it, which release the lease.
func TestDrain_DegradedStepMapsToDrainTimeout(t *testing.T) {
	logger := testLogger(&obs.SafeBuffer{})
	var after bool
	steps := []DrainStep{
		{Name: "postgres", Stop: func(context.Context) error {
			return errors.Join(ErrDrainDegraded, errors.New("escalated"))
		}},
		{Name: "cluster", Stop: func(context.Context) error { after = true; return nil }},
	}
	res := Drain(context.Background(), time.Second, steps, logger)
	if res.ExitCode() != ExitDrainTimeout {
		t.Errorf("ExitCode = %d, want ExitDrainTimeout", res.ExitCode())
	}
	if len(res.StopErrors) != 0 {
		t.Errorf("StopErrors = %v, want none", res.StopErrors)
	}
	if !after {
		t.Error("the step after the degraded one did not run")
	}
}
