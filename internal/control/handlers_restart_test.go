// T113 — POST /v1/restart five-case grid per
// contracts/proxy-self-terminate.md § Tests.

package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProxy implements ProxySelfTerminator for the restart handler
// tests. SelfTerminateCalled records the reason so tests can assert
// the drain path was reached without actually calling os.Exit. The
// fields are written by the handler's background goroutine and read
// from the test goroutine, so the mutex is required under -race.
type fakeProxy struct {
	nodeID   string
	presence string

	mu                  sync.Mutex
	selfTerminateCalled bool
	selfTerminateReason string
}

func (f *fakeProxy) SupervisorPresence() string { return f.presence }
func (f *fakeProxy) LocalNodeID() string        { return f.nodeID }
func (f *fakeProxy) SelfTerminate(_ context.Context, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.selfTerminateCalled = true
	f.selfTerminateReason = reason
}

func (f *fakeProxy) selfTerminateState() (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.selfTerminateCalled, f.selfTerminateReason
}

func newRestartTestServer(t *testing.T, engine Engine, proxy ProxySelfTerminator) *Server {
	t.Helper()
	srv := newTestServer(t, engine, &fakeLeader{leader: true}, &fakeNATS{}, "")
	srv.proxy = proxy
	srv.nodeID = "node-a"
	return srv
}

// 1. postgres happy path — receiving peer's pg restarted.
func TestRestart_PostgresHappyPath(t *testing.T) {
	called := false
	engine := &fakeEngine{restartPostgresFn: func(_ context.Context) error {
		called = true
		return nil
	}}
	proxy := &fakeProxy{nodeID: "node-a", presence: "tini"}
	srv := newRestartTestServer(t, engine, proxy)

	body := `{"target":"postgres","target_node":"node-a"}`
	w := doAuthed(t, srv.Handler(), http.MethodPost, "/v1/restart", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !called {
		t.Fatal("Engine.RestartPostgres was never called")
	}
	if called, _ := proxy.selfTerminateState(); called {
		t.Fatal("SelfTerminate fired on postgres target — should be local pg restart only")
	}
}

// 2. proxy happy path under supervisor → 200 + SelfTerminate fired.
func TestRestart_ProxyHappyPathUnderSupervisor(t *testing.T) {
	proxy := &fakeProxy{nodeID: "node-a", presence: "tini"}
	srv := newRestartTestServer(t, &fakeEngine{}, proxy)
	body := `{"target":"proxy","target_node":"node-a"}`
	w := doAuthed(t, srv.Handler(), http.MethodPost, "/v1/restart", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	// SelfTerminate runs on a background goroutine after a 50ms
	// delay; poll briefly.
	waitFor(t, func() bool {
		called, _ := proxy.selfTerminateState()
		return called
	}, 2*time.Second, "SelfTerminate never invoked")
	if _, reason := proxy.selfTerminateState(); reason != "operator_restart" {
		t.Errorf("reason=%q, want operator_restart", reason)
	}
}

// 3. proxy refused when no supervisor detected.
func TestRestart_ProxyRefusedWithoutSupervisor(t *testing.T) {
	proxy := &fakeProxy{nodeID: "node-a", presence: "none"}
	srv := newRestartTestServer(t, &fakeEngine{}, proxy)
	body := `{"target":"proxy","target_node":"node-a"}`
	w := doAuthed(t, srv.Handler(), http.MethodPost, "/v1/restart", body)
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("status=%d, want 412; body=%s", w.Code, w.Body.String())
	}
	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error == nil || env.Error.Code != CodeSupervisorNotDetected {
		t.Errorf("error.code=%v, want %q", env.Error, CodeSupervisorNotDetected)
	}
	if called, _ := proxy.selfTerminateState(); called {
		t.Fatal("SelfTerminate fired despite supervisor refusal")
	}
}

// 4. proxy refused when target_node mismatches receiving peer.
func TestRestart_ProxyRefusedWithWrongPeer(t *testing.T) {
	proxy := &fakeProxy{nodeID: "node-a", presence: "tini"}
	srv := newRestartTestServer(t, &fakeEngine{}, proxy)
	body := `{"target":"proxy","target_node":"node-b"}`
	w := doAuthed(t, srv.Handler(), http.MethodPost, "/v1/restart", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", w.Code, w.Body.String())
	}
	var env envelope
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Error == nil || env.Error.Code != CodeWrongPeer {
		t.Errorf("error.code=%v, want %q", env.Error, CodeWrongPeer)
	}
}

// 5. proxy refused under audit-unavailable fail-closed gate.
func TestRestart_ProxyRefusedUnderAuditUnavailable(t *testing.T) {
	proxy := &fakeProxy{nodeID: "node-a", presence: "tini"}
	pub := &fakeNATS{}
	pub.setErr(errors.New("nats unavailable"))
	srv := newTestServer(t, &fakeEngine{}, &fakeLeader{leader: true}, pub, "")
	srv.proxy = proxy
	srv.nodeID = "node-a"
	// Prime audit-unhealthy by emitting one record into the broken sink.
	_ = srv.audit.Emit(context.Background(), AuditRecord{Operation: "Probe"})
	body := `{"target":"proxy","target_node":"node-a"}`
	w := doAuthed(t, srv.Handler(), http.MethodPost, "/v1/restart", body)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503; body=%s", w.Code, w.Body.String())
	}
	var env envelope
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Error == nil || env.Error.Code != CodeAuditUnavailable {
		t.Errorf("error.code=%v, want %q", env.Error, CodeAuditUnavailable)
	}
}

func TestRestart_InvalidTargetRejected(t *testing.T) {
	srv := newRestartTestServer(t, &fakeEngine{}, &fakeProxy{nodeID: "node-a", presence: "tini"})
	body := `{"target":"weird","target_node":"node-a"}`
	w := doAuthed(t, srv.Handler(), http.MethodPost, "/v1/restart", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "invalid target") {
		t.Errorf("body missing 'invalid target': %s", w.Body.String())
	}
}
