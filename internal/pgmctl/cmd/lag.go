package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/output"
)

// lagRow is one entry in the lag table.
type lagRow struct {
	NodeID    string `json:"node_id" yaml:"node_id"`
	Role      string `json:"role" yaml:"role"`
	LagBytes  int64  `json:"lag_bytes" yaml:"lag_bytes"`
	ReplayLSN uint64 `json:"replay_lsn" yaml:"replay_lsn"`
	WriteLSN  uint64 `json:"write_lsn" yaml:"write_lsn"`
	Status    output.Severity `json:"status" yaml:"status"`
}

type lagPayload struct {
	CapturedAt time.Time `json:"captured_at" yaml:"captured_at"`
	PrimaryLSN uint64    `json:"primary_lsn" yaml:"primary_lsn"`
	Rows       []lagRow  `json:"rows" yaml:"rows"`
}

func newLagCmd(app *AppContext) *cobra.Command {
	var warn, fail string
	c := &cobra.Command{
		Use:   "lag",
		Short: "Per-standby replication lag in bytes",
		Long: `Show replication lag for every standby — bytes behind the
primary's flush LSN, with severity coloring based on thresholds.

Defaults: --warn 64MB, --fail 1GB.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			warnBytes, err := parseBytes(warn, 64<<20)
			if err != nil {
				return WithExitCode(ExitUsage, fmt.Errorf("--warn: %w", err))
			}
			failBytes, err := parseBytes(fail, 1<<30)
			if err != nil {
				return WithExitCode(ExitUsage, fmt.Errorf("--fail: %w", err))
			}
			if err := app.Setup(); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), commandTimeout(app))
			defer cancel()

			env, err := app.Client.GetJSON(ctx, "/v1/status")
			if err != nil {
				return err
			}
			engine, _, err := decodeStatusEngine(env.EngineResult)
			if err != nil {
				return err
			}

			payload := lagPayload{
				CapturedAt: time.Now().UTC(),
			}
			for _, inst := range engine.Instances {
				if !isStandby(string(inst.Role)) {
					if strings.EqualFold(string(inst.Role), "primary") {
						payload.PrimaryLSN = inst.WriteLSN
					}
					continue
				}
				sev := lagSeverity(inst.LagBytes, warnBytes, failBytes)
				payload.Rows = append(payload.Rows, lagRow{
					NodeID:    inst.NodeID,
					Role:      string(inst.Role),
					LagBytes:  inst.LagBytes,
					ReplayLSN: inst.ReplayLSN,
					WriteLSN:  inst.WriteLSN,
					Status:    sev,
				})
			}
			sort.SliceStable(payload.Rows, func(i, j int) bool { return payload.Rows[i].NodeID < payload.Rows[j].NodeID })

			worst := output.SevPass
			for _, r := range payload.Rows {
				if rank(r.Status) > rank(worst) {
					worst = r.Status
				}
			}

			switch app.Format {
			case output.FormatJSON:
				return output.EmitJSON(cmd.OutOrStdout(), "ReplicationLag", payload)
			case output.FormatYAML:
				return output.EmitYAML(cmd.OutOrStdout(), "ReplicationLag", payload)
			default:
				t := output.NewTable("NODE", "ROLE", "LAG", "REPLAY LSN", "STATUS")
				for _, r := range payload.Rows {
					t.AddRow(
						r.Status.Color(app.Color, r.NodeID),
						r.Role,
						r.Status.Color(app.Color, formatBytes(r.LagBytes)),
						fmt.Sprintf("%x", r.ReplayLSN),
						r.Status.Color(app.Color, string(r.Status)),
					)
				}
				_ = t.Render(cmd.OutOrStdout())
			}

			switch worst {
			case output.SevFail:
				return WithExitCode(ExitUnhealthy, fmt.Errorf("at least one standby exceeds the fail threshold"))
			case output.SevWarn:
				if app.Flags.Strict {
					return WithExitCode(ExitWarnStrict, fmt.Errorf("at least one standby exceeds the warn threshold (--strict)"))
				}
			}
			return nil
		},
	}
	c.Flags().StringVar(&warn, "warn", "64MB", "Lag threshold for WARN severity (e.g. 64MB, 128MiB)")
	c.Flags().StringVar(&fail, "fail", "1GB", "Lag threshold for FAIL severity (e.g. 1GB, 2GiB)")
	return c
}

func lagSeverity(lag int64, warn, fail int64) output.Severity {
	switch {
	case lag >= fail:
		return output.SevFail
	case lag >= warn:
		return output.SevWarn
	default:
		return output.SevPass
	}
}

// parseBytes accepts shorthand like "64MB", "128MiB", "1GB", "1GiB",
// "512KB". Decimal units (MB/GB/KB) multiply by 1000; binary units
// (MiB/GiB/KiB) multiply by 1024. Plain digits are bytes.
func parseBytes(s string, def int64) (int64, error) {
	if s == "" {
		return def, nil
	}
	mult := int64(1)
	num := s
	switch {
	case strings.HasSuffix(s, "KiB"):
		mult, num = 1024, strings.TrimSuffix(s, "KiB")
	case strings.HasSuffix(s, "MiB"):
		mult, num = 1024*1024, strings.TrimSuffix(s, "MiB")
	case strings.HasSuffix(s, "GiB"):
		mult, num = 1024*1024*1024, strings.TrimSuffix(s, "GiB")
	case strings.HasSuffix(s, "TiB"):
		mult, num = 1024*1024*1024*1024, strings.TrimSuffix(s, "TiB")
	case strings.HasSuffix(s, "KB"):
		mult, num = 1000, strings.TrimSuffix(s, "KB")
	case strings.HasSuffix(s, "MB"):
		mult, num = 1000*1000, strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "GB"):
		mult, num = 1000*1000*1000, strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "TB"):
		mult, num = 1000*1000*1000*1000, strings.TrimSuffix(s, "TB")
	case strings.HasSuffix(s, "B"):
		num = strings.TrimSuffix(s, "B")
	}
	var n int64
	if _, err := fmt.Sscanf(num, "%d", &n); err != nil {
		return 0, fmt.Errorf("invalid byte size %q: %w", s, err)
	}
	return n * mult, nil
}

func formatBytes(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%d KiB", n>>10)
	case n < 1<<30:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
	}
}
