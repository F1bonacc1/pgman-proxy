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
	"github.com/f1bonacc1/pgman-proxy/internal/embedded"
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
	Embedded *embedded.Server // feature 002: in-process NATS server (replaces external NATS)
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

	// Gate #4 (feature 002): boot the embedded NATS server. Replaces
	// the 001 external-NATS dial; the in-process pg-manager adapters
	// dial the loopback URL the embedded server reports as ready.
	embSrv, embErr := bootEmbeddedNATS(ctx, cfg, res.Logger)
	if embErr != nil {
		return res, embErr
	}
	res.Embedded = embSrv

	// Gate #4b (feature 002): in-process pg-manager adapter dials the
	// embedded server's loopback client URL.
	conn, err := cluster.Connect(ctx, embSrv.ClientURL(), cfg.NATS, cfg.Node.ID, res.Logger)
	if err != nil {
		return res, &StartupError{Code: ExitDeps, Err: err}
	}
	res.Conn = conn
	res.Health.SetNATSUp(true)

	// Phase 1 (feature 002): cluster-substrate formation.
	//
	// Layered to keep cluster-formation concerns out of pg-manager:
	//
	//   1a. Wait for the embedded NATS routes mesh so JetStream
	//       meta-cluster election has the quorum it needs.
	//   1b. Wait until the JS subsystem answers a trivial RPC. The JS
	//       client's internal per-request timeout is 5 s, so without
	//       this gate the first call inside cluster.BuildHandles
	//       (NewLeadership → ensureBucket → js.KeyValue) fails-closed
	//       on a fresh cold start.
	//   1c. Pre-create the cluster KV bucket with the cluster-size-
	//       derived Replicas count (FR-011a / RD-004). Race-tolerant:
	//       all peers call this concurrently, one wins the create, the
	//       rest re-fetch. Runs before BuildHandles so pg-manager's
	//       ensureBucket sees an existing correctly-replicated bucket
	//       and skips the create entirely (which would otherwise have
	//       defaulted to Replicas=1). Retired by upstream T007 once
	//       pg-manager exposes WithReplicas(int).
	//   1d. Build cluster handles (pg-manager NewLeadership, state
	//       store, event bus). The substrate is now fully formed; any
	//       transient JS RPC errors during pg-manager bootstrap are
	//       absorbed by the upstream SingletonClaimRetryPolicy which
	//       classifies context.DeadlineExceeded as retryable.
	meshCtx, meshCancel := context.WithTimeout(ctx, 2*time.Minute)
	if err := embSrv.WaitForRouteMesh(meshCtx, cfg.Cluster.DeclaredSize, 200*time.Millisecond); err != nil {
		meshCancel()
		return res, &StartupError{Code: ExitDeps, Err: fmt.Errorf("phase1: route mesh: %w", err)}
	}
	meshCancel()

	jsProbeCtx, jsProbeCancel := context.WithTimeout(ctx, 2*time.Minute)
	if err := embedded.WaitForJetStreamResponsive(jsProbeCtx, conn, 500*time.Millisecond); err != nil {
		jsProbeCancel()
		return res, &StartupError{Code: ExitDeps, Err: fmt.Errorf("phase1: jetstream not responsive: %w", err)}
	}
	jsProbeCancel()

	replicaDecision := embedded.DecideReplicas(cfg.Cluster.DeclaredSize, cfg.Cluster.ReplicationFactorOverride)
	if replicaDecision.Warning != "" {
		res.Logger.Warn("embedded_nats.replica_advisory",
			pgmanager.Field{Key: "declared_size", Value: replicaDecision.DeclaredSize},
			pgmanager.Field{Key: "replicas", Value: replicaDecision.Effective()},
			pgmanager.Field{Key: "overridden", Value: replicaDecision.Overridden()},
			pgmanager.Field{Key: "warning", Value: replicaDecision.Warning})
	}
	if err := embedded.PreCreateClusterKV(ctx, conn, cfg.Cluster.ID, replicaDecision.Effective()); err != nil {
		return res, &StartupError{Code: ExitDeps, Err: fmt.Errorf("phase1: pre-create cluster KV: %w", err)}
	}

	handles, err := cluster.BuildHandles(ctx, conn, cfg.Cluster.ID, cfg.Node.ID, res.Logger)
	if err != nil {
		return res, &StartupError{Code: ExitDeps, Err: fmt.Errorf("phase1: build handles: %w", err)}
	}
	res.Cluster = handles

	// Optional: when ReplicationAddr is set, publish it to the cluster
	// KV and install a PeerDSNResolver so pg-manager pulls peer pg
	// addresses from the substrate at basebackup time. When unset
	// (e.g., the integration-test docker-compose topology where peer
	// IDs already resolve as DNS names), pg-manager falls back to its
	// static PeerDSNs map and the KV is not touched.
	var peerDSNResolver func(context.Context, pgmanager.NodeID) (string, error)
	if cfg.Postgres.ReplicationAddr != "" {
		clusterKV, err := embedded.OpenClusterKV(ctx, conn, cfg.Cluster.ID)
		if err != nil {
			return res, &StartupError{Code: ExitDeps, Err: fmt.Errorf("phase1: open cluster KV: %w", err)}
		}
		if err := cluster.PublishPeerAddress(ctx, clusterKV, cfg.Node.ID, cfg.Postgres.ReplicationAddr); err != nil {
			return res, &StartupError{Code: ExitDeps, Err: fmt.Errorf("phase1: publish peer address: %w", err)}
		}
		res.Logger.Info("published peer replication address",
			pgmanager.Field{Key: "node_id", Value: cfg.Node.ID},
			pgmanager.Field{Key: "addr", Value: cfg.Postgres.ReplicationAddr})
		peerDSNResolver = cluster.PeerAddressResolver(clusterKV)
	}

	// Now that the leadership handle exists, wire the storage monitor
	// (the fence callback references it).
	storageMon := embedded.NewStorageMonitor(
		res.Embedded,
		embedded.StorageMonitorOptions{Path: cfg.Cluster.JetStreamDir},
		func(_ context.Context, kind embedded.StorageDegradedKind, path string, _ error) {
			res.Logger.Error("embedded_nats.self_fencing_on_storage_degraded",
				pgmanager.Field{Key: "kind", Value: string(kind)},
				pgmanager.Field{Key: "path", Value: path})
			if res.Cluster != nil && res.Cluster.Leadership != nil {
				res.Cluster.Leadership.Close()
			}
		},
	)
	storageMon.Start(ctx)

	// Feature 002 (T025 / contracts/observability.md): poll the
	// embedded server's Routez snapshot every 2 s and emit
	// `embedded_nats.route_up` / `route_down` events on transitions.
	// Per-peer audit identity comes from the sibling's RemoteName,
	// which is the sibling pgman-proxy node ID (RD-001a).
	embedded.NewRouteWatcher(res.Embedded, 0).Start(ctx)

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
			NodeID:           pgmanager.NodeID(cfg.Node.ID),
			Peers:            nodeIDs(cfg.Peers),
			DataDir:          cfg.Postgres.DataDir,
			BinDir:           cfg.Postgres.BinDir,
			Port:             cfg.Topology.Port,
			PeerDSNs:         peerDSNsForConfig(cfg),
			PeerDSNResolver:  peerDSNResolver,
			LocalPGAddr:      cfg.Postgres.LocalPGAddr,
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
			// Feature 002: surface the embedded-NATS snapshot under
			// `cluster.embedded_nats` in the Status response.
			EmbeddedSnapshot: func() any { return res.Embedded.Snapshot() },
			ClusterID:        cfg.Cluster.ID,
			NodeID:           cfg.Node.ID,
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

// bootEmbeddedNATS resolves the cluster credential, builds the
// embedded NATS server's options, and waits for it to reach the ready
// state — feature 002 startup Gate #4. On any failure the caller is
// expected to map the StartupError code (ExitConfig for credential /
// option-build issues, ExitDeps for ready-timeout) per
// contracts/lifecycle.md.
func bootEmbeddedNATS(ctx context.Context, cfg config.Config, logger *obs.Logger) (*embedded.Server, *StartupError) {
	cc := cfg.Cluster

	// Resolve the cluster password via SecretRef (env / file). The
	// username is non-secret and may live inline in cfg.Cluster.Username.
	password, err := resolveClusterPassword(cc)
	if err != nil {
		return nil, &StartupError{Code: ExitConfig, Err: err}
	}

	// Single-peer convenience: if no credential is configured AND the
	// declared cluster size is 1 with routes_listen disabled, run
	// without cluster auth (loopback-only single-peer dev mode).
	var cred embedded.ClusterCredential
	if cc.Username != "" || password != "" {
		cred, err = embedded.LoadClusterCredential([]byte(cc.Username), []byte(password))
		if err != nil {
			return nil, &StartupError{Code: ExitConfig, Err: fmt.Errorf("cluster credential: %w", err)}
		}
	}

	// Single-peer (declared_size==1) implicitly disables the routes
	// listener (matches validate.go's gating). This avoids opening a
	// routes port on a peer that has no siblings to mesh with.
	routesEnabled := cc.RoutesListen.Enabled && cc.DeclaredSize > 1

	in := embedded.OptionsInput{
		NodeID:               cfg.Node.ID,
		ClusterName:          firstNonEmpty(cc.Name, cc.ID),
		DeclaredSize:         cc.DeclaredSize,
		ClientHost:           cc.ClientListen.Host,
		ClientPort:           cc.ClientListen.Port,
		RoutesEnabled:        routesEnabled,
		RoutesHost:           cc.RoutesListen.Host,
		RoutesPort:           cc.RoutesListen.Port,
		RoutePeers:           cc.RoutePeers,
		Credential:           cred,
		TLSCertFile:          cc.TLS.CertFile,
		TLSKeyFile:           cc.TLS.KeyFile,
		TLSCAFile:            cc.TLS.CAFile,
		PlaintextExplicitAck: cc.TLS.PlaintextExplicitAck,
		JetStreamDir:         cc.JetStreamDir,
	}

	opts, err := embedded.BuildOptions(in)
	if err != nil {
		return nil, &StartupError{Code: ExitConfig, Err: fmt.Errorf("build embedded NATS options: %w", err)}
	}

	emit := func(kind embedded.LifecycleEventKind, fields map[string]any) {
		// Translate the lifecycle event into a structured log entry.
		f := make([]pgmanager.Field, 0, len(fields)+1)
		f = append(f, pgmanager.Field{Key: "event", Value: string(kind)})
		for k, v := range fields {
			f = append(f, pgmanager.Field{Key: k, Value: v})
		}
		logger.Info(string(kind), f...)
	}

	srv, err := embedded.NewServer(opts, cfg.Node.ID, cfg.Cluster.ID, emit)
	if err != nil {
		return nil, &StartupError{Code: ExitConfig, Err: fmt.Errorf("embedded.NewServer: %w", err)}
	}

	if err := srv.Start(ctx, cfg.NATS.ConnectTimeout); err != nil {
		return nil, &StartupError{Code: ExitDeps, Err: fmt.Errorf("embedded NATS startup: %w", err)}
	}
	return srv, nil
}

// resolveClusterPassword reads the cluster password from its
// configured SecretRef (env or file). Returns "" if neither source is
// configured — the caller decides whether that's permissible (single-
// peer dev mode is; multi-peer is rejected at validation).
func resolveClusterPassword(cc config.ClusterConfig) (string, error) {
	if cc.PasswordEnv != "" {
		v := os.Getenv(cc.PasswordEnv)
		if v == "" {
			return "", fmt.Errorf("cluster.password_env=%q resolves to an empty string (FR-010)", cc.PasswordEnv)
		}
		return v, nil
	}
	if cc.PasswordFile != "" {
		data, err := os.ReadFile(cc.PasswordFile)
		if err != nil {
			return "", fmt.Errorf("read cluster.password_file %q: %w", cc.PasswordFile, err)
		}
		// Trim trailing newline that secret-managers commonly add.
		v := strings.TrimRight(string(data), "\r\n ")
		if v == "" {
			return "", fmt.Errorf("cluster.password_file %q is empty after trimming", cc.PasswordFile)
		}
		return v, nil
	}
	return "", nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

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

