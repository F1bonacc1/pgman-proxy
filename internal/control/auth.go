// Bearer-token authentication with hot rotation (T054 / FR-025 / FR-031).
//
// Tokens come from one of two sources, validated as mutually exclusive
// at config-load time:
//
//   - control.auth.token_env: name of an environment variable holding
//     the token. Re-read every request via os.Getenv so an operator
//     `kill -HUP` after `export PGMAN_PROXY_CONTROL_TOKEN=...` rotates
//     without restart.
//   - control.auth.token_file: path to a file containing the token
//     (with optional surrounding whitespace). Re-read every request
//     via os.ReadFile so an `echo new > /etc/pgman-proxy/token` rotates
//     without restart.
//
// Both paths use crypto/subtle.ConstantTimeCompare so token-prefix
// timing attacks return nothing useful.
//
// `actor` is what surfaces in audit records: `bearer:<sha256-prefix>`.
// The plaintext token MUST NEVER appear in any log line, metric, or
// audit record (Constitution II — Fail-Closed Safety implies "no
// secret leakage on the failure path either").

package control

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Authenticator resolves the live token on every call. The struct
// fields capture the configured source and are immutable once
// constructed; rotation happens by the source contents changing.
type Authenticator struct {
	tokenEnv         string
	tokenFile        string
	allowUnauthReads bool
	getEnv           func(string) string // injected for tests
	readFile         func(string) ([]byte, error)
}

// NewAuthenticator constructs an Authenticator from the validated
// config sub-block. Exactly one of tokenEnv / tokenFile MUST be
// non-empty (validation runs in config.Validate); we don't re-check
// here, so callers MUST ensure the config has been validated.
func NewAuthenticator(tokenEnv, tokenFile string, allowUnauthReads bool) *Authenticator {
	return &Authenticator{
		tokenEnv:         tokenEnv,
		tokenFile:        tokenFile,
		allowUnauthReads: allowUnauthReads,
		getEnv:           os.Getenv,
		readFile:         os.ReadFile,
	}
}

// AllowUnauthReads reports whether Status / Diagnose can be served
// without credentials.
func (a *Authenticator) AllowUnauthReads() bool { return a.allowUnauthReads }

// Verify checks the supplied Authorization header against the live
// token source. Returns the audit-friendly actor identifier on success;
// returns ErrAuthRequired when no header is supplied, ErrAuthInvalid on
// any mismatch.
func (a *Authenticator) Verify(authzHeader string) (actor string, err error) {
	const prefix = "Bearer "
	if !strings.HasPrefix(authzHeader, prefix) {
		return "", ErrAuthRequired
	}
	supplied := strings.TrimSpace(authzHeader[len(prefix):])
	if supplied == "" {
		return "", ErrAuthRequired
	}

	expected, err := a.liveToken()
	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}
	if expected == "" {
		// A configured-but-empty token source is a fail-closed signal —
		// reject every credential rather than admit anyone.
		return "", ErrAuthInvalid
	}
	if subtle.ConstantTimeCompare([]byte(supplied), []byte(expected)) != 1 {
		return "", ErrAuthInvalid
	}
	return actorFor(supplied), nil
}

// liveToken returns the current token from the configured source. The
// source is read on every invocation so rotation requires no restart.
// Errors here are typed so callers can map to `internal` rather than
// `auth_invalid` when the source itself is broken (file disappeared).
func (a *Authenticator) liveToken() (string, error) {
	switch {
	case a.tokenEnv != "":
		return a.getEnv(a.tokenEnv), nil
	case a.tokenFile != "":
		raw, err := a.readFile(a.tokenFile)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(raw)), nil
	}
	return "", errors.New("no token source configured")
}

// actorFor returns the audit-friendly identifier for a credential.
// Sentinel format: `bearer:<sha256-prefix-16>`. Sixteen hex characters
// is enough to disambiguate operators in audit logs without leaking
// the secret.
func actorFor(token string) string {
	if token == "" {
		return "anonymous"
	}
	sum := sha256.Sum256([]byte(token))
	return "bearer:" + hex.EncodeToString(sum[:8])
}

// Sentinel auth errors. Defined as exported variables so handlers can
// errors.Is on them when mapping to the documented error codes.
var (
	ErrAuthRequired = errors.New("auth: credential required")
	ErrAuthInvalid  = errors.New("auth: credential invalid")
)
