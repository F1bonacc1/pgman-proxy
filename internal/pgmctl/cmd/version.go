package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/client"
	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/output"
)

// versionPayload is the schema-versioned JSON/YAML form for
// `pgmctl version -o json`.
type versionPayload struct {
	Client clientVersion  `json:"client" yaml:"client"`
	Server *serverVersion `json:"server,omitempty" yaml:"server,omitempty"`
	Skew   string         `json:"skew,omitempty" yaml:"skew,omitempty"`
}

type clientVersion struct {
	Version   string `json:"version" yaml:"version"`
	Commit    string `json:"commit,omitempty" yaml:"commit,omitempty"`
	GoVersion string `json:"go_version" yaml:"go_version"`
}

type serverVersion struct {
	Version string `json:"version" yaml:"version"`
	Commit  string `json:"commit,omitempty" yaml:"commit,omitempty"`
	NATS    string `json:"nats,omitempty" yaml:"nats,omitempty"`
}

func newVersionCmd(app *AppContext) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print pgmctl + server versions and report skew",
		Long: `Print the pgmctl client version, the server's pgman-proxy version
(when reachable), and any version-skew between them.

Skew rules:
  patch  → silent
  minor  → yellow warning
  major  → refuse with exit code 67 unless --insecure-skip-version-check.

When the server endpoint is unconfigured or unreachable, prints the
client version only and exits 0.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()

			payload := versionPayload{
				Client: clientVersion{
					Version:   app.Build.Version,
					Commit:    app.Build.Commit,
					GoVersion: runtime.Version(),
				},
			}

			// Try to reach the server, but tolerate "no endpoint
			// configured" — `pgmctl version` is also useful as a
			// pure client introspection command.
			if err := app.Setup(); err == nil {
				ctx, cancel := context.WithTimeout(cmd.Context(), versionTimeout(app))
				defer cancel()
				sv, fetchErr := app.Client.FetchVersion(ctx)
				if fetchErr == nil && sv != nil {
					payload.Server = &serverVersion{Version: sv.Version, Commit: sv.Commit, NATS: sv.NATS}
					skew := client.Classify(app.Build.Version, sv.Version)
					payload.Skew = skew.String()
					if skew == client.SkewMajor && !app.Flags.InsecureSkipVersionCheck {
						return WithExitCode(ExitVersionSkew, fmt.Errorf("client/server major version skew: client=%s server=%s (pass --insecure-skip-version-check to override)", app.Build.Version, sv.Version))
					}
					if skew == client.SkewMinor && !app.Flags.Quiet {
						fmt.Fprintln(os.Stderr, app.Color.Yellow(fmt.Sprintf("warning: minor version skew (client=%s server=%s)", app.Build.Version, sv.Version)))
					}
				}
			}

			switch app.Format {
			case output.FormatJSON:
				return output.EmitJSON(out, "Version", payload)
			case output.FormatYAML:
				return output.EmitYAML(out, "Version", payload)
			default:
				return renderVersionTable(out, payload, app)
			}
		},
	}
}

func renderVersionTable(out interface {
	Write([]byte) (int, error)
}, p versionPayload, app *AppContext) error {
	if _, err := fmt.Fprintf(out, "pgmctl:       %s\n", p.Client.Version); err != nil {
		return err
	}
	if p.Client.Commit != "" {
		if _, err := fmt.Fprintf(out, "  commit:     %s\n", p.Client.Commit); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(out, "  go:         %s\n", p.Client.GoVersion); err != nil {
		return err
	}
	if p.Server == nil {
		if _, err := fmt.Fprintln(out, app.Color.Yellow("server:       (not reached — pgmctl version is offline)")); err != nil {
			return err
		}
		return nil
	}
	if _, err := fmt.Fprintf(out, "pgman-proxy:  %s\n", p.Server.Version); err != nil {
		return err
	}
	if p.Server.Commit != "" {
		if _, err := fmt.Fprintf(out, "  commit:     %s\n", p.Server.Commit); err != nil {
			return err
		}
	}
	if p.Server.NATS != "" {
		if _, err := fmt.Fprintf(out, "  nats:       %s\n", p.Server.NATS); err != nil {
			return err
		}
	}
	if p.Skew != "" {
		switch p.Skew {
		case "match":
			if _, err := fmt.Fprintf(out, "skew:         %s\n", app.Color.Green(p.Skew)); err != nil {
				return err
			}
		case "patch":
			if _, err := fmt.Fprintf(out, "skew:         %s\n", p.Skew); err != nil {
				return err
			}
		default:
			if _, err := fmt.Fprintf(out, "skew:         %s\n", app.Color.Yellow(p.Skew)); err != nil {
				return err
			}
		}
	}
	return nil
}

func versionTimeout(app *AppContext) time.Duration {
	if app.Flags.Timeout > 0 {
		return app.Flags.Timeout
	}
	return 5 * time.Second
}

// silence unused-import warnings for go build before status.go lands.
var _ = errors.New
