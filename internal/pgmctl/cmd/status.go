package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/output"
)

// pgmanagerStatus mirrors github.com/f1bonacc1/pg-manager.Status as
// returned over the wire. We decode into a local struct rather than
// importing pg-manager just for the JSON tags so pgmctl stays a thin
// client that doesn't depend on engine internals.
//
// Field names use Go-default CamelCase because the server's
// encoder/json default tagging is in play (handlers_read.go writes
// the engine Status verbatim).
type pgmanagerStatus struct {
	ClusterID          string           `json:"ClusterID"`
	LeaderNodeID       string           `json:"LeaderNodeID"`
	PrimaryNodeID      string           `json:"PrimaryNodeID"`
	LocalNodeID        string           `json:"LocalNodeID"`
	LocalRole          string           `json:"LocalRole"`
	LocalState         string           `json:"LocalState"`
	Instances          []instanceStatus `json:"Instances"`
	SyncStandbys       []string         `json:"SyncStandbys,omitempty"`
	LastFailoverAt     time.Time        `json:"LastFailoverAt"`
	LastFailoverReason string           `json:"LastFailoverReason,omitempty"`
}

type instanceStatus struct {
	NodeID     string    `json:"NodeID"`
	Role       string    `json:"Role"`
	State      string    `json:"State"`
	PostgresUp bool      `json:"PostgresUp"`
	ReplayLSN  uint64    `json:"ReplayLSN"`
	WriteLSN   uint64    `json:"WriteLSN"`
	FlushLSN   uint64    `json:"FlushLSN"`
	LagBytes   int64     `json:"LagBytes"`
	LastSeenAt time.Time `json:"LastSeenAt"`
}

// embeddedNATSSnapshot mirrors the 002 contracts/observability.md
// embedded_nats sub-block. We decode it for the "Mesh:" summary line
// but tolerate missing fields (single-peer test paths leave the
// snapshot off entirely).
type embeddedNATSSnapshot struct {
	ServerName        string `json:"server_name"`
	Ready             bool   `json:"ready"`
	ClientListenAddr  string `json:"client_listen_addr"`
	RoutesListenAddr  string `json:"routes_listen_addr"`
	TLSEnabled        bool   `json:"tls_enabled"`
	RoutesMeshed      int    `json:"routes_meshed"`
	ReplicasFactor    int    `json:"replicas_factor"`
}

func newStatusCmd(app *AppContext) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "One-glance cluster health",
		Long: `Render a compact summary of the connected cluster's health:
cluster id, leader, primary, peer count, embedded-NATS mesh state, per-peer
role/fence/lag/last-transition, and the time-of-snapshot.

Exit codes:
  0   healthy
  2   unhealthy (no primary / no leader / any failed peer)
  65  unreachable (connection / TLS / auth failure)
`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := app.Setup(); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), commandTimeout(app))
			defer cancel()

			env, err := app.Client.GetJSON(ctx, "/v1/status")
			if err != nil {
				return err
			}

			engine, embedded, err := decodeStatusEngine(env.EngineResult)
			if err != nil {
				return err
			}

			switch app.Format {
			case output.FormatJSON:
				return output.EmitJSON(cmd.OutOrStdout(), "ClusterStatus", statusJSON(engine, embedded))
			case output.FormatYAML:
				return output.EmitYAML(cmd.OutOrStdout(), "ClusterStatus", statusJSON(engine, embedded))
			default:
				code := renderStatus(cmd.OutOrStdout(), app, engine, embedded)
				if code != ExitOK {
					return WithExitCode(code, fmt.Errorf("cluster is not fully healthy"))
				}
				return nil
			}
		},
	}
}

// decodeStatusEngine handles both wire shapes of engine_result:
//
//  1. raw pgmanager.Status   — when no embedded-server snapshot is
//     wired on the proxy.
//  2. { engine: ..., embedded_nats: ... } — feature 002 form.
func decodeStatusEngine(raw json.RawMessage) (*pgmanagerStatus, *embeddedNATSSnapshot, error) {
	if len(raw) == 0 {
		return nil, nil, fmt.Errorf("empty engine_result from /v1/status")
	}
	var probe struct {
		Engine *json.RawMessage `json:"engine,omitempty"`
	}
	if err := json.Unmarshal(raw, &probe); err == nil && probe.Engine != nil {
		var p struct {
			Engine       pgmanagerStatus      `json:"engine"`
			EmbeddedNATS embeddedNATSSnapshot `json:"embedded_nats"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, nil, fmt.Errorf("decode 002 status envelope: %w", err)
		}
		return &p.Engine, &p.EmbeddedNATS, nil
	}
	var bare pgmanagerStatus
	if err := json.Unmarshal(raw, &bare); err != nil {
		return nil, nil, fmt.Errorf("decode raw engine status: %w", err)
	}
	return &bare, nil, nil
}

// statusJSON is the pgmctl/v1-versioned shape for -o json / -o yaml.
// Stable per FR-038.
type statusJSONShape struct {
	CapturedAt   time.Time             `json:"captured_at" yaml:"captured_at"`
	Cluster      *pgmanagerStatus      `json:"cluster" yaml:"cluster"`
	EmbeddedNATS *embeddedNATSSnapshot `json:"embedded_nats,omitempty" yaml:"embedded_nats,omitempty"`
}

func statusJSON(engine *pgmanagerStatus, embedded *embeddedNATSSnapshot) statusJSONShape {
	return statusJSONShape{
		CapturedAt:   time.Now().UTC(),
		Cluster:      engine,
		EmbeddedNATS: embedded,
	}
}

// renderStatus writes the table view and returns the exit code. The
// caller wraps the non-zero code via WithExitCode so the cobra error
// path picks it up cleanly.
func renderStatus(w io.Writer, app *AppContext, engine *pgmanagerStatus, embedded *embeddedNATSSnapshot) int {
	now := time.Now().UTC().Format("15:04:05Z")
	col := app.Color
	worst := output.SevPass

	cluster := engine.ClusterID
	if cluster == "" {
		cluster = "(unknown)"
	}

	primarySev, primaryStr := primaryHealth(engine)
	leaderSev, leaderStr := leaderHealth(engine)
	worst = worse(worst, primarySev, leaderSev)

	reachable, total := reachableCount(engine)
	peersSev := output.SevPass
	if reachable < total {
		peersSev = output.SevWarn
	}
	if reachable == 0 {
		peersSev = output.SevFail
	}
	worst = worse(worst, peersSev)
	peersStr := peersSev.Color(col, fmt.Sprintf("%d/%d reachable", reachable, total))

	meshStr := meshLine(col, embedded, total)

	fmt.Fprintf(w, "Cluster: %s\tSnapshot: %s\n", cluster, now)
	fmt.Fprintf(w, "Leader:  %s   Primary: %s   Peers: %s\n",
		leaderSev.Color(col, leaderStr),
		primarySev.Color(col, primaryStr),
		peersStr,
	)
	fmt.Fprintln(w, meshStr)
	fmt.Fprintln(w)

	t := output.NewTable("NODE", "ROLE", "FENCE", "LAG", "LAST TRANSITION")
	instances := append([]instanceStatus(nil), engine.Instances...)
	sort.SliceStable(instances, func(i, j int) bool { return instances[i].NodeID < instances[j].NodeID })
	for _, inst := range instances {
		sev := nodeSeverity(inst, engine)
		if sev == output.SevFail {
			worst = output.SevFail
		} else if sev == output.SevWarn && worst != output.SevFail {
			worst = output.SevWarn
		}
		fence := "-"
		if isFenced(inst.State) {
			fence = "yes"
		}
		lag := lagText(inst, engine)
		last := "-"
		if !inst.LastSeenAt.IsZero() {
			last = inst.LastSeenAt.UTC().Format("15:04:05Z")
		}
		t.AddRow(
			sev.Color(col, inst.NodeID),
			sev.Color(col, strings.ToLower(inst.Role)),
			sev.Color(col, fence),
			sev.Color(col, lag),
			last,
		)
	}
	_ = t.Render(w)

	if app.Color.Disabled() {
		fmt.Fprintf(w, "\nOverall: %s %s\n", worst.Marker(), worst)
	}

	switch worst {
	case output.SevPass, output.SevInfo:
		return ExitOK
	case output.SevWarn:
		if app.Flags.Strict {
			return ExitWarnStrict
		}
		return ExitOK
	default:
		return ExitUnhealthy
	}
}

func primaryHealth(e *pgmanagerStatus) (output.Severity, string) {
	if e.PrimaryNodeID == "" {
		return output.SevFail, "(none)"
	}
	return output.SevPass, e.PrimaryNodeID
}

func leaderHealth(e *pgmanagerStatus) (output.Severity, string) {
	if e.LeaderNodeID == "" {
		return output.SevFail, "(unknown)"
	}
	return output.SevPass, e.LeaderNodeID
}

func reachableCount(e *pgmanagerStatus) (reachable, total int) {
	total = len(e.Instances)
	for _, i := range e.Instances {
		if !isFailed(i.State) {
			reachable++
		}
	}
	return reachable, total
}

func meshLine(c *output.Color, e *embeddedNATSSnapshot, total int) string {
	if e == nil {
		return "Mesh:    (embedded NATS snapshot unavailable)"
	}
	sev := output.SevPass
	switch {
	case !e.Ready:
		sev = output.SevFail
	case total > 1 && e.RoutesMeshed < total-1:
		sev = output.SevWarn
	}
	natsState := "OK"
	if !e.Ready {
		natsState = "NOT READY"
	}
	return fmt.Sprintf("Mesh:    %s  ·  embedded NATS: %s",
		sev.Color(c, fmt.Sprintf("%d routes meshed", e.RoutesMeshed)),
		sev.Color(c, natsState),
	)
}

func nodeSeverity(inst instanceStatus, _ *pgmanagerStatus) output.Severity {
	switch {
	case isFailed(inst.State):
		return output.SevFail
	case isFenced(inst.State):
		return output.SevWarn
	case !inst.PostgresUp:
		return output.SevFail
	case isStandby(inst.Role) && inst.LagBytes > warnLagBytes:
		if inst.LagBytes > failLagBytes {
			return output.SevFail
		}
		return output.SevWarn
	}
	return output.SevPass
}

const (
	warnLagBytes = int64(64 * 1024 * 1024)   // 64 MiB
	failLagBytes = int64(1 << 30)            // 1 GiB
)

func isFenced(state string) bool { return strings.EqualFold(state, "fenced") }
func isFailed(state string) bool { return strings.EqualFold(state, "failed") || strings.EqualFold(state, "down") }
func isStandby(role string) bool { return strings.EqualFold(role, "standby") || strings.EqualFold(role, "replica") }

func lagText(i instanceStatus, _ *pgmanagerStatus) string {
	if !isStandby(i.Role) {
		return "-"
	}
	switch {
	case i.LagBytes < 1<<10:
		return fmt.Sprintf("%d B", i.LagBytes)
	case i.LagBytes < 1<<20:
		return fmt.Sprintf("%d KiB", i.LagBytes>>10)
	case i.LagBytes < 1<<30:
		return fmt.Sprintf("%.1f MiB", float64(i.LagBytes)/(1<<20))
	default:
		return fmt.Sprintf("%.2f GiB", float64(i.LagBytes)/(1<<30))
	}
}

func worse(a output.Severity, others ...output.Severity) output.Severity {
	order := map[output.Severity]int{
		output.SevPass:    0,
		output.SevInfo:    1,
		output.SevWarn:    2,
		output.SevUnknown: 2,
		output.SevFail:    3,
	}
	worst := a
	for _, s := range others {
		if order[s] > order[worst] {
			worst = s
		}
	}
	return worst
}

func commandTimeout(app *AppContext) time.Duration {
	if app.Flags.Timeout > 0 {
		return app.Flags.Timeout
	}
	return 10 * time.Second
}
