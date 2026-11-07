// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

// Peer Postgres TLS (spec 001 FR-018): postgres.tls_mode and its
// certificate files become the libpq parameters of every peer DSN
// pg-manager uses for replication, base backup, rewind and peer probes.

package config

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// TLS modes accepted for postgres.tls_mode (libpq sslmode values).
const (
	TLSModeDisable    = "disable"
	TLSModeAllow      = "allow"
	TLSModePrefer     = "prefer"
	TLSModeRequire    = "require"
	TLSModeVerifyCA   = "verify-ca"
	TLSModeVerifyFull = "verify-full"
)

// EffectiveTLSMode is postgres.tls_mode, or verify-full when unset.
func (p PostgresConfig) EffectiveTLSMode() string {
	if p.TLSMode == "" {
		return TLSModeVerifyFull
	}
	return p.TLSMode
}

// PeerSSLParams renders the libpq connection parameters for peer
// connections: sslmode, plus sslrootcert / sslcert / sslkey when set.
// disable carries no certificate parameters.
func (p PostgresConfig) PeerSSLParams() string {
	mode := p.EffectiveTLSMode()
	parts := []string{"sslmode=" + mode}
	if mode != TLSModeDisable {
		if p.TLSCAFile != "" {
			parts = append(parts, "sslrootcert="+p.TLSCAFile)
		}
		if p.TLSCertFile != "" {
			parts = append(parts, "sslcert="+p.TLSCertFile)
		}
		if p.TLSKeyFile != "" {
			parts = append(parts, "sslkey="+p.TLSKeyFile)
		}
	}
	return strings.Join(parts, " ")
}

// DSNHasSSLMode reports whether a libpq keyword/value DSN sets sslmode
// itself.
func DSNHasSSLMode(dsn string) bool {
	for _, f := range strings.Fields(dsn) {
		if strings.HasPrefix(f, "sslmode=") {
			return true
		}
	}
	return false
}

func dsnSSLMode(dsn string) string {
	for _, f := range strings.Fields(dsn) {
		if v, ok := strings.CutPrefix(f, "sslmode="); ok {
			return v
		}
	}
	return ""
}

// TLSWeakenings lists every way peer TLS falls below verify-full: the
// node-wide tls_mode, and hand-written peer_dsns entries that set a
// weaker sslmode themselves. Logged at startup (FR-018). Sorted.
func (p PostgresConfig) TLSWeakenings() []string {
	var out []string
	if mode := p.EffectiveTLSMode(); mode != TLSModeVerifyFull {
		out = append(out, fmt.Sprintf("postgres.tls_mode=%s: peer Postgres connections are not verify-full", mode))
	}
	peers := make([]string, 0, len(p.PeerDSNs))
	for peer := range p.PeerDSNs {
		peers = append(peers, peer)
	}
	sort.Strings(peers)
	for _, peer := range peers {
		if mode := dsnSSLMode(p.PeerDSNs[peer]); mode != "" && mode != TLSModeVerifyFull {
			out = append(out, fmt.Sprintf("postgres.peer_dsns[%s] sets sslmode=%s", peer, mode))
		}
	}
	return out
}

// validatePostgresTLS checks the peer TLS settings.
func validatePostgresTLS(p PostgresConfig, m *MultiError) {
	mode := p.EffectiveTLSMode()
	switch mode {
	case TLSModeDisable, TLSModeAllow, TLSModePrefer, TLSModeRequire, TLSModeVerifyCA, TLSModeVerifyFull:
	default:
		m.Add(fmt.Errorf("postgres.tls_mode %q must be one of: disable, allow, prefer, require, verify-ca, verify-full (FR-018)", p.TLSMode))
		return
	}
	if mode == TLSModeDisable && !p.TLSDisableExplicitAck {
		m.Add(errors.New("postgres.tls_mode=disable rejected without postgres.tls_disable_explicit_ack=true (FR-018)"))
	}
	if (mode == TLSModeVerifyCA || mode == TLSModeVerifyFull) && p.TLSCAFile == "" {
		m.Add(fmt.Errorf("postgres.tls_mode=%s needs postgres.tls_ca_file, the CA that signed the peers' Postgres server certificates "+
			"(PGMAN_PROXY_POSTGRES_TLS_CA_FILE); to run peer replication without TLS instead, set postgres.tls_mode=disable "+
			"and postgres.tls_disable_explicit_ack=true (FR-018)", mode))
	}
	if (p.TLSCertFile == "") != (p.TLSKeyFile == "") {
		m.Add(errors.New("postgres.tls_cert_file and postgres.tls_key_file must be set together"))
	}
}
