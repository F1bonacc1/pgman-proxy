// Package config is the pgmctl kubeconfig-style configuration loader.
//
// Loads and validates the user-facing configuration file at
// $XDG_CONFIG_HOME/pgmctl/config.yaml (falls back to ~/.config/pgmctl/
// config.yaml). Resolves endpoint and credential precedence per FR-006
// and FR-008. Refuses to load if the file is group- or world-readable.
//
// Spec: specs/003-pgmctl-cli/research.md § RD-007.
package config
