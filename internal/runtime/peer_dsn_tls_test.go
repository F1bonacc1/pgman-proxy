// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

package runtime

import (
	"strings"
	"testing"

	pgmanager "github.com/f1bonacc1/pg-manager"

	"github.com/f1bonacc1/pgman-proxy/internal/config"
)

// FR-018: the peer DSNs pg-manager hands to replication
// (primary_conninfo), base backup, rewind and peer probes use
// postgres.tls_mode, not a hardcoded sslmode=disable.
func TestPeerDSNsForConfig_AppliesTLSMode(t *testing.T) {
	cfg := config.Defaults()
	cfg.Peers = []string{"node-a", "node-b"}
	cfg.Topology.Port = 5432
	cfg.Postgres.TLSCAFile = "/etc/pgman-proxy/pg-ca.pem"

	got := peerDSNsForConfig(cfg)
	for _, peer := range []pgmanager.NodeID{"node-a", "node-b"} {
		dsn := got[peer]
		if !strings.Contains(dsn, "sslmode=verify-full") || !strings.Contains(dsn, "sslrootcert=/etc/pgman-proxy/pg-ca.pem") {
			t.Errorf("%s DSN %q: want sslmode=verify-full with the configured CA", peer, dsn)
		}
		if strings.Contains(dsn, "sslmode=disable") {
			t.Errorf("%s DSN %q still hardcodes sslmode=disable", peer, dsn)
		}
	}
}

// A hand-written peer DSN without an sslmode inherits tls_mode; one with
// an explicit sslmode is the operator's choice and is left alone.
func TestPeerDSNsForConfig_OperatorDSNs(t *testing.T) {
	cfg := config.Defaults()
	cfg.Peers = []string{"node-a", "node-b"}
	cfg.Postgres.TLSCAFile = "/ca.pem"
	cfg.Postgres.PeerDSNs = map[string]string{
		"node-a": "host=a.internal port=5432 user=postgres dbname=postgres",
		"node-b": "host=b.internal port=5432 user=postgres dbname=postgres sslmode=require",
	}

	got := peerDSNsForConfig(cfg)
	if a := got["node-a"]; !strings.Contains(a, "sslmode=verify-full sslrootcert=/ca.pem") {
		t.Errorf("node-a DSN %q: an entry without sslmode must inherit tls_mode", a)
	}
	if b := got["node-b"]; b != cfg.Postgres.PeerDSNs["node-b"] {
		t.Errorf("node-b DSN %q: an explicit sslmode must be left as written", b)
	}
}
