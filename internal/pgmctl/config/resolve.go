package config

import (
	"errors"
	"fmt"
	"os"
)

// Resolved is the effective connection profile for one pgmctl
// invocation: the endpoint + credential + TLS settings the client
// will actually use, after applying the precedence rules below.
type Resolved struct {
	Endpoint        string
	ExpectedCluster string
	TLS             TLSBlock
	// Source describes which precedence layer supplied the
	// endpoint. Useful in --verbose output.
	Source string

	// Token source — exactly one will be set after a successful
	// Resolve, mirroring the per-Context invariant.
	TokenEnv     string
	TokenFile    string
	TokenCommand []string
}

// Overrides are the command-line and environment-derived overrides
// that may override the active context for one invocation.
type Overrides struct {
	EndpointFlag string // --endpoint
	ContextFlag  string // --context
	ConfigPath   string // overrides DefaultPath()

	// EndpointEnv is the value of PGMCTL_ENDPOINT (lower precedence
	// than flags and contexts).
	EndpointEnv string
}

// Resolve returns the effective Resolved profile for this invocation.
// Precedence (FR-006):
//
//  1. --endpoint flag (combined with --context, or with
//     PGMCTL_TOKEN env / token in env).
//  2. --context flag → named context from the loaded config.
//  3. current-context from the loaded config.
//  4. PGMCTL_ENDPOINT env (with PGMCTL_TOKEN env required).
//  5. Error.
//
// When --endpoint is supplied without --context, credentials come
// from PGMCTL_TOKEN (env name configured at runtime by main); a
// missing token surfaces as an explicit "no credential supplied"
// error.
func Resolve(ov Overrides) (*Resolved, error) {
	if ov.EndpointEnv == "" {
		ov.EndpointEnv = os.Getenv("PGMCTL_ENDPOINT")
	}
	cfgPath := ov.ConfigPath
	if cfgPath == "" {
		cfgPath = DefaultPath()
	}

	// Layer 1: --endpoint flag (highest precedence).
	if ov.EndpointFlag != "" {
		r := &Resolved{
			Endpoint: ov.EndpointFlag,
			Source:   "--endpoint flag",
		}
		if err := mergeFromContextIfNamed(r, cfgPath, ov.ContextFlag); err != nil {
			return nil, err
		}
		// Token still has to come from somewhere — the merged
		// context's source, or PGMCTL_TOKEN as a fallback.
		if !hasTokenSource(r) {
			if os.Getenv("PGMCTL_TOKEN") == "" {
				return nil, errors.New("--endpoint supplied but no credential found (set PGMCTL_TOKEN, or use --context to pick a configured context)")
			}
			r.TokenEnv = "PGMCTL_TOKEN"
		}
		return r, nil
	}

	// Layers 2 & 3: a configured context (named or current).
	cfg, cfgErr := Load(cfgPath)
	var named string
	switch {
	case ov.ContextFlag != "":
		named = ov.ContextFlag
	case cfg != nil && cfg.CurrentContext != "":
		named = cfg.CurrentContext
	}

	if cfg != nil && named != "" {
		c, err := cfg.Find(named)
		if err != nil {
			return nil, err
		}
		return fromContext(c, "context "+named), nil
	}

	// Layer 4: PGMCTL_ENDPOINT.
	if ov.EndpointEnv != "" {
		if os.Getenv("PGMCTL_TOKEN") == "" {
			return nil, errors.New("PGMCTL_ENDPOINT is set but PGMCTL_TOKEN is not — set PGMCTL_TOKEN or configure a context in $XDG_CONFIG_HOME/pgmctl/config.yaml")
		}
		//nolint:gosec // G101: "PGMCTL_TOKEN" is the env var NAME, not a token value
		return &Resolved{
			Endpoint: ov.EndpointEnv,
			Source:   "PGMCTL_ENDPOINT env",
			TokenEnv: "PGMCTL_TOKEN",
		}, nil
	}

	// Layer 5: nothing configured. Tell the operator how to fix it.
	if cfgErr != nil && !os.IsNotExist(cfgErr) {
		return nil, fmt.Errorf("no endpoint configured and config file at %s could not be loaded: %w", cfgPath, cfgErr)
	}
	return nil, fmt.Errorf("no pgman-proxy endpoint configured — try --endpoint https://<host>:9091 (plus PGMCTL_TOKEN env), --context <name> (with %s populated), or PGMCTL_ENDPOINT=... PGMCTL_TOKEN=... in the environment", cfgPath)
}

func fromContext(c *Context, source string) *Resolved {
	return &Resolved{
		Endpoint:        c.Endpoint,
		ExpectedCluster: c.ExpectedCluster,
		TLS:             c.TLS,
		Source:          source,
		TokenEnv:        c.TokenEnv,
		TokenFile:       c.TokenFile,
		TokenCommand:    c.TokenCommand,
	}
}

func mergeFromContextIfNamed(r *Resolved, cfgPath, name string) error {
	if name == "" {
		return nil
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		return fmt.Errorf("--context %q: %w", name, err)
	}
	c, err := cfg.Find(name)
	if err != nil {
		return err
	}
	// Merge: --endpoint already won; pull credentials + TLS from
	// the named context.
	if r.ExpectedCluster == "" {
		r.ExpectedCluster = c.ExpectedCluster
	}
	r.TLS = c.TLS
	r.TokenEnv = c.TokenEnv
	r.TokenFile = c.TokenFile
	r.TokenCommand = c.TokenCommand
	return nil
}

func hasTokenSource(r *Resolved) bool {
	return r.TokenEnv != "" || r.TokenFile != "" || len(r.TokenCommand) > 0
}
