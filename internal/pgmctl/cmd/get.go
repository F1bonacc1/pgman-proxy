package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/output"
)

// knownGetResources is the canonical set of resource kinds the v1
// `get` / `list` / `describe` commands handle. The order is the order
// `get --help` prints them in.
//
// Resources backed by the history stream (events, audit) are added in
// Phase 4 (US2); a resource referenced before its server-side support
// lands surfaces a clear "deferred to feature 003 Phase 4" error.
var knownGetResources = []string{
	"nodes", "peers",
	"slots",
	"topology",
	"version",
	"events", "audit", // deferred — friendly error
	"config", // deferred — server endpoint not yet available
}

func newGetCmd(app *AppContext) *cobra.Command {
	c := &cobra.Command{
		Use:   "get <resource> [<name>]",
		Short: "Get a resource (nodes, peers, slots, topology, version, events, audit, config)",
		Long: `Fetch a single resource kind. Use one of:

  nodes, peers     — peer table from /v1/status
  slots            — replication slots from /v1/diagnose
  topology         — cluster topology tree (derived from /v1/status)
  version          — client + server versions (uses /v1/version when present)
  events, audit    — DEFERRED (added in feature 003 Phase 4 — history stream)
  config           — DEFERRED (server-side GET /v1/config not yet implemented)
`,
		Args: cobra.MinimumNArgs(1),
		ValidArgsFunction: func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			return knownGetResources, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGet(cmd, app, args, false)
		},
	}
	return c
}

func newListCmd(app *AppContext) *cobra.Command {
	c := &cobra.Command{
		Use:   "list <resource>",
		Short: "List a resource (alias of `get` for collection-shaped resources)",
		Long: `Identical to ` + "`pgmctl get <resource>`" + ` for collection-shaped resources.
Provided so operators don't have to remember whether a resource is a
collection or a singleton.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGet(cmd, app, args, false)
		},
	}
	return c
}

func newDescribeCmd(app *AppContext) *cobra.Command {
	c := &cobra.Command{
		Use:   "describe <resource>[/<name>]",
		Short: "Verbose form of `get` — emits the full record set",
		Long: `Verbose variant of get. The table format is the same; -o json / yaml
emits the full struct including fields normally suppressed in the
narrow column set.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGet(cmd, app, args, true)
		},
	}
	return c
}

// runGet dispatches a single resource kind. Resource and optional name
// are taken from args.
func runGet(cmd *cobra.Command, app *AppContext, args []string, verbose bool) error {
	resource := strings.ToLower(args[0])
	var name string
	// Accept "resource/name" or "resource name" forms.
	if i := strings.Index(resource, "/"); i >= 0 {
		name = resource[i+1:]
		resource = resource[:i]
	} else if len(args) >= 2 {
		name = args[1]
	}

	if err := app.Setup(); err != nil {
		// `get version` is the one exception: it works offline.
		if resource != "version" {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), commandTimeout(app))
	defer cancel()

	switch resource {
	case "nodes", "peers":
		return getNodes(ctx, cmd, app, name, verbose)
	case "slots":
		return getSlots(ctx, cmd, app, name)
	case "topology":
		return getTopology(ctx, cmd, app)
	case "version":
		return getVersion(ctx, cmd, app)
	case "events", "audit":
		category := "event"
		if resource == "audit" {
			category = "audit"
		}
		return runHistory(cmd, app, category, eventsFlags{since: 30 * time.Minute, limit: 1000})
	case "config":
		return WithExitCode(ExitUsage, fmt.Errorf("resource \"config\" needs server-side GET /v1/config; not yet implemented. Use `pgmctl config view` to inspect the local pgmctl config"))
	default:
		return WithExitCode(ExitUsage, fmt.Errorf("unknown resource %q (valid: %s)", resource, strings.Join(knownGetResources, ", ")))
	}
}

// getNodes/peers — table of NodeID / Role / State / PostgresUp / Lag.
func getNodes(ctx context.Context, cmd *cobra.Command, app *AppContext, name string, verbose bool) error {
	env, err := app.Client.GetJSON(ctx, "/v1/status")
	if err != nil {
		return err
	}
	engine, _, err := decodeStatusEngine(env.EngineResult)
	if err != nil {
		return err
	}
	rows := append([]instanceStatus(nil), engine.Instances...)
	if name != "" {
		filtered := rows[:0]
		for _, r := range rows {
			if r.NodeID == name {
				filtered = append(filtered, r)
			}
		}
		rows = filtered
		if len(rows) == 0 {
			return WithExitCode(ExitUnhealthy, fmt.Errorf("no node named %q in cluster %q", name, engine.ClusterID))
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].NodeID < rows[j].NodeID })

	switch app.Format {
	case output.FormatJSON:
		return output.EmitJSON(cmd.OutOrStdout(), "NodeList", rows)
	case output.FormatYAML:
		return output.EmitYAML(cmd.OutOrStdout(), "NodeList", rows)
	}

	if verbose || app.Format == output.FormatWide {
		t := output.NewTable("NODE", "ROLE", "STATE", "POSTGRES", "LAG", "REPLAY LSN", "WRITE LSN", "LAST SEEN")
		for _, r := range rows {
			t.AddRow(
				r.NodeID, strings.ToLower(r.Role), strings.ToLower(r.State),
				boolText(r.PostgresUp), formatBytes(r.LagBytes),
				fmt.Sprintf("%x", r.ReplayLSN), fmt.Sprintf("%x", r.WriteLSN),
				lastSeen(r.LastSeenAt),
			)
		}
		return t.Render(cmd.OutOrStdout())
	}
	t := output.NewTable("NODE", "ROLE", "STATE", "POSTGRES", "LAG")
	for _, r := range rows {
		t.AddRow(r.NodeID, strings.ToLower(r.Role), strings.ToLower(r.State), boolText(r.PostgresUp), formatBytes(r.LagBytes))
	}
	return t.Render(cmd.OutOrStdout())
}

// getSlots — passthrough of /v1/diagnose's replication-slot block.
// Implementation note: pg-manager's Diagnosis doesn't carry slot
// inventory explicitly today; the issues list is the closest proxy.
// We surface the diagnose payload verbatim under -o json/yaml; table
// form filters to slot-tagged issues.
func getSlots(ctx context.Context, cmd *cobra.Command, app *AppContext, name string) error {
	env, err := app.Client.GetJSON(ctx, "/v1/diagnose")
	if err != nil {
		return err
	}
	var d diagnosisShape
	if err := json.Unmarshal(env.EngineResult, &d); err != nil {
		return err
	}

	switch app.Format {
	case output.FormatJSON:
		return output.EmitJSON(cmd.OutOrStdout(), "DiagnosisSlots", d)
	case output.FormatYAML:
		return output.EmitYAML(cmd.OutOrStdout(), "DiagnosisSlots", d)
	}

	t := output.NewTable("COMPONENT", "SEVERITY", "MESSAGE", "SUGGESTION")
	any := false
	for _, i := range d.Issues {
		if !strings.Contains(strings.ToLower(i.Component), "slot") && !strings.Contains(strings.ToLower(i.Component), "wal") && name == "" {
			continue
		}
		if name != "" && !strings.Contains(i.Message, name) && !strings.Contains(i.Component, name) {
			continue
		}
		any = true
		t.AddRow(i.Component, severityFromPGManager(i.Severity).Marker(), i.Message, i.Suggestion)
	}
	if !any {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "no slot-related issues")
		return err
	}
	return t.Render(cmd.OutOrStdout())
}

// getTopology delegates to the topology command's payload builder so
// the two views stay consistent.
func getTopology(ctx context.Context, cmd *cobra.Command, app *AppContext) error {
	env, err := app.Client.GetJSON(ctx, "/v1/status")
	if err != nil {
		return err
	}
	engine, embedded, err := decodeStatusEngine(env.EngineResult)
	if err != nil {
		return err
	}
	payload := buildTopologyPayload(engine, embedded)
	switch app.Format {
	case output.FormatJSON:
		return output.EmitJSON(cmd.OutOrStdout(), "Topology", payload)
	case output.FormatYAML:
		return output.EmitYAML(cmd.OutOrStdout(), "Topology", payload)
	default:
		renderTopologyTree(cmd.OutOrStdout(), app.Color, payload)
		return nil
	}
}

// getVersion is the schema-versioned form for `get version`.
func getVersion(ctx context.Context, cmd *cobra.Command, app *AppContext) error {
	payload := versionPayload{
		Client: clientVersion{
			Version:   app.Build.Version,
			Commit:    app.Build.Commit,
			GoVersion: runtime.Version(),
		},
	}
	if app.Client != nil {
		if sv, err := app.Client.FetchVersion(ctx); err == nil && sv != nil {
			payload.Server = &serverVersion{Version: sv.Version, Commit: sv.Commit, NATS: sv.NATS}
		}
	}
	switch app.Format {
	case output.FormatJSON:
		return output.EmitJSON(cmd.OutOrStdout(), "Version", payload)
	case output.FormatYAML:
		return output.EmitYAML(cmd.OutOrStdout(), "Version", payload)
	default:
		return renderVersionTable(cmd.OutOrStdout(), payload, app)
	}
}

// boolText returns the human-readable form of a bool ("up"/"down").
func boolText(b bool) string {
	if b {
		return "up"
	}
	return "down"
}

func lastSeen(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("15:04:05Z")
}

