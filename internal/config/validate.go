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

	// Feature 002 (FR-002, SC-009): the external-NATS path established by
	// 001 is removed. Any of the legacy `nats.*` keys present in config
	// is rejected at validation with a migration-pointing error so the
	// breaking change is loud, not silent.
	if cfg.NATS.URL != "" {
		m.Add(errors.New("nats.url is no longer supported (feature 002 removed external NATS); see cluster.* for the embedded coordination plane and the migration guide in specs/002-embedded-nats-cluster/quickstart.md"))
	}
	if cfg.NATS.CredsFile != "" || cfg.NATS.TokenEnv != "" {
		m.Add(errors.New("nats.creds_file / nats.token_env are no longer supported (feature 002); cluster credentials live under cluster.username + cluster.password (RD-001a)"))
	}
	if cfg.NATS.ConnectTimeout <= 0 {
		m.Add(errors.New("nats.connect_timeout must be > 0 (used for the loopback dial into the embedded NATS server)"))
	}

	// Feature 002 cluster validation rules
	// (specs/002-embedded-nats-cluster/contracts/config.md § Validation matrix).
	validateCluster(&cfg.Cluster, m)

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
	// postgres.replication_addr is optional. If set, it must parse as
	// host:port. When unset, the proxy does not publish to the cluster
	// KV and pg-manager falls back to its static PeerDSNs map (which
	// uses peer IDs as hostnames — fine for topologies where peer IDs
	// resolve via the substrate's DNS, like docker-compose service
	// networks or K8s StatefulSets).
	if cfg.Postgres.ReplicationAddr != "" {
		if _, _, err := net.SplitHostPort(cfg.Postgres.ReplicationAddr); err != nil {
			m.Add(fmt.Errorf("postgres.replication_addr %q must be host:port: %w", cfg.Postgres.ReplicationAddr, err))
		}
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

// validateCluster runs the embedded-NATS validation rules from
// specs/002-embedded-nats-cluster/contracts/config.md § Validation
// matrix. Append-only: each violation is added to m so operators see
// every problem at once.
func validateCluster(c *ClusterConfig, m *MultiError) {
	if c.Name == "" {
		// Default the cluster name to the cluster ID when unset; this
		// matches the conventional operator workflow (one cluster per
		// pgman-proxy deployment) and keeps the cluster-name guard
		// useful without forcing a duplicate identifier.
		c.Name = c.ID
	}
	if c.DeclaredSize < 1 {
		m.Add(fmt.Errorf("cluster.declared_size must be >= 1, got %d (FR-011a)", c.DeclaredSize))
	}

	// Multi-peer + empty route_peers → fail-closed (FR-008).
	if c.DeclaredSize > 1 && len(c.RoutePeers) == 0 {
		m.Add(errors.New("cluster.route_peers must contain at least one entry when cluster.declared_size > 1 (FR-008)"))
	}

	// Single-peer + non-empty route_peers → warn (handled at startup,
	// not here — validation is fail-closed only).

	// JetStream durable storage required for HA (FR-011).
	if c.DeclaredSize >= 2 && c.JetStreamDir == "" {
		m.Add(errors.New("cluster.jetstream_dir is required when cluster.declared_size >= 2 (FR-011)"))
	}

	// Routes-listener gating (FR-009 + FR-010b).
	// Single-peer (declared_size==1) implicitly disables the routes
	// listener — it has no peers to mesh with. This avoids forcing
	// operators to set cluster.routes_listen.enabled=false explicitly
	// for the single-peer / dev path.
	routesEnabled := c.RoutesListen.Enabled && c.DeclaredSize > 1
	if c.DeclaredSize > 1 && !c.RoutesListen.Enabled {
		m.Add(errors.New("cluster.routes_listen.enabled MUST be true when cluster.declared_size > 1"))
	}
	if routesEnabled {
		routesAddr := fmt.Sprintf("%s:%d", c.RoutesListen.Host, c.RoutesListen.Port)
		isLoopback, err := isLoopbackAddr(routesAddr)
		if err != nil {
			m.Add(fmt.Errorf("cluster.routes_listen %q is not a valid host:port: %w", routesAddr, err))
		} else if !isLoopback {
			// Non-loopback bind: cluster credential required (FR-009).
			if c.Username == "" {
				m.Add(errors.New("cluster.username is required when cluster.routes_listen is non-loopback (FR-009 / RD-001a)"))
			}
			if c.PasswordEnv == "" && c.PasswordFile == "" {
				m.Add(errors.New("cluster.password_env or cluster.password_file is required when cluster.routes_listen is non-loopback (FR-010 / RD-001a)"))
			}
			// FR-010b: TLS material required unless plaintext_explicit_ack=true.
			tlsConfigured := c.TLS.CertFile != "" && c.TLS.KeyFile != ""
			if !tlsConfigured && !c.TLS.PlaintextExplicitAck {
				m.Add(errors.New("cluster-routes plaintext bind on non-loopback rejected without cluster.tls.plaintext_explicit_ack=true (FR-010b)"))
			}
			if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
				m.Add(errors.New("cluster.tls.cert_file and cluster.tls.key_file must both be set or both be empty (FR-010b)"))
			}
		}
	}

	// Replication-factor override audit (FR-011a). Just record that
	// it's set; the actual override-vs-derived warning is emitted at
	// startup so operators see it on every boot.
	// (No fail-closed here.)

	// Mutually exclusive: env vs file for password.
	if c.PasswordEnv != "" && c.PasswordFile != "" {
		m.Add(errors.New("cluster.password_env and cluster.password_file are mutually exclusive (FR-010)"))
	}
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
