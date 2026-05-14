// Package cmd is the cobra command tree for pgmctl.
//
// Subcommand layout matches contracts/cli-commands.md. The root command
// owns global flags (FR-004); each subcommand calls back into client /
// output / confirm packages.
package cmd
