// Command pgmctl-docs generates documentation for the pgmctl
// command tree. Produces:
//
//	docs/pgmctl/reference/<cmd>.md  — one markdown page per subcommand
//	docs/pgmctl/man/pgmctl.1        — top-level groff man page
//
// Invoke via `make pgmctl-docs` (added to the Makefile) or by hand:
//
//	go run ./cmd/pgmctl-docs --reference docs/pgmctl/reference \
//	                        --man docs/pgmctl/man
//
// Idempotent — overwrites every file under the chosen output dirs.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"

	pgmctlcmd "github.com/f1bonacc1/pgman-proxy/internal/pgmctl/cmd"
)

func main() {
	var refDir, manDir string
	flag.StringVar(&refDir, "reference", "docs/pgmctl/reference", "Output dir for per-command markdown pages")
	flag.StringVar(&manDir, "man", "docs/pgmctl/man", "Output dir for groff(1) man pages")
	flag.Parse()

	root := pgmctlcmd.NewRoot(pgmctlcmd.BuildInfo{Version: "v1.0.0", Commit: "docs"})
	root.DisableAutoGenTag = true

	if err := os.MkdirAll(refDir, 0o755); err != nil {
		fail("mkdir %s: %v", refDir, err)
	}
	if err := os.MkdirAll(manDir, 0o755); err != nil {
		fail("mkdir %s: %v", manDir, err)
	}

	if err := doc.GenMarkdownTree(root, refDir); err != nil {
		fail("generate markdown: %v", err)
	}
	fmt.Fprintf(os.Stdout, "wrote per-command markdown to %s\n", refDir)

	header := &doc.GenManHeader{
		Title:   "PGMCTL",
		Section: "1",
		Source:  "pgman-proxy",
		Manual:  "pgmctl Manual",
	}
	if err := doc.GenManTree(root, header, manDir); err != nil {
		fail("generate man pages: %v", err)
	}
	fmt.Fprintf(os.Stdout, "wrote groff man pages to %s\n", manDir)

	// Suppress the unused-import lint when cobra cleans up its tree
	// state under -trimpath builds.
	_ = filepath.Walk
	_ = cobra.Command{}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "pgmctl-docs: "+format+"\n", args...)
	os.Exit(1)
}
