package embedded

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
)

// ClusterCredential holds the shared username + password used for
// cluster-route authentication. Per RD-001a (upstream NATS constraint
// discovery): NATS v2.14 cluster routes only support a single shared
// user/password pair; per-peer NKey-on-routes is not in the upstream
// protocol. Per-peer identity in audit logs comes from the NATS
// server-name field (set to the pgman-proxy node ID at startup).
//
// The username is conventionally a non-secret cluster identifier
// (e.g. "pgman-proxy-cluster-a"); the password is the actual secret
// material. Both are loaded via SecretRef per FR-010 — never
// plaintext-config-resident.
type ClusterCredential struct {
	Username string
	Password string
}

// Validate checks the credential is non-empty and within reasonable
// bounds. Empty username or password is a configuration error;
// passwords < 16 bytes are rejected as too weak (operators should use
// `pgman-proxy cluster-secret-gen` per RD-003 to produce strong values).
func (c ClusterCredential) Validate() error {
	if c.Username == "" {
		return errors.New("cluster credential: username is empty (FR-010)")
	}
	if c.Password == "" {
		return errors.New("cluster credential: password is empty (FR-010)")
	}
	if len(c.Password) < 16 {
		return fmt.Errorf("cluster credential: password is shorter than 16 bytes (got %d); use `pgman-proxy cluster-secret-gen` to produce a strong value", len(c.Password))
	}
	return nil
}

// Redact returns a safe-for-logging form of the credential — username
// in full (it's not secret), password trimmed to its first 8
// characters with a clear redaction marker. Used in audit lines and
// startup events so an operator can identify which credential is in
// effect without leaking the password.
func (c ClusterCredential) Redact() string {
	pwPrefix := c.Password
	if len(pwPrefix) > 8 {
		pwPrefix = pwPrefix[:8]
	}
	return fmt.Sprintf("user=%q pass_prefix=%q…", c.Username, pwPrefix)
}

// LoadClusterCredential parses raw username and password bytes into a
// ClusterCredential. The bytes are conventionally fetched via a
// SecretRef (env / file / secret-manager); callers MUST zero them
// after the call returns when they are no longer needed. The function
// trims surrounding whitespace from both fields so a trailing newline
// in a secret-file source doesn't cause silent auth failures.
func LoadClusterCredential(usernameBytes, passwordBytes []byte) (ClusterCredential, error) {
	c := ClusterCredential{
		Username: strings.TrimSpace(string(usernameBytes)),
		Password: strings.TrimSpace(string(passwordBytes)),
	}
	if err := c.Validate(); err != nil {
		return ClusterCredential{}, err
	}
	return c, nil
}

// GenerateClusterPassword returns a 32-byte cryptographically-random
// password, base32-encoded for ASCII-safety in env vars and config
// files. Used by the `pgman-proxy cluster-secret-gen` subcommand
// (RD-003, repurposed per RD-001a). Result is ~52 characters of
// base32-alphabet text.
func GenerateClusterPassword() (string, error) {
	const passwordBytes = 32
	buf := make([]byte, passwordBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("crypto/rand read: %w", err)
	}
	// Use lower-case base32 without padding for env-var friendliness.
	enc := base32.StdEncoding.WithPadding(base32.NoPadding)
	return strings.ToLower(enc.EncodeToString(buf)), nil
}
