// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

// Command chaos-workload is a synthetic PostgreSQL writer/reader that
// exercises a pgman-proxy fleet under operator-driven chaos.
//
// It inserts monotonically-numbered rows through the proxy and tracks
// in memory which (writer_id, seq) pairs the server acknowledged. A
// periodic verifier sweep reads the table back and asserts every
// acknowledged seq is still present. A row that was acknowledged but
// later not readable is logged as DATA LOSS — that is the signal a
// chaos operator is hunting for.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"
)

const schemaSQL = `
CREATE TABLE IF NOT EXISTS chaos_events (
  writer_id  TEXT        NOT NULL,
  seq        BIGINT      NOT NULL,
  written_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  payload    BYTEA       NOT NULL,
  PRIMARY KEY (writer_id, seq)
);
`

type counters struct {
	writesOK     atomic.Int64
	writesFailed atomic.Int64
	dataLoss     atomic.Int64
	extraRows    atomic.Int64
}

func main() {
	var (
		dsn            = flag.String("dsn", envOr("CHAOS_DSN", "host=127.0.0.1 port=16432 user=postgres dbname=postgres sslmode=disable connect_timeout=2"), "libpq DSN; supports comma-separated host/port for multi-host failover")
		writeRPS       = flag.Int("write-rps", 20, "writes per second target rate")
		verifyInterval = flag.Duration("verify-interval", 5*time.Second, "verifier sweep interval")
		writerID       = flag.String("writer-id", "", "writer_id override (default: random ULID per process start)")
	)
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if *writerID == "" {
		*writerID = ulid.Make().String()
	}
	if *writeRPS <= 0 {
		slog.Error("invalid --write-rps", "value", *writeRPS)
		os.Exit(2)
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := pgxpool.ParseConfig(*dsn)
	if err != nil {
		slog.Error("parse dsn", "err", err)
		os.Exit(2)
	}
	cfg.MaxConns = 4
	cfg.MinConns = 1
	cfg.MaxConnIdleTime = 30 * time.Second
	cfg.HealthCheckPeriod = 5 * time.Second

	pool, err := pgxpool.NewWithConfig(rootCtx, cfg)
	if err != nil {
		slog.Error("connect pool", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := bootstrapSchema(rootCtx, pool); err != nil {
		slog.Error("bootstrap schema", "err", err)
		os.Exit(1)
	}

	slog.Info("chaos-workload started",
		"writer_id", *writerID,
		"write_rps", *writeRPS,
		"verify_interval", verifyInterval.String(),
		"dsn", *dsn,
	)

	var (
		nextSeq        atomic.Int64
		confirmedSeqs  sync.Map
		ctrs           counters
	)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		runWriter(rootCtx, pool, *writerID, *writeRPS, &nextSeq, &confirmedSeqs, &ctrs)
	}()
	go func() {
		defer wg.Done()
		runVerifier(rootCtx, pool, *writerID, *verifyInterval, &nextSeq, &confirmedSeqs, &ctrs)
	}()

	wg.Wait()

	finalCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	verifyOnce(finalCtx, pool, *writerID, &nextSeq, &confirmedSeqs, &ctrs, "final")

	slog.Info("chaos-workload exiting",
		"writer_id", *writerID,
		"writes_ok", ctrs.writesOK.Load(),
		"writes_failed", ctrs.writesFailed.Load(),
		"data_loss_total", ctrs.dataLoss.Load(),
		"extra_rows", ctrs.extraRows.Load(),
	)
}

func bootstrapSchema(ctx context.Context, pool *pgxpool.Pool) error {
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		attemptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := pool.Exec(attemptCtx, schemaSQL)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		slog.Warn("schema bootstrap failed; retrying", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("schema bootstrap timed out: %w", lastErr)
}

func runWriter(
	ctx context.Context,
	pool *pgxpool.Pool,
	writerID string,
	rps int,
	nextSeq *atomic.Int64,
	confirmedSeqs *sync.Map,
	ctrs *counters,
) {
	interval := time.Second / time.Duration(rps)
	if interval <= 0 {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	const insertSQL = `INSERT INTO chaos_events (writer_id, seq, payload) VALUES ($1, $2, $3)`

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		seq := nextSeq.Add(1)
		payload := make([]byte, 64)
		_, _ = rand.Read(payload)

		opCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := pool.Exec(opCtx, insertSQL, writerID, seq, payload)
		cancel()

		if err == nil {
			confirmedSeqs.Store(seq, struct{}{})
			ctrs.writesOK.Add(1)
			continue
		}
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return
		}
		ctrs.writesFailed.Add(1)
		slog.Warn("write failed", "writer_id", writerID, "seq", seq, "err", err)
	}
}

func runVerifier(
	ctx context.Context,
	pool *pgxpool.Pool,
	writerID string,
	interval time.Duration,
	nextSeq *atomic.Int64,
	confirmedSeqs *sync.Map,
	ctrs *counters,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		verifyOnce(ctx, pool, writerID, nextSeq, confirmedSeqs, ctrs, "verify")
	}
}

func verifyOnce(
	ctx context.Context,
	pool *pgxpool.Pool,
	writerID string,
	nextSeq *atomic.Int64,
	confirmedSeqs *sync.Map,
	ctrs *counters,
	phase string,
) {
	maxSeq := nextSeq.Load()
	if maxSeq == 0 {
		return
	}

	opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	rows, err := pool.Query(opCtx, `SELECT seq FROM chaos_events WHERE writer_id = $1 AND seq <= $2`, writerID, maxSeq)
	if err != nil {
		slog.Warn("verify query failed", "phase", phase, "err", err)
		return
	}
	present := make(map[int64]struct{}, 1024)
	for rows.Next() {
		var s int64
		if scanErr := rows.Scan(&s); scanErr != nil {
			slog.Warn("verify scan failed", "phase", phase, "err", scanErr)
			rows.Close()
			return
		}
		present[s] = struct{}{}
	}
	if rows.Err() != nil {
		slog.Warn("verify rows iter failed", "phase", phase, "err", rows.Err())
		rows.Close()
		return
	}
	rows.Close()

	var confirmedCount, missing int64
	confirmedSeqs.Range(func(k, _ any) bool {
		confirmedCount++
		seq := k.(int64)
		if _, ok := present[seq]; !ok {
			missing++
			slog.Error("DATA LOSS — acknowledged commit not readable",
				"writer_id", writerID,
				"seq", seq,
				"phase", phase,
			)
		}
		return true
	})

	dbCount := int64(len(present))
	extras := dbCount - (confirmedCount - missing)
	if extras < 0 {
		extras = 0
	}

	if missing > 0 {
		ctrs.dataLoss.Add(missing)
	}
	ctrs.extraRows.Store(extras)

	slog.Info("verify",
		"phase", phase,
		"writer_id", writerID,
		"writes_ok", ctrs.writesOK.Load(),
		"writes_failed", ctrs.writesFailed.Load(),
		"data_loss_total", ctrs.dataLoss.Load(),
		"extra_rows", extras,
		"confirmed_in_mem", confirmedCount,
		"rows_in_db", dbCount,
		"max_seq", maxSeq,
	)
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
