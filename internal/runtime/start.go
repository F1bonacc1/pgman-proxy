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
	"github.com/nats-io/nats.go/jetstream"

	"github.com/f1bonacc1/pgman-proxy/internal/cluster"
	"github.com/f1bonacc1/pgman-proxy/internal/config"
	"github.com/f1bonacc1/pgman-proxy/internal/control"
	"github.com/f1bonacc1/pgman-proxy/internal/embedded"
	"github.com/f1bonacc1/pgman-proxy/internal/fanout"
	"github.com/f1bonacc1/pgman-proxy/internal/history"
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
	Fanout   *fanout.Server     // feature 003: per-peer SliceStatus responder
	History  *history.Publisher // feature 003: cluster-wide event + audit history sink
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
	if meshErr := embSrv.WaitForRouteMesh(meshCtx, cfg.Cluster.DeclaredSize, 200*time.Millisecond); meshErr != nil {
		meshCancel()
		return res, &StartupError{Code: ExitDeps, Err: fmt.Errorf("phase1: route mesh: %w", meshErr)}
	}
	meshCancel()

	jsProbeCtx, jsProbeCancel := context.WithTimeout(ctx, 2*time.Minute)
	if jsErr := embedded.WaitForJetStreamResponsive(jsProbeCtx, conn, 500*time.Millisecond); jsErr != nil {
		jsProbeCancel()
		return res, &StartupError{Code: ExitDeps, Err: fmt.Errorf("phase1: jetstream not responsive: %w", jsErr)}
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
	if kvErr := embedded.PreCreateClusterKV(ctx, conn, cfg.Cluster.ID, replicaDecision.Effective()); kvErr != nil {
		return res, &StartupError{Code: ExitDeps, Err: fmt.Errorf("phase1: pre-create cluster KV: %w", kvErr)}
	}

	handles, err := cluster.BuildHandles(ctx, conn, cfg.Cluster.ID, cfg.Node.ID, res.Logger)
	if err != nil {
		return res, &StartupError{Code: ExitDeps, Err: fmt.Errorf("phase1: build handles: %w", err)}
	}
	res.Cluster = handles

	// Feature 003 — bootstrap the cluster's history JetStream (T020).
	// Idempotent + race-tolerant; every peer calls it. The publisher is
	// shared across the audit emitter and any per-peer event sites that
	// also want to land records in the cluster-wide history log.
	js, jsErr := jetstream.New(conn)
	if jsErr != nil {
		return res, &StartupError{Code: ExitDeps, Err: fmt.Errorf("phase1: history jetstream context: %w", jsErr)}
	}
	historyOpts := history.DefaultStreamOptions(replicaDecision.Effective())
	if _, hErr := history.EnsureHistoryStream(ctx, js, cfg.Cluster.ID, historyOpts); hErr != nil {
		return res, &StartupError{Code: ExitDeps, Err: fmt.Errorf("phase1: ensure history stream: %w", hErr)}
	}
	res.History = history.NewPublisher(js, cfg.Cluster.ID, cfg.Node.ID)
	res.Logger.SetHistorySink(historyLoggerSink{p: res.History})
	res.Logger.Event("proxy.history_stream_ready", cfg.Node.ID,
		pgmanager.Field{Key: "stream", Value: history.StreamName(cfg.Cluster.ID)},
		pgmanager.Field{Key: "replicas", Value: replicaDecision.Effective()})

	// Optional: when ReplicationAddr is set, publish it to the cluster
	// KV and install a PeerDSNResolver so pg-manager pulls peer pg
	// addresses from the substrate at basebackup time. When unset
	// (e.g., the integration-test docker-compose topology where peer
	// IDs already resolve as DNS names), pg-manager falls back to its
	// static PeerDSNs map and the KV is not touched.
	var peerDSNResolver func(context.Context, pgmanager.NodeID) (string, error)
	if cfg.Postgres.ReplicationAddr != "" {
		clusterKV, kvOpenErr := embedded.OpenClusterKV(ctx, conn, cfg.Cluster.ID)
		if kvOpenErr != nil {
			return res, &StartupError{Code: ExitDeps, Err: fmt.Errorf("phase1: open cluster KV: %w", kvOpenErr)}
		}
		if pubErr := cluster.PublishPeerAddress(ctx, clusterKV, cfg.Node.ID, cfg.Postgres.ReplicationAddr); pubErr != nil {
			return res, &StartupError{Code: ExitDeps, Err: fmt.Errorf("phase1: publish peer address: %w", pubErr)}
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

	// Subscribe to pg-manager coordination events (FR-006). Pass the
	// history publisher so leadership / state-transition / fenced /
	// failover events land in the history JetStream (003 FR-007).
	subs, err := cluster.SubscribeCoordinationEvents(conn, cfg.Cluster.ID, cfg.Node.ID, res.Logger, res.Metrics,
		historyLoggerSink{p: res.History})
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

	// Gate #6.5: Honor pg-manager's "assume standby until proven primary"
	// startup_with_pgdata contract at the postgres level. Without this,
	// a node whose PGDATA was last operating as primary (e.g. it was
	// docker-restarted mid-failover) comes back as a postgres-level
	// primary while pg-manager declares role=standby — a latent
	// split-brain that depends on auto_demote's stability+cooldown gates
	// to ever heal. Writing standby.signal before manager.Start forces
	// postgres to come up as a standby; if quorum proves us the
	// rightful primary, pg-manager's became_leader → pg_promote path
	// removes the signal during promotion (pg_ctl promote is idempotent
	// against an already-primary backend per pgproto/pgexec.go).
	if signalErr := ensureStandbySignalIfInitialized(cfg.Postgres.DataDir, res.Logger); signalErr != nil {
		return res, &StartupError{Code: ExitDeps, Err: fmt.Errorf("ensure standby.signal: %w", signalErr)}
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
			NodeID:          pgmanager.NodeID(cfg.Node.ID),
			Peers:           nodeIDs(cfg.Peers),
			DataDir:         cfg.Postgres.DataDir,
			BinDir:          cfg.Postgres.BinDir,
			Port:            cfg.Topology.Port,
			PeerDSNs:        peerDSNsForConfig(cfg),
			PeerDSNResolver: peerDSNResolver,
			LocalPGAddr:     cfg.Postgres.LocalPGAddr,
		},
		Policy:               policyFromConfig(cfg),
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

	// Feature 003 — per-peer status aggregation substrate. pg-manager's
	// Manager.Status() returns per-peer scalars but does not populate
	// the cluster-wide Instances slice or PrimaryNodeID; the fanout
	// server lets every peer answer a SliceStatus request with its own
	// snapshot so /v1/status can stitch the full cluster view.
	fanoutSrv := fanout.NewServer(conn, cfg.Cluster.ID, cfg.Node.ID)
	if rerr := fanoutSrv.Register(fanout.SliceStatus, statusResponderHandler(m.Status)); rerr != nil {
		return res, &StartupError{Code: ExitDeps, Err: fmt.Errorf("fanout register SliceStatus: %w", rerr)}
	}
	// T019 — SliceNATSMesh: every peer returns its own embedded-NATS
	// snapshot so the connected peer can stitch a cluster-wide mesh
	// view (003 FR-013, dump artifact peers/<id>/nats_mesh.json).
	if rerr := fanoutSrv.Register(fanout.SliceNATSMesh, natsMeshResponderHandler(res.Embedded)); rerr != nil {
		return res, &StartupError{Code: ExitDeps, Err: fmt.Errorf("fanout register SliceNATSMesh: %w", rerr)}
	}
	// T019 — SliceConfig / SliceDoctor: handlers register a stub that
	// returns slice_not_implemented so callers see a structured error
	// per fanout-protocol.md § Aggregation rules rather than a NATS
	// timeout. Replaced by US3 (doctor) and US6 (config) phases.
	if rerr := fanoutSrv.Register(fanout.SliceConfig, sliceNotImplementedHandler(fanout.SliceConfig)); rerr != nil {
		return res, &StartupError{Code: ExitDeps, Err: fmt.Errorf("fanout register SliceConfig: %w", rerr)}
	}
	if rerr := fanoutSrv.Register(fanout.SliceDoctor, sliceNotImplementedHandler(fanout.SliceDoctor)); rerr != nil {
		return res, &StartupError{Code: ExitDeps, Err: fmt.Errorf("fanout register SliceDoctor: %w", rerr)}
	}
	if serr := fanoutSrv.Serve(); serr != nil {
		return res, &StartupError{Code: ExitDeps, Err: fmt.Errorf("fanout server: %w", serr)}
	}
	res.Fanout = fanoutSrv
	fanoutClient := fanout.NewClient(conn, cfg.Cluster.ID)
	statusAgg := newStatusAggregator(fanoutClient, cfg.Peers, 750*time.Millisecond, res.Logger)
	// Wire the GET /v1/history querier closure; the control package can't
	// import internal/history directly (it stays infrastructure-free), so
	// we hand it a closure that captures the live JetStream context.
	historyQuerier := &historyRunner{js: js, clusterID: cfg.Cluster.ID}
	historyWatcher := &watchSubscriber{w: history.NewWatcher(js, cfg.Cluster.ID)}

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
		audit := control.NewAudit(cfg.Cluster.ID, res.Logger, conn, res.Metrics).WithHistory(res.History)
		router := control.NewLeaderRouter(cfg.Control.LeaderRouteMode, cfg.Control.LeaderRouteTimeout,
			cfg.Cluster.ID, conn, &managerLeaderState{m: m})

		// Feature 003 / US6 — supervisor-presence detection. Used by the
		// POST /v1/restart target=proxy pre-flight to refuse self-
		// terminate when no recognised supervisor would bring us back.
		supervisor := DetectSupervisor(cfg.Proxy.AssumeSupervised)
		res.Logger.Info("proxy.supervisor_presence_detected",
			pgmanager.Field{Key: "presence", Value: string(supervisor)},
			pgmanager.Field{Key: "assume_supervised", Value: cfg.Proxy.AssumeSupervised})
		selfTerm := &selfTerminator{
			localNodeID: cfg.Node.ID,
			presence:    string(supervisor),
			logger:      res.Logger,
		}
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
			// Feature 003: enrich /v1/status with cluster-wide Instances
			// + PrimaryNodeID via fan-out to peers.
			Aggregator: statusAgg,
			// Feature 003: GET /v1/history backed by the cluster's
			// JetStream history stream (FR-016a).
			History: historyQuerier,
			// Feature 003: /v1/watch/* SSE handlers subscribe to the
			// cluster's history JetStream via this watcher.
			Watch: historyWatcher,
			// Feature 003 / US6: self-terminator for /v1/restart
			// target=proxy. Drain + os.Exit handled in shutdown.go.
			Proxy: selfTerm,
			// Reloader currently unwired — POST /v1/config/set will
			// return engine_error until the SIGHUP plumbing exposes
			// an in-process trigger. Operators stage YAML changes and
			// SIGHUP today; the endpoint is reachable for audit-trail
			// continuity.
			Reloader:  nil,
			ClusterID: cfg.Cluster.ID,
			NodeID:    cfg.Node.ID,
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

		// Gate #11a: wait for the history stream's RAFT group to elect
		// a leader before the boot-gate audit emit below. Without this,
		// on a cold cluster start the audit Emit races stream-leader
		// election: publishes return `nats: no response from stream`,
		// the fatal-on-first-failure gate puts every peer into an
		// exit-81 boot loop, and the cluster wedges (chaos-rig RCA,
		// 2026-05-16). Bounded by 30 s; if the stream truly cannot
		// reach quorum in that time, exit-81 is the right answer.
		streamReadyCtx, streamReadyCancel := context.WithTimeout(ctx, 30*time.Second)
		if err := embedded.WaitForStreamReady(streamReadyCtx, js, history.StreamName(cfg.Cluster.ID), 30*time.Second, 500*time.Millisecond); err != nil {
			streamReadyCancel()
			return res, &StartupError{Code: ExitControl, Err: fmt.Errorf("history stream not ready: %w", err)}
		}
		streamReadyCancel()

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
		res.Logger.Event("control_plane started", cfg.Node.ID,
			pgmanager.Field{Key: "addr", Value: cfg.Control.ListenAddr},
			pgmanager.Field{Key: "auth_source", Value: authSource})
	}

	res.Logger.Info("manager started",
		pgmanager.Field{Key: "singleton_claim_attempts", Value: 1})
	res.Health.SetManagerReady(true)
	return res, nil
}

// ensureStandbySignalIfInitialized writes <DataDir>/standby.signal when
// PGDATA is initialized (PG_VERSION present) and no signal file exists
// yet. Idempotent — the function is a no-op when PGDATA is empty (first
// boot will run initdb) or when standby.signal is already present.
//
// Rationale: pg-manager's startup_with_pgdata transition declares the
// node as role=standby on the strength of "an initialized PGDATA"
// (state/transitions.go:75 "assume standby until proven primary"), but
// it does NOT enforce that postgres on disk agrees. If the previous
// session ended while this node was operating as primary (postmaster
// shutdown, host restart, container restart) PGDATA has no
// standby.signal and pg_ctl start brings postgres up as a primary —
// creating a window where pg-manager's role state and postgres's
// in-recovery state disagree. That window depends on auto_demote's
// stability + cooldown gates to close; under chaos load (rapid
// failover sequences) it never closes and the cluster ends up with
// two postgres primaries serving writes against diverged WALs.
//
// Writing standby.signal pre-emptively closes the window at the
// process-startup boundary. If quorum subsequently proves us the
// rightful primary, pg-manager's became_leader → StatePromoting path
// calls pg_ctl promote, which removes standby.signal and exits
// recovery. The cost is one extra promote on the rightful primary's
// startup path; the benefit is the elimination of a structural
// split-brain class.
func ensureStandbySignalIfInitialized(dataDir string, logger pgmanager.Logger) error {
	if dataDir == "" {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dataDir, "PG_VERSION")); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat PG_VERSION: %w", err)
	}
	signalPath := filepath.Join(dataDir, "standby.signal")
	if _, err := os.Stat(signalPath); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat standby.signal: %w", err)
	}
	f, err := os.OpenFile(signalPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return fmt.Errorf("create standby.signal: %w", err)
	}
	_ = f.Close()
	logger.Info(
		"startup_with_pgdata: wrote standby.signal preemptively",
		pgmanager.Field{Key: "data_dir", Value: dataDir},
	)
	return nil
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

// policyFromConfig translates pgman-proxy's config.PolicyConfig into the
// pgmanager.Policy literal handed to manager.New. AutoDemote and
// AutoRebootstrap default to disabled (the pg-manager safe-by-default
// posture: divergence is detected and parked, but no destructive
// recovery runs without explicit operator opt-in). Operators set the
// per-feature `enabled` flag in YAML or via
// PGMAN_PROXY_POLICY_AUTO_(DEMOTE|REBOOTSTRAP)_ENABLED to turn them on.
//
// Closes Gap O: the YAML fields existed (config.go:170-171) but the
// previous Policy literal omitted them entirely, so an ex-primary that
// warm-restarted under process-compose `restart: always` came back as
// primary and produced a two-postmaster split-brain visible only via
// `divergence.parked` events that the reconciler couldn't act on.
func policyFromConfig(cfg config.Config) pgmanager.Policy {
	return pgmanager.Policy{
		FailoverDelay:    cfg.Policy.FailoverDelay,
		SwitchoverDelay:  cfg.Policy.SwitchoverDelay,
		PromoteTimeout:   cfg.Policy.PromoteTimeout,
		LivenessInterval: cfg.Policy.LivenessInterval,
		LivenessFailures: cfg.Policy.LivenessFailures,
		Replication: pgmanager.QuorumSync{
			MinSync: cfg.Policy.QuorumSync.MinSync,
			Pool:    nodeIDs(cfg.Peers),
		},
		AutoDemote:      autoDemotePolicy(cfg.Policy.AutoDemote),
		AutoRebootstrap: autoRebootstrapPolicy(cfg.Policy.AutoRebootstrap),
	}
}

// defaultAutoDemoteProbeFailureThreshold matches the example assembly
// at pg-manager/examples/three_node_nats/main.go:526 — "use-site
// default of 3 applies only when Enabled=false" per
// manager.go:937-941, so callers turning AutoDemote on MUST supply a
// positive value or manager.New rejects with config_invalid. Three
// is the documented production-shape default.
const defaultAutoDemoteProbeFailureThreshold = 3

// autoDemotePolicy builds the upstream AutoDemotePolicy from the proxy's
// AutoDemoteCfg. When disabled the zero value is correct. When enabled,
// populate ProbeFailureThreshold (mandatory) and forward any explicit
// duration overrides from the proxy config; zero values fall through to
// pg-manager's use-site defaults (1h cooldown, 15s leadership-stability
// window, 5s probe timeout — see pg-manager/types.go AutoDemotePolicy).
func autoDemotePolicy(c config.AutoDemoteCfg) pgmanager.AutoDemotePolicy {
	if !c.Enabled {
		return pgmanager.AutoDemotePolicy{}
	}
	return pgmanager.AutoDemotePolicy{
		Enabled:                   true,
		ProbeFailureThreshold:     defaultAutoDemoteProbeFailureThreshold,
		Cooldown:                  c.Cooldown,
		LeadershipStabilityWindow: c.LeadershipStabilityWindow,
		ProbeTimeout:              c.ProbeTimeout,
	}
}

// autoRebootstrapPolicy builds the upstream AutoRebootstrapPolicy from
// the proxy's AutoRecoveryCfg. When disabled the zero value is correct.
// When enabled, forward any explicit duration overrides; zero values
// fall through to pg-manager's documented defaults (1h cooldown, 5min
// persistence window — see pg-manager/types.go AutoRebootstrapPolicy).
// Chaos rigs override via PGMAN_PROXY_POLICY_AUTO_REBOOTSTRAP_COOLDOWN
// and PGMAN_PROXY_POLICY_AUTO_REBOOTSTRAP_PERSISTENCE_WINDOW so a
// successful rebootstrap doesn't park the cluster against a fresh
// stale-WAL condition for an hour (STAB-03 Part 1).
func autoRebootstrapPolicy(c config.AutoRecoveryCfg) pgmanager.AutoRebootstrapPolicy {
	return pgmanager.AutoRebootstrapPolicy{
		Enabled:           c.Enabled,
		Cooldown:          c.Cooldown,
		PersistenceWindow: c.PersistenceWindow,
	}
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
		// Translate the lifecycle event into a structured log entry AND
		// (when a history sink has been wired) append it to the cluster
		// history stream so /v1/history sees every embedded-NATS event.
		f := make([]pgmanager.Field, 0, len(fields)+1)
		f = append(f, pgmanager.Field{Key: "event", Value: string(kind)})
		for k, v := range fields {
			f = append(f, pgmanager.Field{Key: k, Value: v})
		}
		logger.Event(string(kind), cfg.Node.ID, f...)
	}

	srv, err := embedded.NewServer(opts, cfg.Node.ID, cfg.Cluster.ID, emit)
	if err != nil {
		return nil, &StartupError{Code: ExitConfig, Err: fmt.Errorf("embedded.NewServer: %w", err)}
	}

	if err := srv.Start(ctx, cfg.Cluster.ReadyTimeout); err != nil {
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
