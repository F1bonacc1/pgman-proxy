// Package config holds the validated, fully-resolved configuration for one
// pgman-proxy peer. Layered loading is in loader.go (flags > env > YAML >
// defaults); cross-field validation is in validate.go.
//
// The struct shape mirrors specs/001-active-active-pg-proxy/contracts/config.md
// and data-model.md § Entity: ProxyConfig. Field renames here are MINOR-version
// events for the project (Constitution V).
package config

import "time"

// Deployment-mode tags. The tag drives mode-aware listener defaults
// (US2 / FR-013): sidecar binds loopback so off-host clients can't
// reach the proxy or observability surface; standalone and
// microservice bind all-interfaces.
const (
	DeploymentModeStandalone   = "standalone"
	DeploymentModeMicroservice = "microservice"
	DeploymentModeSidecar      = "sidecar"
)

// Config is the fully-resolved configuration for one peer.
type Config struct {
	// DeploymentMode selects mode-aware listener defaults; one of
	// "standalone", "microservice", "sidecar" (FR-013, US2). When the
	// operator leaves Proxy.ListenAddr / Obs.HealthAddr / Control.ListenAddr
	// at their defaults, sidecar mode rewrites all-interfaces binds
	// (0.0.0.0:* and bare :*) to loopback. Default: "standalone".
	DeploymentMode string         `yaml:"deployment_mode" json:"deployment_mode"`
	Cluster        ClusterConfig  `yaml:"cluster"         json:"cluster"`
	Node           NodeConfig     `yaml:"node"            json:"node"`
	Peers          []string       `yaml:"peers"           json:"peers"`
	NATS           NATSConfig     `yaml:"nats"            json:"nats"`
	Proxy          ProxyConfig    `yaml:"proxy"           json:"proxy"`
	Postgres       PostgresConfig `yaml:"postgres"        json:"postgres"`
	Topology       TopologyConfig `yaml:"topology"        json:"topology"`
	Policy         PolicyConfig   `yaml:"policy"          json:"policy"`
	Obs            ObsConfig      `yaml:"obs"             json:"obs"`
	Control        ControlConfig  `yaml:"control"         json:"control"`
	Shutdown       ShutdownConfig `yaml:"shutdown"        json:"shutdown"`
}

// ClusterConfig identifies the cluster this peer joins, AND (per
// feature 002) carries the embedded-NATS coordination-plane settings
// that replace 001's external-NATS dependency. The shape mirrors
// `specs/002-embedded-nats-cluster/contracts/config.md`.
type ClusterConfig struct {
	// 001 fields.
	ID string `yaml:"id" json:"id"`

	// Embedded-NATS fields (feature 002).
	Name                      string          `yaml:"name"                        json:"name"`
	DeclaredSize              int             `yaml:"declared_size"               json:"declared_size"`
	ClientListen              EndpointConfig  `yaml:"client_listen"               json:"client_listen"`
	RoutesListen              RoutesListenCfg `yaml:"routes_listen"              json:"routes_listen"`
	RoutePeers                []string        `yaml:"route_peers"                 json:"route_peers"`
	TLS                       ClusterTLSCfg   `yaml:"tls"                         json:"tls"`
	Username                  string          `yaml:"username"                    json:"username"`
	PasswordEnv               string          `yaml:"password_env"                json:"password_env"`
	PasswordFile              string          `yaml:"password_file"               json:"password_file"`
	JetStreamDir              string          `yaml:"jetstream_dir"               json:"jetstream_dir"`
	ReplicationFactorOverride int             `yaml:"replication_factor_override" json:"replication_factor_override"`
}

// EndpointConfig is a host/port pair used for the embedded-NATS
// listeners (FR-018, FR-019).
type EndpointConfig struct {
	Host string `yaml:"host" json:"host"`
	Port int    `yaml:"port" json:"port"`
}

// RoutesListenCfg extends EndpointConfig with the explicit
// "enabled" flag so single-peer deployments can disable the routes
// listener entirely without leaving a hanging port.
type RoutesListenCfg struct {
	Host    string `yaml:"host"    json:"host"`
	Port    int    `yaml:"port"    json:"port"`
	Enabled bool   `yaml:"enabled" json:"enabled"`
}

// ClusterTLSCfg carries cluster-route TLS material (FR-010b).
type ClusterTLSCfg struct {
	CertFile             string `yaml:"cert_file"              json:"cert_file"`
	KeyFile              string `yaml:"key_file"               json:"key_file"`
	CAFile               string `yaml:"ca_file"                json:"ca_file"`
	PlaintextExplicitAck bool   `yaml:"plaintext_explicit_ack" json:"plaintext_explicit_ack"`
}

// NodeConfig identifies this peer.
type NodeConfig struct {
	ID string `yaml:"id" json:"id"`
}

// NATSConfig configures the connection to the NATS coordination plane.
type NATSConfig struct {
	URL            string        `yaml:"url"             json:"url"`
	ConnectTimeout time.Duration `yaml:"connect_timeout" json:"connect_timeout"`
	ReconnectWait  time.Duration `yaml:"reconnect_wait"  json:"reconnect_wait"`
	MaxReconnects  int           `yaml:"max_reconnects"  json:"max_reconnects"`
	CredsFile      string        `yaml:"creds_file"      json:"creds_file"`
	TokenEnv       string        `yaml:"token_env"       json:"token_env"`
}

// ProxyConfig governs the data-plane PostgreSQL listener.
type ProxyConfig struct {
	ListenAddr   string        `yaml:"listen_addr"   json:"listen_addr"`
	DialTimeout  time.Duration `yaml:"dial_timeout"  json:"dial_timeout"`
	SwitchPolicy string        `yaml:"switch_policy" json:"switch_policy"` // hard_close | drain | pause
}

// PostgresConfig governs how this peer talks to its local PostgreSQL.
type PostgresConfig struct {
	BinDir                string            `yaml:"bin_dir"                  json:"bin_dir"`
	DataDir               string            `yaml:"data_dir"                 json:"data_dir"`
	Port                  int               `yaml:"port"                     json:"port"`
	LocalDSNEnv           string            `yaml:"local_dsn_env"            json:"local_dsn_env"`
	// ReplicationAddr is the host:port that OTHER peers use to reach
	// this node's local Postgres for replication (pg_basebackup,
	// walsender). Distinct from LocalDSNEnv: in K8s the local view
	// (127.0.0.1) is not reachable from peers. Required when more than
	// one peer is configured. Published to the cluster KV at startup so
	// pg-manager's PeerDSNResolver can hand it to followers seeding from
	// this node.
	ReplicationAddr       string            `yaml:"replication_addr"         json:"replication_addr"`
	TLSMode               string            `yaml:"tls_mode"                 json:"tls_mode"`
	TLSDisableExplicitAck bool              `yaml:"tls_disable_explicit_ack" json:"tls_disable_explicit_ack"`
	PeerDSNs              map[string]string `yaml:"peer_dsns"                json:"peer_dsns"`
	// HBAExtras lists pg_hba.conf lines appended after initdb on the
	// elected bootstrap leader. Followers inherit the rewritten file via
	// pg_basebackup, so this field is only consulted on whichever peer
	// wins the bootstrap election.
	//
	// REQUIRED FOR REPLICATION: without an entry permitting replication
	// connections from peer hosts (e.g. `host replication all <peer-cidr>
	// scram-sha-256`), pg_basebackup will be refused with "no pg_hba.conf
	// entry for replication connection". The library will not synthesise
	// rules on its own — operators MUST supply the policy.
	HBAExtras []string `yaml:"hba_extras"  json:"hba_extras"`
	// ConfExtras lists postgresql.conf lines appended after initdb on
	// the elected bootstrap leader. Same scope as HBAExtras.
	ConfExtras []string `yaml:"conf_extras" json:"conf_extras"`
}

// TopologyConfig tunes the cluster topology view (carries through to
// pg-manager's pgmanager.Topology).
type TopologyConfig struct {
	Port int `yaml:"port" json:"port"` // upstream PostgreSQL port; default 5432
}

// PolicyConfig carries pg-manager's Policy fields. Exposed here because
// operators routinely tune timeouts, but the engine logic stays upstream.
type PolicyConfig struct {
	FailoverDelay    time.Duration   `yaml:"failover_delay"    json:"failover_delay"`
	SwitchoverDelay  time.Duration   `yaml:"switchover_delay"  json:"switchover_delay"`
	PromoteTimeout   time.Duration   `yaml:"promote_timeout"   json:"promote_timeout"`
	LivenessInterval time.Duration   `yaml:"liveness_interval" json:"liveness_interval"`
	LivenessFailures int             `yaml:"liveness_failures" json:"liveness_failures"`
	QuorumSync       QuorumSyncCfg   `yaml:"quorum_sync"       json:"quorum_sync"`
	AutoRebootstrap  AutoRecoveryCfg `yaml:"auto_rebootstrap"  json:"auto_rebootstrap"`
	AutoDemote       AutoRecoveryCfg `yaml:"auto_demote"       json:"auto_demote"`
}

// QuorumSyncCfg mirrors pgmanager.QuorumSync.
type QuorumSyncCfg struct {
	MinSync int `yaml:"min_sync" json:"min_sync"`
}

// AutoRecoveryCfg toggles a single auto-recovery loop.
type AutoRecoveryCfg struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
}

// ObsConfig drives logger/metrics/health/tracer wiring.
type ObsConfig struct {
	LogLevel    string  `yaml:"log_level"    json:"log_level"`
	MetricsAddr string  `yaml:"metrics_addr" json:"metrics_addr"`
	HealthAddr  string  `yaml:"health_addr"  json:"health_addr"`
	OTel        OTelCfg `yaml:"otel"         json:"otel"`
}

// OTelCfg configures the OTLP exporter; empty endpoint = noop tracer.
type OTelCfg struct {
	Endpoint string `yaml:"endpoint" json:"endpoint"`
}

// ControlConfig governs the LCM control plane (FR-021..FR-034).
type ControlConfig struct {
	ListenAddr         string        `yaml:"listen_addr"          json:"listen_addr"`
	LeaderRouteMode    string        `yaml:"leader_route_mode"    json:"leader_route_mode"`    // forward | redirect
	LeaderRouteTimeout time.Duration `yaml:"leader_route_timeout" json:"leader_route_timeout"` // FR-034
	Auth               ControlAuth   `yaml:"auth"                 json:"auth"`
	TLS                ControlTLS    `yaml:"tls"                  json:"tls"`
}

// ControlAuth carries the bearer-token source.
type ControlAuth struct {
	TokenEnv         string `yaml:"token_env"          json:"token_env"`
	TokenFile        string `yaml:"token_file"         json:"token_file"`
	AllowUnauthReads bool   `yaml:"allow_unauth_reads" json:"allow_unauth_reads"`
}

// ControlTLS carries control-plane TLS material (FR-033).
type ControlTLS struct {
	CertFile             string `yaml:"cert_file"              json:"cert_file"`
	KeyFile              string `yaml:"key_file"               json:"key_file"`
	PlaintextExplicitAck bool   `yaml:"plaintext_explicit_ack" json:"plaintext_explicit_ack"`
}

// ShutdownConfig bounds graceful shutdown.
type ShutdownConfig struct {
	DrainBudget time.Duration `yaml:"drain_budget" json:"drain_budget"`
}

// Defaults returns a Config populated with the documented defaults from
// contracts/config.md. Required keys remain zero — validation surfaces
// missing values.
func Defaults() Config {
	return Config{
		DeploymentMode: DeploymentModeStandalone,
		Cluster: ClusterConfig{
			// Embedded-NATS defaults (feature 002 /
			// specs/002-embedded-nats-cluster/contracts/config.md):
			ClientListen: EndpointConfig{
				Host: "127.0.0.1",
				Port: 4222,
			},
			RoutesListen: RoutesListenCfg{
				Host:    "0.0.0.0",
				Port:    6222,
				Enabled: true,
			},
		},
		NATS: NATSConfig{
			ConnectTimeout: 10 * time.Second,
			ReconnectWait:  2 * time.Second,
			MaxReconnects:  -1,
		},
		Proxy: ProxyConfig{
			DialTimeout:  5 * time.Second,
			SwitchPolicy: "hard_close",
		},
		Postgres: PostgresConfig{
			Port:    5432,
			TLSMode: "verify-full",
		},
		Topology: TopologyConfig{Port: 5432},
		Policy: PolicyConfig{
			FailoverDelay:    30 * time.Second,
			SwitchoverDelay:  30 * time.Second,
			PromoteTimeout:   60 * time.Second,
			LivenessInterval: 5 * time.Second,
			LivenessFailures: 3,
			QuorumSync:       QuorumSyncCfg{MinSync: 1},
		},
		Obs: ObsConfig{
			LogLevel:    "info",
			MetricsAddr: ":9090",
			HealthAddr:  ":9090",
		},
		Control: ControlConfig{
			LeaderRouteMode:    "forward",
			LeaderRouteTimeout: 30 * time.Second,
		},
		Shutdown: ShutdownConfig{DrainBudget: 30 * time.Second},
	}
}
