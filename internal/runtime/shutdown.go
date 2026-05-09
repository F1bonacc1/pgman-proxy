package runtime

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
	"time"

	pgmanager "github.com/f1bonacc1/pg-manager"

	"github.com/f1bonacc1/pgman-proxy/internal/obs"
)

// SignalContext returns a context that is cancelled on SIGINT or
// SIGTERM. SIGUSR1 / SIGHUP are received but logged-only in v1
// (contracts/lifecycle.md § Signal handling).
func SignalContext(parent context.Context, logger *obs.Logger) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)

	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGINT, syscall.SIGTERM)

	other := make(chan os.Signal, 4)
	signal.Notify(other, syscall.SIGHUP, syscall.SIGUSR1)

	go func() {
		select {
		case sig := <-term:
			logger.Info("shutdown signal", pgmanager.Field{Key: "signal", Value: sig.String()})
			cancel()
		case <-ctx.Done():
		}
	}()
	go func() {
		for {
			select {
			case sig := <-other:
				logger.Info("signal received (informational only in v1)",
					pgmanager.Field{Key: "signal", Value: sig.String()})
			case <-ctx.Done():
				return
			}
		}
	}()
	return ctx, func() {
		signal.Stop(term)
		signal.Stop(other)
		cancel()
	}
}

// DrainResult records the outcome of a graceful-shutdown attempt.
type DrainResult struct {
	Duration   time.Duration
	TimedOut   bool
	StopErrors []error
}

// ExitCode maps the drain result to the documented exit code. A
// non-empty StopErrors list maps to ExitInternal so the supervisor sees
// the failure; otherwise a clean stop maps to ExitOK and a timeout maps
// to ExitDrainTimeout.
func (r DrainResult) ExitCode() int {
	if len(r.StopErrors) > 0 {
		return ExitInternal
	}
	if r.TimedOut {
		return ExitDrainTimeout
	}
	return ExitOK
}

// Drain runs each stop step in order, bounded by the supplied drain
// budget. Stop steps are documented in contracts/lifecycle.md § Graceful
// shutdown flow:
//
//  1. control-plane HTTP server (LCM stops accepting first)
//  2. data-plane proxy listener
//  3. proxy.Stop() (per switch policy)
//  4. manager.Stop()
//  5. NATS Drain
//
// Each step receives a context whose deadline is the remaining budget,
// so a slow step doesn't starve later ones.
func Drain(parent context.Context, budget time.Duration, steps []DrainStep, logger *obs.Logger) DrainResult {
	deadline := time.Now().Add(budget)
	res := DrainResult{}
	start := time.Now()

	for i, step := range steps {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			res.TimedOut = true
			logger.Warn("shutdown drain timeout",
				pgmanager.Field{Key: "step", Value: step.Name},
				pgmanager.Field{Key: "step_index", Value: i})
			break
		}
		stepCtx, cancel := context.WithDeadline(parent, deadline)
		err := step.Stop(stepCtx)
		cancel()
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			res.StopErrors = append(res.StopErrors, err)
			logger.Warn("shutdown step error",
				pgmanager.Field{Key: "step", Value: step.Name},
				pgmanager.Field{Key: "error", Value: err.Error()})
		}
	}

	res.Duration = time.Since(start)
	if !res.TimedOut && len(res.StopErrors) == 0 {
		logger.Info("shutdown complete",
			pgmanager.Field{Key: "duration_ms", Value: res.Duration.Milliseconds()})
	}
	return res
}

// DrainStep is a named stop function with bounded duration semantics.
type DrainStep struct {
	Name string
	Stop func(context.Context) error
}
