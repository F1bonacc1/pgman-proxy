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
	LocalRole          roleField        `json:"LocalRole"`
	LocalState         stateField       `json:"LocalState"`
	Instances          []instanceStatus `json:"Instances"`
	SyncStandbys       []string         `json:"SyncStandbys,omitempty"`
	LastFailoverAt     time.Time        `json:"LastFailoverAt"`
	LastFailoverReason string           `json:"LastFailoverReason,omitempty"`
}

type instanceStatus struct {
	NodeID     string     `json:"NodeID"`
	Role       roleField  `json:"Role"`
	State      stateField `json:"State"`
	PostgresUp bool       `json:"PostgresUp"`
	ReplayLSN  uint64     `json:"ReplayLSN"`
	WriteLSN   uint64     `json:"WriteLSN"`
	FlushLSN   uint64     `json:"FlushLSN"`
	LagBytes   int64      `json:"LagBytes"`
	LastSeenAt time.Time  `json:"LastSeenAt"`
}

// roleField / stateField are wire-shape adapters for pg-manager's
// Role / State, which the engine serializes as integer enum ordinals
// today (no MarshalJSON method on the upstream types) but pgmctl tests
// and contracts speak in the canonical string names ("primary",
// "running", …). UnmarshalJSON accepts both forms and stores the
// String() form so downstream comparisons stay readable. MarshalJSON
// emits the plain string so `pgmctl status -o json` always shows
// "primary" not 2 (FR-038 schema stability).
//
// Enum ordinals are mirrored from `../pg-manager/types.go`. They are
// part of pg-manager's public surface and a change there would be a
// breaking API bump per its constitution, so this map is stable.
type roleField string
type stateField string

var pgmRoleNames = map[int]string{
	0: "unknown",
	1: "primary",
	2: "standby",
	3: "standby_designated",
}

var pgmStateNames = map[int]string{
	0: "unknown",
	1: "init",
	2: "bootstrapping",
	3: "running",
	4: "promoting",
	5: "demoting",
	6: "rewinding",
	7: "fenced",
	8: "failed",
	9: "stopped",
}

func (r *roleField) UnmarshalJSON(b []byte) error {
	s, err := decodeEnumWire(b, pgmRoleNames)
	if err != nil {
		return err
	}
	*r = roleField(s)
	return nil
}

func (r roleField) MarshalJSON() ([]byte, error) { return json.Marshal(string(r)) }

func (s *stateField) UnmarshalJSON(b []byte) error {
	v, err := decodeEnumWire(b, pgmStateNames)
	if err != nil {
		return err
	}
	*s = stateField(v)
	return nil
}

func (s stateField) MarshalJSON() ([]byte, error) { return json.Marshal(string(s)) }

func decodeEnumWire(b []byte, names map[int]string) (string, error) {
	if len(b) == 0 || string(b) == "null" {
		return "", nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return "", err
		}
		return s, nil
	}
	var i int
	if err := json.Unmarshal(b, &i); err != nil {
		return "", err
	}
	if name, ok := names[i]; ok {
		return name, nil
	}
	return fmt.Sprintf("invalid(%d)", i), nil
}

// embeddedNATSSnapshot mirrors the 002 contracts/observability.md
// embedded_nats sub-block. We decode it for the "Mesh:" summary line
// but tolerate missing fields (single-peer test paths leave the
// snapshot off entirely).
type embeddedNATSSnapshot struct {
	ServerName       string `json:"server_name"`
	Ready            bool   `json:"ready"`
	ClientListenAddr string `json:"client_listen_addr"`
	RoutesListenAddr string `json:"routes_listen_addr"`
	TLSEnabled       bool   `json:"tls_enabled"`
	RoutesMeshed     int    `json:"routes_meshed"`
	ReplicasFactor   int    `json:"replicas_factor"`
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
//
// LeaderBeliefStale / PrimaryBeliefStale (omitempty when false) flag
// the case where the connected node's substrate is non-quorate: any
// leader / primary identity it returns was frozen at the moment it
// lost contact and cannot be trusted. The flags are derivable from
// Substrate.Quorate + Cluster.LeaderNodeID/PrimaryNodeID, but pgmctl
// exposes them as first-class fields so automation does not have to
// reimplement the inference.
type statusJSONShape struct {
	CapturedAt         time.Time             `json:"captured_at" yaml:"captured_at"`
	Cluster            *pgmanagerStatus      `json:"cluster" yaml:"cluster"`
	EmbeddedNATS       *embeddedNATSSnapshot `json:"embedded_nats,omitempty" yaml:"embedded_nats,omitempty"`
	Substrate          substrateQuorum       `json:"substrate" yaml:"substrate"`
	LeaderBeliefStale  bool                  `json:"leader_belief_stale,omitempty" yaml:"leader_belief_stale,omitempty"`
	PrimaryBeliefStale bool                  `json:"primary_belief_stale,omitempty" yaml:"primary_belief_stale,omitempty"`
}

// substrateQuorum surfaces whether the connecting peer can reach a
// majority of the cluster via NATS fan-out. Derived from the per-peer
// Instances response shape (peers that don't reply are marked
// unreachable by the fan-out aggregator). When Quorate is false the
// JetStream KV bucket — which backs pg-manager's leadership lease and
// state store — is not writable; the cluster is operating beyond
// FTT=(DeclaredSize-1)/2 and the lone-side primary (if any) will
// block writes at COMMIT via synchronous_commit. Surfaced so
// operators can distinguish "single node hiccup" from "cluster
// unavailable for writes" at a glance.
type substrateQuorum struct {
	Required   int  `json:"required" yaml:"required"`
	Responding int  `json:"responding" yaml:"responding"`
	Total      int  `json:"total" yaml:"total"`
	Quorate    bool `json:"quorate" yaml:"quorate"`
}

func statusJSON(engine *pgmanagerStatus, embedded *embeddedNATSSnapshot) statusJSONShape {
	responding, _, total := peerCounts(engine)
	sub := computeSubstrateQuorum(responding, total)
	leaderStale, primaryStale := beliefsStale(engine, sub)
	return statusJSONShape{
		CapturedAt:         time.Now().UTC(),
		Cluster:            engine,
		EmbeddedNATS:       embedded,
		Substrate:          sub,
		LeaderBeliefStale:  leaderStale,
		PrimaryBeliefStale: primaryStale,
	}
}

// beliefsStale returns whether the connected node's leader / primary
// fields are unverifiable. The local snapshot retains whatever was
// true when substrate connectivity was last good; once the fan-out
// fails to reach a majority, those fields are frozen — they no longer
// reflect cluster reality and may name a node that has already lost
// the leader-key race or been demoted. The condition does not
// require LeaderNodeID == LocalNodeID: even a standby that named
// some other primary pre-partition is reporting a stale belief
// (it cannot have observed any state change since the partition).
//
// Returns (false, false) when no peers are present or when LeaderNodeID /
// PrimaryNodeID are empty — there is no claim to label stale.
func beliefsStale(e *pgmanagerStatus, sub substrateQuorum) (leader, primary bool) {
	if sub.Total == 0 || sub.Quorate {
		return false, false
	}
	leader = e.LeaderNodeID != ""
	primary = e.PrimaryNodeID != ""
	return
}

// computeSubstrateQuorum derives the majority-quorum verdict from the
// fan-out responding count. A single-node cluster has Required=1 and
// is always Quorate when self is up. Empty Instances (total=0) is
// treated as a structural failure rather than "quorate by vacuity" —
// the caller produces a non-zero exit code.
func computeSubstrateQuorum(responding, total int) substrateQuorum {
	required := 0
	if total > 0 {
		required = total/2 + 1
	}
	return substrateQuorum{
		Required:   required,
		Responding: responding,
		Total:      total,
		Quorate:    total > 0 && responding >= required,
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

	responding, healthy, total := peerCounts(engine)
	substrate := computeSubstrateQuorum(responding, total)
	leaderStale, primaryStale := beliefsStale(engine, substrate)
	if leaderStale {
		// Belief-stale: the local snapshot was frozen at the moment
		// substrate went away. Whatever leader it names cannot have
		// been re-confirmed since. Mark FAIL so the headline matches
		// the QUORUM LOST verdict on the line below.
		leaderSev = output.SevFail
		leaderStr = engine.LeaderNodeID + " (stale)"
	}
	if primaryStale {
		primarySev = output.SevFail
		primaryStr = engine.PrimaryNodeID + " (stale)"
	}
	worst = worse(worst, primarySev, leaderSev)

	peersSev := output.SevPass
	switch {
	case healthy == 0:
		peersSev = output.SevFail
	case healthy < total, responding < total:
		peersSev = output.SevWarn
	}
	if total > 0 && !substrate.Quorate {
		// Quorum loss is strictly more severe than a slow / unhealthy
		// peer — KV writes are refused, so the cluster cannot persist
		// leadership transitions or state-store mutations. Force FAIL
		// on the Peers segment even if `healthy >= 1`.
		peersSev = output.SevFail
	}
	worst = worse(worst, peersSev)
	peersMsg := fmt.Sprintf("%d/%d healthy", healthy, total)
	if responding < total {
		peersMsg += fmt.Sprintf(" · %d/%d responding", responding, total)
	}
	peersStr := peersSev.Color(col, peersMsg)

	meshStr := meshLine(col, embedded, total)

	_, _ = fmt.Fprintf(w, "Cluster: %s\tSnapshot: %s\n", cluster, now)
	_, _ = fmt.Fprintf(w, "Leader:  %s   Primary: %s   Peers: %s\n",
		leaderSev.Color(col, leaderStr),
		primarySev.Color(col, primaryStr),
		peersStr,
	)
	_, _ = fmt.Fprintln(w, meshStr)
	if total > 0 && !substrate.Quorate {
		// One extra line — only emitted on quorum loss — so a paged
		// operator can't miss it. The "Leader:" / "Primary:" segments
		// above now carry the "(stale)" suffix, but this line is the
		// causal explanation: synchronous_commit will block writes on
		// whichever node still believes it is primary.
		quorumLine := fmt.Sprintf(
			"Substrate: QUORUM LOST  ·  %d/%d responding (need %d)  ·  writes will block on sync_commit",
			substrate.Responding, substrate.Total, substrate.Required,
		)
		_, _ = fmt.Fprintln(w, output.SevFail.Color(col, quorumLine))
	}
	_, _ = fmt.Fprintln(w)

	t := output.NewTable("NODE", "ROLE", "STATE", "FENCE", "LAG", "LAST TRANSITION")
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
		if isFenced(string(inst.State)) {
			fence = "yes"
		}
		state := strings.ToLower(string(inst.State))
		if state == "" {
			state = "-"
		}
		lag := lagText(inst, engine)
		last := "-"
		if !inst.LastSeenAt.IsZero() {
			last = inst.LastSeenAt.UTC().Format("15:04:05Z")
		}
		t.AddRow(
			sev.Color(col, inst.NodeID),
			sev.Color(col, strings.ToLower(string(inst.Role))),
			sev.Color(col, state),
			sev.Color(col, fence),
			sev.Color(col, lag),
			last,
		)
	}
	_ = t.Render(w)

	if app.Color.Disabled() {
		_, _ = fmt.Fprintf(w, "\nOverall: %s %s\n", worst.Marker(), worst)
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

// peerCounts splits the Instances slice into three counts:
//
//   - responding — peer returned a real fan-out reply (State != "" and
//     != "unknown"). Synthesised sibling_unreachable rows arrive with
//     State=Unknown and are NOT counted as responding.
//   - healthy    — peer is responding AND reports State="running" with
//     PostgresUp=true. A peer in StateFailed responded but is not
//     healthy — distinct from "did not respond".
//   - total      — every row in Instances, including unreachable ones.
//
// The summary line in renderStatus distinguishes these two failure
// modes so an operator can tell "lost contact with peer X" from
// "peer X is up but Postgres won't start".
func peerCounts(e *pgmanagerStatus) (responding, healthy, total int) {
	total = len(e.Instances)
	for _, i := range e.Instances {
		state := strings.ToLower(string(i.State))
		if state != "" && state != "unknown" {
			responding++
		}
		if i.PostgresUp && state == "running" {
			healthy++
		}
	}
	return
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
	case isFailed(string(inst.State)):
		return output.SevFail
	case isFenced(string(inst.State)):
		return output.SevWarn
	case !inst.PostgresUp:
		return output.SevFail
	case isStandby(string(inst.Role)) && inst.LagBytes > warnLagBytes:
		if inst.LagBytes > failLagBytes {
			return output.SevFail
		}
		return output.SevWarn
	}
	return output.SevPass
}

const (
	warnLagBytes = int64(64 * 1024 * 1024) // 64 MiB
	failLagBytes = int64(1 << 30)          // 1 GiB
)

func isFenced(state string) bool { return strings.EqualFold(state, "fenced") }
func isFailed(state string) bool {
	return strings.EqualFold(state, "failed") || strings.EqualFold(state, "down")
}
func isStandby(role string) bool {
	return strings.EqualFold(role, "standby") || strings.EqualFold(role, "replica")
}

func lagText(i instanceStatus, _ *pgmanagerStatus) string {
	if !isStandby(string(i.Role)) {
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
