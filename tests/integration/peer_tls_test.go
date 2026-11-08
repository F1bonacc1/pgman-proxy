// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/f1bonacc1/pgman-proxy/tests/integration/conninfo"
)

// TestPeerReplicationUsesVerifyFullTLS pins spec 001 FR-018 end to end:
// the harness runs with the default postgres.tls_mode (verify-full), a
// CA file and Postgres serving TLS, so every standby's walreceiver must
// stream over TLS with the configured sslmode and CA. Before FR-018 was
// wired, peer DSNs hardcoded sslmode=disable whatever tls_mode said.
func TestPeerReplicationUsesVerifyFullTLS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	primary, err := waitForConvergence(ctx, time.Now().Add(2*time.Minute))
	if err != nil {
		t.Fatalf("cluster did not converge: %v", err)
	}

	// Every streaming walsender on the primary runs over TLS.
	out, err := localPsql(ctx, primary,
		"SELECT count(*) FILTER (WHERE s.ssl), count(*) FROM pg_stat_replication r JOIN pg_stat_ssl s USING (pid) WHERE r.state = 'streaming'")
	if err != nil {
		t.Fatalf("query pg_stat_ssl on %s: %v", primary, err)
	}
	fields := strings.Split(strings.TrimSpace(out), "|")
	if len(fields) != 2 || fields[1] != "2" || fields[0] != fields[1] {
		t.Fatalf("on primary %s: (ssl streams | streams) = %q, want 2|2: every standby must stream over TLS", primary, strings.TrimSpace(out))
	}

	// Each standby connects with the configured sslmode and CA. Compare
	// parsed values: pg_basebackup -R writes sslmode='verify-full'
	// quoted, pg-manager's own rewrite writes it bare, and which one a
	// standby holds depends on how it last joined (2026-11-08 nightly).
	for _, p := range Peers() {
		if p.Name == primary {
			continue
		}
		raw, err := localPsql(ctx, p.Name, "SHOW primary_conninfo")
		if err != nil {
			t.Fatalf("SHOW primary_conninfo on %s: %v", p.Name, err)
		}
		raw = strings.TrimSpace(raw)
		kv, err := conninfo.Parse(raw)
		if err != nil {
			t.Fatalf("%s primary_conninfo %q: %v", p.Name, raw, err)
		}
		for key, want := range map[string]string{
			"sslmode":     "verify-full",
			"sslrootcert": "/etc/pgman-tls/ca.crt",
		} {
			if got := kv[key]; got != want {
				t.Errorf("%s primary_conninfo %s = %q, want %q (conninfo %q)", p.Name, key, got, want, raw)
			}
		}
	}
}
