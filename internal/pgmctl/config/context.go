package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Config is the root document persisted at
// $XDG_CONFIG_HOME/pgmctl/config.yaml. apiVersion is locked at
// pgmctl/v1; renames are MINOR-version events (FR-038).
type Config struct {
	APIVersion     string    `yaml:"apiVersion"`
	Kind           string    `yaml:"kind"`
	CurrentContext string    `yaml:"current-context"`
	Contexts       []Context `yaml:"contexts"`
}

// Context binds an endpoint, a credential source, and TLS material to
// a named profile. Exactly one of TokenEnv / TokenFile / TokenCommand
// MUST be set. See research.md RD-005 and RD-007.
type Context struct {
	Name            string   `yaml:"name"`
	Endpoint        string   `yaml:"endpoint"`
	ExpectedCluster string   `yaml:"expected_cluster,omitempty"`
	TokenEnv        string   `yaml:"token_env,omitempty"`
	TokenFile       string   `yaml:"token_file,omitempty"`
	TokenCommand    []string `yaml:"token_command,omitempty"`
	TLS             TLSBlock `yaml:"tls,omitempty"`
}

// TLSBlock is the per-context TLS trust + override material.
type TLSBlock struct {
	CAFile                string `yaml:"ca_file,omitempty"`
	ServerName            string `yaml:"server_name,omitempty"`
	InsecureSkipTLSVerify bool   `yaml:"insecure_skip_tls_verify,omitempty"`
}

// Validate enforces the per-context invariants documented in
// data-model.md § Context.
func (c Context) Validate() error {
	if c.Name == "" {
		return errors.New("context.name is required")
	}
	if c.Endpoint == "" {
		return fmt.Errorf("context %q: endpoint is required", c.Name)
	}

	u, err := url.Parse(c.Endpoint)
	if err != nil {
		return fmt.Errorf("context %q: endpoint %q is not a valid URL: %w", c.Name, c.Endpoint, err)
	}
	if u.Scheme != "https" && !isLoopback(u.Host) {
		return fmt.Errorf("context %q: endpoint must use https:// unless host is loopback (got %q)", c.Name, c.Endpoint)
	}

	srcs := 0
	if c.TokenEnv != "" {
		srcs++
	}
	if c.TokenFile != "" {
		srcs++
	}
	if len(c.TokenCommand) > 0 {
		srcs++
	}
	if srcs == 0 {
		return fmt.Errorf("context %q: exactly one of token_env / token_file / token_command must be set", c.Name)
	}
	if srcs > 1 {
		return fmt.Errorf("context %q: more than one token source set (env=%v, file=%v, command=%v); pick one", c.Name, c.TokenEnv != "", c.TokenFile != "", len(c.TokenCommand) > 0)
	}
	return nil
}

func isLoopback(host string) bool {
	// strip optional port
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	host = strings.Trim(host, "[]")
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return strings.HasPrefix(host, "127.")
}

// Find returns the named context or an error if not present.
func (c *Config) Find(name string) (*Context, error) {
	for i := range c.Contexts {
		if c.Contexts[i].Name == name {
			return &c.Contexts[i], nil
		}
	}
	return nil, fmt.Errorf("context %q not found in config (known: %s)", name, c.knownContextNames())
}

func (c *Config) knownContextNames() string {
	names := make([]string, 0, len(c.Contexts))
	for _, ctx := range c.Contexts {
		names = append(names, ctx.Name)
	}
	if len(names) == 0 {
		return "<none>"
	}
	return strings.Join(names, ", ")
}
