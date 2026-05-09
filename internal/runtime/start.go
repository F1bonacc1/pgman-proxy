package runtime

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	pgmanager "github.com/f1bonacc1/pg-manager"
	"github.com/f1bonacc1/pg-manager/adapters/osfs"
	"github.com/f1bonacc1/pg-manager/manager"
	"github.com/nats-io/nats.go"

	"github.com/f1bonacc1/pgman-proxy/internal/cluster"
	"github.com/f1bonacc1/pgman-proxy/internal/config"
	"github.com/f1bonacc1/pgman-proxy/internal/control"
	"github.com/f1bonacc1/pgman-proxy/internal/obs"
)

// StartupResult bundles everything constructed during the startup gate
// sequence so the caller (cmd/pgman-proxy/main.go) can drive shutdown
// in reverse order.
type StartupResult struct {
	Logger   *obs.Logger
	Metrics  *obs.MetricSet
	Health   *obs.Health
	ObsSrv   *obs.Server
	Cluster  *cluster.Handles
	Manager  *manager.Manager
	Conn     *nats.Conn
	EventSub []*nats.Subscription
	Control  *control.Server
}

// StartupError carries the documented exit code so the caller can map
// failures to the right process exit (contracts/lifecycle.md).
type StartupError struct {
	Code int
	Err  error
}

// Error implements error.
func (e *StartupError) Error() string {
	return fmt.Sprintf("startup failed at gate (%s): %v", ExitName(e.Code), e.Err)
}

// Unwrap returns the underlying error so errors.Is/As traversal works.
func (e *StartupError) Unwrap() error { return e.Err }

// Start runs the documented 11-step startup gate sequence
// (contracts/lifecycle.md § Startup sequence). Failure at any gate
// returns a StartupError carrying the matching exit code; the caller
// is responsible for tearing down whatever was already constructed
// (StartupResult fields are populated incrementally).
//
// Note: gate #10 (control-plane bind) and #11 (initial LCM-audit emit)
// will be wired by the US4 phase; this function returns the populated
// StartupResult through gate #9 today and the LCM phase will extend it.
func Start(ctx context.Context, cfg config.Config, version string) (*StartupResult, *StartupError) {
	res := &StartupResult{}

	// Gate #1: Argument parsing — caller already did this.
	// Gate #2: Configuration load + validate — caller already did this.

	// Gate #3: Observability bootstrap.
	res.Logger = obs.NewLogger(nil, cfg.Obs.LogLevel, cfg.Cluster.ID, cfg.Node.ID, "runtime")
	res.Logger.Info("config loaded", pgmanager.Field{Key: "version", Value: version})
	res.Metrics = obs.NewMetrics(cfg.Cluster.ID, cfg.Node.ID)
	res.Health = obs.NewHealth()
	res.ObsSrv = obs.NewServer(cfg.Obs.HealthAddr, res.Health, res.Metrics.Registry)

	// Probe the obs listener up-front so we fail fast if the port is busy.
	if err := probeBind(cfg.Obs.HealthAddr); err != nil {
		return res, &StartupError{Code: ExitObs, Err: fmt.Errorf("obs bind %s: %w", cfg.Obs.HealthAddr, err)}
	}
	go func() {
		if err := res.ObsSrv.Start(ctx); err != nil {
			res.Logger.Error("obs server stopped with error", pgmanager.Field{Key: "error", Value: err.Error()})
		}
	}()

	// Gate #4: NATS connect.
	conn, err := cluster.Connect(ctx, cfg.NATS, cfg.Node.ID, res.Logger)
	if err != nil {
		return res, &StartupError{Code: ExitDeps, Err: err}
	}
	res.Conn = conn
	res.Health.SetNATSUp(true)

	// Gate #5: NATS adapters.
	handles, err := cluster.BuildHandles(ctx, conn, cfg.Cluster.ID, cfg.Node.ID, res.Logger)
	if err != nil {
		return res, &StartupError{Code: ExitDeps, Err: err}
	}
	res.Cluster = handles

	// Subscribe to pg-manager coordination events (FR-006).
	subs, err := cluster.SubscribeCoordinationEvents(conn, cfg.Cluster.ID, res.Logger, res.Metrics)
	if err != nil {
		return res, &StartupError{Code: ExitDeps, Err: err}
	}
	res.EventSub = subs

	// Gate #6: Postgres executor.
	pg, err := manager.RealPostgresExecutor(manager.PostgresOptions{
		BinDir:   cfg.Postgres.BinDir,
		LocalDSN: localDSNFromConfig(cfg),
	})
	if err != nil {
		return res, &StartupError{Code: ExitDeps, Err: fmt.Errorf("postgres executor: %w", err)}
	}

	// Gate #7: Manager constructed.
	mgrCfg := manager.Config{
		Leadership: handles.Leadership,
		State:      handles.State,
		Postgres:   pg,
		Logger:     res.Logger,
		Events:     handles.Bus,
		FS:         osfs.New(),
		Topology: pgmanager.Topology{
			NodeID:   pgmanager.NodeID(cfg.Node.ID),
			Peers:    nodeIDs(cfg.Peers),
			DataDir:  cfg.Postgres.DataDir,
			BinDir:   cfg.Postgres.BinDir,
			Port:     cfg.Topology.Port,
			PeerDSNs: peerDSNsForConfig(cfg),
		},
		Policy: pgmanager.Policy{
			FailoverDelay:    cfg.Policy.FailoverDelay,
			SwitchoverDelay:  cfg.Policy.SwitchoverDelay,
			PromoteTimeout:   cfg.Policy.PromoteTimeout,
			LivenessInterval: cfg.Policy.LivenessInterval,
			LivenessFailures: cfg.Policy.LivenessFailures,
			Replication: pgmanager.QuorumSync{
				MinSync: cfg.Policy.QuorumSync.MinSync,
				Pool:    nodeIDs(cfg.Peers),
			},
		},
		ClusterID:            cfg.Cluster.ID,
		AutoApplyConfChanges: true,
		PostInitDB:           postInitDBHook(cfg, res.Logger),
	}
	if cfg.Proxy.ListenAddr != "" {
		mgrCfg.Proxy = &pgmanager.ProxyConfig{
			ListenAddr:     cfg.Proxy.ListenAddr,
			DialTimeout:    cfg.Proxy.DialTimeout,
			OnSwitchPolicy: parseSwitchPolicy(cfg.Proxy.SwitchPolicy),
		}
	}
	m, err := manager.New(mgrCfg)
	if err != nil {
		return res, &StartupError{Code: ExitDeps, Err: fmt.Errorf("manager.New: %w", err)}
	}
	res.Manager = m

	// Gate #8: Data-plane listener bind probe — best-effort up-front so
	// startup fails on EADDRINUSE before pg-manager kicks off background
	// goroutines.
	if cfg.Proxy.ListenAddr != "" {
		if err := probeBind(cfg.Proxy.ListenAddr); err != nil {
			return res, &StartupError{Code: ExitListen, Err: fmt.Errorf("proxy listen %s: %w", cfg.Proxy.ListenAddr, err)}
		}
		res.Health.SetListenerUp(true)
	}

	// Gate #9: Singleton claim resolved — handled inside manager.Start.

	// Gate #10: Control-plane bind (FR-021..FR-034).
	if cfg.Control.ListenAddr != "" {
		if err := probeBind(cfg.Control.ListenAddr); err != nil {
			return res, &StartupError{Code: ExitControl, Err: fmt.Errorf("control plane listen %s: %w", cfg.Control.ListenAddr, err)}
		}
		auth := control.NewAuthenticator(cfg.Control.Auth.TokenEnv, cfg.Control.Auth.TokenFile, cfg.Control.Auth.AllowUnauthReads)
		audit := control.NewAudit(cfg.Cluster.ID, res.Logger, conn, res.Metrics)
		router := control.NewLeaderRouter(cfg.Control.LeaderRouteMode, cfg.Control.LeaderRouteTimeout,
			cfg.Cluster.ID, conn, &managerLeaderState{m: m})
		ctrl, err := control.NewServer(control.Config{
			Addr:                 cfg.Control.ListenAddr,
			TLSCertFile:          cfg.Control.TLS.CertFile,
			TLSKeyFile:           cfg.Control.TLS.KeyFile,
			PlaintextExplicitAck: cfg.Control.TLS.PlaintextExplicitAck,
			Auth:                 auth,
			Audit:                audit,
			Router:               router,
			Engine:               m,
			Logger:               res.Logger,
			Metrics:              res.Metrics,
			ClusterID:            cfg.Cluster.ID,
			NodeID:               cfg.Node.ID,
		})
		if err != nil {
			return res, &StartupError{Code: ExitControl, Err: err}
		}
		res.Control = ctrl
		go func() {
			if err := ctrl.Start(ctx); err != nil {
				res.Logger.Error("control plane stopped with error",
					pgmanager.Field{Key: "error", Value: err.Error()})
			}
		}()

		// Gate #11: initial LCM-audit emit verifying both sinks (FR-027).
		// A failure here exits with EX_CONTROL so operators see the
		// audit pipeline is broken before any LCM request lands.
		startup := control.AuditRecord{
			Time:      "now",
			RequestID: "control_plane_started",
			Operation: "control_plane",
			Actor:     "system",
			Outcome:   control.OutcomeAccepted,
			ClusterID: cfg.Cluster.ID,
			NodeID:    cfg.Node.ID,
		}
		if err := audit.Emit(ctx, startup); err != nil {
			return res, &StartupError{Code: ExitControl, Err: fmt.Errorf("initial audit emit: %w", err)}
		}

		// Documented `control_plane started` event
		// (contracts/observability.md § Required event names).
		authSource := "env_var"
		if cfg.Control.Auth.TokenFile != "" {
			authSource = "file"
		}
		res.Logger.Info("control_plane started",
			pgmanager.Field{Key: "addr", Value: cfg.Control.ListenAddr},
			pgmanager.Field{Key: "auth_source", Value: authSource})
	}

	res.Logger.Info("manager started",
		pgmanager.Field{Key: "singleton_claim_attempts", Value: 1})
	res.Health.SetManagerReady(true)
	return res, nil
}

// managerLeaderState adapts *manager.Manager to the control.LeaderState
// interface. The walking-skeleton implementation is conservative: until
// pg-manager exposes a richer leadership view, we return what we know
// from the local manager and let the redirect path land on
// `leadership_in_transition` whenever the leader address is unknown.
type managerLeaderState struct {
	m *manager.Manager
}

func (m *managerLeaderState) IsLeader() bool {
	st, err := m.m.Status(context.Background())
	if err != nil {
		return false
	}
	return st.LocalRole == pgmanager.RolePrimary
}

func (m *managerLeaderState) LeaderID() string {
	st, err := m.m.Status(context.Background())
	if err != nil {
		return ""
	}
	return string(st.LeaderNodeID)
}

// LeaderAddr returns the leader's control-plane address. The library
// doesn't yet expose peer control-plane addresses on the state-store
// view; until it does, return "" so redirect-mode lands on
// `leadership_in_transition` rather than a stale URL. Operators using
// `forward` mode are unaffected.
func (m *managerLeaderState) LeaderAddr() string {
	return ""
}

// probeBind opens a TCP listener at addr and immediately closes it,
// surfacing EADDRINUSE before downstream code mounts servers.
func probeBind(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return ln.Close()
}

// localDSNFromConfig resolves the DSN from the env-var name configured
// in postgres.local_dsn_env (per FR-017).
func localDSNFromConfig(cfg config.Config) string {
	// The actual env lookup happens in the binary. Here we forward
	// whatever was set; main.go reads PGMAN_PROXY_<env_name> at startup.
	// For now we expose the sentinel so manager.RealPostgresExecutor
	// fails fast if the operator forgot the env var.
	return "${" + cfg.Postgres.LocalDSNEnv + "}"
}

func nodeIDs(peers []string) []pgmanager.NodeID {
	out := make([]pgmanager.NodeID, len(peers))
	for i, p := range peers {
		out[i] = pgmanager.NodeID(p)
	}
	return out
}

func peerDSNsForConfig(cfg config.Config) map[pgmanager.NodeID]string {
	out := make(map[pgmanager.NodeID]string, len(cfg.Peers))
	if len(cfg.Postgres.PeerDSNs) == 0 {
		// Default: derive a libpq conninfo per peer from the topology
		// port. This matches the pg-manager three_node_nats example.
		for _, p := range cfg.Peers {
			out[pgmanager.NodeID(p)] = fmt.Sprintf(
				"host=%s port=%d user=postgres dbname=postgres sslmode=disable",
				p, cfg.Topology.Port,
			)
		}
		return out
	}
	for k, v := range cfg.Postgres.PeerDSNs {
		out[pgmanager.NodeID(k)] = v
	}
	return out
}

func parseSwitchPolicy(s string) pgmanager.SwitchPolicy {
	switch s {
	case "drain":
		return pgmanager.SwitchDrain
	case "pause":
		return pgmanager.SwitchPause
	default:
		return pgmanager.SwitchHardClose
	}
}

// ConnectTimeoutDefault is exported so tests / docs can reference the
// default startup-gate-#4 budget without round-tripping through
// config.Defaults().
const ConnectTimeoutDefault = 10 * time.Second

// postInitDBHook returns the pg-manager PostInitDB callback that
// patches postgresql.conf and pg_hba.conf with operator-supplied lines
// after the elected bootstrap leader's initdb. Returns nil when no
// extras are configured (pg-manager treats nil as a no-op).
//
// The hook runs once on the leader; followers inherit the rewritten
// files via pg_basebackup. Without HBA extras allowing replication
// from peer hosts the cluster CANNOT reach Running state — operators
// must supply at least one `host replication ...` rule.
func postInitDBHook(cfg config.Config, logger *obs.Logger) func(context.Context) error {
	if len(cfg.Postgres.HBAExtras) == 0 && len(cfg.Postgres.ConfExtras) == 0 {
		return nil
	}
	dataDir := cfg.Postgres.DataDir
	confExtras := strings.Join(cfg.Postgres.ConfExtras, "\n")
	hbaExtras := strings.Join(cfg.Postgres.HBAExtras, "\n")
	return func(_ context.Context) error {
		if confExtras != "" {
			if err := appendLines(filepath.Join(dataDir, "postgresql.conf"),
				"# pgman-proxy: postgres.conf_extras\n"+confExtras+"\n"); err != nil {
				return fmt.Errorf("patch postgresql.conf: %w", err)
			}
			logger.Info("postgresql.conf patched",
				pgmanager.Field{Key: "lines", Value: len(cfg.Postgres.ConfExtras)})
		}
		if hbaExtras != "" {
			if err := appendLines(filepath.Join(dataDir, "pg_hba.conf"),
				"# pgman-proxy: postgres.hba_extras\n"+hbaExtras+"\n"); err != nil {
				return fmt.Errorf("patch pg_hba.conf: %w", err)
			}
			logger.Info("pg_hba.conf patched",
				pgmanager.Field{Key: "lines", Value: len(cfg.Postgres.HBAExtras)})
		}
		return nil
	}
}

func appendLines(path, body string) error {
	// path is constructed from cfg.Postgres.DataDir which is operator-
	// supplied at startup; we are running with the postgres user inside
	// our own data directory, so the open is trusted.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck
	_, err = f.WriteString(body)
	return err
}
