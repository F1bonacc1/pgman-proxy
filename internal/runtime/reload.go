package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	pgmanager "github.com/f1bonacc1/pg-manager"
	"github.com/nats-io/nats-server/v2/server"

	"github.com/f1bonacc1/pgman-proxy/internal/config"
	"github.com/f1bonacc1/pgman-proxy/internal/embedded"
	"github.com/f1bonacc1/pgman-proxy/internal/obs"
)

// ReloadHandler bundles the state needed to service a SIGHUP signal
// (FR-014a / RD-001a). The host installs one of these alongside its
// Start() result and calls Wait() in a goroutine; on each SIGHUP the
// handler re-reads configuration and applies the
// embedded.ReloadDiff to the running server.
type ReloadHandler struct {
	// CurrentCfg is the in-memory config; updated in place after a
	// successful reload (so subsequent SIGHUPs diff against the new
	// state). The host MUST NOT mutate it concurrently.
	CurrentCfg *config.Config

	// LoadOpts captures the original load inputs — yaml path, env
	// reader, original CLI flags — so the reload re-reads the same
	// surface the operator originally configured.
	LoadOpts config.LoadOptions

	// Embedded is the running embedded NATS server.
	Embedded *embedded.Server

	// Logger surfaces the structured `embedded_nats.reload_applied`
	// event documented in contracts/observability.md.
	Logger *obs.Logger

	// Metrics is the project's metric set; the handler bumps
	// `pgman_proxy_embedded_nats_sighup_reload_outcomes_total` on
	// every reload outcome.
	Metrics *obs.MetricSet
}

// Wait listens for SIGHUP until the supplied context is cancelled.
// Each SIGHUP triggers a re-read of configuration, a diff against the
// current in-memory state, and (when the diff is non-empty) a
// `srv.ReloadOptions` call that applies the route-list / password
// changes without restarting the embedded server.
//
// Non-allow-listed key changes are surfaced as a structured warning
// and ignored at the NATS level — the in-memory config does not
// advance them.
func (h *ReloadHandler) Wait(ctx context.Context) {
	if h == nil || h.Embedded == nil {
		return
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	defer signal.Stop(ch)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			if err := h.applyOnce(); err != nil {
				h.Logger.Warn("embedded_nats.reload_failed",
					pgmanager.Field{Key: "error", Value: err.Error()})
				h.bumpOutcome("error")
				continue
			}
		}
	}
}

// applyOnce performs a single reload pass. Exposed for tests.
func (h *ReloadHandler) applyOnce() error {
	// Re-read configuration via the same loader the host originally
	// used. Reusing LoadOpts ensures we read the YAML / env in the
	// same order and respect the same flag overrides.
	newCfg, _, err := config.Load(h.LoadOpts)
	if err != nil {
		return fmt.Errorf("config re-read: %w", err)
	}
	if vErr := config.Validate(newCfg); vErr != nil {
		return fmt.Errorf("config re-validate: %w", vErr)
	}

	// Resolve the cluster password from the (possibly rotated) source.
	oldPw, err := resolveClusterPassword(h.CurrentCfg.Cluster)
	if err != nil {
		oldPw = "" // tolerate missing old; the diff is still meaningful
	}
	newPw, err := resolveClusterPassword(newCfg.Cluster)
	if err != nil {
		return fmt.Errorf("resolve new cluster password: %w", err)
	}

	oldOpts, err := optionsFromConfig(*h.CurrentCfg, oldPw)
	if err != nil {
		return fmt.Errorf("build old options: %w", err)
	}
	newOpts, err := optionsFromConfig(newCfg, newPw)
	if err != nil {
		return fmt.Errorf("build new options: %w", err)
	}

	diff, err := embedded.ComputeDiff(embedded.ReloadInputs{
		OldOpts:        oldOpts,
		NewOpts:        newOpts,
		OldPasswordRaw: oldPw,
		NewPasswordRaw: newPw,
	})
	if err != nil {
		return fmt.Errorf("compute diff: %w", err)
	}

	if diff.IsEmpty() {
		h.bumpOutcome("applied") // no-op reload still counts as success
		return nil
	}

	// Apply only when there's an allow-listed change. Skipped-keys
	// alone (no routes/password change) yields a "partial_skipped"
	// outcome with no NATS-level reload.
	hasAllowListed := len(diff.RoutesAdded) > 0 || len(diff.RoutesRemoved) > 0 || diff.PasswordRotated
	if hasAllowListed {
		if err := h.Embedded.Reload(newOpts); err != nil {
			return fmt.Errorf("nats reload: %w", err)
		}
	}

	// Update in-memory config to reflect ONLY the applied changes.
	if hasAllowListed {
		h.CurrentCfg.Cluster.RoutePeers = newCfg.Cluster.RoutePeers
		// The password isn't stored on the struct (it's resolved
		// per-call from SecretRef) — nothing to copy.
	}

	// Surface the diff via the structured-log pipeline.
	h.Embedded.EmitReloadApplied(
		diff.RoutesAdded,
		diff.RoutesRemoved,
		diff.PasswordRotated,
		diff.PasswordOldPrefix,
		diff.PasswordNewPrefix,
		diff.SkippedKeys,
		diff.SkippedReason,
	)

	if len(diff.SkippedKeys) > 0 && hasAllowListed {
		h.bumpOutcome("partial_skipped")
	} else if len(diff.SkippedKeys) > 0 {
		h.bumpOutcome("partial_skipped")
	} else {
		h.bumpOutcome("applied")
	}
	return nil
}

// bumpOutcome increments the SIGHUP-reload outcome counter when the
// host wired one. Tolerates a nil MetricSet so unit tests can run
// without a registry.
func (h *ReloadHandler) bumpOutcome(result string) {
	if h == nil || h.Metrics == nil || h.Metrics.EmbeddedNATSReloadOutcomes == nil {
		return
	}
	h.Metrics.EmbeddedNATSReloadOutcomes.WithLabelValues(result).Inc()
}

// optionsFromConfig is a small adapter from `config.Config` to the
// embedded `OptionsInput` builder. Mirrors `bootEmbeddedNATS` in
// start.go; keeps the diff computation honest about what BuildOptions
// will produce.
func optionsFromConfig(cfg config.Config, password string) (*server.Options, error) {
	cc := cfg.Cluster
	var cred embedded.ClusterCredential
	if cc.Username != "" || password != "" {
		c, err := embedded.LoadClusterCredential([]byte(cc.Username), []byte(password))
		if err != nil {
			return nil, fmt.Errorf("load cluster credential: %w", err)
		}
		cred = c
	}
	routesEnabled := cc.RoutesListen.Enabled && cc.DeclaredSize > 1
	in := embedded.OptionsInput{
		NodeID:               cfg.Node.ID,
		ClusterName:          firstNonEmptyReload(cc.Name, cc.ID),
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
		return nil, err
	}
	return opts, nil
}

func firstNonEmptyReload(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ErrReloadHandlerNotInstalled is returned when the host calls Reload
// before installing a handler. Currently unused at runtime; kept here
// so test code has a sentinel to assert against.
var ErrReloadHandlerNotInstalled = errors.New("reload handler not installed")
