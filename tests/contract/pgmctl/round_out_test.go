package pgmctl_contract

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/cmd"
)

// TestTopology_RendersTree asserts the topology tree shows leader,
// primary, sync standbys, and a peer node tree.
func TestTopology_RendersTree(t *testing.T) {
	srv := startFakeServer(t, statusHealthy())
	defer srv.Close()
	out := runRoot(t, srv.URL, "topology")
	want := []string{"pgman-pc", "leader:", "primary:", "sync_standbys:", "embedded_nats:", "peers", "node-a", "node-b", "node-c"}
	for _, w := range want {
		if !bytes.Contains(out, []byte(w)) {
			t.Errorf("topology output missing %q\n---\n%s", w, out)
		}
	}
}

// TestTopology_JSON asserts the topology document is schema-versioned.
func TestTopology_JSON(t *testing.T) {
	srv := startFakeServer(t, statusHealthy())
	defer srv.Close()
	out := runRoot(t, srv.URL, "topology", "-o", "json")
	var doc struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Payload    struct {
			ClusterID string `json:"cluster_id"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if doc.APIVersion != "pgmctl/v1" || doc.Kind != "Topology" || doc.Payload.ClusterID != "pgman-pc" {
		t.Errorf("unexpected topology doc: %+v", doc)
	}
}

// TestHealth_RollupAllOK asserts the health rollup line set with a
// healthy cluster.
func TestHealth_RollupAllOK(t *testing.T) {
	srv := startFakeServerWithDiagnose(t, statusHealthy(), map[string]any{
		"Healthy": true,
		"Issues":  []map[string]any{},
	})
	defer srv.Close()
	out := runRoot(t, srv.URL, "health")
	want := []string{"control-plane:", "embedded-nats:", "primary:", "leader:", "quorum:", "replication:", "overall:"}
	for _, w := range want {
		if !bytes.Contains(out, []byte(w)) {
			t.Errorf("health output missing %q\n%s", w, out)
		}
	}
}

// TestLag_ParsesAndRenders asserts lag command renders a per-standby
// table and identifies the high-lag node as WARN.
func TestLag_ParsesAndRenders(t *testing.T) {
	srv := startFakeServer(t, statusHealthy())
	defer srv.Close()
	out := runRoot(t, srv.URL, "lag", "--no-color")
	if !bytes.Contains(out, []byte("node-b")) || !bytes.Contains(out, []byte("node-c")) {
		t.Errorf("lag table missing standby nodes\n%s", out)
	}
	if !bytes.Contains(out, []byte("WARN")) {
		t.Errorf("lag did not flag the 100 MiB standby as WARN\n%s", out)
	}
}

// TestLag_RejectsBadThreshold asserts a malformed --warn value exits
// EX_USAGE (64).
func TestLag_RejectsBadThreshold(t *testing.T) {
	err := execRoot(t, "https://127.0.0.1:1", "lag", "--warn", "not-a-size")
	if got := cmd.ExitCodeFromError(err); got != cmd.ExitUsage {
		t.Errorf("ExitCode = %d, want %d (EX_USAGE)", got, cmd.ExitUsage)
	}
}

// TestGet_NodesTable asserts `pgmctl get nodes` renders a sortable
// table containing every peer.
func TestGet_NodesTable(t *testing.T) {
	srv := startFakeServer(t, statusHealthy())
	defer srv.Close()
	out := runRoot(t, srv.URL, "get", "nodes")
	for _, want := range []string{"node-a", "node-b", "node-c", "primary", "standby"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("get nodes missing %q\n%s", want, out)
		}
	}
}

// TestGet_NodesByName asserts that a name filter narrows to one row.
func TestGet_NodesByName(t *testing.T) {
	srv := startFakeServer(t, statusHealthy())
	defer srv.Close()
	out := runRoot(t, srv.URL, "get", "nodes", "node-b")
	if !bytes.Contains(out, []byte("node-b")) {
		t.Errorf("get nodes node-b missing node-b\n%s", out)
	}
	if bytes.Contains(out, []byte("node-a")) || bytes.Contains(out, []byte("node-c")) {
		t.Errorf("get nodes node-b leaked other nodes\n%s", out)
	}
}

// TestGet_Version_Offline asserts that `pgmctl get version` works
// without a server.
func TestGet_Version_Offline(t *testing.T) {
	// Use an unreachable endpoint; FetchVersion will fail but the
	// client-side fields should still render.
	t.Setenv("PGMCTL_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("PGMCTL_TOKEN", "x")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	root := cmd.NewRoot(cmd.BuildInfo{Version: "1.0.0", Commit: "abc1234"})
	root.SetArgs([]string{"get", "version", "-o", "json"})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	_ = root.Execute()

	if !strings.Contains(out.String(), `"version": "1.0.0"`) {
		t.Errorf("get version offline did not show client version\n%s", out.String())
	}
}

// TestGet_ConfigResource_DeferredError asserts that `get config`
// still surfaces a useful EX_USAGE error (server-side GET /v1/config
// is not implemented yet). `get events` / `get audit` are wired
// against /v1/history elsewhere — see events_test.go.
func TestGet_ConfigResource_DeferredError(t *testing.T) {
	srv := startFakeServer(t, statusHealthy())
	defer srv.Close()

	err := execRoot(t, srv.URL, "get", "config")
	if err == nil {
		t.Fatalf("get config: want error, got nil")
	}
	if got := cmd.ExitCodeFromError(err); got != cmd.ExitUsage {
		t.Errorf("get config: ExitCode = %d, want %d", got, cmd.ExitUsage)
	}
}

// TestConfig_Lifecycle_SetUseDelete asserts the full config CRUD path
// on a temp config file.
func TestConfig_Lifecycle_SetUseDelete(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	t.Setenv("PGMCTL_CONFIG", cfgPath)
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("HOME", tmp)

	// set-context
	exec(t, "config", "set-context", "dev",
		"--endpoint", "https://127.0.0.1:9091",
		"--token-env", "PGMCTL_DEV_TOKEN",
		"--expected-cluster", "dev-cluster",
	)
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("set-context did not create %s: %v", cfgPath, err)
	}
	mode, _ := os.Stat(cfgPath)
	if mode.Mode().Perm() != 0o600 {
		t.Errorf("config file mode = %o, want 0600", mode.Mode().Perm())
	}

	// view (should not error, should redact token by default)
	out := capture(t, "config", "view")
	if !strings.Contains(out, "PGMCTL_DEV_TOKEN") {
		t.Errorf("view did not reference the env var name\n%s", out)
	}
	if !strings.Contains(out, "redacted") {
		t.Errorf("view did not mark the token as redacted\n%s", out)
	}

	// use-context (should be a no-op since set-context already pinned it as current)
	exec(t, "config", "use-context", "dev")

	// delete-context — current-context, so requires --force
	err := execRootNoEnv(t, "config", "delete-context", "dev")
	if got := cmd.ExitCodeFromError(err); got != cmd.ExitUsage {
		t.Errorf("delete-context current-context: ExitCode = %d, want %d", got, cmd.ExitUsage)
	}
	exec(t, "config", "delete-context", "dev", "--force")
}

// helpers --------------------------------------------------------------

func startFakeServerWithDiagnose(t *testing.T, statusBody map[string]any, diagnoseBody map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/version", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r, t) {
			return
		}
		writeEnvelope(w, "Version", map[string]string{"version": "1.0.0", "commit": "abc1234", "nats": "v2.14.0"})
	})
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r, t) {
			return
		}
		writeEnvelope(w, "Status", statusBody)
	})
	mux.HandleFunc("/v1/diagnose", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r, t) {
			return
		}
		writeEnvelope(w, "Diagnose", diagnoseBody)
	})
	return httptest.NewServer(mux)
}

func exec(t *testing.T, args ...string) {
	t.Helper()
	root := cmd.NewRoot(cmd.BuildInfo{Version: "1.0.0", Commit: "abc1234"})
	root.SetArgs(args)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute(%v): %v\nstderr=%s", args, err, stderr.String())
	}
}

func capture(t *testing.T, args ...string) string {
	t.Helper()
	root := cmd.NewRoot(cmd.BuildInfo{Version: "1.0.0", Commit: "abc1234"})
	root.SetArgs(args)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute(%v): %v\nstderr=%s", args, err, stderr.String())
	}
	return stdout.String()
}

func execRootNoEnv(t *testing.T, args ...string) error {
	t.Helper()
	root := cmd.NewRoot(cmd.BuildInfo{Version: "1.0.0", Commit: "abc1234"})
	root.SetArgs(args)
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	return root.Execute()
}

// silence unused imports in some test build configurations
var _ time.Time
