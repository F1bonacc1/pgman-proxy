// Package watch implements the live SSE-driven views: status (diff
// redraw), transitions / events / node (append-only).
//
// Spec: specs/003-pgmctl-cli/research.md § RD-010 (redraw strategy)
// and § RD-004 (SSE wire format).
package watch
