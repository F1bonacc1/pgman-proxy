package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/output"
	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/watch"
)

// newWatchCmd builds the `pgmctl watch ...` subtree per
// contracts/cli-commands.md § watch. Four subcommands; -o json/yaml
// rejected because watch is a TTY-oriented live view (use
// `events --since 0` for machine consumption).
func newWatchCmd(app *AppContext) *cobra.Command {
	c := &cobra.Command{
		Use:   "watch",
		Short: "Live views of cluster state via Server-Sent Events",
		Long: `Subscribe to the cluster control plane's /v1/watch/* SSE endpoints
and render a live view that updates as state changes.

Subcommands:
  status        Fixed-line cluster summary, redrawn on every change.
  transitions   Append-only state-transition log.
  events        Append-only history-event log (filterable).
  node <id>     Append-only stream of one node's events.

Watch streams reconnect automatically with exponential backoff. A
gap_marker line indicates the stream may have missed events.

For machine-readable consumption use 'pgmctl events --since 0' instead
of redirecting watch output; -o json/yaml is rejected here.`,
	}
	c.AddCommand(
		newWatchStatusCmd(app),
		newWatchTransitionsCmd(app),
		newWatchEventsCmd(app),
		newWatchNodeCmd(app),
	)
	return c
}

func newWatchStatusCmd(app *AppContext) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Live cluster status (fixed-line redraw)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := rejectStructuredOutput(app); err != nil {
				return err
			}
			if err := app.Setup(); err != nil {
				return err
			}
			return runWatchStatus(cmd.Context(), cmd, app)
		},
	}
}

func newWatchTransitionsCmd(app *AppContext) *cobra.Command {
	return &cobra.Command{
		Use:   "transitions",
		Short: "Live state-transition stream",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := rejectStructuredOutput(app); err != nil {
				return err
			}
			if err := app.Setup(); err != nil {
				return err
			}
			return runWatchAppend(cmd.Context(), cmd, app, "/v1/watch/transitions")
		},
	}
}

func newWatchEventsCmd(app *AppContext) *cobra.Command {
	var (
		typeFilter []string
		nodeFilter []string
		since      time.Duration
	)
	c := &cobra.Command{
		Use:   "events",
		Short: "Live history-event stream (filterable)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := rejectStructuredOutput(app); err != nil {
				return err
			}
			if err := app.Setup(); err != nil {
				return err
			}
			query := []string{}
			for _, t := range typeFilter {
				query = append(query, "type="+t)
			}
			for _, n := range nodeFilter {
				query = append(query, "node="+n)
			}
			if since > 0 {
				query = append(query, "since="+since.String())
			}
			path := "/v1/watch/events"
			if len(query) > 0 {
				path += "?" + strings.Join(query, "&")
			}
			return runWatchAppend(cmd.Context(), cmd, app, path)
		},
	}
	c.Flags().StringSliceVar(&typeFilter, "type", nil, "Filter to specific event types (repeatable)")
	c.Flags().StringSliceVar(&nodeFilter, "node", nil, "Filter to specific node ids (repeatable)")
	c.Flags().DurationVar(&since, "since", 0, "Replay events newer than this duration before live tail begins")
	return c
}

func newWatchNodeCmd(app *AppContext) *cobra.Command {
	return &cobra.Command{
		Use:   "node <node-id>",
		Short: "Live event stream filtered to a single node",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rejectStructuredOutput(app); err != nil {
				return err
			}
			if err := app.Setup(); err != nil {
				return err
			}
			return runWatchAppend(cmd.Context(), cmd, app, "/v1/watch/node/"+args[0])
		},
	}
}

// rejectStructuredOutput rejects -o json / -o yaml. Watch is a TTY-
// oriented live view; redirecting it through jq is a foot-gun.
func rejectStructuredOutput(app *AppContext) error {
	if app.Format == output.FormatJSON || app.Format == output.FormatYAML {
		return WithExitCode(ExitUsage, fmt.Errorf("pgmctl watch does not support -o %s; use 'pgmctl events --since 0' for machine-readable consumption", app.Format))
	}
	return nil
}

// runWatchStatus owns the status redraw loop.
func runWatchStatus(ctx context.Context, cmd *cobra.Command, app *AppContext) error {
	out := cmd.OutOrStdout()
	renderer := &watch.StatusRenderer{
		W:        out,
		GreenFn:  app.Color.Green,
		YellowFn: app.Color.Yellow,
		RedFn:    app.Color.Red,
	}

	// Seed the initial snapshot with a one-shot /v1/status so the
	// first paint happens immediately even before any history event
	// fires.
	snap, _ := fetchStatusFrame(ctx, app)
	if snap != nil {
		_ = renderer.Render(*snap)
	}

	streamer := watch.NewStreamer(func(c context.Context, lastID string) (*http.Response, error) {
		headers := map[string]string{}
		if lastID != "" {
			headers["Last-Event-ID"] = lastID
		}
		return app.Client.StreamSSE(c, "/v1/watch/status", headers)
	})

	tail := watch.TailOptions{
		OnReconnect: func(attempt int, delay time.Duration) {
			_, _ = fmt.Fprintln(out, app.Color.Yellow(fmt.Sprintf("[reconnect attempt %d — sleeping %s]", attempt, delay)))
		},
		OnGap: func(reason string) {
			_, _ = fmt.Fprintln(out, app.Color.Yellow(fmt.Sprintf("[gap_marker: %s — refreshing snapshot]", reason)))
			renderer.Reset()
		},
	}
	frames, errs := watch.Tail(ctx, streamer, tail)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case f, ok := <-frames:
			if !ok {
				if err := <-errs; err != nil {
					return WithExitCode(ExitNetwork, err)
				}
				return nil
			}
			if f.Event != "status_update" {
				continue
			}
			var sf watch.StatusFrame
			if err := json.Unmarshal([]byte(f.Data), &sf); err != nil {
				continue
			}
			_ = renderer.Render(sf)
		}
	}
}

// runWatchAppend drives an append-only stream renderer (transitions,
// events, node).
func runWatchAppend(ctx context.Context, cmd *cobra.Command, app *AppContext, path string) error {
	out := cmd.OutOrStdout()
	renderer := &watch.AppendRenderer{
		W:        out,
		YellowFn: app.Color.Yellow,
		RedFn:    app.Color.Red,
	}

	streamer := watch.NewStreamer(func(c context.Context, lastID string) (*http.Response, error) {
		headers := map[string]string{}
		if lastID != "" {
			headers["Last-Event-ID"] = lastID
		}
		return app.Client.StreamSSE(c, path, headers)
	})

	tail := watch.TailOptions{
		OnReconnect: func(attempt int, delay time.Duration) {
			renderer.RenderReconnectAttempt(attempt, delay)
		},
		OnGap: func(_ string) {
			// AppendRenderer's gap_marker print is the divider in
			// Render; nothing to do here.
		},
	}
	frames, errs := watch.Tail(ctx, streamer, tail)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case f, ok := <-frames:
			if !ok {
				if err := <-errs; err != nil {
					return WithExitCode(ExitNetwork, err)
				}
				return nil
			}
			_ = renderer.Render(f)
		}
	}
}

// fetchStatusFrame does a one-shot /v1/status fetch and translates
// the engine_result envelope into a StatusFrame the watch renderer
// can consume. Returns nil on failure (the live stream may still
// recover).
func fetchStatusFrame(ctx context.Context, app *AppContext) (*watch.StatusFrame, error) {
	env, err := app.Client.GetJSON(ctx, "/v1/status")
	if err != nil {
		return nil, err
	}
	// Status engine_result is either a raw pgmanager.Status or
	// `{engine, embedded_nats}`. Probe both shapes (same logic as
	// status.go).
	var probe struct {
		Engine       *json.RawMessage `json:"engine,omitempty"`
		EmbeddedNATS *json.RawMessage `json:"embedded_nats,omitempty"`
	}
	if jerr := json.Unmarshal(env.EngineResult, &probe); jerr != nil {
		return nil, jerr
	}
	body := env.EngineResult
	if probe.Engine != nil {
		body = *probe.Engine
	}
	var sf watch.StatusFrame
	if jerr := json.Unmarshal(body, &sf); jerr != nil {
		return nil, jerr
	}
	if probe.EmbeddedNATS != nil {
		var emb watch.StatusEmbeddedNATS
		if jerr := json.Unmarshal(*probe.EmbeddedNATS, &emb); jerr == nil {
			sf.EmbeddedNATS = &emb
		}
	}
	return &sf, nil
}
