package cmd

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	cfgpkg "github.com/f1bonacc1/pgman-proxy/internal/pgmctl/config"
	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/confirm"
)

// newConfigCmd builds the `pgmctl config …` subtree (FR-007).
//
// Subcommands:
//
//	config view
//	config use-context <name>
//	config set-context <name> [--endpoint ...] [...]
//	config delete-context <name>
//
// All operations are local file ops; none touches the network.
func newConfigCmd(app *AppContext) *cobra.Command {
	c := &cobra.Command{
		Use:   "config",
		Short: "Manage the local pgmctl configuration file (contexts, endpoints, credentials)",
		Long: `Manage the pgmctl kubeconfig-style configuration at
$XDG_CONFIG_HOME/pgmctl/config.yaml (fallback ~/.config/pgmctl/config.yaml).

All operations are local file operations; nothing on the network is touched.`,
	}
	c.AddCommand(newConfigView(app), newConfigUseContext(app), newConfigSetContext(app), newConfigDeleteContext(app))
	return c
}

func newConfigView(app *AppContext) *cobra.Command {
	var showSecrets bool
	c := &cobra.Command{
		Use:   "view",
		Short: "Render the active configuration with secrets redacted",
		Long: `Print the configuration file as YAML. Secret source references
(token files, token commands, env var names) are shown verbatim because
they are NOT the secret values themselves. To intentionally surface the
actual token contents, pass --show-secrets — refused in non-TTY contexts.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := app.Flags.ConfigPath
			if path == "" {
				path = cfgpkg.DefaultPath()
			}
			cfg, err := cfgpkg.Load(path)
			if err != nil {
				return WithExitCode(ExitConfig, err)
			}
			if showSecrets && !confirm.IsTTY(cmd.InOrStdin(), cmd.OutOrStdout()) {
				return WithExitCode(ExitUsage, fmt.Errorf("--show-secrets refused in non-TTY context"))
			}
			return renderConfig(cmd.OutOrStdout(), cfg, path, showSecrets)
		},
	}
	c.Flags().BoolVar(&showSecrets, "show-secrets", false, "Reveal token values inline (refused in non-TTY contexts)")
	return c
}

func newConfigUseContext(app *AppContext) *cobra.Command {
	return &cobra.Command{
		Use:   "use-context <name>",
		Short: "Set the current-context to <name>",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := app.Flags.ConfigPath
			if path == "" {
				path = cfgpkg.DefaultPath()
			}
			cfg, err := cfgpkg.Load(path)
			if err != nil {
				return WithExitCode(ExitConfig, err)
			}
			if _, err := cfg.Find(args[0]); err != nil {
				return WithExitCode(ExitConfig, err)
			}
			cfg.CurrentContext = args[0]
			if err := cfgpkg.Save(path, cfg); err != nil {
				return WithExitCode(ExitConfig, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "current-context set to %q\n", args[0])
			return nil
		},
	}
}

func newConfigSetContext(app *AppContext) *cobra.Command {
	var endpoint, expectedCluster, tokenEnv, tokenFile, caFile, serverName string
	var tokenCommand []string
	var insecureSkipTLS bool
	c := &cobra.Command{
		Use:   "set-context <name>",
		Short: "Create or update a named context",
		Long: `Create a new context, or update the named context in place.

Credentials: exactly one of --token-env, --token-file, or --token-command
must be supplied. Plaintext tokens are NEVER accepted on the flag list —
that's a deliberate non-feature to keep secrets out of shell history.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := app.Flags.ConfigPath
			if path == "" {
				path = cfgpkg.DefaultPath()
			}
			cfg, err := cfgpkg.Load(path)
			if err != nil {
				// Missing file is OK on set-context — it bootstraps
				// a new config.
				cfg = &cfgpkg.Config{APIVersion: "pgmctl/v1", Kind: "Config"}
			}

			// Find or append.
			name := args[0]
			var existing *cfgpkg.Context
			for i := range cfg.Contexts {
				if cfg.Contexts[i].Name == name {
					existing = &cfg.Contexts[i]
					break
				}
			}
			if existing == nil {
				cfg.Contexts = append(cfg.Contexts, cfgpkg.Context{Name: name})
				existing = &cfg.Contexts[len(cfg.Contexts)-1]
			}
			if endpoint != "" {
				existing.Endpoint = endpoint
			}
			if expectedCluster != "" {
				existing.ExpectedCluster = expectedCluster
			}
			if tokenEnv != "" {
				existing.TokenEnv = tokenEnv
				existing.TokenFile = ""
				existing.TokenCommand = nil
			}
			if tokenFile != "" {
				existing.TokenFile = tokenFile
				existing.TokenEnv = ""
				existing.TokenCommand = nil
			}
			if len(tokenCommand) > 0 {
				existing.TokenCommand = append([]string(nil), tokenCommand...)
				existing.TokenEnv = ""
				existing.TokenFile = ""
			}
			if caFile != "" {
				existing.TLS.CAFile = caFile
			}
			if serverName != "" {
				existing.TLS.ServerName = serverName
			}
			if cmd.Flags().Changed("insecure-skip-tls-verify") {
				existing.TLS.InsecureSkipTLSVerify = insecureSkipTLS
			}

			if err := existing.Validate(); err != nil {
				return WithExitCode(ExitConfig, err)
			}
			if cfg.CurrentContext == "" {
				cfg.CurrentContext = name
			}
			if err := cfgpkg.Save(path, cfg); err != nil {
				return WithExitCode(ExitConfig, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "context %q saved to %s\n", name, path)
			return nil
		},
	}
	c.Flags().StringVar(&endpoint, "endpoint", "", "Control-plane URL (https://host:port)")
	c.Flags().StringVar(&expectedCluster, "expected-cluster", "", "Cluster id pinned to this context")
	c.Flags().StringVar(&tokenEnv, "token-env", "", "Env var name holding the bearer token")
	c.Flags().StringVar(&tokenFile, "token-file", "", "File path holding the bearer token")
	c.Flags().StringSliceVar(&tokenCommand, "token-command", nil, "Command whose stdout is the bearer token (repeatable for argv parts)")
	c.Flags().StringVar(&caFile, "tls-ca-file", "", "TLS trust anchor PEM bundle")
	c.Flags().StringVar(&serverName, "tls-server-name", "", "TLS SNI server name override")
	c.Flags().BoolVar(&insecureSkipTLS, "insecure-skip-tls-verify", false, "Disable TLS verification for this context")
	return c
}

func newConfigDeleteContext(app *AppContext) *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:   "delete-context <name>",
		Short: "Remove a context",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := app.Flags.ConfigPath
			if path == "" {
				path = cfgpkg.DefaultPath()
			}
			cfg, err := cfgpkg.Load(path)
			if err != nil {
				return WithExitCode(ExitConfig, err)
			}
			name := args[0]
			if _, err := cfg.Find(name); err != nil {
				return WithExitCode(ExitConfig, err)
			}
			if cfg.CurrentContext == name && !force {
				return WithExitCode(ExitUsage, fmt.Errorf("context %q is current-context; pass --force to delete anyway (current-context will be cleared)", name))
			}
			out := cfg.Contexts[:0]
			for _, c := range cfg.Contexts {
				if c.Name != name {
					out = append(out, c)
				}
			}
			cfg.Contexts = out
			if cfg.CurrentContext == name {
				cfg.CurrentContext = ""
			}
			if err := cfgpkg.Save(path, cfg); err != nil {
				return WithExitCode(ExitConfig, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "context %q removed from %s\n", name, path)
			return nil
		},
	}
	c.Flags().BoolVar(&force, "force", false, "Allow deleting the current-context (clears current-context)")
	return c
}

func renderConfig(w io.Writer, cfg *cfgpkg.Config, path string, showSecrets bool) error {
	// Copy so we can mutate without touching disk state.
	view := *cfg
	view.Contexts = append([]cfgpkg.Context(nil), cfg.Contexts...)
	sort.SliceStable(view.Contexts, func(i, j int) bool { return view.Contexts[i].Name < view.Contexts[j].Name })

	if !showSecrets {
		for i := range view.Contexts {
			c := &view.Contexts[i]
			if c.TokenEnv != "" {
				c.TokenEnv = "env:" + c.TokenEnv + " (redacted)"
			}
			if c.TokenFile != "" {
				c.TokenFile = c.TokenFile + " (referenced)"
			}
			if len(c.TokenCommand) > 0 {
				c.TokenCommand = []string{"<command:" + strings.Join(c.TokenCommand, " ") + "> (referenced)"}
			}
		}
	}
	fmt.Fprintf(w, "# pgmctl configuration — %s\n", path)
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	defer enc.Close()
	return enc.Encode(view)
}
