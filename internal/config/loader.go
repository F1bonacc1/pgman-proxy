package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Sources records which layer supplied each populated value. Useful for
// the "config_loaded" structured log event (contracts/observability.md).
type Sources struct {
	YAMLPath   string
	EnvPresent []string
	FlagsSet   []string
}

// LoadOptions carries the inputs to the layered loader.
type LoadOptions struct {
	YAMLPath string // empty = no file
	Env      func(string) string
	Flags    map[string]string // already-parsed CLI overrides; nil OK
}

// Load merges the three configuration layers into a fully-resolved Config.
// Precedence: Flags > Env > YAML > Defaults. The returned Sources records
// where each populated value came from for audit logging.
func Load(opts LoadOptions) (Config, Sources, error) {
	if opts.Env == nil {
		opts.Env = os.Getenv
	}
	cfg := Defaults()
	src := Sources{}

	if opts.YAMLPath != "" {
		raw, err := os.ReadFile(opts.YAMLPath)
		if err != nil {
			return cfg, src, fmt.Errorf("read config %q: %w", opts.YAMLPath, err)
		}
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return cfg, src, fmt.Errorf("parse yaml %q: %w", opts.YAMLPath, err)
		}
		src.YAMLPath = opts.YAMLPath
	}

	if err := applyEnv(&cfg, &src, opts.Env); err != nil {
		return cfg, src, err
	}
	if err := applyFlags(&cfg, &src, opts.Flags); err != nil {
		return cfg, src, err
	}
	cfg = ApplyModeDefaults(cfg)
	return cfg, src, nil
}

// applyEnv overlays canonical PGMAN_PROXY_* env vars and the documented
// backward-compatible aliases (NATS_URL, CLUSTER_ID, NODE_ID, PEERS,
// PGDATA, PG_BINDIR, LOCAL_DSN, PROXY_LISTEN). Canonical wins on collision;
// the alias's presence is logged at startup so operators can audit.
func applyEnv(cfg *Config, src *Sources, env func(string) string) error {
	// Two-table dispatch: passthrough setters (cannot fail) vs parsed
	// setters (can return parse errors). Splitting here lets the linter
	// see that the passthrough path is total.
	passthrough := map[string]func(string){
		"PGMAN_PROXY_DEPLOYMENT_MODE":           func(v string) { cfg.DeploymentMode = v },
		"PGMAN_PROXY_CLUSTER_ID":                func(v string) { cfg.Cluster.ID = v },
		"PGMAN_PROXY_CLUSTER_NAME":              func(v string) { cfg.Cluster.Name = v },
		"PGMAN_PROXY_CLUSTER_USERNAME":          func(v string) { cfg.Cluster.Username = v },
		"PGMAN_PROXY_CLUSTER_PASSWORD_ENV":      func(v string) { cfg.Cluster.PasswordEnv = v },
		"PGMAN_PROXY_CLUSTER_PASSWORD_FILE":     func(v string) { cfg.Cluster.PasswordFile = v },
		"PGMAN_PROXY_CLUSTER_JETSTREAM_DIR":     func(v string) { cfg.Cluster.JetStreamDir = v },
		"PGMAN_PROXY_CLUSTER_TLS_CERT_FILE":     func(v string) { cfg.Cluster.TLS.CertFile = v },
		"PGMAN_PROXY_CLUSTER_TLS_KEY_FILE":      func(v string) { cfg.Cluster.TLS.KeyFile = v },
		"PGMAN_PROXY_CLUSTER_TLS_CA_FILE":       func(v string) { cfg.Cluster.TLS.CAFile = v },
		"PGMAN_PROXY_CLUSTER_ROUTE_PEERS":       func(v string) { cfg.Cluster.RoutePeers = parseCSV(v) },
		"PGMAN_PROXY_NODE_ID":                   func(v string) { cfg.Node.ID = v },
		"PGMAN_PROXY_PEERS":                     func(v string) { cfg.Peers = parseCSV(v) },
		"PGMAN_PROXY_PROXY_LISTEN_ADDR":         func(v string) { cfg.Proxy.ListenAddr = v },
		"PGMAN_PROXY_PROXY_SWITCH_POLICY":       func(v string) { cfg.Proxy.SwitchPolicy = v },
		"PGMAN_PROXY_POSTGRES_BIN_DIR":          func(v string) { cfg.Postgres.BinDir = v },
		"PGMAN_PROXY_POSTGRES_DATA_DIR":         func(v string) { cfg.Postgres.DataDir = v },
		"PGMAN_PROXY_POSTGRES_LOCAL_DSN_ENV":    func(v string) { cfg.Postgres.LocalDSNEnv = v },
		"PGMAN_PROXY_POSTGRES_TLS_MODE":         func(v string) { cfg.Postgres.TLSMode = v },
		"PGMAN_PROXY_POSTGRES_HBA_EXTRAS":       func(v string) { cfg.Postgres.HBAExtras = parseLines(v) },
		"PGMAN_PROXY_POSTGRES_CONF_EXTRAS":      func(v string) { cfg.Postgres.ConfExtras = parseLines(v) },
		"PGMAN_PROXY_OBS_LOG_LEVEL":             func(v string) { cfg.Obs.LogLevel = v },
		"PGMAN_PROXY_OBS_METRICS_ADDR":          func(v string) { cfg.Obs.MetricsAddr = v },
		"PGMAN_PROXY_OBS_HEALTH_ADDR":           func(v string) { cfg.Obs.HealthAddr = v },
		"PGMAN_PROXY_CONTROL_LISTEN_ADDR":       func(v string) { cfg.Control.ListenAddr = v },
		"PGMAN_PROXY_CONTROL_LEADER_ROUTE_MODE": func(v string) { cfg.Control.LeaderRouteMode = v },
		"PGMAN_PROXY_CONTROL_AUTH_TOKEN_ENV":    func(v string) { cfg.Control.Auth.TokenEnv = v },
		"PGMAN_PROXY_CONTROL_AUTH_TOKEN_FILE":   func(v string) { cfg.Control.Auth.TokenFile = v },
		"PGMAN_PROXY_CONTROL_TLS_CERT_FILE":     func(v string) { cfg.Control.TLS.CertFile = v },
		"PGMAN_PROXY_CONTROL_TLS_KEY_FILE":      func(v string) { cfg.Control.TLS.KeyFile = v },
	}
	parsed := map[string]func(string) error{
		"PGMAN_PROXY_CLUSTER_DECLARED_SIZE":               intSet(&cfg.Cluster.DeclaredSize),
		"PGMAN_PROXY_CLUSTER_CLIENT_LISTEN_PORT":          intSet(&cfg.Cluster.ClientListen.Port),
		"PGMAN_PROXY_CLUSTER_ROUTES_LISTEN_PORT":          intSet(&cfg.Cluster.RoutesListen.Port),
		"PGMAN_PROXY_CLUSTER_REPLICATION_FACTOR_OVERRIDE": intSet(&cfg.Cluster.ReplicationFactorOverride),
		"PGMAN_PROXY_CLUSTER_TLS_PLAINTEXT_EXPLICIT_ACK":  boolSet(&cfg.Cluster.TLS.PlaintextExplicitAck),
		// NATS timing knobs are retained for the loopback dial; the
		// URL/CredsFile/TokenEnv fields are no longer accepted from
		// any source (validation rejects them with a migration error).
		"PGMAN_PROXY_NATS_CONNECT_TIMEOUT":               durSet(&cfg.NATS.ConnectTimeout),
		"PGMAN_PROXY_NATS_RECONNECT_WAIT":                durSet(&cfg.NATS.ReconnectWait),
		"PGMAN_PROXY_PROXY_DIAL_TIMEOUT":                 durSet(&cfg.Proxy.DialTimeout),
		"PGMAN_PROXY_POSTGRES_PORT":                      intSet(&cfg.Postgres.Port),
		"PGMAN_PROXY_POSTGRES_TLS_DISABLE_EXPLICIT_ACK":  boolSet(&cfg.Postgres.TLSDisableExplicitAck),
		"PGMAN_PROXY_CONTROL_LEADER_ROUTE_TIMEOUT":       durSet(&cfg.Control.LeaderRouteTimeout),
		"PGMAN_PROXY_CONTROL_AUTH_ALLOW_UNAUTH_READS":    boolSet(&cfg.Control.Auth.AllowUnauthReads),
		"PGMAN_PROXY_CONTROL_TLS_PLAINTEXT_EXPLICIT_ACK": boolSet(&cfg.Control.TLS.PlaintextExplicitAck),
		"PGMAN_PROXY_SHUTDOWN_DRAIN_BUDGET":              durSet(&cfg.Shutdown.DrainBudget),
	}

	for k, set := range passthrough {
		if v := env(k); v != "" {
			set(v)
			src.EnvPresent = append(src.EnvPresent, k)
		}
	}
	for k, set := range parsed {
		if v := env(k); v != "" {
			if err := set(v); err != nil {
				return fmt.Errorf("env %s: %w", k, err)
			}
			src.EnvPresent = append(src.EnvPresent, k)
		}
	}

	// Backward-compatible aliases (per contracts/config.md). Canonical wins.
	// Feature 002: NATS_URL alias removed — external NATS is no longer
	// supported. Setting NATS_URL no longer routes anywhere; if a legacy
	// deployment script exports it, validation will not see it (the
	// config never resolves it) so the legacy YAML key is the only way
	// the migration error fires.
	aliases := []struct {
		envKey string
		canon  string
		set    func(string)
	}{
		{"CLUSTER_ID", "PGMAN_PROXY_CLUSTER_ID", func(v string) { cfg.Cluster.ID = v }},
		{"NODE_ID", "PGMAN_PROXY_NODE_ID", func(v string) { cfg.Node.ID = v }},
		{"PEERS", "PGMAN_PROXY_PEERS", func(v string) { cfg.Peers = parseCSV(v) }},
		{"PGDATA", "PGMAN_PROXY_POSTGRES_DATA_DIR", func(v string) { cfg.Postgres.DataDir = v }},
		{"PG_BINDIR", "PGMAN_PROXY_POSTGRES_BIN_DIR", func(v string) { cfg.Postgres.BinDir = v }},
		{"PROXY_LISTEN", "PGMAN_PROXY_PROXY_LISTEN_ADDR", func(v string) { cfg.Proxy.ListenAddr = v }},
	}
	for _, a := range aliases {
		v := env(a.envKey)
		if v == "" {
			continue
		}
		if env(a.canon) != "" {
			// Canonical already won; record the alias for audit logging
			// but don't overwrite.
			src.EnvPresent = append(src.EnvPresent, a.envKey+" (alias, canonical wins)")
			continue
		}
		a.set(v)
		src.EnvPresent = append(src.EnvPresent, a.envKey+" (alias)")
	}
	return nil
}

// applyFlags overlays parsed CLI flags. Keys are short flag names from
// contracts/cli.md (cluster-id, node-id, peers, nats, listen, switch-policy,
// log-level, metrics). All known flag setters are passthrough (no parse
// errors at this layer); unknown flags produce a config error.
func applyFlags(cfg *Config, src *Sources, flags map[string]string) error {
	if len(flags) == 0 {
		return nil
	}
	bind := map[string]func(string){
		"cluster-id":   func(v string) { cfg.Cluster.ID = v },
		"cluster-name": func(v string) { cfg.Cluster.Name = v },
		"node-id":      func(v string) { cfg.Node.ID = v },
		"peers":        func(v string) { cfg.Peers = parseCSV(v) },
		// Feature 002: --nats flag removed (external NATS retired).
		"listen":        func(v string) { cfg.Proxy.ListenAddr = v },
		"switch-policy": func(v string) { cfg.Proxy.SwitchPolicy = v },
		"log-level":     func(v string) { cfg.Obs.LogLevel = v },
		"metrics":       func(v string) { cfg.Obs.MetricsAddr = v },
	}
	for k, v := range flags {
		set, ok := bind[k]
		if !ok {
			return fmt.Errorf("unknown flag --%s", k)
		}
		set(v)
		src.FlagsSet = append(src.FlagsSet, k)
	}
	return nil
}

func parseCSV(v string) []string {
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseLines splits on '\n' and the literal escape sequence '\n', so
// operators can supply multi-line pg_hba / postgresql.conf snippets via
// either a YAML block scalar or a single-line env variable.
func parseLines(v string) []string {
	if v == "" {
		return nil
	}
	v = strings.ReplaceAll(v, `\n`, "\n")
	parts := strings.Split(v, "\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimRight(p, "\r ")
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func durSet(dst *time.Duration) func(string) error {
	return func(v string) error {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", v, err)
		}
		if d < 0 {
			return errors.New("duration must be non-negative")
		}
		*dst = d
		return nil
	}
}

func intSet(dst *int) func(string) error {
	return func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("invalid integer %q: %w", v, err)
		}
		*dst = n
		return nil
	}
}

func boolSet(dst *bool) func(string) error {
	return func(v string) error {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("invalid bool %q: %w", v, err)
		}
		*dst = b
		return nil
	}
}
