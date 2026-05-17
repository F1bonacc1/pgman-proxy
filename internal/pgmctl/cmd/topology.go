package cmd

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/output"
)

// topologyPayload is the pgmctl/v1-versioned shape for -o json/yaml.
type topologyPayload struct {
	ClusterID    string                `json:"cluster_id" yaml:"cluster_id"`
	Leader       string                `json:"leader_node_id" yaml:"leader_node_id"`
	Primary      string                `json:"primary_node_id" yaml:"primary_node_id"`
	SyncStandbys []string              `json:"sync_standbys" yaml:"sync_standbys"`
	Peers        []topologyPeer        `json:"peers" yaml:"peers"`
	EmbeddedNATS *embeddedNATSSnapshot `json:"embedded_nats,omitempty" yaml:"embedded_nats,omitempty"`
}

type topologyPeer struct {
	NodeID     string `json:"node_id" yaml:"node_id"`
	Role       string `json:"role" yaml:"role"`
	State      string `json:"state" yaml:"state"`
	PostgresUp bool   `json:"postgres_up" yaml:"postgres_up"`
	LagBytes   int64  `json:"lag_bytes" yaml:"lag_bytes"`
	IsSync     bool   `json:"is_sync_standby" yaml:"is_sync_standby"`
}

func newTopologyCmd(app *AppContext) *cobra.Command {
	return &cobra.Command{
		Use:   "topology",
		Short: "Render the cluster topology (peers, roles, sync standbys)",
		Long: `Display the cluster topology in human-readable tree form by
default; -o json / yaml emit a schema-versioned document.

Derived from GET /v1/status — does not require any new server-side
endpoint.`,
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
		},
	}
}

func buildTopologyPayload(e *pgmanagerStatus, n *embeddedNATSSnapshot) topologyPayload {
	syncSet := make(map[string]bool, len(e.SyncStandbys))
	for _, s := range e.SyncStandbys {
		syncSet[s] = true
	}
	peers := make([]topologyPeer, 0, len(e.Instances))
	for _, i := range e.Instances {
		peers = append(peers, topologyPeer{
			NodeID:     i.NodeID,
			Role:       string(i.Role),
			State:      string(i.State),
			PostgresUp: i.PostgresUp,
			LagBytes:   i.LagBytes,
			IsSync:     syncSet[i.NodeID],
		})
	}
	sort.SliceStable(peers, func(i, j int) bool { return peers[i].NodeID < peers[j].NodeID })

	return topologyPayload{
		ClusterID:    e.ClusterID,
		Leader:       e.LeaderNodeID,
		Primary:      e.PrimaryNodeID,
		SyncStandbys: append([]string(nil), e.SyncStandbys...),
		Peers:        peers,
		EmbeddedNATS: n,
	}
}

func renderTopologyTree(w io.Writer, c *output.Color, p topologyPayload) {
	_, _ = fmt.Fprintf(w, "%s\n", c.Bold(p.ClusterID))
	_, _ = fmt.Fprintf(w, "├── leader:  %s\n", leaderLabel(c, p.Leader))
	_, _ = fmt.Fprintf(w, "├── primary: %s\n", primaryLabel(c, p.Primary))
	_, _ = fmt.Fprintf(w, "├── sync_standbys: %s\n", syncListLabel(c, p.SyncStandbys))
	if p.EmbeddedNATS != nil {
		_, _ = fmt.Fprintf(w, "├── embedded_nats: %s (%d routes meshed)\n",
			natsReadyLabel(c, p.EmbeddedNATS.Ready),
			p.EmbeddedNATS.RoutesMeshed)
	}
	_, _ = fmt.Fprintln(w, "└── peers")
	for i, peer := range p.Peers {
		prefix := "    ├── "
		if i == len(p.Peers)-1 {
			prefix = "    └── "
		}
		_, _ = fmt.Fprintf(w, "%s%s\n", prefix, peerLabel(c, peer))
	}
}

func leaderLabel(c *output.Color, id string) string {
	if id == "" {
		return c.Red("(unknown)")
	}
	return c.Green(id)
}

func primaryLabel(c *output.Color, id string) string {
	if id == "" {
		return c.Red("(none)")
	}
	return c.Green(id)
}

func syncListLabel(c *output.Color, ids []string) string {
	if len(ids) == 0 {
		return c.Yellow("(none — async replication only)")
	}
	return c.Green(strings.Join(ids, ", "))
}

func natsReadyLabel(c *output.Color, ready bool) string {
	if ready {
		return c.Green("ready")
	}
	return c.Red("NOT READY")
}

func peerLabel(c *output.Color, p topologyPeer) string {
	role := strings.ToLower(p.Role)
	state := strings.ToLower(p.State)
	flags := []string{}
	if p.IsSync {
		flags = append(flags, "sync")
	}
	if !p.PostgresUp {
		flags = append(flags, "postgres-down")
	}
	tail := ""
	if len(flags) > 0 {
		tail = "  [" + strings.Join(flags, ",") + "]"
	}
	line := fmt.Sprintf("%s  role=%s  state=%s%s", p.NodeID, role, state, tail)
	switch {
	case strings.EqualFold(p.State, "failed") || !p.PostgresUp:
		return c.Red(line)
	case strings.EqualFold(p.State, "fenced"):
		return c.Yellow(line)
	default:
		return c.Green(line)
	}
}
