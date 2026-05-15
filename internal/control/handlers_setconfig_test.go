// T114 — POST /v1/config/set allow-list test grid.

package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
)

// fakeReloader records Reload calls so tests can assert the trigger
// fired exactly once.
type fakeReloader struct {
	called atomic.Uint64
	err    error
}

func (r *fakeReloader) Reload(_ context.Context) error {
	r.called.Add(1)
	return r.err
}

func newSetConfigTestServer(t *testing.T, reloader Reloader) *Server {
	t.Helper()
	srv := newTestServer(t, &fakeEngine{}, &fakeLeader{leader: true}, &fakeNATS{}, "")
	srv.reloader = reloader
	return srv
}

func TestSetConfig_RoutePeers_Accepted(t *testing.T) {
	r := &fakeReloader{}
	srv := newSetConfigTestServer(t, r)
	body := `{"key":"cluster.route_peers"}`
	w := doAuthed(t, srv.Handler(), http.MethodPost, "/v1/config/set", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if r.called.Load() != 1 {
		t.Errorf("reloader called %d times, want 1", r.called.Load())
	}
	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Outcome != OutcomeAccepted || env.Operation != "SetConfig" {
		t.Errorf("envelope: %+v", env)
	}
}

func TestSetConfig_Password_Accepted(t *testing.T) {
	r := &fakeReloader{}
	srv := newSetConfigTestServer(t, r)
	body := `{"key":"cluster.password"}`
	w := doAuthed(t, srv.Handler(), http.MethodPost, "/v1/config/set", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if r.called.Load() != 1 {
		t.Errorf("reloader called %d times, want 1", r.called.Load())
	}
}

func TestSetConfig_DisallowedKey_Rejected(t *testing.T) {
	r := &fakeReloader{}
	srv := newSetConfigTestServer(t, r)
	body := `{"key":"postgres.bin_dir"}`
	w := doAuthed(t, srv.Handler(), http.MethodPost, "/v1/config/set", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", w.Code, w.Body.String())
	}
	var env envelope
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Error == nil || env.Error.Code != CodeSetConfigKeyDisallowed {
		t.Errorf("error.code=%v, want %q", env.Error, CodeSetConfigKeyDisallowed)
	}
	if r.called.Load() != 0 {
		t.Errorf("reloader fired despite disallowed key (%d calls)", r.called.Load())
	}
}

func TestSetConfig_EmptyKey_Rejected(t *testing.T) {
	srv := newSetConfigTestServer(t, &fakeReloader{})
	body := `{"key":""}`
	w := doAuthed(t, srv.Handler(), http.MethodPost, "/v1/config/set", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", w.Code, w.Body.String())
	}
	var env envelope
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Error == nil || env.Error.Code != CodeInvalidArgument {
		t.Errorf("error.code=%v, want %q", env.Error, CodeInvalidArgument)
	}
}

func TestSetConfig_ReloaderError_Reported(t *testing.T) {
	r := &fakeReloader{err: errors.New("nats reload failed")}
	srv := newSetConfigTestServer(t, r)
	body := `{"key":"cluster.route_peers"}`
	w := doAuthed(t, srv.Handler(), http.MethodPost, "/v1/config/set", body)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500; body=%s", w.Code, w.Body.String())
	}
	var env envelope
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Error == nil || env.Error.Code != CodeEngineError {
		t.Errorf("error.code=%v, want %q", env.Error, CodeEngineError)
	}
}

func TestSetConfig_ReloaderUnwired_EngineError(t *testing.T) {
	srv := newSetConfigTestServer(t, nil)
	body := `{"key":"cluster.route_peers"}`
	w := doAuthed(t, srv.Handler(), http.MethodPost, "/v1/config/set", body)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500; body=%s", w.Code, w.Body.String())
	}
}
