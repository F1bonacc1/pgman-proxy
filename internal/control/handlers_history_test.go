package control

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	pgmanager "github.com/f1bonacc1/pg-manager"

	"github.com/f1bonacc1/pgman-proxy/internal/history"
)

// stubHistoryQuerier returns canned events; per-call args are
// recorded so tests can assert on the parsed Query.
type stubHistoryQuerier struct {
	events  []history.HistoryEvent
	lastQ   history.Query
	calls   int
	failErr error
}

func (s *stubHistoryQuerier) Query(_ context.Context, q history.Query) (history.Result, error) {
	s.calls++
	s.lastQ = q
	if s.failErr != nil {
		return history.Result{}, s.failErr
	}
	return history.Result{
		APIVersion: "pgman-proxy/v1",
		Kind:       "HistoryQueryResult",
		Events:     s.events,
	}, nil
}

// newHistoryTestServer reuses newTestServer and additionally injects
// a HistoryQuerier so the new /v1/history handler has something to
// delegate to.
func newHistoryTestServer(t *testing.T, h HistoryQuerier) *Server {
	t.Helper()
	srv := newTestServer(t, &fakeEngine{}, &fakeLeader{leader: true}, &fakeNATS{}, "")
	srv.history = h
	return srv
}

func TestHistoryQuery_Happy(t *testing.T) {
	stub := &stubHistoryQuerier{
		events: []history.HistoryEvent{
			{APIVersion: "pgman-proxy/v1", Kind: "HistoryEvent", ID: "01hxy", Type: "state_transition", NodeID: "node-a", Time: time.Now().UTC()},
		},
	}
	srv := newHistoryTestServer(t, stub)

	w := doAuthed(t, srv.Handler(), http.MethodGet, "/v1/history?since=30m&category=event&node=node-a", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v\n%s", err, w.Body.String())
	}
	if env.Outcome != OutcomeAccepted {
		t.Errorf("outcome = %s, want accepted", env.Outcome)
	}
	if env.Operation != "HistoryQuery" {
		t.Errorf("operation = %s, want HistoryQuery", env.Operation)
	}

	if stub.calls != 1 {
		t.Errorf("calls = %d, want 1", stub.calls)
	}
	if stub.lastQ.Since != 30*time.Minute {
		t.Errorf("since = %s, want 30m", stub.lastQ.Since)
	}
	if stub.lastQ.Category != history.CategoryEvent {
		t.Errorf("category = %q, want event", stub.lastQ.Category)
	}
	if len(stub.lastQ.Nodes) != 1 || stub.lastQ.Nodes[0] != "node-a" {
		t.Errorf("nodes = %v, want [node-a]", stub.lastQ.Nodes)
	}
}

func TestHistoryQuery_BadSince(t *testing.T) {
	stub := &stubHistoryQuerier{}
	srv := newHistoryTestServer(t, stub)
	w := doAuthed(t, srv.Handler(), http.MethodGet, "/v1/history?since=not-a-duration", "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if stub.calls != 0 {
		t.Errorf("stub called %d times, want 0", stub.calls)
	}
}

func TestHistoryQuery_NilQuerier_ReturnsEmpty(t *testing.T) {
	srv := newHistoryTestServer(t, nil)
	w := doAuthed(t, srv.Handler(), http.MethodGet, "/v1/history", "")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Outcome != OutcomeAccepted {
		t.Errorf("outcome = %s, want accepted", env.Outcome)
	}
}

// Reference unused imports silently for tooling.
var _ pgmanager.NodeID
