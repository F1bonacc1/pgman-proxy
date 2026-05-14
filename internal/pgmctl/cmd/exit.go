package cmd

import (
	"context"
	"errors"

	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/client"
	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/confirm"
)

// Stable exit codes per data-model.md § ExitCode.
const (
	ExitOK             = 0
	ExitWarnStrict     = 1
	ExitUnhealthy      = 2
	ExitPartial        = 3
	ExitSubjectNA      = 4
	ExitUnknown        = 5
	ExitUsage          = 64
	ExitNetwork        = 65
	ExitVersionSkew    = 67
	ExitConfig         = 78
	ExitTimeout        = 124
)

// codedError lets a subcommand mark its error with a documented exit
// code so the main wrapper can pick it up from the error chain.
type codedError struct {
	code int
	err  error
}

func (e *codedError) Error() string { return e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }
func (e *codedError) ExitCode() int { return e.code }

// WithExitCode wraps err with an exit code. If err is nil, returns
// nil so callers can use it in a single-line return path.
func WithExitCode(code int, err error) error {
	if err == nil {
		return nil
	}
	return &codedError{code: code, err: err}
}

// ExitCodeFromError extracts the exit code carried by err. Falls
// back to ExitUnhealthy (2) for ordinary errors.
func ExitCodeFromError(err error) int {
	if err == nil {
		return ExitOK
	}
	var ce *codedError
	if errors.As(err, &ce) {
		return ce.code
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ExitTimeout
	}
	var netErr *client.NetworkError
	if errors.As(err, &netErr) {
		return ExitNetwork
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case "auth_required", "auth_invalid":
			return ExitNetwork
		}
		return ExitUnhealthy
	}
	if errors.Is(err, confirm.ErrRefused) || errors.Is(err, confirm.ErrNotTTY) {
		return ExitUsage
	}
	return ExitUnhealthy
}
