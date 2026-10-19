// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

package runtime

import (
	"context"
	"fmt"
	"time"

	pgmanager "github.com/f1bonacc1/pg-manager"

	"github.com/f1bonacc1/pgman-proxy/internal/obs"
)

// Budget split for StopPostgres. The step runs between the manager step
// and the steps that release the leadership lease and shut NATS down,
// so it must leave those steps time to run: without the lease release a
// failover waits for the lease to expire.
const (
	// postgresStopTailReserve is kept for the drain steps after
	// "postgres": event unsubscribes, the lease delete (which
	// Leadership.Close bounds at 2 s), the NATS drain and the embedded
	// server shutdown.
	postgresStopTailReserve = 3 * time.Second
	// postgresStopImmediateWindow is kept for the immediate fallback.
	postgresStopImmediateWindow = 2 * time.Second
	// postgresStopFastFloor is the least time fast mode gets, even when
	// that eats into the reserves above.
	postgresStopFastFloor = time.Second
)

// postgresStopper is the part of pgmanager.PostgresExecutor the
// shutdown step uses.
type postgresStopper interface {
	IsRunning(ctx context.Context) (bool, error)
	Stop(ctx context.Context, mode pgmanager.StopMode) error
}

// StopPostgres shuts the local PostgreSQL down as a drain step (issue
// #11). pg-manager's Manager.Stop leaves Postgres running and hands its
// shutdown to the host. Without this step the bundled image's tini
// exits with pgman-proxy and the container runtime SIGKILLs the
// postmaster: no shutdown checkpoint, and on a primary any WAL not yet
// streamed survives only on the old primary.
//
// Fast mode on a primary lets the walsenders ship the remaining WAL,
// including the shutdown checkpoint, before they exit. The caller MUST
// still hold the leadership lease while this runs, so no standby
// promotes on a fork point short of that WAL.
//
// When fast mode does not finish within its share of ctx's deadline,
// the step escalates to immediate mode and returns an error wrapping
// ErrDrainDegraded. When Postgres is not running it does nothing: a
// stop against an orphaned pid file would otherwise block for the full
// pg_ctl timeout (pg-manager milestone 014).
func StopPostgres(ctx context.Context, pg postgresStopper, logger *obs.Logger) error {
	start := time.Now()
	running, err := pg.IsRunning(ctx)
	if err == nil && !running {
		logger.Info("postgres already stopped")
		return nil
	}
	// An IsRunning error means unknown: take the stop path.

	fastCtx, cancel := ctx, context.CancelFunc(func() {})
	if window, ok := fastStopWindow(ctx); ok {
		fastCtx, cancel = context.WithTimeout(ctx, window)
	}
	fastErr := pg.Stop(fastCtx, pgmanager.StopFast)
	cancel()
	if fastErr == nil {
		logger.Info("postgres stopped",
			pgmanager.Field{Key: "mode", Value: pgmanager.StopFast.String()},
			pgmanager.Field{Key: "duration_ms", Value: time.Since(start).Milliseconds()})
		return nil
	}
	logger.Warn("postgres stop escalated",
		pgmanager.Field{Key: "from", Value: pgmanager.StopFast.String()},
		pgmanager.Field{Key: "to", Value: pgmanager.StopImmediate.String()},
		pgmanager.Field{Key: "error", Value: fastErr.Error()})

	// Immediate mode is cheap: the postmaster SIGQUITs its children and
	// exits without a checkpoint. Its window keeps the tail reserve.
	immCtx, immCancel := context.WithTimeout(ctx, postgresStopImmediateWindow)
	immErr := pg.Stop(immCtx, pgmanager.StopImmediate)
	immCancel()
	if immErr != nil {
		logger.Error("postgres stop failed",
			pgmanager.Field{Key: "fast_error", Value: fastErr.Error()},
			pgmanager.Field{Key: "immediate_error", Value: immErr.Error()},
			pgmanager.Field{Key: "duration_ms", Value: time.Since(start).Milliseconds()})
		// %v, not %w: a wrapped context error would make Drain ignore
		// a Postgres that is still running.
		return fmt.Errorf("postgres stop: fast: %v; immediate: %v", fastErr, immErr)
	}
	logger.Warn("postgres stopped",
		pgmanager.Field{Key: "mode", Value: pgmanager.StopImmediate.String()},
		pgmanager.Field{Key: "duration_ms", Value: time.Since(start).Milliseconds()})
	return fmt.Errorf("%w: postgres fast shutdown escalated to immediate: %v", ErrDrainDegraded, fastErr)
}

// fastStopWindow is how long fast mode may run: ctx's remaining time
// minus the immediate and tail reserves, never below
// postgresStopFastFloor. It reports false when ctx has no deadline, in
// which case fast mode is bounded by ctx alone.
func fastStopWindow(ctx context.Context) (time.Duration, bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0, false
	}
	return max(time.Until(deadline)-postgresStopImmediateWindow-postgresStopTailReserve, postgresStopFastFloor), true
}
