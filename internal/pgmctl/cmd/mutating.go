package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/client"
	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/confirm"
)

// Mutating ops (US6 / contracts/cli-commands.md):
//   single-resource:        fence, unfence, set-config
//   cluster-affecting:      failover, switchover, promote, restart, delete
//
// The clearest semantic split lives in the prompt shape — single-
// resource ops get [y/N], cluster-affecting ops require the typed
// cluster name (or --force --cluster <name>). FR-039: every accepted
// op prints its request_id to stdout so operators can correlate with
// `pgmctl get audit --request-id <id>`.

// printRequestID writes "request_id=<id>" to stdout (FR-039). When
// the envelope carries no request id (server-side bug), it prints a
// warning so the operator notices.
func printRequestID(cmd *cobra.Command, env *client.Envelope) {
	w := cmd.OutOrStdout()
	if env == nil || env.RequestID == "" {
		_, _ = fmt.Fprintln(w, "warning: server did not return a request_id; audit correlation will be impossible")
		return
	}
	_, _ = fmt.Fprintf(w, "request_id=%s\n", env.RequestID)
}

// singleResourcePrompt wraps confirm.Prompt with the global --yes
// flag. Returns nil on accept / ErrRefused on reject / ErrNotTTY when
// the caller has neither --yes nor a TTY.
func singleResourcePrompt(app *AppContext, op, target string) error {
	cluster := app.Flags.Cluster
	if cluster == "" && app.Resolved != nil {
		cluster = app.Resolved.ExpectedCluster
	}
	return confirm.Prompt(os.Stdin, os.Stderr, op, target, cluster, app.Flags.Yes)
}

// clusterAffectingPrompt wraps confirm.ConfirmClusterName with the
// global --force / --cluster pair. Cluster-affecting ops MUST have a
// known cluster id; we read it from the active context or --cluster.
func clusterAffectingPrompt(app *AppContext, op, target string) error {
	expected := app.Flags.Cluster
	if expected == "" && app.Resolved != nil {
		expected = app.Resolved.ExpectedCluster
	}
	if expected == "" {
		return WithExitCode(ExitConfig, errors.New("cluster-affecting operations require an expected cluster id; set --cluster or activate a context with expected_cluster"))
	}
	matched := app.Flags.Cluster == expected
	return confirm.ConfirmClusterName(os.Stdin, os.Stderr, op, target, expected, app.Flags.Force, matched)
}

// --- fence / unfence ---------------------------------------------------

func newFenceCmd(app *AppContext) *cobra.Command {
	return &cobra.Command{
		Use:   "fence <node-id>",
		Short: "Add a node to the cluster fence list (blocks future promotion)",
		Long: `Fence a single peer so the reconciler refuses to PROMOTE it.

Fence is a promotion-eligibility marker, not a failover: it does NOT
demote a running node or move writes off it. pg-manager preserves the
node's role on fence, so fencing the current primary would leave a
primary that is simultaneously serving writes and marked ineligible (an
incoherent state — 'pgmctl doctor' then reports "no primary observed").
Fencing the current primary is therefore refused; use 'pgmctl failover'
or 'pgmctl switchover --target <peer>' to move writes off the primary
first, then fence the old primary once it is a standby. Pass --force to
fence the primary anyway.

Single-resource op; prompts [y/N] unless --yes is set.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFenceOp(cmd, app, "fence", args[0], "/v1/fence")
		},
	}
}

func newUnfenceCmd(app *AppContext) *cobra.Command {
	return &cobra.Command{
		Use:   "unfence <node-id>",
		Short: "Remove a node from the cluster fence list",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFenceOp(cmd, app, "unfence", args[0], "/v1/unfence")
		},
	}
}

func runFenceOp(cmd *cobra.Command, app *AppContext, op, target, path string) error {
	if err := app.Setup(); err != nil {
		return err
	}
	// Guard the fence-the-primary footgun (not unfence — leaving the
	// fence list is always safe). Fencing the live primary is a no-op
	// for writes and produces an incoherent snapshot; refuse unless
	// --force. See fenceCurrentPrimaryGuard.
	if op == "fence" {
		if err := fenceCurrentPrimaryGuard(cmd, app, target); err != nil {
			return err
		}
	}
	if err := singleResourcePrompt(app, op, target); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), commandTimeout(app))
	defer cancel()
	body := map[string]any{"target": target, "request_id": client.NewRequestID()}
	env, err := app.Client.PostJSON(ctx, path, body)
	if err != nil {
		return err
	}
	printRequestID(cmd, env)
	return nil
}

// fenceCurrentPrimaryGuard refuses to fence the node that is currently
// the cluster primary, unless --force is set.
//
// Why: pg-manager's Fence is a *promotion-eligibility* marker, not a
// failover. EventOperatorFence preserves the node's role (it transitions
// state→Fenced with NewRole=curRole), the StateFenced act-phase is a
// no-op, and the data plane keeps routing writes to whoever is
// primary+leader — none of which the fence list touches. So fencing the
// live primary neither demotes it nor moves writes; it only blocks
// FUTURE promotion. The result is an incoherent snapshot — a node that
// is simultaneously serving writes and marked fenced — which is exactly
// why 'pgmctl doctor' then reports "no primary observed" while
// cluster.has-leader still passes. The correct tool to move writes off
// the primary is failover / switchover.
//
// Strictly additive and best-effort: if cluster status can't be fetched
// or decoded we warn and proceed, so any invocation that worked before
// this guard still works during a control-plane hiccup. The only new
// hard block is the provable case where status confirms the target IS
// the current primary.
func fenceCurrentPrimaryGuard(cmd *cobra.Command, app *AppContext, target string) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), commandTimeout(app))
	defer cancel()

	env, err := app.Client.GetJSON(ctx, "/v1/status")
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: could not fetch cluster status to check whether %q is the current primary (%v); proceeding with fence\n",
			target, err)
		return nil
	}
	engine, _, err := decodeStatusEngine(env.EngineResult)
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: could not decode cluster status to check whether %q is the current primary (%v); proceeding with fence\n",
			target, err)
		return nil
	}
	if engine.PrimaryNodeID != target {
		return nil
	}
	if app.Flags.Force {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: %q is the current primary; --force given, fencing anyway. This does NOT demote it or move writes — it only blocks future promotion, leaving a primary marked ineligible. Use `pgmctl failover`/`switchover` to actually move writes.\n",
			target)
		return nil
	}
	return WithExitCode(ExitUsage, fmt.Errorf(
		"%[1]q is the current primary — fencing will not demote it or move writes off it. "+
			"Fence only blocks FUTURE promotion, so this would leave a primary that is both "+
			"serving writes and marked fenced (`pgmctl doctor` then reports \"no primary observed\"). "+
			"Run `pgmctl failover` or `pgmctl switchover --target <peer>` to move writes off %[1]q first, "+
			"then fence it once it is a standby. Pass --force to fence the primary anyway",
		target))
}

// --- set-config --------------------------------------------------------

func newSetConfigCmd(app *AppContext) *cobra.Command {
	var key string
	c := &cobra.Command{
		Use:   "set-config --key <key>",
		Short: "Trigger an in-process reload of a hot-reload-allow-listed key",
		Long: `Re-reads the on-disk YAML and applies allow-listed changes.
The allow-list is intentionally narrow: cluster.route_peers, cluster.password.
Operators stage the change (YAML or secret rotation) and call this to apply.

Mirrors POST /v1/config/set; rejects disallowed keys with HTTP 400
set_config_key_disallowed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := app.Setup(); err != nil {
				return err
			}
			if key == "" {
				return WithExitCode(ExitUsage, errors.New("--key is required"))
			}
			// Client-side allow-list mirror (FR-014a) — refuse before
			// hitting the wire so operators see a fast error.
			if !clientSetConfigAllowList[key] {
				return WithExitCode(ExitUsage, fmt.Errorf("key %q is not in the hot-reload allow-list (peer routes + cluster password only)", key))
			}
			if err := singleResourcePrompt(app, "set-config", key); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), commandTimeout(app))
			defer cancel()
			body := map[string]any{"key": key, "request_id": client.NewRequestID()}
			env, err := app.Client.PostJSON(ctx, "/v1/config/set", body)
			if err != nil {
				return err
			}
			printRequestID(cmd, env)
			return nil
		},
	}
	c.Flags().StringVar(&key, "key", "", "Allow-listed config key (cluster.route_peers | cluster.password)")
	return c
}

// clientSetConfigAllowList mirrors the server-side allow-list (002
// FR-014a). Pinned client-side so a stale binary against a newer
// server still surfaces a fast refuse.
var clientSetConfigAllowList = map[string]bool{
	"cluster.route_peers": true,
	"cluster.password":    true,
}

// --- failover / switchover / promote -----------------------------------

func newFailoverCmd(app *AppContext) *cobra.Command {
	return &cobra.Command{
		Use:   "failover",
		Short: "Trigger an unplanned failover of the current primary",
		Long: `Unplanned failover: the cluster picks a new primary. Cluster-
affecting; requires typed cluster name (or --force --cluster <name>).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := app.Setup(); err != nil {
				return err
			}
			if err := clusterAffectingPrompt(app, "failover", ""); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), commandTimeout(app))
			defer cancel()
			body := map[string]any{"request_id": client.NewRequestID()}
			env, err := app.Client.PostJSON(ctx, "/v1/failover", body)
			if err != nil {
				return err
			}
			printRequestID(cmd, env)
			return nil
		},
	}
}

func newSwitchoverCmd(app *AppContext) *cobra.Command {
	var target string
	c := &cobra.Command{
		Use:   "switchover --target <node-id>",
		Short: "Gracefully promote a specific peer",
		Long: `Planned switchover to the named target. Cluster-affecting; requires
typed cluster name (or --force --cluster <name>).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := app.Setup(); err != nil {
				return err
			}
			if target == "" {
				return WithExitCode(ExitUsage, errors.New("--target is required"))
			}
			if err := clusterAffectingPrompt(app, "switchover", target); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), commandTimeout(app))
			defer cancel()
			body := map[string]any{"target": target, "request_id": client.NewRequestID()}
			env, err := app.Client.PostJSON(ctx, "/v1/switchover", body)
			if err != nil {
				return err
			}
			printRequestID(cmd, env)
			return nil
		},
	}
	c.Flags().StringVar(&target, "target", "", "Node id to promote")
	return c
}

func newPromoteCmd(app *AppContext) *cobra.Command {
	return &cobra.Command{
		Use:   "promote",
		Short: "Manually promote THIS peer (local-only override)",
		Long: `Manual override: ask the local peer to promote regardless of
consensus state. Cluster-affecting; requires typed cluster name.

The --endpoint flag determines which peer is promoted (Promote is
local-only per 001 — the operation acts on the peer receiving the
request).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := app.Setup(); err != nil {
				return err
			}
			if err := clusterAffectingPrompt(app, "promote", ""); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), commandTimeout(app))
			defer cancel()
			body := map[string]any{"request_id": client.NewRequestID()}
			env, err := app.Client.PostJSON(ctx, "/v1/promote", body)
			if err != nil {
				return err
			}
			printRequestID(cmd, env)
			return nil
		},
	}
}

// --- restart -----------------------------------------------------------

func newRestartCmd(app *AppContext) *cobra.Command {
	var target, targetNode string
	c := &cobra.Command{
		Use:   "restart --target=<postgres|proxy> [--target-node <id>]",
		Short: "Restart a peer's PostgreSQL or the pgman-proxy process itself",
		Long: `Two modes:

  --target=postgres
    Restarts the LOCAL Postgres on the peer the request lands on. Use
    --endpoint to direct the request to a specific peer (or rely on
    the active context). Engine emits state-transition events; if the
    target is the current primary, a failover may follow.

  --target=proxy
    Restarts the pgman-proxy process itself on the target peer. The
    peer MUST be running under a process supervisor (tini / systemd /
    k8s / process-compose) that will bring it back; otherwise the
    server returns 412 supervisor_not_detected.

Cluster-affecting in both modes; requires typed cluster name (or
--force --cluster <name>).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := app.Setup(); err != nil {
				return err
			}
			if target != "postgres" && target != "proxy" {
				return WithExitCode(ExitUsage, fmt.Errorf("--target must be 'postgres' or 'proxy', got %q", target))
			}
			if target == "proxy" && targetNode == "" {
				return WithExitCode(ExitUsage, errors.New("--target-node is required when --target=proxy (the receiving peer must match this id)"))
			}
			label := "restart " + target
			if targetNode != "" {
				label += " on " + targetNode
			}
			if err := clusterAffectingPrompt(app, label, targetNode); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), commandTimeout(app))
			defer cancel()
			body := map[string]any{
				"target":      target,
				"target_node": targetNode,
				"request_id":  client.NewRequestID(),
			}
			env, err := app.Client.PostJSON(ctx, "/v1/restart", body)
			if err != nil {
				return err
			}
			printRequestID(cmd, env)
			return nil
		},
	}
	c.Flags().StringVar(&target, "target", "postgres", "What to restart: postgres | proxy")
	c.Flags().StringVar(&targetNode, "target-node", "", "Node id (required for --target=proxy)")
	return c
}
