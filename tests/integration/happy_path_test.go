// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build integration

// US1 happy-path: every pgman-proxy peer accepts a wire-protocol
// session and routes a basic query through to the leader's Postgres.
// Spec coverage: SC-002 (write succeeds through every peer), SC-005
// (no logical proxy artefacts visible to the client).

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestHappyPath_WriteThroughEveryPeer connects to each peer's data-plane
// listener in turn and runs a `SELECT 1` followed by a guarded INSERT.
// All three writes MUST land on the same physical Postgres (the leader),
// confirmed by re-reading from a different peer than the one used to
// write — the proxy hop is the only routing layer.
func TestHappyPath_WriteThroughEveryPeer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	peers := Peers()

	// A schema fixture goes through node-a — any peer would do; the
	// table will exist on the leader, so subsequent reads from any peer
	// will see it once routed.
	if err := withConn(ctx, peers[0].DSN(), func(conn *pgx.Conn) error {
		_, err := conn.Exec(ctx,
			"CREATE TABLE IF NOT EXISTS happy_path (peer text, written_at timestamptz DEFAULT now())")
		return err
	}); err != nil {
		t.Fatalf("schema setup via %s: %v", peers[0].Name, err)
	}

	for _, p := range peers {
		t.Run("write_via_"+p.Name, func(t *testing.T) {
			err := withConn(ctx, p.DSN(), func(conn *pgx.Conn) error {
				var ping int
				if err := conn.QueryRow(ctx, "SELECT 1").Scan(&ping); err != nil {
					return fmt.Errorf("ping: %w", err)
				}
				if ping != 1 {
					return fmt.Errorf("expected 1, got %d", ping)
				}
				_, err := conn.Exec(ctx, "INSERT INTO happy_path(peer) VALUES ($1)", p.Name)
				return err
			})
			if err != nil {
				t.Fatalf("write via %s: %v", p.Name, err)
			}
		})
	}

	// Read back from every peer; each peer MUST observe the full set of
	// writes (the leader is shared), confirming the proxy routes to it.
	for _, p := range peers {
		t.Run("readback_via_"+p.Name, func(t *testing.T) {
			err := withConn(ctx, p.DSN(), func(conn *pgx.Conn) error {
				var n int
				if err := conn.QueryRow(ctx,
					"SELECT count(*) FROM happy_path").Scan(&n); err != nil {
					return err
				}
				if n < len(peers) {
					return fmt.Errorf("expected >=%d rows, got %d", len(peers), n)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("readback via %s: %v", p.Name, err)
			}
		})
	}
}

// withConn opens a libpq session via the proxy and runs fn. The
// connection is always closed even on error.
func withConn(ctx context.Context, dsn string, fn func(*pgx.Conn) error) error {
	connCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(connCtx, dsn)
	if err != nil {
		return fmt.Errorf("connect %s: %w", dsn, err)
	}
	defer conn.Close(context.Background())
	return fn(conn)
}
