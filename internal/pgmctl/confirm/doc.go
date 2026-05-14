// Package confirm implements the two confirmation walls pgmctl uses
// before any mutating operation.
//
//   - Single-resource ops (fence/unfence/set-config): [y/N] prompt;
//     -y bypasses (FR-028).
//   - Cluster-affecting ops (failover/switchover/promote/restart/
//     delete): typed cluster-name confirmation; --force --cluster
//     <name> bypasses with name-match enforced (FR-029). -y alone
//     MUST NOT bypass.
//
// In non-TTY contexts without an override flag, the prompt refuses
// rather than assuming "y".
package confirm
