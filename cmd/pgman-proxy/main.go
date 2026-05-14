// Command pgman-proxy is the active/active PostgreSQL HA proxy and
// lifecycle manager described in
// specs/001-active-active-pg-proxy/spec.md.
//
// The wire-protocol work lives in github.com/f1bonacc1/pg-manager/proxy;
// the LCM logic lives in github.com/f1bonacc1/pg-manager/manager. This
// binary is the deployment scaffold around them — process lifecycle,
// configuration, NATS wiring, and observability glue.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime/debug"
	"strings"
	"time"

	pgmanager "github.com/f1bonacc1/pg-manager"

	"github.com/f1bonacc1/pgman-proxy/internal/config"
	"github.com/f1bonacc1/pgman-proxy/internal/obs"
	"github.com/f1bonacc1/pgman-proxy/internal/runtime"
)

// Build-time variables wired by goreleaser ldflags.
var (
	version = "dev"
	commit  = ""
	date    = ""
)

func main() {
	code := run(os.Args[1:])
	os.Exit(code)
}

func run(args []string) int {
	// Subcommand dispatch (feature 002 / RD-003): operator-facing
	// utilities live as `pgman-proxy <subcmd>` rather than top-level
	// flags so the flag namespace stays focused on the daemon-mode
	// invocation. Currently the only registered subcommand is
	// `cluster-secret-gen`.
	if len(args) > 0 {
		switch args[0] {
		case "cluster-secret-gen":
			return runClusterSecretGen(args[1:])
		}
	}

	fs := flag.NewFlagSet("pgman-proxy", flag.ContinueOnError)
	var (
		configPath   string
		clusterID    string
		clusterName  string
		nodeID       string
		peers        string
		listenAddr   string
		switchPolicy string
		logLevel     string
		metricsAddr  string
		printConfig  bool
		showVersion  bool
	)
	fs.StringVar(&configPath, "config", "", "YAML configuration file path")
	fs.StringVar(&clusterID, "cluster-id", "", "override cluster.id")
	fs.StringVar(&clusterName, "cluster-name", "", "override cluster.name (feature 002 — embedded NATS cluster)")
	fs.StringVar(&nodeID, "node-id", "", "override node.id")
	fs.StringVar(&peers, "peers", "", "override peers (CSV)")
	// Feature 002: --nats flag removed; external NATS is no longer
	// supported. Coordination plane is embedded in-process.
	fs.StringVar(&listenAddr, "listen", "", "override proxy.listen_addr")
	fs.StringVar(&switchPolicy, "switch-policy", "", "override proxy.switch_policy (hard_close|drain|pause)")
	fs.StringVar(&logLevel, "log-level", "", "override obs.log_level (debug|info|warn|error)")
	fs.StringVar(&metricsAddr, "metrics", "", "override obs.metrics_addr")
	fs.BoolVar(&printConfig, "print-config", false, "render the merged, validated config to stdout and exit")
	fs.BoolVar(&showVersion, "version", false, "print version and exit")

	if err := fs.Parse(args); err != nil {
		// flag.ContinueOnError already wrote a message to stderr.
		return runtime.ExitConfig
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "pgman-proxy: unexpected positional arguments: %v\n", fs.Args())
		return runtime.ExitConfig
	}
	if showVersion && printConfig {
		fmt.Fprintln(os.Stderr, "pgman-proxy: --version and --print-config are mutually exclusive")
		return runtime.ExitConfig
	}

	if showVersion {
		printVersionInfo()
		return runtime.ExitOK
	}

	flagOverrides := map[string]string{}
	if clusterID != "" {
		flagOverrides["cluster-id"] = clusterID
	}
	if nodeID != "" {
		flagOverrides["node-id"] = nodeID
	}
	if peers != "" {
		flagOverrides["peers"] = peers
	}
	if clusterName != "" {
		flagOverrides["cluster-name"] = clusterName
	}
	if listenAddr != "" {
		flagOverrides["listen"] = listenAddr
	}
	if switchPolicy != "" {
		flagOverrides["switch-policy"] = switchPolicy
	}
	if logLevel != "" {
		flagOverrides["log-level"] = logLevel
	}
	if metricsAddr != "" {
		flagOverrides["metrics"] = metricsAddr
	}

	cfg, _, err := config.Load(config.LoadOptions{
		YAMLPath: configPath,
		Env:      os.Getenv,
		Flags:    flagOverrides,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgman-proxy: config error: %v\n", err)
		return runtime.ExitConfig
	}
	if err := config.Validate(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "pgman-proxy: %v\n", err)
		return runtime.ExitConfig
	}

	if printConfig {
		emitRedactedConfig(cfg)
		return runtime.ExitOK
	}

	// Top-level panic guard so an unexpected panic exits cleanly with
	// the documented EX_INTERNAL code instead of a Go runtime trace.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "pgman-proxy: internal panic: %v\n%s\n", r, debug.Stack())
			os.Exit(runtime.ExitInternal)
		}
	}()

	logger := obs.NewLogger(os.Stderr, cfg.Obs.LogLevel, cfg.Cluster.ID, cfg.Node.ID, "main")
	ctx, cancelSignals := runtime.SignalContext(context.Background(), logger)
	defer cancelSignals()

	res, startErr := runtime.Start(ctx, cfg, version)
	if startErr != nil {
		logger.Error("startup failed",
			pgmanager.Field{Key: "code", Value: startErr.Code},
			pgmanager.Field{Key: "exit_name", Value: runtime.ExitName(startErr.Code)},
			pgmanager.Field{Key: "error", Value: startErr.Err.Error()})
		teardownPartial(ctx, res, logger, cfg.Shutdown.DrainBudget)
		return startErr.Code
	}

	// Run the manager loop until ctx is cancelled or it returns.
	startResult := make(chan error, 1)
	go func() { startResult <- res.Manager.Start(ctx) }()

	if px := res.Manager.Proxy(); px != nil {
		// Documented `proxy listener bound` event
		// (contracts/observability.md § Required event names).
		logger.Info("proxy listener bound",
			pgmanager.Field{Key: "addr", Value: cfg.Proxy.ListenAddr})
		go func() {
			if err := px.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("proxy.Start", pgmanager.Field{Key: "error", Value: err.Error()})
			}
			logger.Info("proxy listener closed",
				pgmanager.Field{Key: "addr", Value: cfg.Proxy.ListenAddr})
		}()
	}

	var managerErr error
	select {
	case <-ctx.Done():
		managerErr = <-startResult
		if errors.Is(managerErr, ctx.Err()) {
			managerErr = nil
		}
	case managerErr = <-startResult:
		// Manager.Start returned BEFORE shutdown — singleton-claim
		// budget exhausted, setup failure, or similar. Cancel ctx so
		// downstream goroutines wind down.
		cancelSignals()
	}

	exitCode := runtime.ExitOK
	if managerErr != nil {
		// Singleton-claim retry exhaustion is its own exit code.
		if isSingletonError(managerErr) {
			exitCode = runtime.ExitSingleton
		} else {
			exitCode = runtime.ExitInternal
		}
		// Documented `manager start failed` event
		// (contracts/observability.md § Required event names).
		logger.Error("manager start failed",
			pgmanager.Field{Key: "error", Value: managerErr.Error()})
	}

	drain := runtime.Drain(context.Background(), cfg.Shutdown.DrainBudget,
		buildShutdownSteps(res),
		logger,
	)
	if drain.ExitCode() != runtime.ExitOK {
		exitCode = drain.ExitCode()
	}
	return exitCode
}

// teardownPartial runs the shutdown steps in best-effort mode for a
// startup failure. We may have a partially-constructed StartupResult;
// each step guards against nil receivers.
func teardownPartial(ctx context.Context, res *runtime.StartupResult, logger *obs.Logger, budget time.Duration) {
	if res == nil {
		return
	}
	_ = runtime.Drain(ctx, budget, buildShutdownSteps(res), logger)
}

func buildShutdownSteps(res *runtime.StartupResult) []runtime.DrainStep {
	steps := []runtime.DrainStep{}
	// FR-021..FR-034 + contracts/lifecycle.md § Graceful shutdown flow:
	// the control plane MUST stop FIRST so no new mutating LCM call
	// can land while the engine is winding down.
	if res.Control != nil {
		steps = append(steps, runtime.DrainStep{
			Name: "control",
			Stop: func(ctx context.Context) error { return res.Control.Stop(ctx) },
		})
	}
	// Feature 003: drain the fan-out responder right after the control
	// plane stops so no in-flight /v1/status enrichment can race with
	// the NATS conn teardown that follows in the embedded-nats step.
	if res.Fanout != nil {
		steps = append(steps, runtime.DrainStep{
			Name: "fanout",
			Stop: func(_ context.Context) error { return res.Fanout.Close() },
		})
	}
	if res.ObsSrv != nil {
		steps = append(steps, runtime.DrainStep{
			Name: "obs",
			Stop: func(ctx context.Context) error { return res.ObsSrv.Stop(ctx) },
		})
	}
	if res.Manager != nil {
		steps = append(steps, runtime.DrainStep{
			Name: "manager",
			Stop: func(ctx context.Context) error { return res.Manager.Stop(ctx) },
		})
	}
	if res.Cluster != nil {
		steps = append(steps, runtime.DrainStep{
			Name: "cluster",
			Stop: func(_ context.Context) error {
				res.Cluster.Close(res.Logger)
				return nil
			},
		})
	}
	for _, sub := range res.EventSub {
		sub := sub
		steps = append(steps, runtime.DrainStep{
			Name: "event-subscription",
			Stop: func(_ context.Context) error { return sub.Unsubscribe() },
		})
	}
	// Feature 002: embedded NATS server drains LAST. Order matters —
	// pg-manager adapters must release their leadership lease through
	// the embedded server before the server exits, otherwise the lease
	// stays held in the cluster's view until expiry
	// (contracts/lifecycle.md § Shutdown sequence).
	if res.Embedded != nil {
		steps = append(steps, runtime.DrainStep{
			Name: "embedded-nats",
			Stop: func(ctx context.Context) error { return res.Embedded.Shutdown(ctx) },
		})
	}
	return steps
}

func isSingletonError(err error) bool {
	if err == nil {
		return false
	}
	// pg-manager exposes singleton errors via the manager package; until
	// we wire a typed sentinel here, match on the documented message
	// fragment used by manager.Manager.Start. Treat every error
	// containing "singleton" as the singleton-claim budget exhaustion.
	return strings.Contains(err.Error(), "singleton")
}

// emitRedactedConfig writes the merged config to stdout with secret
// fields replaced by ***REDACTED*** (FR-017 / contracts/config.md).
func emitRedactedConfig(cfg config.Config) {
	type redactedAuth struct {
		TokenEnv         string `yaml:"token_env" json:"token_env"`
		TokenFile        string `yaml:"token_file" json:"token_file"`
		AllowUnauthReads bool   `yaml:"allow_unauth_reads" json:"allow_unauth_reads"`
	}
	out := struct {
		Cluster  any            `json:"cluster"`
		Node     any            `json:"node"`
		Peers    []string       `json:"peers"`
		NATS     any            `json:"nats"`
		Proxy    any            `json:"proxy"`
		Postgres any            `json:"postgres"`
		Topology any            `json:"topology"`
		Policy   any            `json:"policy"`
		Obs      any            `json:"obs"`
		Control  map[string]any `json:"control"`
		Shutdown any            `json:"shutdown"`
		Note     string         `json:"_note"`
	}{
		Cluster:  cfg.Cluster,
		Node:     cfg.Node,
		Peers:    cfg.Peers,
		NATS:     cfg.NATS,
		Proxy:    cfg.Proxy,
		Postgres: cfg.Postgres,
		Topology: cfg.Topology,
		Policy:   cfg.Policy,
		Obs:      cfg.Obs,
		Control: map[string]any{
			"listen_addr":          cfg.Control.ListenAddr,
			"leader_route_mode":    cfg.Control.LeaderRouteMode,
			"leader_route_timeout": cfg.Control.LeaderRouteTimeout.String(),
			"auth": redactedAuth{
				TokenEnv:         cfg.Control.Auth.TokenEnv,
				TokenFile:        cfg.Control.Auth.TokenFile,
				AllowUnauthReads: cfg.Control.Auth.AllowUnauthReads,
			},
			"tls": map[string]any{
				"cert_file":              cfg.Control.TLS.CertFile,
				"key_file":               cfg.Control.TLS.KeyFile,
				"plaintext_explicit_ack": cfg.Control.TLS.PlaintextExplicitAck,
			},
		},
		Shutdown: cfg.Shutdown,
		Note:     "secret values are sourced via *_env / *_file indirection; nothing here contains plaintext credentials",
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

func printVersionInfo() {
	fmt.Printf("pgman-proxy %s\n", version)
	if commit != "" {
		fmt.Printf("commit:  %s\n", commit)
	}
	if date != "" {
		fmt.Printf("date:    %s\n", date)
	}
	fmt.Printf("go:      %s\n", goVersion())
	fmt.Printf("license: Apache-2.0\n")
}

func goVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		return info.GoVersion
	}
	return "unknown"
}
