package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

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

			// Quorum heuristic: if we don't have a primary OR leader,
			// quorum is broken. Otherwise we trust the proxy.
			quorumSev := output.SevPass
			quorumMsg := "ok"
			if engine.PrimaryNodeID == "" || engine.LeaderNodeID == "" {
				quorumSev, quorumMsg = output.SevFail, "missing primary or leader"
			}
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
