package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/client"
	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/dump"
)

type dumpFlags struct {
	output          string
	since           time.Duration
	redactLevel     string
	perSliceTimeout time.Duration
}

func newDumpCmd(app *AppContext) *cobra.Command {
	var f dumpFlags
	c := &cobra.Command{
		Use:   "dump",
		Short: "Capture every slice into a single tar.gz / tar artifact",
		Long: `Captures cluster-wide state in parallel — status, topology, history
events + audit, doctor (when implemented), clock-skew, config — and writes
a single tar archive at --output. Use --output - to stream raw tar to
stdout (compression off).

Per-slice failures don't fail the dump: each slice is recorded in
manifest.json with an outcome (ok|failed). The command exits 0 when every
slice succeeded and 3 (EX_PARTIAL) when at least one slice failed (FR-037).

Exit codes:
  0   clean dump
  3   one or more slices failed (manifest documents the gap)
  124 outer command timeout`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := app.Setup(); err != nil {
				return err
			}
			return runDump(cmd, app, f)
		},
	}
	c.Flags().StringVar(&f.output, "output", "", "Output path; '-' streams raw tar to stdout")
	c.Flags().DurationVar(&f.since, "since", 0, "Time window for history slices (e.g. 30m, 24h); empty = server default")
	c.Flags().StringVar(&f.redactLevel, "redact-level", "normal", "Redaction level: normal|strict")
	c.Flags().DurationVar(&f.perSliceTimeout, "per-slice-timeout", 10*time.Second, "Per-slice fetch timeout")
	_ = c.MarkFlagRequired("output")
	return c
}

func runDump(cmd *cobra.Command, app *AppContext, f dumpFlags) error {
	level, err := dump.ParseRedactLevel(f.redactLevel)
	if err != nil {
		return WithExitCode(ExitUsage, err)
	}

	out, gz, closer, err := openDumpOutput(cmd.OutOrStdout(), f.output)
	if err != nil {
		return err
	}
	defer closer()

	started := time.Now()
	manifest := dump.NewManifest(
		dump.BuildInfo{Version: app.Build.Version, Commit: app.Build.Commit},
		string(level),
		app.Client.ExpectedCluster(),
		app.Client.Endpoint(),
		started,
	)

	w, err := dump.NewWriter(out, gz, manifest)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), commandTimeout(app))
	defer cancel()

	fetcher := newClientFetcher(app.Client)
	collector := dump.NewCollector(f.perSliceTimeout)
	results := collector.Run(ctx, dump.DefaultSpecs(fetcher, f.since))

	red := dump.NewRedactor(level)
	for _, r := range results {
		if r.Outcome == dump.OutcomeOK {
			r.Data = red.Apply(r.Data)
		}
		if werr := w.WriteSlice(r); werr != nil {
			_ = w.Close()
			return fmt.Errorf("write slice %s: %w", r.Name, werr)
		}
	}

	if level == dump.RedactStrict {
		corr := red.SortedCorrelation()
		corrJSON, mErr := json.MarshalIndent(corr, "", "  ")
		if mErr != nil {
			_ = w.Close()
			return fmt.Errorf("encode correlation table: %w", mErr)
		}
		if err := w.WriteRaw("correlation.json", corrJSON); err != nil {
			_ = w.Close()
			return fmt.Errorf("write correlation table: %w", err)
		}
	}

	if err := w.Close(); err != nil {
		return err
	}

	// Status banner on stderr so it doesn't contaminate stdout
	// streaming. Suppressed when --quiet.
	if !app.Flags.Quiet {
		dur := time.Since(started)
		fmt.Fprintf(cmd.ErrOrStderr(),
			"dump complete: %d slices captured, outcome=%s, %s elapsed\n",
			len(manifest.Slices), manifest.Outcome(), dur.Round(time.Millisecond))
	}

	if manifest.Outcome() != dump.OutcomeOK {
		return WithExitCode(ExitPartial, errors.New("dump finished with one or more failed slices (see manifest)"))
	}
	return nil
}

// openDumpOutput resolves the --output target into an io.Writer plus
// a flag indicating whether gzip wrapping is wanted. Stdout streaming
// ("--output -") deliberately disables gzip per FR-034 so consumers
// can pipe the raw tar into another tool. The returned closer flushes
// and closes file targets; for stdout the closer is a no-op.
func openDumpOutput(_ io.Writer, path string) (io.Writer, bool, func(), error) {
	if path == "-" {
		// cobra defaults out to a buffer for some test paths; route to
		// real stdout so the tar lands where the operator pipes it.
		return os.Stdout, false, func() {}, nil
	}
	if path == "" {
		return nil, false, func() {}, errors.New("--output is required (use '-' for stdout)")
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	gz := strings.HasSuffix(strings.ToLower(path), ".tar.gz") ||
		strings.HasSuffix(strings.ToLower(path), ".tgz")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, false, func() {}, fmt.Errorf("open %s: %w", path, err)
	}
	return f, gz, func() { _ = f.Close() }, nil
}

// clientFetcher adapts *client.Client to the dump.Fetcher interface so
// the dump package stays free of any HTTP-specific dep.
type clientFetcher struct {
	c *client.Client
}

func newClientFetcher(c *client.Client) *clientFetcher { return &clientFetcher{c: c} }

func (f *clientFetcher) GetJSON(ctx context.Context, path string) (json.RawMessage, error) {
	env, err := f.c.GetJSON(ctx, path)
	if err != nil {
		return nil, err
	}
	if len(env.EngineResult) == 0 {
		return nil, errors.New("empty engine_result")
	}
	return env.EngineResult, nil
}
