// pgmctl explain dispatcher (T100 / FR-018).
//
// Subjects are evaluated against the live cluster by composing four
// existing data sources — Status, Diagnose, the doctor battery, and
// the history stream — into a Diagnosis / Evidence / Suggested-next-
// steps narrative. No new server endpoint exists; every subject is a
// pure client-side projection over the data layers we already ship.

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/doctor"
	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/output"
)

// subject enumeration documented in cli-commands.md § explain and
// FR-018. Names are stable wire form; renames are MINOR-version events.
const (
	subjFailoverStuck     = "failover-stuck"
	subjNodeNotPromoting  = "node-not-promoting"
	subjReplicationBroken = "replication-broken"
	subjLeaderElection    = "leader-election"
	subjCurrentState      = "current-state"
	subjLastEvent         = "last-event"
)

// ExplainOutput is the JSON / YAML wire shape. The table form is
// rendered separately by emitNarrative.
type ExplainOutput struct {
	APIVersion         string             `json:"apiVersion" yaml:"apiVersion"`
	Kind               string             `json:"kind" yaml:"kind"`
	Subject            string             `json:"subject" yaml:"subject"`
	Diagnosis          string             `json:"diagnosis" yaml:"diagnosis"`
	Evidence           []ExplainEvidence  `json:"evidence" yaml:"evidence"`
	SuggestedNextSteps []string           `json:"suggested_next_steps" yaml:"suggested_next_steps"`
}

// ExplainEvidence is one cited fact backing the diagnosis.
type ExplainEvidence struct {
	Source    string `json:"source" yaml:"source"`              // history | doctor | status
	Timestamp string `json:"timestamp,omitempty" yaml:"timestamp,omitempty"`
	Detail    string `json:"detail" yaml:"detail"`
	Reference string `json:"reference,omitempty" yaml:"reference,omitempty"` // history ULID / check name
}

// ErrSubjectNotApplicable is the typed sentinel a subject's evaluator
// returns when the cluster state doesn't match the subject's premise
// (e.g. asking why a failover is stuck on a healthy cluster). Mapped
// to ExitSubjectNA (4) by the caller per cli-commands.md § explain.
var ErrSubjectNotApplicable = errors.New("subject does not apply to the current cluster state")

// SubjectArg holds the optional positional argument every per-node
// subject carries (failover-stuck does not take one; the others may).
type SubjectArg struct {
	Subject string
	Arg     string
}

func newExplainCmd(app *AppContext) *cobra.Command {
	c := &cobra.Command{
		Use:   "explain <subject> [<arg>]",
		Short: "Compose a plain-English narrative from cluster facts",
		Long: `pgmctl explain <subject> renders a three-section narrative
(Diagnosis / Evidence / Suggested next steps) by composing the existing
data layers: GET /v1/status, GET /v1/diagnose, POST /v1/doctor/run, and
GET /v1/history.

Subjects (FR-018):
  failover-stuck                  Why isn't a recently-elected leader being
                                  recognised by the rest of the cluster?
  node-not-promoting <node>       Why hasn't <node> been promoted yet?
  replication-broken <node>       Why is <node> not streaming WAL?
  leader-election                 Recent leader-election history.
  current-state                   One-line cluster shape rollup.
  last-event                      Most recent history record + its context.

Exit codes:
  0  narrative rendered
  4  EX_SUBJECT_NA — subject's premise doesn't match cluster state`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := app.Setup(); err != nil {
				return err
			}
			sa := SubjectArg{Subject: args[0]}
			if len(args) > 1 {
				sa.Arg = args[1]
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), commandTimeout(app))
			defer cancel()
			return runExplain(ctx, cmd, app, sa)
		},
	}
	return c
}

func runExplain(ctx context.Context, cmd *cobra.Command, app *AppContext, sa SubjectArg) error {
	out, err := evaluateSubject(ctx, app, sa)
	if err != nil {
		if errors.Is(err, ErrSubjectNotApplicable) {
			fmt.Fprintf(cmd.ErrOrStderr(), "subject %q does not apply: %s\n", sa.Subject, err.Error())
			return WithExitCode(ExitSubjectNA, err)
		}
		return err
	}

	switch app.Format {
	case output.FormatJSON:
		return output.EmitJSON(cmd.OutOrStdout(), "Explain", out)
	case output.FormatYAML:
		return output.EmitYAML(cmd.OutOrStdout(), "Explain", out)
	default:
		emitNarrative(cmd.OutOrStdout(), app.Color, out)
		return nil
	}
}

// evaluateSubject dispatches to the per-subject evaluator. Each
// evaluator is responsible for fetching whatever it needs and either
// (a) returning an ExplainOutput populated with diagnosis + evidence
// + next steps, or (b) returning ErrSubjectNotApplicable wrapped with
// a one-line reason.
func evaluateSubject(ctx context.Context, app *AppContext, sa SubjectArg) (ExplainOutput, error) {
	subj := strings.ToLower(strings.TrimSpace(sa.Subject))
	switch subj {
	case subjFailoverStuck:
		return explainFailoverStuck(ctx, app)
	case subjNodeNotPromoting:
		if sa.Arg == "" {
			return ExplainOutput{}, fmt.Errorf("subject %q requires a <node> argument", subj)
		}
		return explainNodeNotPromoting(ctx, app, sa.Arg)
	case subjReplicationBroken:
		if sa.Arg == "" {
			return ExplainOutput{}, fmt.Errorf("subject %q requires a <node> argument", subj)
		}
		return explainReplicationBroken(ctx, app, sa.Arg)
	case subjLeaderElection:
		return explainLeaderElection(ctx, app)
	case subjCurrentState:
		return explainCurrentState(ctx, app)
	case subjLastEvent:
		return explainLastEvent(ctx, app)
	default:
		return ExplainOutput{}, fmt.Errorf("unknown subject %q; see `pgmctl explain --help`", subj)
	}
}

// emitNarrative is the table-form renderer documented in
// cli-commands.md § explain.
func emitNarrative(w io.Writer, c *output.Color, out ExplainOutput) {
	fmt.Fprintf(w, "%s\n", c.Bold("DIAGNOSIS"))
	fmt.Fprintf(w, "  %s\n\n", out.Diagnosis)

	fmt.Fprintf(w, "%s\n", c.Bold("EVIDENCE"))
	if len(out.Evidence) == 0 {
		fmt.Fprintln(w, "  (no supporting evidence cited)")
	}
	for _, ev := range out.Evidence {
		ref := ""
		if ev.Reference != "" {
			ref = fmt.Sprintf(" (id=%s)", ev.Reference)
		}
		ts := ev.Timestamp
		if ts == "" {
			ts = "             "
		}
		fmt.Fprintf(w, "  %-20s  %s%s  [from: %s]\n", ts, ev.Detail, ref, ev.Source)
	}
	fmt.Fprintln(w)

	fmt.Fprintf(w, "%s\n", c.Bold("SUGGESTED NEXT STEPS"))
	for i, step := range out.SuggestedNextSteps {
		fmt.Fprintf(w, "  %d. %s\n", i+1, step)
	}
}

// --- shared fetch helpers ---

// fetchStatus returns the decoded engine status. Mirrors the topology
// path's use of decodeStatusEngine.
func fetchStatus(ctx context.Context, app *AppContext) (*pgmanagerStatus, *embeddedNATSSnapshot, error) {
	env, err := app.Client.GetJSON(ctx, "/v1/status")
	if err != nil {
		return nil, nil, err
	}
	return decodeStatusEngine(env.EngineResult)
}

// fetchDoctorReport calls POST /v1/doctor/run with an empty body to
// capture the full v1 battery. Failures are wrapped — explain
// evaluators decide whether to surface the gap or proceed with
// partial evidence.
func fetchDoctorReport(ctx context.Context, app *AppContext) (doctor.Report, error) {
	env, err := app.Client.PostJSON(ctx, "/v1/doctor/run", map[string]string{})
	if err != nil {
		return doctor.Report{}, err
	}
	var rep doctor.Report
	if len(env.EngineResult) > 0 {
		if jerr := json.Unmarshal(env.EngineResult, &rep); jerr != nil {
			return doctor.Report{}, fmt.Errorf("decode doctor report: %w", jerr)
		}
	}
	return rep, nil
}

// fetchHistory runs GET /v1/history with the supplied filters. limit
// caps the result set; types narrows by event type.
func fetchHistory(ctx context.Context, app *AppContext, category string, types []string, limit int) ([]eventRow, error) {
	path := "/v1/history?limit=" + fmt.Sprintf("%d", limit)
	if category != "" {
		path += "&category=" + category
	}
	for _, t := range types {
		path += "&type=" + t
	}
	env, err := app.Client.GetJSON(ctx, path)
	if err != nil {
		return nil, err
	}
	var result historyResult
	if len(env.EngineResult) > 0 {
		if jerr := json.Unmarshal(env.EngineResult, &result); jerr != nil {
			return nil, fmt.Errorf("decode history result: %w", jerr)
		}
	}
	return result.Events, nil
}
