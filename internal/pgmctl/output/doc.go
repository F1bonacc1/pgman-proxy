// Package output renders pgmctl results in table / JSON / YAML / wide
// forms with color-aware severity mapping.
//
// Color is auto-disabled when stdout is not a TTY, when --no-color is
// set, or when NO_COLOR is set (FR-036). Non-table outputs are
// schema-versioned (`apiVersion: pgmctl/v1`) per FR-038.
//
// Spec: specs/003-pgmctl-cli/research.md § RD-006.
package output
