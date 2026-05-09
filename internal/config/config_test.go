package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// envFn returns an env(string)string function backed by a static map.
// Used to drive Load deterministically in tests.
func envFn(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func validBaseConfig() Config {
	cfg := Defaults()
	cfg.Cluster.ID = "demo"
	cfg.Node.ID = "node-a"
	cfg.Peers = []string{"node-a", "node-b", "node-c"}
	cfg.NATS.URL = "nats://nats:4222"
	cfg.Proxy.ListenAddr = "0.0.0.0:6432"
	cfg.Postgres.BinDir = "/usr/lib/postgresql/17/bin"
	cfg.Postgres.DataDir = "/var/lib/postgresql/data"
	cfg.Postgres.LocalDSNEnv = "LOCAL_DSN"
	cfg.Control.Auth.TokenEnv = "PGMAN_PROXY_CONTROL_TOKEN"
	return cfg
}

func TestValidate_Happy(t *testing.T) {
	if err := Validate(validBaseConfig()); err != nil {
		t.Fatalf("expected valid config, got: %v", err)
	}
}

func TestValidate_RequiredKeysMissing(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*Config)
		wantParts []string
	}{
		{"cluster.id missing", func(c *Config) { c.Cluster.ID = "" }, []string{"cluster.id is required"}},
		{"node.id missing", func(c *Config) { c.Node.ID = "" }, []string{"node.id is required"}},
		{"peers empty", func(c *Config) { c.Peers = nil }, []string{"peers must contain at least one entry"}},
		{"node.id not in peers", func(c *Config) { c.Peers = []string{"node-x"} }, []string{`peers must contain node.id "node-a"`}},
		{"nats.url missing", func(c *Config) { c.NATS.URL = "" }, []string{"nats.url is required"}},
		{"proxy.listen_addr missing", func(c *Config) { c.Proxy.ListenAddr = "" }, []string{"proxy.listen_addr is required"}},
		{"postgres.bin_dir missing", func(c *Config) { c.Postgres.BinDir = "" }, []string{"postgres.bin_dir is required"}},
		{"postgres.data_dir missing", func(c *Config) { c.Postgres.DataDir = "" }, []string{"postgres.data_dir is required"}},
		{"postgres.local_dsn_env missing", func(c *Config) { c.Postgres.LocalDSNEnv = "" }, []string{"local_dsn_env is required"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validBaseConfig()
			tc.mutate(&cfg)
			err := Validate(cfg)
			if err == nil {
				t.Fatalf("want validation error, got nil")
			}
			for _, want := range tc.wantParts {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing substring %q", err.Error(), want)
				}
			}
		})
	}
}

func TestValidate_TLSDisableRequiresAck(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Postgres.TLSMode = "disable"
	if err := Validate(cfg); err == nil {
		t.Fatal("expected error for tls_mode=disable without ack")
	}
	cfg.Postgres.TLSDisableExplicitAck = true
	if err := Validate(cfg); err != nil {
		t.Fatalf("ack should permit tls_mode=disable; got %v", err)
	}
}

func TestValidate_ControlAuthMutuallyExclusive(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Control.Auth.TokenEnv = "X"
	cfg.Control.Auth.TokenFile = "/tmp/y"
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("want mutually-exclusive error, got %v", err)
	}
}

func TestValidate_ControlAuthMissing(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Control.Auth.TokenEnv = ""
	cfg.Control.Auth.TokenFile = ""
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "no control-plane token source") {
		t.Fatalf("want missing-token error, got %v", err)
	}
}

func TestValidate_LeaderRouteMode(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Control.LeaderRouteMode = "broadcast" // invalid
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "leader_route_mode") {
		t.Fatalf("want enum error, got %v", err)
	}
}

func TestValidate_LeaderRouteTimeout(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Control.LeaderRouteTimeout = 0
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "leader_route_timeout") {
		t.Fatalf("want timeout-too-low error, got %v", err)
	}
	cfg = validBaseConfig()
	cfg.Control.LeaderRouteTimeout = 10 * time.Minute
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "leader_route_timeout") {
		t.Fatalf("want timeout-too-high error, got %v", err)
	}
}

func TestValidate_ControlPlaneTLS_Loopback(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Control.ListenAddr = "127.0.0.1:9091"
	if err := Validate(cfg); err != nil {
		t.Fatalf("loopback plaintext should be permitted, got %v", err)
	}
}

func TestValidate_ControlPlaneTLS_NonLoopbackRequiresTLSorAck(t *testing.T) {
	cfg := validBaseConfig()
	cfg.Control.ListenAddr = "0.0.0.0:9091"
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "plaintext bind on non-loopback") {
		t.Fatalf("want non-loopback-without-TLS error, got %v", err)
	}

	// With explicit ack, plaintext is allowed.
	cfg.Control.TLS.PlaintextExplicitAck = true
	if err := Validate(cfg); err != nil {
		t.Fatalf("plaintext_explicit_ack should permit non-loopback bind, got %v", err)
	}

	// With TLS, plaintext_explicit_ack not needed.
	cfg.Control.TLS.PlaintextExplicitAck = false
	cfg.Control.TLS.CertFile = "/etc/pgman-proxy/tls.crt"
	cfg.Control.TLS.KeyFile = "/etc/pgman-proxy/tls.key"
	if err := Validate(cfg); err != nil {
		t.Fatalf("TLS configured should permit non-loopback bind, got %v", err)
	}

	// Half-set TLS is rejected.
	cfg.Control.TLS.KeyFile = ""
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "must both be set") {
		t.Fatalf("want half-set TLS error, got %v", err)
	}
}

func TestLoad_EnvOverridesYAML(t *testing.T) {
	yaml := `
cluster: { id: from-yaml }
node: { id: node-a }
peers: [node-a]
nats: { url: nats://from-yaml:4222 }
proxy: { listen_addr: 0.0.0.0:6432 }
postgres:
  bin_dir: /usr/lib/postgresql/17/bin
  data_dir: /data
  local_dsn_env: LOCAL_DSN
control:
  auth: { token_env: TOK }
`
	dir := t.TempDir()
	yamlPath := dir + "/c.yaml"
	if err := writeFile(yamlPath, yaml); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	cfg, _, err := Load(LoadOptions{
		YAMLPath: yamlPath,
		Env: envFn(map[string]string{
			"PGMAN_PROXY_CLUSTER_ID": "from-env",
		}),
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Cluster.ID != "from-env" {
		t.Errorf("env should win over yaml; got %q", cfg.Cluster.ID)
	}
	if cfg.NATS.URL != "nats://from-yaml:4222" {
		t.Errorf("yaml value should survive when env absent; got %q", cfg.NATS.URL)
	}
}

func TestLoad_BackwardCompatAliases(t *testing.T) {
	cfg, src, err := Load(LoadOptions{
		Env: envFn(map[string]string{
			"NATS_URL":     "nats://alias:4222",
			"CLUSTER_ID":   "alias-cluster",
			"NODE_ID":      "node-a",
			"PEERS":        "node-a,node-b",
			"PROXY_LISTEN": "0.0.0.0:6432",
		}),
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NATS.URL != "nats://alias:4222" {
		t.Errorf("NATS_URL alias not honoured; got %q", cfg.NATS.URL)
	}
	if cfg.Cluster.ID != "alias-cluster" {
		t.Errorf("CLUSTER_ID alias not honoured; got %q", cfg.Cluster.ID)
	}
	// Source should record the alias usage.
	found := false
	for _, s := range src.EnvPresent {
		if strings.Contains(s, "NATS_URL") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Sources should record NATS_URL alias; got %v", src.EnvPresent)
	}
}

func TestLoad_FlagsOverrideEnv(t *testing.T) {
	cfg, _, err := Load(LoadOptions{
		Env:   envFn(map[string]string{"PGMAN_PROXY_NATS_URL": "nats://env:4222"}),
		Flags: map[string]string{"nats": "nats://flag:4222"},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NATS.URL != "nats://flag:4222" {
		t.Errorf("flag should win over env; got %q", cfg.NATS.URL)
	}
}

func TestLoad_UnknownFlag(t *testing.T) {
	_, _, err := Load(LoadOptions{
		Flags: map[string]string{"this-is-not-a-flag": "x"},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("want unknown-flag error, got %v", err)
	}
}

// TestApplyModeDefaults_Sidecar covers US2 / FR-013: sidecar mode
// rewrites all-interfaces binds to loopback so off-host clients can't
// reach the proxy or observability surface.
func TestApplyModeDefaults_Sidecar(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"v4_explicit_zero", "0.0.0.0:6432", "127.0.0.1:6432"},
		{"v6_explicit_zero", "[::]:6432", "127.0.0.1:6432"},
		{"bare_colon_port", ":9090", "127.0.0.1:9090"},
		{"already_loopback", "127.0.0.1:6432", "127.0.0.1:6432"},
		{"hostname_pinned", "db.internal:6432", "db.internal:6432"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.DeploymentMode = DeploymentModeSidecar
			cfg.Proxy.ListenAddr = tc.in
			got := ApplyModeDefaults(cfg).Proxy.ListenAddr
			if got != tc.want {
				t.Errorf("Proxy.ListenAddr: got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestApplyModeDefaults_NonSidecarUntouched: standalone and microservice
// modes leave addresses alone — only sidecar rewrites.
func TestApplyModeDefaults_NonSidecarUntouched(t *testing.T) {
	for _, mode := range []string{DeploymentModeStandalone, DeploymentModeMicroservice} {
		t.Run(mode, func(t *testing.T) {
			cfg := Defaults()
			cfg.DeploymentMode = mode
			cfg.Proxy.ListenAddr = "0.0.0.0:6432"
			cfg.Obs.HealthAddr = ":9090"
			out := ApplyModeDefaults(cfg)
			if out.Proxy.ListenAddr != "0.0.0.0:6432" || out.Obs.HealthAddr != ":9090" {
				t.Errorf("non-sidecar should not rewrite, got Proxy=%q Obs=%q",
					out.Proxy.ListenAddr, out.Obs.HealthAddr)
			}
		})
	}
}

// TestValidate_DeploymentMode rejects unknown modes (FR-013).
func TestValidate_DeploymentMode(t *testing.T) {
	cfg := validBaseConfig()
	cfg.DeploymentMode = "kubernetes-operator"
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "deployment_mode") {
		t.Fatalf("want deployment_mode enum error, got %v", err)
	}
}

func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}
