package cmd

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/doctor"
	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/output"
)

type doctorFlags struct {
	list  bool
	check string
}

func newDoctorCmd(app *AppContext) *cobra.Command {
	var f doctorFlags
	c := &cobra.Command{
		Use:   "doctor",
		Short: "Run cluster health checks and render findings",
		Long: `pgmctl doctor runs the server-published v1 check battery and renders
the results with severity coloring. Use --list to inspect the catalog
without running anything; --check <name> to run a single check.

Exit codes:
  0   every check PASS or INFO (and --strict not set)
  1   one or more WARN findings AND --strict
  2   one or more FAIL findings
  5   one or more UNKNOWN findings (and no FAIL)`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := app.Setup(); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), commandTimeout(app))
			defer cancel()

			if f.list {
				return runDoctorList(ctx, cmd, app)
			}
			return runDoctorRun(ctx, cmd, app, f.check)
		},
	}
	c.Flags().BoolVar(&f.list, "list", false, "List the registered checks; do not execute them")
	c.Flags().StringVar(&f.check, "check", "", "Run a single check by name (omit to run all)")
	return c
}

// runDoctorList implements `pgmctl doctor --list`.
func runDoctorList(ctx context.Context, cmd *cobra.Command, app *AppContext) error {
	env, err := app.Client.GetJSON(ctx, "/v1/doctor/checks")
	if err != nil {
		return err
	}
	var resp struct {
		Checks []doctor.Check `json:"checks"`
	}
	if len(env.EngineResult) > 0 {
		if err := json.Unmarshal(env.EngineResult, &resp); err != nil {
			return fmt.Errorf("decode catalog: %w", err)
		}
	}
	switch app.Format {
	case output.FormatJSON:
		return output.EmitJSON(cmd.OutOrStdout(), "DoctorChecks", resp)
	case output.FormatYAML:
		return output.EmitYAML(cmd.OutOrStdout(), "DoctorChecks", resp)
	}
	return doctor.RenderCatalog(cmd.OutOrStdout(), app.Color, resp.Checks)
}

// runDoctorRun implements `pgmctl doctor [--check <name>]`.
func runDoctorRun(ctx context.Context, cmd *cobra.Command, app *AppContext, check string) error {
	body := map[string]string{}
	if check != "" {
		body["check"] = check
	}
	env, err := app.Client.PostJSON(ctx, "/v1/doctor/run", body)
	if err != nil {
		return err
	}
	var rep doctor.Report
	if len(env.EngineResult) > 0 {
		if err := json.Unmarshal(env.EngineResult, &rep); err != nil {
			return fmt.Errorf("decode doctor report: %w", err)
		}
	}
	switch app.Format {
	case output.FormatJSON:
		if err := output.EmitJSON(cmd.OutOrStdout(), "DoctorReport", rep); err != nil {
			return err
		}
	case output.FormatYAML:
		if err := output.EmitYAML(cmd.OutOrStdout(), "DoctorReport", rep); err != nil {
			return err
		}
	default:
		if err := doctor.RenderTable(cmd.OutOrStdout(), app.Color, rep); err != nil {
			return err
		}
	}
	return doctorExitForReport(rep, app.Flags.Strict)
}

// doctorExitForReport maps the worst-severity outcome to a pgmctl
// exit code per data-model.md § ExitCode. WARN gates on --strict.
func doctorExitForReport(rep doctor.Report, strict bool) error {
	switch doctor.WorstSeverity(rep) {
	case "FAIL":
		return WithExitCode(ExitUnhealthy, fmt.Errorf("doctor: %d FAIL finding(s)", rep.Summary.Fail))
	case "UNKNOWN":
		return WithExitCode(ExitUnknown, fmt.Errorf("doctor: %d UNKNOWN finding(s)", rep.Summary.Unknown))
	case "WARN":
		if strict {
			return WithExitCode(ExitWarnStrict, fmt.Errorf("doctor: %d WARN finding(s) (--strict)", rep.Summary.Warn))
		}
	}
	return nil
}
