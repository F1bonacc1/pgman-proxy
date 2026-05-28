package pgmctl_contract

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/cmd"
)

// These tests lock the fence-the-current-primary guard (FR-028a). Fence
// is a promotion-eligibility marker, not a failover: fencing the live
// primary neither demotes it nor moves writes, it only blocks future
// promotion — leaving an incoherent snapshot (a primary that is both
// serving writes and marked fenced). `pgmctl fence <primary>` therefore
// refuses unless --force. statusHealthy() reports PrimaryNodeID=node-a,
// so node-a is the primary and node-b/node-c are standbys.

// TestFence_CurrentPrimary_RefusedByDefault asserts that fencing the
// current primary is refused with EX_USAGE before any POST /v1/fence,
// and that the message points the operator at failover/switchover.
// --yes is passed so the *only* thing that can block is the guard (not
// the non-TTY prompt, which would also yield EX_USAGE).
func TestFence_CurrentPrimary_RefusedByDefault(t *testing.T) {
	srv, rec := startFenceServer(t, statusHealthy(), http.StatusOK)

	stdout, _, err := runFenceCmd(t, srv.URL, "fence", "node-a", "--yes")
	if err == nil {
		t.Fatalf("fencing the current primary must error; stdout=%s", stdout)
	}
	if got := cmd.ExitCodeFromError(err); got != cmd.ExitUsage {
		t.Errorf("ExitCode = %d, want %d (EX_USAGE)", got, cmd.ExitUsage)
	}
	if rec.Calls() != 0 {
		t.Errorf("/v1/fence was called %d times; the guard must block before the POST", rec.Calls())
	}
	for _, want := range []string{"node-a", "current primary", "failover", "switchover", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal message missing %q; got: %v", want, err)
		}
	}
}

// TestFence_Standby_Allowed asserts that fencing a non-primary peer is
// unaffected by the guard and reaches POST /v1/fence with the target.
func TestFence_Standby_Allowed(t *testing.T) {
	srv, rec := startFenceServer(t, statusHealthy(), http.StatusOK)

	stdout, _, err := runFenceCmd(t, srv.URL, "fence", "node-b", "--yes")
	if err != nil {
		t.Fatalf("fencing a standby should succeed; err=%v stdout=%s", err, stdout)
	}
	if rec.Calls() != 1 {
		t.Fatalf("/v1/fence calls = %d, want 1", rec.Calls())
	}
	if rec.Target() != "node-b" {
		t.Errorf("fence target = %q, want node-b", rec.Target())
	}
	if !strings.Contains(stdout, "request_id=") {
		t.Errorf("expected request_id on stdout (FR-039); got %s", stdout)
	}
}

// TestFence_CurrentPrimary_ForceOverrides asserts --force bypasses the
// guard (POST proceeds) while still warning that fence does not move
// writes.
func TestFence_CurrentPrimary_ForceOverrides(t *testing.T) {
	srv, rec := startFenceServer(t, statusHealthy(), http.StatusOK)

	_, stderr, err := runFenceCmd(t, srv.URL, "fence", "node-a", "--yes", "--force")
	if err != nil {
		t.Fatalf("fence --force on the primary should succeed; err=%v stderr=%s", err, stderr)
	}
	if rec.Calls() != 1 || rec.Target() != "node-a" {
		t.Errorf("fence calls=%d target=%q, want 1 / node-a", rec.Calls(), rec.Target())
	}
	if !strings.Contains(stderr, "current primary") || !strings.Contains(stderr, "--force") {
		t.Errorf("expected a --force override warning on stderr; got %s", stderr)
	}
}

// TestFence_StatusUnavailable_ProceedsWithWarning asserts the guard is
// strictly additive: when GET /v1/status fails it warns and proceeds
// rather than blocking, so an invocation that worked before the guard
// still works during a control-plane hiccup. Targets node-a (the
// would-be primary) to prove the would-be-blocked case proceeds.
func TestFence_StatusUnavailable_ProceedsWithWarning(t *testing.T) {
	srv, rec := startFenceServer(t, statusHealthy(), http.StatusServiceUnavailable)

	_, stderr, err := runFenceCmd(t, srv.URL, "fence", "node-a", "--yes")
	if err != nil {
		t.Fatalf("status-unavailable must not block fence (best-effort guard); err=%v stderr=%s", err, stderr)
	}
	if rec.Calls() != 1 {
		t.Errorf("fence should still POST when status can't be verified; calls=%d", rec.Calls())
	}
	if !strings.Contains(stderr, "could not") {
		t.Errorf("expected a 'could not fetch status' warning on stderr; got %s", stderr)
	}
}

// --- helpers ----------------------------------------------------------

// fenceRecorder records POST /v1/fence calls so a test can assert the
// guard blocked (0 calls) or allowed (1 call, with target) the op.
type fenceRecorder struct {
	mu     sync.Mutex
	calls  int
	target string
}

func (f *fenceRecorder) record(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	var body struct {
		Target string `json:"target"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.target = body.Target
}

func (f *fenceRecorder) Calls() int     { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }
func (f *fenceRecorder) Target() string { f.mu.Lock(); defer f.mu.Unlock(); return f.target }

// startFenceServer serves /v1/status (with the given body, or an error
// envelope when statusCode is non-OK) and a recording /v1/fence.
func startFenceServer(t *testing.T, statusBody map[string]any, statusCode int) (*httptest.Server, *fenceRecorder) {
	t.Helper()
	rec := &fenceRecorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r, t) {
			return
		}
		if statusCode != 0 && statusCode != http.StatusOK {
			w.WriteHeader(statusCode)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"operation": "Status", "request_id": "err", "outcome": "rejected",
				"error": map[string]string{"code": "engine_error", "message": "status unavailable"},
			})
			return
		}
		writeEnvelope(w, "Status", statusBody)
	})
	mux.HandleFunc("/v1/fence", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r, t) {
			return
		}
		rec.record(r)
		writeEnvelope(w, "Fence", map[string]any{})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, rec
}

// runFenceCmd runs the root command capturing stdout, stderr, and the
// returned error (existing runRoot fatals on error / discards stderr).
func runFenceCmd(t *testing.T, endpoint string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	t.Setenv("PGMCTL_ENDPOINT", endpoint)
	t.Setenv("PGMCTL_TOKEN", fakeToken)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	root := cmd.NewRoot(cmd.BuildInfo{Version: "1.0.0", Commit: "abc1234"})
	root.SetArgs(args)
	var outBuf, errBuf strings.Builder
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	err = root.Execute()
	return outBuf.String(), errBuf.String(), err
}
