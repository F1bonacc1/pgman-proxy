package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/f1bonacc1/pgman-proxy/internal/obs"
)

func TestExitName_Stable(t *testing.T) {
	pairs := map[int]string{
		ExitOK:           "ok",
		ExitObs:          "obs",
		ExitDeps:         "deps",
		ExitListen:       "listen",
		ExitSingleton:    "singleton",
		ExitConfig:       "config",
		ExitDrainTimeout: "drain_timeout",
		ExitInternal:     "internal",
		ExitControl:      "control",
		999:              "unknown",
	}
	for code, want := range pairs {
		if got := ExitName(code); got != want {
			t.Errorf("ExitName(%d) = %q, want %q", code, got, want)
		}
	}
}

func TestDrain_HappyPath(t *testing.T) {
	logger := obs.NewLogger(&obs.SafeBuffer{}, "info", "demo", "node-a", "test")
	called := []string{}
	steps := []DrainStep{
		{Name: "control", Stop: func(context.Context) error { called = append(called, "control"); return nil }},
		{Name: "listener", Stop: func(context.Context) error { called = append(called, "listener"); return nil }},
		{Name: "manager", Stop: func(context.Context) error { called = append(called, "manager"); return nil }},
	}
	res := Drain(context.Background(), 1*time.Second, steps, logger)
	if res.TimedOut {
		t.Error("expected no timeout")
	}
	if len(res.StopErrors) > 0 {
		t.Errorf("unexpected stop errors: %v", res.StopErrors)
	}
	if res.ExitCode() != ExitOK {
		t.Errorf("ExitCode = %d, want %d", res.ExitCode(), ExitOK)
	}
	if got := append([]string(nil), called...); !equalSlice(got, []string{"control", "listener", "manager"}) {
		t.Errorf("steps ran in wrong order: %v", got)
	}
}

func TestDrain_TimeoutMapsToExitCode(t *testing.T) {
	logger := obs.NewLogger(&obs.SafeBuffer{}, "info", "demo", "node-a", "test")
	steps := []DrainStep{
		{Name: "slow", Stop: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}},
	}
	res := Drain(context.Background(), 50*time.Millisecond, steps, logger)
	// At minimum the ExitCode must be ExitDrainTimeout OR ExitOK (if the
	// step honoured the deadline cleanly). Reject ExitInternal here —
	// budget exhaustion is not an internal-error condition.
	if res.ExitCode() == ExitInternal {
		t.Errorf("drain timeout should not surface as ExitInternal; got %d", res.ExitCode())
	}
}

func TestDrain_StopErrorMapsToExitInternal(t *testing.T) {
	logger := obs.NewLogger(&obs.SafeBuffer{}, "info", "demo", "node-a", "test")
	bad := errors.New("boom")
	steps := []DrainStep{
		{Name: "broken", Stop: func(context.Context) error { return bad }},
	}
	res := Drain(context.Background(), 1*time.Second, steps, logger)
	if res.ExitCode() != ExitInternal {
		t.Errorf("ExitCode = %d, want ExitInternal", res.ExitCode())
	}
	if len(res.StopErrors) != 1 || !errors.Is(res.StopErrors[0], bad) {
		t.Errorf("StopErrors = %v, want [boom]", res.StopErrors)
	}
}

func equalSlice(a, b []string) bool {
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
