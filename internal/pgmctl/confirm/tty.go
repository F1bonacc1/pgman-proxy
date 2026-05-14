package confirm

import (
	"io"
	"os"

	"github.com/mattn/go-isatty"
)

// IsTTY reports whether the given Reader/Writer pair are both
// TTY-backed file descriptors. Used to decide whether to prompt or
// refuse-without-override per FR-028 / FR-029.
func IsTTY(in io.Reader, out io.Writer) bool {
	inOk := false
	outOk := false
	if f, ok := in.(*os.File); ok {
		inOk = isatty.IsTerminal(f.Fd())
	}
	if f, ok := out.(*os.File); ok {
		outOk = isatty.IsTerminal(f.Fd())
	}
	return inOk && outOk
}
