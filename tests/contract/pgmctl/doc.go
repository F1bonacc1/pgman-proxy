// Package pgmctl_contract holds contract tests for the pgmctl CLI
// — golden-file diffs, output schema assertions, exit-code
// behaviour, and per-command flag wiring.
//
// Tests use an in-process fake control-plane server; nothing in this
// package requires a real pgman-proxy cluster.
package pgmctl_contract
