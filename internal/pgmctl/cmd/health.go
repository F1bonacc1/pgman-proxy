package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/output"
)

// healthLine is one component of the rollup (FR-014).
type healthLine struct {
	Component string          `json:"component" yaml:"component"`
	Status    output.Severity `json:"status" yaml:"status"`
	Message   string          `json:"message" yaml:"message"`
}

type healthPayload struct {
	Overall output.Severity `json:"overall" yaml:"overall"`
	Lines   []healthLine    `json:"components" yaml:"components"`
}

// diagnosisShape mirrors pg-manager's Diagnosis wire format (Go-default
// CamelCase JSON tags from the existing handler).
type diagnosisShape struct {
	Healthy bool          `json:"Healthy"`
	Issues  []issueRecord `json:"Issues,omitempty"`
}

type issueRecord struct {
	Severity   int    `json:"Severity"`
	Component  string `json:"Component"`
	Message    string `json:"Message"`
	Suggestion string `json:"Suggestion"`
}

// Map pg-manager's Severity int enum to our user-facing Severity. The
// upstream enum ordering: 0=Info, 1=Warn, 2=Error (per the existing
// pg-manager source; renames are MINOR-version events per Constitution V).
func severityFromPGManager(n int) output.Severity {
	switch n {
	case 0:
		return output.SevInfo
	case 1:
		return output.SevWarn
	case 2:
		return output.SevFail
	default:
		return output.SevUnknown
	}
}

func newHealthCmd(app *AppContext) *cobra.Command {
	return &cobra.Command{
		Use:   "health",
		Short: "One-line-per-component health rollup",
		Long: `Compose a single line per cluster component (control plane,
embedded NATS, primary, quorum, replication) for use as the body of a
higher-level monitor's status check.

Composed client-side from GET /v1/status and GET /v1/diagnose.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := app.Setup(); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), commandTimeout(app))
			defer cancel()

			statusEnv, err := app.Client.GetJSON(ctx, "/v1/status")
			if err != nil {
				return err
			}
			engine, embedded, err := decodeStatusEngine(statusEnv.EngineResult)
			if err != nil {
				return err
			}

			payload := healthPayload{Lines: []healthLine{}}
			payload.Lines = append(payload.Lines, healthLine{
				Component: "control-plane",
				Status:    output.SevPass,
				Message:   "reachable",
			})

			natsSev := output.SevPass
			natsMsg := "snapshot unavailable"
			if embedded != nil {
				natsMsg = fmt.Sprintf("%d routes meshed", embedded.RoutesMeshed)
				if !embedded.Ready {
					natsSev = output.SevFail
					natsMsg = "not ready"
				}
			} else {
				natsSev = output.SevInfo
			}
			payload.Lines = append(payload.Lines, healthLine{
				Component: "embedded-nats",
				Status:    natsSev,
				Message:   natsMsg,
			})

			primSev, primMsg := output.SevPass, engine.PrimaryNodeID
			if engine.PrimaryNodeID == "" {
				primSev, primMsg = output.SevFail, "no primary elected"
			}
			payload.Lines = append(payload.Lines, healthLine{
				Component: "primary",
				Status:    primSev,
				Message:   primMsg,
			})

			leadSev, leadMsg := output.SevPass, engine.LeaderNodeID
			if engine.LeaderNodeID == "" {
				leadSev, leadMsg = output.SevFail, "no leader elected"
			}
			payload.Lines = append(payload.Lines, healthLine{
				Component: "leader",
				Status:    leadSev,
				Message:   leadMsg,
			})

			peersSev, peersMsg := peersHealth(engine)
			payload.Lines = append(payload.Lines, healthLine{
				Component: "peers",
				Status:    peersSev,
				Message:   peersMsg,
			})

			// Quorum: ≥ floor(N/2)+1 peers reachable AND primary AND
			// leader present. Without inspecting Instances this used to
			// short-circuit to PASS whenever a single peer answered for
			// the cluster, hiding the death of every other peer.
			quorumSev, quorumMsg := quorumHealth(engine)
			payload.Lines = append(payload.Lines, healthLine{
				Component: "quorum",
				Status:    quorumSev,
				Message:   quorumMsg,
			})

			// Best-effort: use /v1/diagnose to add a replication line.
			diagEnv, dErr := app.Client.GetJSON(ctx, "/v1/diagnose")
			repSev, repMsg := output.SevUnknown, "diagnose unavailable"
			if dErr == nil && len(diagEnv.EngineResult) > 0 {
				var d diagnosisShape
				if jerr := json.Unmarshal(diagEnv.EngineResult, &d); jerr == nil {
					repSev, repMsg = replicationSummary(d)
				}
			}
			payload.Lines = append(payload.Lines, healthLine{
				Component: "replication",
				Status:    repSev,
				Message:   repMsg,
			})

			payload.Overall = worstOf(payload.Lines)

			switch app.Format {
			case output.FormatJSON:
				return output.EmitJSON(cmd.OutOrStdout(), "Health", payload)
			case output.FormatYAML:
				return output.EmitYAML(cmd.OutOrStdout(), "Health", payload)
			default:
				renderHealth(cmd.OutOrStdout(), app.Color, payload)
			}

			switch payload.Overall {
			case output.SevPass, output.SevInfo:
				return nil
			case output.SevWarn:
				if app.Flags.Strict {
					return WithExitCode(ExitWarnStrict, fmt.Errorf("warnings present (--strict)"))
				}
				return nil
			default:
				return WithExitCode(ExitUnhealthy, fmt.Errorf("cluster is not fully healthy"))
			}
		},
	}
}

// peersHealth folds the stitched per-peer Instances slice into a
// one-line "X/Y healthy" rollup. A peer is considered healthy when
// PostgresUp=true and State=running. Missing or fenced peers degrade
// the line: any FAILED → FAIL; any UNKNOWN / FENCED → WARN.
func peersHealth(engine *pgmanagerStatus) (output.Severity, string) {
	total := len(engine.Instances)
	if total == 0 {
		// /v1/status's aggregator is unwired; we can't tell anything.
		return output.SevUnknown, "no peer information"
	}
	var failed, unknown, fenced []string
	healthy := 0
	for _, inst := range engine.Instances {
		state := strings.ToLower(string(inst.State))
		switch state {
		case "running":
			if inst.PostgresUp {
				healthy++
				continue
			}
			failed = append(failed, fmt.Sprintf("%s(pg-down)", inst.NodeID))
		case "failed":
			failed = append(failed, string(inst.NodeID))
		case "fenced":
			fenced = append(fenced, string(inst.NodeID))
		case "unknown", "":
			unknown = append(unknown, string(inst.NodeID))
		default:
			unknown = append(unknown, fmt.Sprintf("%s(%s)", inst.NodeID, state))
		}
	}

	parts := []string{}
	if len(failed) > 0 {
		parts = append(parts, "failed: "+strings.Join(failed, ","))
	}
	if len(fenced) > 0 {
		parts = append(parts, "fenced: "+strings.Join(fenced, ","))
	}
	if len(unknown) > 0 {
		parts = append(parts, "unreachable: "+strings.Join(unknown, ","))
	}

	base := fmt.Sprintf("%d/%d healthy", healthy, total)
	if len(parts) == 0 {
		return output.SevPass, base
	}
	msg := base + " (" + strings.Join(parts, "; ") + ")"
	switch {
	case len(failed) > 0:
		return output.SevFail, msg
	default:
		return output.SevWarn, msg
	}
}

// quorumHealth computes whether the cluster still has a write-eligible
// majority. The classic majority quorum rule is `reachable ≥ N/2 + 1`
// where N is the declared peer count — without this, a single
// surviving peer that happens to be primary+leader incorrectly papers
// over the death of every other peer.
func quorumHealth(engine *pgmanagerStatus) (output.Severity, string) {
	total := len(engine.Instances)
	if total == 0 {
		return output.SevUnknown, "no peer information"
	}
	reachable := 0
	for _, inst := range engine.Instances {
		state := strings.ToLower(string(inst.State))
		if state != "" && state != "unknown" {
			reachable++
		}
	}
	need := total/2 + 1
	switch {
	case engine.PrimaryNodeID == "" || engine.LeaderNodeID == "":
		return output.SevFail, "missing primary or leader"
	case reachable < need:
		return output.SevFail, fmt.Sprintf("only %d/%d reachable (need %d)", reachable, total, need)
	default:
		return output.SevPass, fmt.Sprintf("%d/%d reachable (≥ %d)", reachable, total, need)
	}
}

func replicationSummary(d diagnosisShape) (output.Severity, string) {
	if d.Healthy {
		return output.SevPass, "all streams healthy"
	}
	worst := output.SevPass
	msgs := []string{}
	for _, i := range d.Issues {
		s := severityFromPGManager(i.Severity)
		if rank(s) > rank(worst) {
			worst = s
		}
		// Only include replication-tagged issues in the rollup line.
		if i.Component == "replication" || i.Component == "wal" || i.Component == "slot" || i.Component == "" {
			msgs = append(msgs, fmt.Sprintf("%s: %s", i.Component, i.Message))
		}
	}
	if len(msgs) == 0 {
		return worst, "see `pgmctl get audit` for details"
	}
	combined := msgs[0]
	if len(msgs) > 1 {
		combined = fmt.Sprintf("%s (+%d more)", msgs[0], len(msgs)-1)
	}
	return worst, combined
}

func worstOf(lines []healthLine) output.Severity {
	worst := output.SevPass
	for _, l := range lines {
		if rank(l.Status) > rank(worst) {
			worst = l.Status
		}
	}
	return worst
}

func rank(s output.Severity) int {
	switch s {
	case output.SevPass:
		return 0
	case output.SevInfo:
		return 1
	case output.SevWarn, output.SevUnknown:
		return 2
	default:
		return 3
	}
}

func renderHealth(w io.Writer, c *output.Color, p healthPayload) {
	for _, l := range p.Lines {
		fmt.Fprintf(w, "%s: %s\n", l.Component, l.Status.Color(c, fmt.Sprintf("%s — %s", l.Status, l.Message)))
	}
	fmt.Fprintf(w, "overall: %s\n", p.Overall.Color(c, string(p.Overall)))
}
