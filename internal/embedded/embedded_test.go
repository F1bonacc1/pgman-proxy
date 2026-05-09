package embedded

import (
	"strings"
	"testing"
)

// TestDeriveReplicas_Table exercises the FR-011a / RD-004 derivation
// branches.
func TestDeriveReplicas_Table(t *testing.T) {
	tests := []struct {
		size     int
		want     int
		wantWarn bool
	}{
		{1, 1, false},
		{2, 2, true},
		{3, 3, false},
		{4, 3, false},
		{5, 3, false},
		{10, 3, false},
	}
	for _, tc := range tests {
		got, warn := DeriveReplicas(tc.size)
		if got != tc.want {
			t.Errorf("DeriveReplicas(%d) = %d, want %d", tc.size, got, tc.want)
		}
		if (warn != "") != tc.wantWarn {
			t.Errorf("DeriveReplicas(%d) warn=%q wantWarn=%v", tc.size, warn, tc.wantWarn)
		}
	}
}

// TestDeriveReplicas_PanicsOnInvalidSize asserts the invariant from
// RD-004: declared size <= 0 is a programming bug, not a recoverable
// error path.
func TestDeriveReplicas_PanicsOnInvalidSize(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on declared size = 0")
		}
	}()
	_, _ = DeriveReplicas(0)
}

// TestDecideReplicas_OverrideAuditWarning asserts the FR-011a override
// path emits an audit-line warning.
func TestDecideReplicas_OverrideAuditWarning(t *testing.T) {
	d := DecideReplicas(3, 5)
	if d.Effective() != 5 {
		t.Errorf("Effective() = %d, want 5", d.Effective())
	}
	if !d.Overridden() {
		t.Error("Overridden() = false, want true")
	}
	if !strings.Contains(d.Warning, "override") {
		t.Errorf("Warning missing override audit text: %q", d.Warning)
	}
}

// TestDecideReplicas_NoOverride asserts the no-override path matches
// the derivation table verbatim.
func TestDecideReplicas_NoOverride(t *testing.T) {
	d := DecideReplicas(3, 0)
	if d.Effective() != 3 {
		t.Errorf("Effective() = %d, want 3", d.Effective())
	}
	if d.Overridden() {
		t.Error("Overridden() = true, want false")
	}
}

// TestLoadClusterCredential_ValidationFailures covers each validation
// path documented in contracts/cluster-credentials.md § Failure
// scenarios + storage rules.
func TestLoadClusterCredential_ValidationFailures(t *testing.T) {
	tests := []struct {
		name    string
		user    string
		pass    string
		wantSub string
	}{
		{"empty username", "", "longenoughpassword12345", "username is empty"},
		{"empty password", "cluster-prod", "", "password is empty"},
		{"short password", "cluster-prod", "tooshort", "shorter than 16 bytes"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadClusterCredential([]byte(tc.user), []byte(tc.pass))
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q missing substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestLoadClusterCredential_TrimsWhitespace asserts secret-file
// trailing newlines don't cause silent auth failures.
func TestLoadClusterCredential_TrimsWhitespace(t *testing.T) {
	c, err := LoadClusterCredential([]byte(" cluster-prod \n"), []byte(" longenoughpassword12345\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Username != "cluster-prod" {
		t.Errorf("Username = %q, want %q (whitespace must be trimmed)", c.Username, "cluster-prod")
	}
	if c.Password != "longenoughpassword12345" {
		t.Errorf("Password not trimmed: %q", c.Password)
	}
}

// TestRedactOutput verifies the safe-for-logging form of a credential
// reveals the username (non-secret) and at most 8 chars of the
// password (FR-010a redaction).
func TestRedactOutput(t *testing.T) {
	c := ClusterCredential{Username: "cluster-prod", Password: "longenoughpassword12345"}
	out := c.Redact()
	if !strings.Contains(out, "cluster-prod") {
		t.Errorf("redact output missing username: %q", out)
	}
	if strings.Contains(out, "longenoughpassword12345") {
		t.Errorf("redact output leaks full password: %q", out)
	}
	if !strings.Contains(out, "longenou") {
		t.Errorf("redact output missing 8-char password prefix: %q", out)
	}
}

// TestGenerateClusterPassword_Strength asserts the generator produces
// passwords that pass Validate() — base32-encoded 32-byte values are
// >= 16 bytes and base32-alphabet-clean.
func TestGenerateClusterPassword_Strength(t *testing.T) {
	pw, err := GenerateClusterPassword()
	if err != nil {
		t.Fatalf("GenerateClusterPassword: %v", err)
	}
	if len(pw) < 16 {
		t.Errorf("generated password too short: %d bytes", len(pw))
	}
	c := ClusterCredential{Username: "cluster-prod", Password: pw}
	if err := c.Validate(); err != nil {
		t.Errorf("generated password fails Validate(): %v", err)
	}
	// Base32-lowercase alphabet (without padding) — sanity check.
	for _, r := range pw {
		if !((r >= 'a' && r <= 'z') || (r >= '2' && r <= '7')) {
			t.Errorf("password contains non-base32 character %q", r)
			break
		}
	}
}

// TestBuildOptions_SinglePeer covers the smallest non-trivial input:
// one-peer configuration, no credentials, no TLS, no routes listener.
func TestBuildOptions_SinglePeer(t *testing.T) {
	opts, err := BuildOptions(OptionsInput{
		NodeID:        "peer-a",
		ClusterName:   "demo",
		DeclaredSize:  1,
		ClientHost:    "127.0.0.1",
		ClientPort:    14222,
		RoutesEnabled: false,
	})
	if err != nil {
		t.Fatalf("BuildOptions: %v", err)
	}
	if opts.ServerName != "peer-a" {
		t.Errorf("ServerName = %q, want %q", opts.ServerName, "peer-a")
	}
	if opts.Host != "127.0.0.1" || opts.Port != 14222 {
		t.Errorf("client listener = %s:%d, want 127.0.0.1:14222", opts.Host, opts.Port)
	}
	if opts.Cluster.Port != 0 {
		t.Errorf("expected zero cluster port for single-peer, got %d", opts.Cluster.Port)
	}
	if !opts.JetStream {
		t.Error("expected JetStream to be enabled")
	}
}

// TestBuildOptions_ThreePeer covers the HA shape: routes enabled,
// credential supplied, TLS material absent (loopback OK).
func TestBuildOptions_ThreePeer(t *testing.T) {
	opts, err := BuildOptions(OptionsInput{
		NodeID:        "peer-a",
		ClusterName:   "demo",
		DeclaredSize:  3,
		ClientHost:    "127.0.0.1",
		ClientPort:    14222,
		RoutesEnabled: true,
		RoutesHost:    "127.0.0.1", // loopback in test
		RoutesPort:    16222,
		RoutePeers:    []string{"127.0.0.1:16223", "127.0.0.1:16224"},
		Credential: ClusterCredential{
			Username: "demo-cluster",
			Password: "longenoughpassword12345",
		},
	})
	if err != nil {
		t.Fatalf("BuildOptions: %v", err)
	}
	if opts.Cluster.Name != "demo" {
		t.Errorf("Cluster.Name = %q, want %q", opts.Cluster.Name, "demo")
	}
	if opts.Cluster.Username != "demo-cluster" || opts.Cluster.Password != "longenoughpassword12345" {
		t.Errorf("cluster credential not applied: user=%q password_set=%v", opts.Cluster.Username, opts.Cluster.Password != "")
	}
	if len(opts.Routes) != 2 {
		t.Errorf("Routes len = %d, want 2", len(opts.Routes))
	}
	for _, r := range opts.Routes {
		if r.User == nil {
			t.Errorf("route URL %q lacks embedded credential", r.String())
		}
	}
}

// TestBuildOptions_RequiresNodeID asserts the precondition that
// drives ServerName + audit identity.
func TestBuildOptions_RequiresNodeID(t *testing.T) {
	_, err := BuildOptions(OptionsInput{ClusterName: "demo", DeclaredSize: 1})
	if err == nil {
		t.Fatal("expected error when NodeID is empty")
	}
	if !strings.Contains(err.Error(), "NodeID") {
		t.Errorf("error missing NodeID: %v", err)
	}
}

// TestBuildOptions_RequiresClusterName asserts the cluster-name
// guard from FR-009 / RD-001a.
func TestBuildOptions_RequiresClusterName(t *testing.T) {
	_, err := BuildOptions(OptionsInput{NodeID: "peer-a", DeclaredSize: 1})
	if err == nil {
		t.Fatal("expected error when ClusterName is empty")
	}
	if !strings.Contains(err.Error(), "ClusterName") {
		t.Errorf("error missing ClusterName: %v", err)
	}
}
