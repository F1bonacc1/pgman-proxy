// Package client is the pgmctl HTTP / SSE client layer.
//
// It owns the single control-plane connection per invocation (FR-006),
// bearer-token auth (FR-008), TLS verification (FR-009 / FR-010), and
// the strict no-retry rule for mutating operations (FR-039).
//
// Spec: specs/003-pgmctl-cli/contracts/cli-commands.md
package client
