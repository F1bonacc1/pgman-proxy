package pgmctl_contract

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/cmd"
)

const fakeToken = "test-token"

// TestStatus_HealthyCluster_JSON asserts the schema-versioned JSON
// envelope (FR-038) and that the bearer token is propagated.
func TestStatus_HealthyCluster_JSON(t *testing.T) {
	srv := startFakeServer(t, statusHealthy())
	defer srv.Close()

	out := runRoot(t, srv.URL, "status", "-o", "json")

	var doc struct {
		APIVersion string                 `json:"apiVersion"`
		Kind       string                 `json:"kind"`
		Payload    map[string]interface{} `json:"payload"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("decode: %v\nout=%s", err, out)
	}
	if doc.APIVersion != "pgmctl/v1" {
		t.Errorf("apiVersion = %q, want pgmctl/v1", doc.APIVersion)
	}
	if doc.Kind != "ClusterStatus" {
		t.Errorf("kind = %q, want ClusterStatus", doc.Kind)
	}
	cluster, ok := doc.Payload["cluster"].(map[string]interface{})
	if !ok {
		t.Fatalf("payload.cluster missing or wrong type: %v", doc.Payload)
	}
	if got := cluster["LeaderNodeID"]; got != "node-a" {
		t.Errorf("LeaderNodeID = %v, want node-a", got)
	}
}

// TestStatus_NoColorEnv_StripsANSI asserts SC-008: NO_COLOR=1
// suppresses every ANSI escape.
func TestStatus_NoColorEnv_StripsANSI(t *testing.T) {
	srv := startFakeServer(t, statusHealthy())
	defer srv.Close()

	t.Setenv("NO_COLOR", "1")
	out := runRoot(t, srv.URL, "status")

	if bytes.Contains(out, []byte("\x1b[")) {
		t.Errorf("output contained ANSI escapes under NO_COLOR=1:\n%s", out)
	}
}

// TestStatus_NoColorFlag_StripsANSI asserts the --no-color flag has
// the same effect.
func TestStatus_NoColorFlag_StripsANSI(t *testing.T) {
	srv := startFakeServer(t, statusHealthy())
	defer srv.Close()

	out := runRoot(t, srv.URL, "status", "--no-color")
	if bytes.Contains(out, []byte("\x1b[")) {
		t.Errorf("output contained ANSI escapes under --no-color:\n%s", out)
	}
}

// TestUnknownFlag_ExitsEX_USAGE asserts FR-037: bad flags exit 64.
func TestUnknownFlag_ExitsEX_USAGE(t *testing.T) {
	err := execRoot(t, "https://127.0.0.1:1", "--bogus")
	if got := cmd.ExitCodeFromError(err); got != cmd.ExitUsage {
		t.Errorf("ExitCode = %d, want %d (EX_USAGE)", got, cmd.ExitUsage)
	}
}

// TestQuietVerboseMutex_ExitsEX_USAGE asserts FR-005: --quiet and
// --verbose are mutually exclusive.
func TestQuietVerboseMutex_ExitsEX_USAGE(t *testing.T) {
	err := execRoot(t, "https://127.0.0.1:1", "--quiet", "-v", "status")
	if got := cmd.ExitCodeFromError(err); got != cmd.ExitUsage {
		t.Errorf("ExitCode = %d, want %d (EX_USAGE)", got, cmd.ExitUsage)
	}
}

// TestStatus_NetworkError_ExitsEX_NETWORK asserts FR-037: connection
// failures map to EX_NETWORK (65), distinct from cluster-unhealthy.
func TestStatus_NetworkError_ExitsEX_NETWORK(t *testing.T) {
	// 127.0.0.1:1 is the standard "definitely refused" endpoint.
	err := execRoot(t, "http://127.0.0.1:1", "status")
	if got := cmd.ExitCodeFromError(err); got != cmd.ExitNetwork {
		t.Errorf("ExitCode = %d, want %d (EX_NETWORK); err=%v", got, cmd.ExitNetwork, err)
	}
}

// TestStatus_NoEndpoint_ExitsEX_CONFIG asserts the documented
// EX_CONFIG (78) exit code for missing configuration.
func TestStatus_NoEndpoint_ExitsEX_CONFIG(t *testing.T) {
	// Run without setting PGMCTL_ENDPOINT or any context.
	t.Setenv("PGMCTL_ENDPOINT", "")
	t.Setenv("PGMCTL_TOKEN", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // empty config dir
	t.Setenv("HOME", t.TempDir())

	root := cmd.NewRoot(cmd.BuildInfo{Version: "test", Commit: "test"})
	root.SetArgs([]string{"status"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	err := root.Execute()
	if got := cmd.ExitCodeFromError(err); got != cmd.ExitConfig {
		t.Errorf("ExitCode = %d, want %d (EX_CONFIG); err=%v", got, cmd.ExitConfig, err)
	}
}

// TestStatus_SubstrateQuorumLost_JSON asserts the JSON shape exposes
// substrate.{required,responding,total,quorate} when fan-out reports
// only 1 of 3 peers responding (the 2-of-3-down scenario). Chaos-rig
// observation, 2026-05-16: operators paging on a lone-survivor cluster
// could not tell from `pgmctl status` that the cluster had lost
// quorum and writes were blocking on sync_commit — the header still
// read "Primary: node-c". This contract locks the new substrate
// sub-block that surfaces it.
func TestStatus_SubstrateQuorumLost_JSON(t *testing.T) {
	srv := startFakeServer(t, statusQuorumLost())
	defer srv.Close()

	out := runRoot(t, srv.URL, "status", "-o", "json")

	var doc struct {
		Payload map[string]interface{} `json:"payload"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("decode: %v\nout=%s", err, out)
	}
	sub, ok := doc.Payload["substrate"].(map[string]interface{})
	if !ok {
		t.Fatalf("payload.substrate missing or wrong type: %v", doc.Payload)
	}
	if got := sub["quorate"]; got != false {
		t.Errorf("substrate.quorate = %v, want false", got)
	}
	if got := sub["responding"]; got != float64(1) {
		t.Errorf("substrate.responding = %v, want 1", got)
	}
	if got := sub["required"]; got != float64(2) {
		t.Errorf("substrate.required = %v, want 2", got)
	}
	if got := sub["total"]; got != float64(3) {
		t.Errorf("substrate.total = %v, want 3", got)
	}
}

// TestStatus_SubstrateQuorumLost_Text asserts the human-readable
// rendering surfaces the QUORUM LOST line prominently, names the
// substrate consequence ("writes will block on sync_commit"), and
// downgrades the exit code to ExitUnhealthy.
func TestStatus_SubstrateQuorumLost_Text(t *testing.T) {
	srv := startFakeServer(t, statusQuorumLost())
	defer srv.Close()

	t.Setenv("PGMCTL_ENDPOINT", srv.URL)
	t.Setenv("PGMCTL_TOKEN", fakeToken)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("NO_COLOR", "1")

	root := cmd.NewRoot(cmd.BuildInfo{Version: "1.0.0", Commit: "abc1234"})
	root.SetArgs([]string{"status"})
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	err := root.Execute()

	if code := cmd.ExitCodeFromError(err); code != cmd.ExitUnhealthy {
		t.Errorf("ExitCode = %d, want %d (ExitUnhealthy); err=%v\nstdout=%s",
			code, cmd.ExitUnhealthy, err, stdout.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"Substrate: QUORUM LOST",
		"1/3 responding",
		"need 2",
		"sync_commit",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status text missing %q\nfull output:\n%s", want, out)
		}
	}
}

// TestStatus_VersionEndpoint_SchemaVersioned asserts that
// `pgmctl version -o json` returns the pgmctl/v1 envelope (FR-038).
func TestStatus_VersionEndpoint_SchemaVersioned(t *testing.T) {
	srv := startFakeServer(t, statusHealthy())
	defer srv.Close()
	out := runRoot(t, srv.URL, "version", "-o", "json")
	if !strings.Contains(string(out), `"apiVersion": "pgmctl/v1"`) {
		t.Errorf("version JSON missing apiVersion=pgmctl/v1:\n%s", out)
	}
	if !strings.Contains(string(out), `"kind": "Version"`) {
		t.Errorf("version JSON missing kind=Version:\n%s", out)
	}
}

// helpers --------------------------------------------------------------

func runRoot(t *testing.T, endpoint string, args ...string) []byte {
	t.Helper()
	t.Setenv("PGMCTL_ENDPOINT", endpoint)
	t.Setenv("PGMCTL_TOKEN", fakeToken)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	root := cmd.NewRoot(cmd.BuildInfo{Version: "1.0.0", Commit: "abc1234"})
	root.SetArgs(args)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute(%v): %v\nstderr=%s", args, err, stderr.String())
	}
	return stdout.Bytes()
}

func execRoot(t *testing.T, endpoint string, args ...string) error {
	t.Helper()
	t.Setenv("PGMCTL_ENDPOINT", endpoint)
	t.Setenv("PGMCTL_TOKEN", fakeToken)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	root := cmd.NewRoot(cmd.BuildInfo{Version: "1.0.0", Commit: "abc1234"})
	root.SetArgs(args)
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	return root.Execute()
}

// statusQuorumLost models the 2-of-3-down chaos scenario: node-c is
// the lone responding peer (still believes it's primary, but its
// JetStream KV bucket has lost write quorum). node-a and node-b are
// synthesised by the fan-out aggregator with State="unknown" — the
// peerCounts helper counts those as not-responding.
func statusQuorumLost() map[string]interface{} {
	now := time.Now().UTC()
	return map[string]interface{}{
		"engine": map[string]interface{}{
			"ClusterID":     "pgman-pc",
			"LeaderNodeID":  "node-c",
			"PrimaryNodeID": "node-c",
			"LocalNodeID":   "node-c",
			"LocalRole":     "primary",
			"LocalState":    "running",
			"Instances": []map[string]interface{}{
				{"NodeID": "node-a", "Role": "standby", "State": "unknown", "PostgresUp": false, "LagBytes": 0, "LastSeenAt": time.Time{}},
				{"NodeID": "node-b", "Role": "standby", "State": "unknown", "PostgresUp": false, "LagBytes": 0, "LastSeenAt": time.Time{}},
				{"NodeID": "node-c", "Role": "primary", "State": "running", "PostgresUp": true, "LagBytes": 0, "LastSeenAt": now},
			},
			"SyncStandbys":   []string{},
			"LastFailoverAt": time.Time{},
		},
		"embedded_nats": map[string]interface{}{
			"server_name":        "node-c",
			"ready":              true,
			"client_listen_addr": "127.0.0.1:4222",
			"routes_listen_addr": "0.0.0.0:6222",
			"tls_enabled":        false,
			"routes_meshed":      0,
			"replicas_factor":    3,
		},
	}
}

func statusHealthy() map[string]interface{} {
	now := time.Now().UTC()
	return map[string]interface{}{
		"engine": map[string]interface{}{
			"ClusterID":     "pgman-pc",
			"LeaderNodeID":  "node-a",
			"PrimaryNodeID": "node-a",
			"LocalNodeID":   "node-a",
			"LocalRole":     "primary",
			"LocalState":    "running",
			"Instances": []map[string]interface{}{
				{"NodeID": "node-a", "Role": "primary", "State": "running", "PostgresUp": true, "LagBytes": 0, "LastSeenAt": now},
				{"NodeID": "node-b", "Role": "standby", "State": "running", "PostgresUp": true, "LagBytes": 8192, "LastSeenAt": now},
				{"NodeID": "node-c", "Role": "standby", "State": "running", "PostgresUp": true, "LagBytes": 100 * 1024 * 1024, "LastSeenAt": now},
			},
			"SyncStandbys":   []string{"node-b"},
			"LastFailoverAt": time.Time{},
		},
		"embedded_nats": map[string]interface{}{
			"server_name":        "node-a",
			"ready":              true,
			"client_listen_addr": "127.0.0.1:4222",
			"routes_listen_addr": "0.0.0.0:6222",
			"tls_enabled":        false,
			"routes_meshed":      2,
			"replicas_factor":    3,
		},
	}
}

func startFakeServer(t *testing.T, statusBody map[string]interface{}) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/version", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r, t) {
			return
		}
		writeEnvelope(w, "Version", map[string]string{
			"version": "1.0.0",
			"commit":  "abc1234",
			"nats":    "v2.14.0",
		})
	})
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r, t) {
			return
		}
		writeEnvelope(w, "Status", statusBody)
	})
	return httptest.NewServer(mux)
}

func writeEnvelope(w http.ResponseWriter, op string, body any) {
	w.Header().Set("Content-Type", "application/json")
	raw, _ := json.Marshal(body)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"operation":     op,
		"request_id":    "01HZZ" + op,
		"outcome":       "accepted",
		"engine_result": json.RawMessage(raw),
	})
}

func checkAuth(w http.ResponseWriter, r *http.Request, t *testing.T) bool {
	t.Helper()
	got := r.Header.Get("Authorization")
	want := "Bearer " + fakeToken
	if got != want {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"operation": "Auth", "request_id": "fail", "outcome": "rejected",
			"error": map[string]string{"code": "auth_invalid", "message": "bad token"},
		})
		return false
	}
	return true
}

// suppress unused warning for the os import when t.Setenv handles all
// env interaction. Future contract tests may need direct os.Setenv.
var _ = os.Setenv
