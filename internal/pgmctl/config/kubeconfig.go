package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// DefaultPath returns the configured config-file path.
// Precedence: PGMCTL_CONFIG env > $XDG_CONFIG_HOME/pgmctl/config.yaml
// > $HOME/.config/pgmctl/config.yaml.
func DefaultPath() string {
	if p := os.Getenv("PGMCTL_CONFIG"); p != "" {
		return p
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "pgmctl", "config.yaml")
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".config", "pgmctl", "config.yaml")
	}
	return ".pgmctl.yaml"
}

// Load reads, parses, and validates the kubeconfig-style configuration
// at path. Returns (nil, ErrNotExist-wrapped) if the file is missing
// — callers may treat that as a "no contexts configured" condition
// and rely on env/flag overrides instead.
//
// Permissions: refuses to load when group- or world-readable
// (mirrors ssh / kubectl strictness). RD-007.
func Load(path string) (*Config, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("pgmctl config %q is group- or world-readable (mode %o); refuse to load (`chmod 600 %s` to fix)", path, info.Mode().Perm(), path)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}

	if cfg.APIVersion == "" {
		cfg.APIVersion = "pgmctl/v1"
	}
	if cfg.APIVersion != "pgmctl/v1" {
		return nil, fmt.Errorf("config %q: unsupported apiVersion %q (want pgmctl/v1)", path, cfg.APIVersion)
	}
	if cfg.Kind == "" {
		cfg.Kind = "Config"
	}

	for _, c := range cfg.Contexts {
		if err := c.Validate(); err != nil {
			return nil, fmt.Errorf("config %q: %w", path, err)
		}
	}

	if cfg.CurrentContext != "" {
		if _, err := cfg.Find(cfg.CurrentContext); err != nil {
			return nil, fmt.Errorf("config %q: current-context: %w", path, err)
		}
	}

	return &cfg, nil
}

// Save writes cfg back to path with mode 0600. Creates the parent
// directory with mode 0700 if missing.
func Save(path string, cfg *Config) error {
	if cfg == nil {
		return errors.New("nil Config")
	}
	if cfg.APIVersion == "" {
		cfg.APIVersion = "pgmctl/v1"
	}
	if cfg.Kind == "" {
		cfg.Kind = "Config"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return os.WriteFile(path, raw, 0o600)
}
