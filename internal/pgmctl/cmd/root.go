package cmd

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/client"
	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/config"
	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/output"
)

// BuildInfo is the version and commit baked at build time by the
// Makefile's pgmctl target.
type BuildInfo struct {
	Version string
	Commit  string
}

// GlobalFlags holds every global flag value documented in
// contracts/cli-commands.md § Global flags. One instance is created
// in NewRoot and shared via cobra's persistent flags.
type GlobalFlags struct {
	OutputFormat              string
	NoColor                   bool
	Quiet                     bool
	Verbose                   int
	Timeout                   time.Duration
	Yes                       bool
	Force                     bool
	Endpoint                  string
	Context                   string
	Cluster                   string
	InsecureSkipTLSVerify     bool
	InsecureSkipVersionCheck  bool
	Strict                    bool
	ConfigPath                string
}

// AppContext is the per-invocation state every subcommand consumes:
// resolved profile, http client, output format, colour helper. It is
// constructed lazily by Setup() to keep `--help` cheap and to defer
// auth / TLS errors until they actually matter.
type AppContext struct {
	Build       BuildInfo
	Flags       *GlobalFlags
	Format      output.Format
	Color       *output.Color

	// Resolved is the active context. Nil until Setup() is called.
	Resolved *config.Resolved
	// Client is the HTTP client. Nil until Setup() is called.
	Client *client.Client
}

// Setup resolves the active profile and constructs the HTTP client.
// MUST be called by any subcommand that talks to the server. Cheap
// for commands like `pgmctl version --help` to skip.
func (a *AppContext) Setup() error {
	if a.Resolved != nil {
		return nil
	}
	r, err := config.Resolve(config.Overrides{
		EndpointFlag: a.Flags.Endpoint,
		ContextFlag:  a.Flags.Context,
		ConfigPath:   a.Flags.ConfigPath,
	})
	if err != nil {
		return WithExitCode(ExitConfig, err)
	}
	a.Resolved = r
	c, err := client.New(client.Options{
		Resolved:       r,
		UserAgent:      "pgmctl/" + a.Build.Version,
		SkipTLSVerify:  a.Flags.InsecureSkipTLSVerify,
		RequestTimeout: a.requestTimeout(),
	})
	if err != nil {
		return WithExitCode(ExitConfig, err)
	}
	if a.Flags.InsecureSkipTLSVerify {
		fmt.Fprintln(os.Stderr, a.Color.Yellow("warning: --insecure-skip-tls-verify is set; the server's TLS certificate will NOT be validated"))
	}
	a.Client = c
	if a.Flags.Cluster != "" && c.ExpectedCluster() != "" && a.Flags.Cluster != c.ExpectedCluster() {
		return WithExitCode(ExitConfig, fmt.Errorf("--cluster %q does not match the active context's expected_cluster %q", a.Flags.Cluster, c.ExpectedCluster()))
	}
	return nil
}

func (a *AppContext) requestTimeout() time.Duration {
	if a.Flags.Timeout > 0 {
		return a.Flags.Timeout + 5*time.Second
	}
	return 30 * time.Second
}

// NewRoot builds the cobra command tree.
func NewRoot(b BuildInfo) *cobra.Command {
	flags := &GlobalFlags{}
	app := &AppContext{Build: b, Flags: flags}

	root := &cobra.Command{
		Use:   "pgmctl",
		Short: "Operator CLI for pgman-proxy clusters",
		Long: `pgmctl is the operator CLI for a running pgman-proxy cluster.
It consumes the existing HTTP control-plane and embedded-NATS observability
surfaces and renders them with kubectl-style ergonomics.

Spec: specs/003-pgmctl-cli/spec.md
`,
		Version:       b.Version,
		SilenceUsage:  true,
		SilenceErrors: false,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if flags.Quiet && flags.Verbose > 0 {
				return WithExitCode(ExitUsage, fmt.Errorf("--quiet and --verbose are mutually exclusive"))
			}
			f, err := output.ParseFormat(flags.OutputFormat)
			if err != nil {
				return WithExitCode(ExitUsage, err)
			}
			app.Format = f
			app.Color = output.NewColor(flags.NoColor, cmd.OutOrStdout())
			return nil
		},
	}

	// Suppress cobra's default help-only error wrapper.
	root.SetVersionTemplate(`pgmctl version {{.Version}}` + "\n")

	// Map cobra/pflag flag-parse errors to EX_USAGE (64) per FR-037.
	// Cobra's default is to surface them as plain errors, which our
	// fallback classifier would map to ExitUnhealthy.
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return WithExitCode(ExitUsage, err)
	})

	pf := root.PersistentFlags()
	pf.StringVarP(&flags.OutputFormat, "output", "o", "table", "Output format (table, json, yaml, wide)")
	pf.BoolVar(&flags.NoColor, "no-color", false, "Suppress ANSI escapes unconditionally")
	pf.BoolVarP(&flags.Quiet, "quiet", "q", false, "Suppress non-essential output")
	pf.CountVarP(&flags.Verbose, "verbose", "v", "Increase verbosity (-v / -vv / -vvv)")
	pf.DurationVar(&flags.Timeout, "timeout", 10*time.Second, "Overall command timeout")
	pf.BoolVarP(&flags.Yes, "yes", "y", false, "Skip single-resource confirmation prompts")
	pf.BoolVar(&flags.Force, "force", false, "Skip cluster-name confirmation (requires --cluster <name>)")
	pf.StringVar(&flags.Endpoint, "endpoint", "", "Single-shot pgman-proxy endpoint override")
	pf.StringVar(&flags.Context, "context", "", "Configured context name")
	pf.StringVar(&flags.Cluster, "cluster", "", "Expected cluster id; pinned before any cluster-affecting op")
	pf.BoolVar(&flags.InsecureSkipTLSVerify, "insecure-skip-tls-verify", false, "Disable TLS verification (warned)")
	pf.BoolVar(&flags.InsecureSkipVersionCheck, "insecure-skip-version-check", false, "Proceed despite minor / major version skew")
	pf.BoolVar(&flags.Strict, "strict", false, "Treat WARN as non-zero exit")
	pf.StringVar(&flags.ConfigPath, "config", "", "Path to pgmctl config (overrides $XDG_CONFIG_HOME/pgmctl/config.yaml)")

	root.AddCommand(
		newStatusCmd(app),
		newVersionCmd(app),
		newTopologyCmd(app),
		newHealthCmd(app),
		newLagCmd(app),
		newGetCmd(app),
		newListCmd(app),
		newDescribeCmd(app),
		newConfigCmd(app),
		newEventsCmd(app),
		newDumpCmd(app),
		newDoctorCmd(app),
	)

	return root
}
