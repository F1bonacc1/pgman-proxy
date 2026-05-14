// Package history implements the JetStream-backed durable event /
// audit history stream introduced in feature 003 (FR-016a).
//
// The stream replaces no existing surface; it is additive. Subjects:
//
//	pgman_proxy.<cluster_id>.history.event.>
//	pgman_proxy.<cluster_id>.history.audit.>
//
// Spec: specs/003-pgmctl-cli/contracts/history-stream.md.
package history
