// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

package cluster

import "testing"

// The resolver path (peer addresses published to the KV) must carry the
// configured TLS parameters, not a hardcoded sslmode=disable (FR-018).
func TestPeerDSN_CarriesSSLParams(t *testing.T) {
	got := PeerDSN("10.0.0.2", "5432", "sslmode=verify-full sslrootcert=/ca.pem")
	want := "host=10.0.0.2 port=5432 user=postgres dbname=postgres sslmode=verify-full sslrootcert=/ca.pem"
	if got != want {
		t.Fatalf("PeerDSN = %q, want %q", got, want)
	}
}
