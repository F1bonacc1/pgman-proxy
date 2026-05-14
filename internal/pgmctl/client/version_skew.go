package client

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// SkewClass classifies the client/server version delta.
type SkewClass int

const (
	SkewMatch SkewClass = iota
	SkewPatch
	SkewMinor
	SkewMajor
	SkewUnknown
)

func (s SkewClass) String() string {
	switch s {
	case SkewMatch:
		return "match"
	case SkewPatch:
		return "patch"
	case SkewMinor:
		return "minor"
	case SkewMajor:
		return "major"
	default:
		return "unknown"
	}
}

// ServerVersion is what /v1/version returns (engine_result body). We
// only consume the fields we need; unknown fields are ignored.
type ServerVersion struct {
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	NATS    string `json:"nats,omitempty"`
}

// FetchVersion calls GET /v1/version and decodes the server's version
// payload. Used both by `pgmctl version` (US1) and by the global
// version-skew check that runs before any cluster-affecting op.
func (c *Client) FetchVersion(ctx context.Context) (*ServerVersion, error) {
	env, err := c.GetJSON(ctx, "/v1/version")
	if err != nil {
		return nil, err
	}
	if len(env.EngineResult) == 0 {
		// /v1/version doesn't exist on the server (older feature
		// 001 build). Treat as unknown — pgmctl prints "(server
		// version unavailable)" rather than refusing.
		return nil, nil
	}
	var v ServerVersion
	if err := json.Unmarshal(env.EngineResult, &v); err != nil {
		return nil, fmt.Errorf("decode /v1/version: %w", err)
	}
	return &v, nil
}

// Classify computes the skew between the client and server semantic
// versions. Returns SkewUnknown if either side can't be parsed.
func Classify(client, server string) SkewClass {
	c, ok1 := parseSemver(client)
	s, ok2 := parseSemver(server)
	if !ok1 || !ok2 {
		return SkewUnknown
	}
	switch {
	case c.major != s.major:
		return SkewMajor
	case c.minor != s.minor:
		return SkewMinor
	case c.patch != s.patch:
		return SkewPatch
	default:
		return SkewMatch
	}
}

type semver struct{ major, minor, patch int }

func parseSemver(s string) (semver, bool) {
	s = strings.TrimPrefix(s, "v")
	// Strip any pre-release / build / -dirty / -<sha> suffix.
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.SplitN(s, ".", 3)
	if len(parts) < 2 {
		return semver{}, false
	}
	if len(parts) == 2 {
		parts = append(parts, "0")
	}
	out := semver{}
	var err error
	if out.major, err = strconv.Atoi(parts[0]); err != nil {
		return out, false
	}
	if out.minor, err = strconv.Atoi(parts[1]); err != nil {
		return out, false
	}
	if out.patch, err = strconv.Atoi(parts[2]); err != nil {
		return out, false
	}
	return out, true
}
