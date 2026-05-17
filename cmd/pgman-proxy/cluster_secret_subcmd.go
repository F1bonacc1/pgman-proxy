package main

import (
	"fmt"
	"os"

	"github.com/f1bonacc1/pgman-proxy/internal/embedded"
	"github.com/f1bonacc1/pgman-proxy/internal/runtime"
)

// runClusterSecretGen implements the `pgman-proxy cluster-secret-gen`
// subcommand (feature 002 / RD-001a / contracts/cluster-credentials.md
// § Generation).
//
// Generates a cryptographically-random 32-byte cluster password,
// base32-encoded for ASCII-safety in env vars and config files.
// Prints the result to stdout in the canonical labelled form so an
// operator can pipe it into a secret-manager or `EnvironmentFile`
// without parsing.
//
// Exits 0 on success, runtime.ExitInternal on entropy failure.
func runClusterSecretGen(args []string) int {
	if len(args) > 0 {
		_, _ = fmt.Fprintf(os.Stderr, "pgman-proxy cluster-secret-gen: unexpected arguments: %v\n", args)
		return runtime.ExitConfig
	}
	pw, err := embedded.GenerateClusterPassword()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "pgman-proxy cluster-secret-gen: %v\n", err)
		return runtime.ExitInternal
	}
	fmt.Printf("password: %s\n", pw)
	return runtime.ExitOK
}
