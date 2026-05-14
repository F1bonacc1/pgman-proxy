package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	cfg "github.com/f1bonacc1/pgman-proxy/internal/pgmctl/config"
)

// TokenSource resolves a bearer token at every request. Per FR-031 of
// feature 001, tokens are re-read on every request so rotation
// requires no restart; this matches kubectl's exec-credential pattern.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	// SourceID is a short non-secret label (e.g. "env:PGMCTL_TOKEN"
	// or "file:/var/run/secrets/.../prod-east.token") used in
	// --verbose output. The plaintext token MUST NEVER be returned
	// by SourceID.
	SourceID() string
}

// NewTokenSource picks the correct source from a resolved profile.
// Exactly one of TokenEnv / TokenFile / TokenCommand is populated
// after a successful config.Resolve.
func NewTokenSource(r *cfg.Resolved) (TokenSource, error) {
	switch {
	case r.TokenEnv != "":
		return envSource{name: r.TokenEnv}, nil
	case r.TokenFile != "":
		p, err := expandHome(r.TokenFile)
		if err != nil {
			return nil, err
		}
		return fileSource{path: p}, nil
	case len(r.TokenCommand) > 0:
		return commandSource{argv: append([]string{}, r.TokenCommand...)}, nil
	default:
		return nil, errors.New("no token source resolved")
	}
}

type envSource struct{ name string }

func (s envSource) Token(_ context.Context) (string, error) {
	v := os.Getenv(s.name)
	if v == "" {
		return "", fmt.Errorf("env %s is empty or unset", s.name)
	}
	return strings.TrimSpace(v), nil
}
func (s envSource) SourceID() string { return "env:" + s.name }

type fileSource struct{ path string }

func (s fileSource) Token(_ context.Context) (string, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return "", fmt.Errorf("read token file %s: %w", s.path, err)
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return "", fmt.Errorf("token file %s is empty", s.path)
	}
	return tok, nil
}
func (s fileSource) SourceID() string { return "file:" + s.path }

type commandSource struct{ argv []string }

func (s commandSource) Token(ctx context.Context) (string, error) {
	if len(s.argv) == 0 {
		return "", errors.New("empty token command")
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, s.argv[0], s.argv[1:]...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("token command %v failed: %w", s.argv, err)
	}
	tok := strings.TrimSpace(string(out))
	if tok == "" {
		return "", fmt.Errorf("token command %v produced empty output", s.argv)
	}
	return tok, nil
}
func (s commandSource) SourceID() string {
	return "command:" + strings.Join(s.argv, " ")
}

func expandHome(p string) (string, error) {
	if !strings.HasPrefix(p, "~") {
		return p, nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand %q: %w", p, err)
	}
	if p == "~" {
		return h, nil
	}
	return filepath.Join(h, strings.TrimPrefix(p, "~/")), nil
}
