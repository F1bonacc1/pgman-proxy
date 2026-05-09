package config

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"
)

// nodeIDRegexp matches the cluster_id / node_id / peer-name shape required
// by spec data-model.md § ProxyPeer (lowercase alnum + hyphens, length 1..63).
var nodeIDRegexp = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// MultiError aggregates several validation failures into a single error
// surface so the operator sees every problem at once instead of fixing
// them one-by-one across restarts.
type MultiError struct{ Errs []error }

// Error implements the error interface, joining one issue per line.
func (m *MultiError) Error() string {
	if len(m.Errs) == 0 {
		return ""
	}
	parts := make([]string, len(m.Errs))
	for i, e := range m.Errs {
		parts[i] = "  - " + e.Error()
	}
	return fmt.Sprintf("config validation failed (%d issue(s)):\n%s", len(m.Errs), strings.Join(parts, "\n"))
}

// Add appends an error if non-nil.
func (m *MultiError) Add(err error) {
	if err != nil {
		m.Errs = append(m.Errs, err)
	}
}

// OrNil returns nil if no errors were accumulated, else the MultiError.
func (m *MultiError) OrNil() error {
	if len(m.Errs) == 0 {
		return nil
	}
	return m
}

// Validate runs every cross-field rule from contracts/config.md
// § Validation outcomes plus the LCM-amendment rules (FR-033, FR-034).
// Returns nil on success, a *MultiError otherwise.
//
// Validation MUST be conservative: any ambiguous configuration is
// rejected (Constitution II — Fail-Closed Safety).
func Validate(cfg Config) error {
	m := &MultiError{}

	if !validDeploymentMode(cfg.DeploymentMode) {
		m.Add(fmt.Errorf("deployment_mode %q must be one of: standalone, microservice, sidecar (FR-013)", cfg.DeploymentMode))
	}

	if cfg.Cluster.ID == "" {
		m.Add(errors.New("cluster.id is required"))
	} else if !nodeIDRegexp.MatchString(cfg.Cluster.ID) {
		m.Add(fmt.Errorf("cluster.id %q is not a valid identifier", cfg.Cluster.ID))
	}
	if cfg.Node.ID == "" {
		m.Add(errors.New("node.id is required"))
	} else if !nodeIDRegexp.MatchString(cfg.Node.ID) {
		m.Add(fmt.Errorf("node.id %q is not a valid identifier", cfg.Node.ID))
	}

	if len(cfg.Peers) == 0 {
		m.Add(errors.New("peers must contain at least one entry"))
	} else {
		seen := false
		for _, p := range cfg.Peers {
			if !nodeIDRegexp.MatchString(p) {
				m.Add(fmt.Errorf("peers entry %q is not a valid identifier", p))
			}
			if p == cfg.Node.ID {
				seen = true
			}
		}
		if cfg.Node.ID != "" && !seen {
			m.Add(fmt.Errorf("peers must contain node.id %q", cfg.Node.ID))
		}
	}

	if cfg.NATS.URL == "" {
		m.Add(errors.New("nats.url is required"))
	}
	if cfg.NATS.ConnectTimeout <= 0 {
		m.Add(errors.New("nats.connect_timeout must be > 0"))
	}

	if cfg.Proxy.ListenAddr == "" {
		m.Add(errors.New("proxy.listen_addr is required"))
	}
	if !validSwitchPolicy(cfg.Proxy.SwitchPolicy) {
		m.Add(fmt.Errorf("proxy.switch_policy %q must be one of: hard_close, drain, pause", cfg.Proxy.SwitchPolicy))
	}

	if cfg.Postgres.BinDir == "" {
		m.Add(errors.New("postgres.bin_dir is required"))
	}
	if cfg.Postgres.DataDir == "" {
		m.Add(errors.New("postgres.data_dir is required"))
	}
	if cfg.Postgres.LocalDSNEnv == "" {
		m.Add(errors.New("postgres.local_dsn_env is required (env-var name holding the DSN)"))
	}
	if cfg.Postgres.TLSMode == "disable" && !cfg.Postgres.TLSDisableExplicitAck {
		m.Add(errors.New("postgres.tls_mode=disable rejected without postgres.tls_disable_explicit_ack=true (FR-018)"))
	}

	// Control plane (FR-021..FR-034)
	if cfg.Control.Auth.TokenEnv != "" && cfg.Control.Auth.TokenFile != "" {
		m.Add(errors.New("control.auth.token_env and control.auth.token_file are mutually exclusive"))
	}
	mutatingAuthRequired := !cfg.Control.Auth.AllowUnauthReads ||
		// Always required for mutating ops even when reads are unauth (FR-025).
		true
	if mutatingAuthRequired && cfg.Control.Auth.TokenEnv == "" && cfg.Control.Auth.TokenFile == "" {
		m.Add(errors.New("no control-plane token source configured: set control.auth.token_env or control.auth.token_file (FR-025)"))
	}

	if !validLeaderRouteMode(cfg.Control.LeaderRouteMode) {
		m.Add(fmt.Errorf("control.leader_route_mode %q must be one of: forward, redirect (FR-026)", cfg.Control.LeaderRouteMode))
	}
	if cfg.Control.LeaderRouteTimeout <= 0 || cfg.Control.LeaderRouteTimeout > 5*time.Minute {
		m.Add(fmt.Errorf("control.leader_route_timeout %s outside (0, 5m] (FR-034)", cfg.Control.LeaderRouteTimeout))
	}

	// FR-033 — TLS required on non-loopback bind unless plaintext is acked.
	if cfg.Control.ListenAddr != "" {
		if (cfg.Control.TLS.CertFile == "") != (cfg.Control.TLS.KeyFile == "") {
			m.Add(errors.New("control.tls.cert_file and control.tls.key_file must both be set or both be empty (FR-033)"))
		}
		loopback, err := isLoopbackAddr(cfg.Control.ListenAddr)
		if err != nil {
			m.Add(fmt.Errorf("control.listen_addr %q is not a valid host:port: %w", cfg.Control.ListenAddr, err))
		} else if !loopback {
			tlsConfigured := cfg.Control.TLS.CertFile != "" && cfg.Control.TLS.KeyFile != ""
			if !tlsConfigured && !cfg.Control.TLS.PlaintextExplicitAck {
				m.Add(errors.New("control plane plaintext bind on non-loopback rejected without control.tls.plaintext_explicit_ack=true (FR-033)"))
			}
		}
	}

	if cfg.Shutdown.DrainBudget < 0 {
		m.Add(errors.New("shutdown.drain_budget must be non-negative"))
	}

	return m.OrNil()
}

func validSwitchPolicy(s string) bool {
	switch s {
	case "hard_close", "drain", "pause":
		return true
	}
	return false
}

func validDeploymentMode(s string) bool {
	switch s {
	case DeploymentModeStandalone, DeploymentModeMicroservice, DeploymentModeSidecar:
		return true
	}
	return false
}

func validLeaderRouteMode(s string) bool {
	switch s {
	case "forward", "redirect":
		return true
	}
	return false
}

// isLoopbackAddr reports whether the host portion of "host:port" resolves
// to a loopback address. A leading colon (":9091") binds all interfaces
// and is therefore NOT loopback.
func isLoopbackAddr(addr string) (bool, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false, err
	}
	if host == "" {
		// ":port" — bind all interfaces.
		return false, nil
	}
	ip := net.ParseIP(host)
	if ip != nil {
		return ip.IsLoopback(), nil
	}
	// Non-IP (e.g., "localhost") — treat documented loopback names as loopback.
	switch host {
	case "localhost", "ip6-localhost":
		return true, nil
	}
	return false, nil
}
