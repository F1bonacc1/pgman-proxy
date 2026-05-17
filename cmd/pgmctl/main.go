// Command pgmctl is the operator CLI for a running pgman-proxy
// cluster — a single statically-linked Go binary that consumes the
// control-plane HTTP surface (feature 001) and the embedded-NATS
// observability sub-block (feature 002).
//
// pgmctl is a *client*: it never embeds NATS, never opens a
// PostgreSQL connection, never opens cluster routes.
//
// Spec: specs/003-pgmctl-cli/spec.md.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	pgmctlcmd "github.com/f1bonacc1/pgman-proxy/internal/pgmctl/cmd"
)

// Build-time variables wired by `make pgmctl` ldflags. Defaults are
// used by `go run` and unflagged `go build`.
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	root := pgmctlcmd.NewRoot(pgmctlcmd.BuildInfo{Version: version, Commit: commit})
	root.SetContext(ctx)

	if err := root.Execute(); err != nil {
		// cobra already printed the usage/error message; we only
		// need to exit non-zero. The actual exit code is set by the
		// subcommand via cmd/exit.go.
		_, _ = fmt.Fprintln(os.Stderr)
		os.Exit(pgmctlcmd.ExitCodeFromError(err))
	}
}
