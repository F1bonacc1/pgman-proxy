// Package fanout implements the inter-peer NATS request/reply
// protocol introduced in feature 003 (FR-006a) so a single connected
// peer can aggregate per-peer slices for pgmctl.
//
// Spec: specs/003-pgmctl-cli/contracts/fanout-protocol.md.
package fanout
