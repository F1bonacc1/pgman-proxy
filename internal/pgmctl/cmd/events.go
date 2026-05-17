package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/output"
)

// eventRow mirrors history.HistoryEvent on the wire. We don't import
// internal/history into pgmctl to keep the client a clean dep-free
// surface; field names match the documented wire form.
type eventRow struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	ID         string         `json:"id"`
	Time       time.Time      `json:"time"`
	Category   string         `json:"category"`
	Type       string         `json:"type"`
	ClusterID  string         `json:"cluster_id"`
	NodeID     string         `json:"node_id,omitempty"`
	Details    map[string]any `json:"details,omitempty"`
	TraceID    string         `json:"trace_id,omitempty"`
	SpanID     string         `json:"span_id,omitempty"`
}

type historyResult struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Events     []eventRow `json:"events"`
	NextCursor string     `json:"next_cursor,omitempty"`
	Truncated  bool       `json:"truncated"`
}

// eventsFlags is the shared filter set for `pgmctl events` and the
// `events`/`audit` resource forms of `pgmctl get`.
type eventsFlags struct {
	since     time.Duration
	until     string
	types     []string
	nodes     []string
	limit     int
	cursor    string
	category  string // event|audit|both ("" = both)
	listTypes bool   // group-by-type rollup over the window
}

func newEventsCmd(app *AppContext) *cobra.Command {
	var f eventsFlags
	c := &cobra.Command{
		Use:   "events",
		Short: "Tail the cluster's event history",
		Long: `Stream the cluster's event history from the JetStream-backed
history stream (feature 003 FR-016a) via GET /v1/history.

Defaults: --since 30m, --limit 1000.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := app.Setup(); err != nil {
				return err
			}
			return runHistory(cmd, app, "event", f)
		},
	}
	addEventsFlags(c, &f)
	return c
}

// addEventsFlags wires the shared filter flag set onto a cobra
// command. The `--category` flag is intentionally NOT registered
// here — both call sites (`pgmctl events` and `pgmctl get
// events/audit`) fix the category at the command level, so a flag
// would be confusing.
func addEventsFlags(c *cobra.Command, f *eventsFlags) {
	c.Flags().DurationVar(&f.since, "since", 30*time.Minute, "Only show events newer than this duration (e.g. 5m, 24h)")
	c.Flags().StringVar(&f.until, "until", "", "Only show events older than this RFC3339 timestamp")
	c.Flags().StringSliceVar(&f.types, "type", nil, "Filter to specific event types (repeatable)")
	c.Flags().StringSliceVar(&f.nodes, "node", nil, "Filter to specific node ids (repeatable)")
	c.Flags().IntVar(&f.limit, "limit", 1000, "Maximum number of events to return")
	c.Flags().StringVar(&f.cursor, "cursor", "", "Resume from after this event id (ULID)")
	c.Flags().BoolVar(&f.listTypes, "list-types", false, "Show the distinct event types observed in the window (with counts + last-seen)")
}

// runHistory dispatches a single GET /v1/history call with the given
// category override. If category is "", the runner uses f.category
// (which may itself be empty for "both categories").
func runHistory(cmd *cobra.Command, app *AppContext, categoryOverride string, f eventsFlags) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), commandTimeout(app))
	defer cancel()

	q := url.Values{}
	if f.since > 0 {
		q.Set("since", f.since.String())
	}
	if f.until != "" {
		q.Set("until", f.until)
	}
	for _, t := range f.types {
		q.Add("type", t)
	}
	for _, n := range f.nodes {
		q.Add("node", n)
	}
	if f.limit > 0 {
		q.Set("limit", strconv.Itoa(f.limit))
	}
	if f.cursor != "" {
		q.Set("cursor", f.cursor)
	}
	category := categoryOverride
	if category == "" {
		category = f.category
	}
	if category != "" {
		q.Set("category", category)
	}
	path := "/v1/history"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}

	env, err := app.Client.GetJSON(ctx, path)
	if err != nil {
		return err
	}
	var result historyResult
	if len(env.EngineResult) > 0 {
		if err := json.Unmarshal(env.EngineResult, &result); err != nil {
			return fmt.Errorf("decode /v1/history response: %w", err)
		}
	}

	if f.listTypes {
		return renderTypeRollup(cmd.OutOrStdout(), app, result)
	}

	switch app.Format {
	case output.FormatJSON:
		return output.EmitJSON(cmd.OutOrStdout(), "HistoryQueryResult", result)
	case output.FormatYAML:
		return output.EmitYAML(cmd.OutOrStdout(), "HistoryQueryResult", result)
	}
	return renderEventsTable(cmd.OutOrStdout(), result)
}

// typeRollupRow is one row of the --list-types output: one type, how
// many records carry it, and when the most recent one fired.
type typeRollupRow struct {
	Type     string    `json:"type" yaml:"type"`
	Count    int       `json:"count" yaml:"count"`
	LastSeen time.Time `json:"last_seen" yaml:"last_seen"`
}

type typeRollupResult struct {
	APIVersion string          `json:"apiVersion" yaml:"apiVersion"`
	Kind       string          `json:"kind" yaml:"kind"`
	Window     string          `json:"window" yaml:"window"`
	Total      int             `json:"total_events" yaml:"total_events"`
	Types      []typeRollupRow `json:"types" yaml:"types"`
}

// renderTypeRollup groups history records by type, counting and
// recording each type's most recent occurrence. Rows are sorted by
// count desc, then by type name asc. Output respects --output json/yaml.
func renderTypeRollup(w io.Writer, app *AppContext, r historyResult) error {
	counts := map[string]int{}
	lastSeen := map[string]time.Time{}
	for _, ev := range r.Events {
		counts[ev.Type]++
		if ev.Time.After(lastSeen[ev.Type]) {
			lastSeen[ev.Type] = ev.Time
		}
	}

	rows := make([]typeRollupRow, 0, len(counts))
	for t, n := range counts {
		rows = append(rows, typeRollupRow{Type: t, Count: n, LastSeen: lastSeen[t]})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		return rows[i].Type < rows[j].Type
	})

	result := typeRollupResult{
		APIVersion: "pgmctl/v1",
		Kind:       "HistoryTypeRollup",
		Total:      len(r.Events),
		Types:      rows,
	}

	switch app.Format {
	case output.FormatJSON:
		return output.EmitJSON(w, "HistoryTypeRollup", result)
	case output.FormatYAML:
		return output.EmitYAML(w, "HistoryTypeRollup", result)
	}

	if len(rows) == 0 {
		_, err := fmt.Fprintln(w, "no events in window")
		return err
	}
	t := output.NewTable("COUNT", "TYPE", "LAST SEEN")
	for _, row := range rows {
		t.AddRow(
			fmt.Sprintf("%d", row.Count),
			row.Type,
			row.LastSeen.UTC().Format(time.RFC3339),
		)
	}
	if err := t.Render(w); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "\n%d total event(s) over window; %d distinct type(s).\n",
		result.Total, len(rows))
	if r.Truncated {
		_, _ = fmt.Fprintln(w, "(result was truncated by --limit; counts cover only the returned slice)")
	}
	return nil
}

// renderEventsTable writes the documented `TIME TYPE NODE DETAILS`
// single-line-per-event form (contracts/cli-commands.md § events).
func renderEventsTable(w io.Writer, r historyResult) error {
	if len(r.Events) == 0 {
		_, err := fmt.Fprintln(w, "no events in window")
		return err
	}
	t := output.NewTable("TIME", "TYPE", "NODE", "DETAILS")
	for _, ev := range r.Events {
		t.AddRow(
			ev.Time.UTC().Format(time.RFC3339),
			ev.Type,
			ev.NodeID,
			summariseDetails(ev.Details),
		)
	}
	if err := t.Render(w); err != nil {
		return err
	}
	if r.Truncated {
		_, err := fmt.Fprintf(w, "\n(result truncated — next-cursor=%s; pass --cursor to resume)\n", r.NextCursor)
		return err
	}
	return nil
}

// summariseDetails compresses the per-event details map into a single
// "k=v k=v" string for the table view. -o json/yaml carries the full
// payload. Keys are sorted so identical events render identically —
// Go's map iteration order is randomized, so without an explicit sort
// the same record produces a different string on every render and an
// operator can't tell duplicates from genuine variants by eye.
func summariseDetails(d map[string]any) string {
	if len(d) == 0 {
		return ""
	}
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, d[k]))
	}
	return strings.Join(parts, " ")
}
