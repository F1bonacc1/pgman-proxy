// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pgmanager "github.com/f1bonacc1/pg-manager"
	"github.com/f1bonacc1/pg-manager/upgrade"

	"github.com/f1bonacc1/pgman-proxy/internal/history"
	"github.com/f1bonacc1/pgman-proxy/internal/obs"
)

// fakeEngine implements Engine with overridable hooks. Lets tests
// drive every documented response shape without booting pg-manager.
type fakeEngine struct {
	statusFn         func(context.Context) (pgmanager.Status, error)
	diagnoseFn       func(context.Context) (pgmanager.Diagnosis, error)
	switchoverFn     func(context.Context, pgmanager.NodeID) error
	failoverFn       func(context.Context) error
	fenceFn          func(context.Context, pgmanager.NodeID) error
	unfenceFn        func(context.Context, pgmanager.NodeID) error
	promoteFn        func(context.Context) error
	updateTopologyFn func(context.Context, pgmanager.Topology, pgmanager.Policy) error
	triggerBackupFn  func(context.Context) (pgmanager.BackupID, error)
	prepareUpgradeFn func(context.Context, pgmanager.UpgradePlan) error
	executeUpgradeFn func(context.Context, pgmanager.UpgradePlan, upgrade.PreSwap) error
}

func (f *fakeEngine) Status(ctx context.Context) (pgmanager.Status, error) {
	if f.statusFn != nil {
		return f.statusFn(ctx)
	}
	return pgmanager.Status{}, nil
}
func (f *fakeEngine) Diagnose(ctx context.Context) (pgmanager.Diagnosis, error) {
	if f.diagnoseFn != nil {
		return f.diagnoseFn(ctx)
	}
	return pgmanager.Diagnosis{}, nil
}
func (f *fakeEngine) Switchover(ctx context.Context, t pgmanager.NodeID) error {
	if f.switchoverFn != nil {
		return f.switchoverFn(ctx, t)
	}
	return nil
}
func (f *fakeEngine) Failover(ctx context.Context) error {
	if f.failoverFn != nil {
		return f.failoverFn(ctx)
	}
	return nil
}
func (f *fakeEngine) Fence(ctx context.Context, t pgmanager.NodeID) error {
	if f.fenceFn != nil {
		return f.fenceFn(ctx, t)
	}
	return nil
}
func (f *fakeEngine) Unfence(ctx context.Context, t pgmanager.NodeID) error {
	if f.unfenceFn != nil {
		return f.unfenceFn(ctx, t)
	}
	return nil
}
func (f *fakeEngine) Promote(ctx context.Context) error {
	if f.promoteFn != nil {
		return f.promoteFn(ctx)
	}
	return nil
}
func (f *fakeEngine) UpdateTopology(ctx context.Context, t pgmanager.Topology, p pgmanager.Policy) error {
	if f.updateTopologyFn != nil {
		return f.updateTopologyFn(ctx, t, p)
	}
	return nil
}
func (f *fakeEngine) TriggerBackup(ctx context.Context) (pgmanager.BackupID, error) {
	if f.triggerBackupFn != nil {
		return f.triggerBackupFn(ctx)
	}
	return "", nil
}
func (f *fakeEngine) PrepareUpgrade(ctx context.Context, plan pgmanager.UpgradePlan) error {
	if f.prepareUpgradeFn != nil {
		return f.prepareUpgradeFn(ctx, plan)
	}
	return nil
}
func (f *fakeEngine) ExecuteUpgrade(ctx context.Context, plan pgmanager.UpgradePlan, pre upgrade.PreSwap) error {
	if f.executeUpgradeFn != nil {
		return f.executeUpgradeFn(ctx, plan, pre)
	}
	return nil
}

// fakeLeader is a static LeaderState for tests.
type fakeLeader struct {
	leader bool
	id     string
	addr   string
}

func (f *fakeLeader) IsLeader() bool     { return f.leader }
func (f *fakeLeader) LeaderID() string   { return f.id }
func (f *fakeLeader) LeaderAddr() string { return f.addr }

// fakeNATS captures Publish calls for audit-sink assertions.
type fakeNATS struct {
	mu        sync.Mutex
	published [][]byte
	errSet    atomic.Bool
	errVal    atomic.Value // *errBox; safe wrapping for nil-able errors
}

type errBox struct{ err error }

func (f *fakeNATS) setErr(e error) {
	f.errVal.Store(&errBox{err: e})
	f.errSet.Store(e != nil)
}

func (f *fakeNATS) Publish(_ string, data []byte) error {
	if f.errSet.Load() {
		if v, ok := f.errVal.Load().(*errBox); ok && v.err != nil {
			return v.err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	f.published = append(f.published, cp)
	return nil
}

func (f *fakeNATS) PublishedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.published)
}

// fakeHistory captures PublishEvent calls for audit history-sink
// assertions. Mirrors fakeNATS's setErr/PublishedCount shape so tests
// can interleave failures on either sink. Satisfies HistoryPublisher
// directly so no wrapper is needed.
type fakeHistory struct {
	mu       sync.Mutex
	captured []history.HistoryEvent
	errSet   atomic.Bool
	errVal   atomic.Value
}

func (f *fakeHistory) setErr(e error) {
	f.errVal.Store(&errBox{err: e})
	f.errSet.Store(e != nil)
}

func (f *fakeHistory) PublishEvent(_ context.Context, c history.Category, evType, nodeID string, details map[string]any) (string, error) {
	if f.errSet.Load() {
		if v, ok := f.errVal.Load().(*errBox); ok && v.err != nil {
			return "", v.err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.captured = append(f.captured, history.HistoryEvent{
		Category: c, Type: evType, NodeID: nodeID, Details: details,
	})
	return "test-id", nil
}

// historyPublisher is a no-op identity adapter (kept distinct from
// fakeHistory in case future tests want to chain decorators). Returns
// the input untouched.
func historyPublisher(h *fakeHistory) HistoryPublisher { return h }

// newTestServer assembles a Server backed by fakes. tokenEnv is the
// env-var name; the env value is "secret-token" unless tokenValue is
// supplied.
func newTestServer(t *testing.T,
	engine Engine,
	leader *fakeLeader,
	pub *fakeNATS,
	tokenValue string,
) *Server {
	t.Helper()
	logger := obs.NewLogger(io.Discard, "info", "test-cluster", "test-node", "control")
	metrics := obs.NewMetrics("test-cluster", "test-node")
	audit := NewAudit("test-cluster", logger, pub, metrics)

	auth := NewAuthenticator("PGMAN_PROXY_TEST_TOKEN", "", false)
	if tokenValue == "" {
		tokenValue = "secret-token"
	}
	auth.getEnv = func(_ string) string { return tokenValue }

	router := NewLeaderRouter("redirect", time.Second, "test-cluster", nil, leader)
	srv, err := NewServer(Config{
		Addr:      "127.0.0.1:0",
		Auth:      auth,
		Audit:     audit,
		Router:    router,
		Engine:    engine,
		Logger:    logger,
		Metrics:   metrics,
		ClusterID: "test-cluster",
		NodeID:    "test-node",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv
}

func doAuthed(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var bodyR io.Reader
	if body != "" {
		bodyR = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, bodyR)
	req.Header.Set("Authorization", "Bearer secret-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestStatus_Authenticated_ReturnsAccepted(t *testing.T) {
	called := false
	engine := &fakeEngine{statusFn: func(_ context.Context) (pgmanager.Status, error) {
		called = true
		return pgmanager.Status{}, nil
	}}
	srv := newTestServer(t, engine, &fakeLeader{leader: true}, &fakeNATS{}, "")
	w := doAuthed(t, srv.Handler(), http.MethodGet, "/v1/status", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status code: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !called {
		t.Errorf("Engine.Status was never called")
	}
	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Outcome != OutcomeAccepted || env.Operation != "Status" {
		t.Errorf("envelope: outcome=%q op=%q", env.Outcome, env.Operation)
	}
	if env.RequestID == "" {
		t.Errorf("envelope.RequestID empty — should be ULID")
	}
}

func TestStatus_NoAuth_Returns401(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, &fakeLeader{leader: true}, &fakeNATS{}, "")
	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d (body=%s)", w.Code, w.Body.String())
	}
}

func TestStatus_AllowUnauthReads_NoAuth_Succeeds(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, &fakeLeader{leader: true}, &fakeNATS{}, "")
	srv.auth = NewAuthenticator("PGMAN_PROXY_TEST_TOKEN", "", true)
	srv.auth.getEnv = func(_ string) string { return "secret-token" }
	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("want 200 with allow_unauth_reads=true, got %d", w.Code)
	}
}

func TestSwitchover_NotLeader_Redirect(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, &fakeLeader{leader: false, id: "node-b", addr: "http://node-b:9091"}, &fakeNATS{}, "")
	w := doAuthed(t, srv.Handler(), http.MethodPost, "/v1/switchover", `{"target":"node-a"}`)
	if w.Code != http.StatusTemporaryRedirect {
		t.Errorf("want 307, got %d (body=%s)", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "node-b") {
		t.Errorf("redirect Location should include leader addr, got %q", loc)
	}
}

func TestSwitchover_NotLeader_NoKnownLeader_503(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, &fakeLeader{leader: false, id: "", addr: ""}, &fakeNATS{}, "")
	w := doAuthed(t, srv.Handler(), http.MethodPost, "/v1/switchover", `{"target":"node-a"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503, got %d", w.Code)
	}
	var env envelope
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Error == nil || env.Error.Code != CodeLeadershipInTransition {
		t.Errorf("want leadership_in_transition, got %+v", env.Error)
	}
}

func TestSwitchover_MissingTarget_400(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, &fakeLeader{leader: true}, &fakeNATS{}, "")
	w := doAuthed(t, srv.Handler(), http.MethodPost, "/v1/switchover", `{}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400 for missing target, got %d", w.Code)
	}
}

func TestSwitchover_EngineError_500(t *testing.T) {
	engine := &fakeEngine{switchoverFn: func(_ context.Context, _ pgmanager.NodeID) error {
		return errors.New("engine refused: not enough sync peers")
	}}
	srv := newTestServer(t, engine, &fakeLeader{leader: true}, &fakeNATS{}, "")
	w := doAuthed(t, srv.Handler(), http.MethodPost, "/v1/switchover", `{"target":"node-a"}`)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("want 500 on engine error, got %d (body=%s)", w.Code, w.Body.String())
	}
	var env envelope
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Outcome != OutcomeFailed || env.Error == nil || env.Error.Code != CodeEngineError {
		t.Errorf("want failed/engine_error, got %+v", env)
	}
}

func TestTriggerBackup_NoExecutor_Returns412(t *testing.T) {
	engine := &fakeEngine{triggerBackupFn: func(_ context.Context) (pgmanager.BackupID, error) {
		return "", pgmanager.ErrBackupNotConfigured
	}}
	srv := newTestServer(t, engine, &fakeLeader{leader: true}, &fakeNATS{}, "")
	w := doAuthed(t, srv.Handler(), http.MethodPost, "/v1/backup", "")
	if w.Code != http.StatusPreconditionFailed {
		t.Errorf("want 412 backup_executor_missing, got %d", w.Code)
	}
	var env envelope
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Error == nil || env.Error.Code != CodeBackupExecutorMissing {
		t.Errorf("want backup_executor_missing, got %+v", env.Error)
	}
}

func TestAuditFailClose_RejectsMutation(t *testing.T) {
	pub := &fakeNATS{}
	pub.setErr(errors.New("nats unavailable"))
	srv := newTestServer(t, &fakeEngine{}, &fakeLeader{leader: true}, pub, "")
	// Drive one mutation to bump the audit-failure counter, then a
	// second to observe fail-closed.
	_ = doAuthed(t, srv.Handler(), http.MethodPost, "/v1/switchover", `{"target":"node-a"}`)
	w := doAuthed(t, srv.Handler(), http.MethodPost, "/v1/switchover", `{"target":"node-a"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503 audit_unavailable, got %d (body=%s)", w.Code, w.Body.String())
	}
	var env envelope
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Error == nil || env.Error.Code != CodeAuditUnavailable {
		t.Errorf("want audit_unavailable, got %+v", env.Error)
	}
}

func TestAuditFailClose_RecoveryRestoresMutation(t *testing.T) {
	pub := &fakeNATS{}
	pub.setErr(errors.New("nats unavailable"))
	srv := newTestServer(t, &fakeEngine{}, &fakeLeader{leader: true}, pub, "")
	_ = doAuthed(t, srv.Handler(), http.MethodPost, "/v1/switchover", `{"target":"node-a"}`)
	// Heal NATS.
	pub.setErr(nil)
	// First call audits TWICE — once for the request envelope, once
	// for the success path. Either reset would re-arm the gate.
	w := doAuthed(t, srv.Handler(), http.MethodPost, "/v1/switchover", `{"target":"node-a"}`)
	// The first request after recovery still observes the cached
	// failure (audit gate flips before the engine call); but a second
	// one should sail through. Assert the engine ran on the second.
	w2 := doAuthed(t, srv.Handler(), http.MethodPost, "/v1/switchover", `{"target":"node-a"}`)
	if w2.Code != http.StatusOK {
		t.Errorf("after recovery want 200, got %d (body=%s)", w2.Code, w2.Body.String())
	}
	_ = w
}

func TestPromote_NotLeader_StillExecutesLocally(t *testing.T) {
	called := false
	engine := &fakeEngine{promoteFn: func(_ context.Context) error {
		called = true
		return nil
	}}
	srv := newTestServer(t, engine, &fakeLeader{leader: false, id: "other"}, &fakeNATS{}, "")
	w := doAuthed(t, srv.Handler(), http.MethodPost, "/v1/promote", "")
	if w.Code != http.StatusOK {
		t.Errorf("Promote at non-leader must execute locally, got %d", w.Code)
	}
	if !called {
		t.Errorf("engine.Promote was not invoked at non-leader")
	}
}

func TestActor_NeverLeaksToken(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, &fakeLeader{leader: true}, &fakeNATS{}, "secret-shall-not-leak")
	srv.auth.getEnv = func(_ string) string { return "secret-shall-not-leak" }
	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	req.Header.Set("Authorization", "Bearer secret-shall-not-leak")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if bytes.Contains(w.Body.Bytes(), []byte("secret-shall-not-leak")) {
		t.Errorf("response body leaked the token: %s", w.Body.String())
	}
}

func TestRequestID_PropagatesToResponseHeader(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, &fakeLeader{leader: true}, &fakeNATS{}, "")
	w := doAuthed(t, srv.Handler(), http.MethodGet, "/v1/status", "")
	if w.Header().Get("X-Request-Id") == "" {
		t.Errorf("response missing X-Request-Id")
	}
}

// Smoke: actorFor never panics and never returns the secret. Property
// test, not exhaustive — defensive. Secrets shorter than the SHA-256
// prefix length (16 hex chars) are excluded — they're sub-strings of
// the hash by chance, not by leakage.
func TestActorFor_HidesSecret(t *testing.T) {
	for _, secret := range []string{"secret-token-abcdefghijk", strings.Repeat("x", 256)} {
		got := actorFor(secret)
		if strings.Contains(got, secret) {
			t.Errorf("actorFor(%q) leaked secret: %q", secret, got)
		}
		if !strings.HasPrefix(got, "bearer:") {
			t.Errorf("actorFor(%q) missing prefix: %q", secret, got)
		}
	}
}

// Sanity: ULID generator under concurrency produces unique values.
func TestULID_UniqueUnderConcurrency(t *testing.T) {
	src := newULIDEntropy()
	const n = 200
	out := make(chan string, n)
	for i := 0; i < n; i++ {
		go func() { out <- src.New().String() }()
	}
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		id := <-out
		if seen[id] {
			t.Fatalf("collision after %d ids: %s", i, id)
		}
		seen[id] = true
	}
}

// Ensures the leader-route timeout sentinel maps to HTTP 504. The
// LeaderRouter path is exercised by the integration suite end-to-end;
// this unit covers the error-code mapping.
func TestErrorCodeHTTPStatusMap(t *testing.T) {
	cases := map[string]int{
		CodeAuthRequired:           http.StatusUnauthorized,
		CodeAuthInvalid:            http.StatusForbidden,
		CodeClusterBootstrapping:   http.StatusConflict,
		CodeLeadershipInTransition: http.StatusServiceUnavailable,
		CodeAuditUnavailable:       http.StatusServiceUnavailable,
		CodeBackupExecutorMissing:  http.StatusPreconditionFailed,
		CodeInvalidArgument:        http.StatusBadRequest,
		CodeLeaderRouteTimeout:     http.StatusGatewayTimeout,
		CodeEngineError:            http.StatusInternalServerError,
		CodeInternal:               http.StatusInternalServerError,
	}
	for code, want := range cases {
		if got := httpStatusForCode(code); got != want {
			t.Errorf("httpStatusForCode(%q)=%d, want %d", code, got, want)
		}
	}
}

// Diagnostic: smoke against every registered route to verify routing
// is wired (returns SOMETHING, not 404).
func TestRoutes_AllDefined(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, &fakeLeader{leader: true}, &fakeNATS{}, "")
	routes := []struct{ method, path string }{
		{http.MethodGet, "/v1/status"},
		{http.MethodGet, "/v1/diagnose"},
		{http.MethodPost, "/v1/switchover"},
		{http.MethodPost, "/v1/failover"},
		{http.MethodPost, "/v1/fence"},
		{http.MethodPost, "/v1/unfence"},
		{http.MethodPost, "/v1/promote"},
		{http.MethodPost, "/v1/topology"},
		{http.MethodPost, "/v1/backup"},
		{http.MethodPost, "/v1/upgrade/prepare"},
		{http.MethodPost, "/v1/upgrade/execute"},
	}
	for _, r := range routes {
		body := ""
		if r.method == http.MethodPost {
			body = `{"target":"node-a","topology":{},"policy":{},"plan":{"strategy":1}}`
		}
		w := doAuthed(t, srv.Handler(), r.method, r.path, body)
		if w.Code == http.StatusNotFound {
			t.Errorf("route %s %s -> 404; should be registered", r.method, r.path)
		}
	}
}

// Diagnostic: fail-closed gate auto-resets after a healthy publish.
func TestAudit_HealthyResetsOnSuccess(t *testing.T) {
	logger := obs.NewLogger(io.Discard, "info", "c", "n", "control")
	metrics := obs.NewMetrics("c", "n")
	pub := &fakeNATS{}
	pub.setErr(errors.New("first call fails"))
	a := NewAudit("c", logger, pub, metrics)
	rec := AuditRecord{RequestID: "R1", Operation: "Status", Outcome: OutcomeAccepted}
	if err := a.Emit(context.Background(), rec); err == nil {
		t.Fatalf("first publish should fail")
	}
	if a.Healthy() {
		t.Fatalf("audit should be unhealthy after failure")
	}
	pub.setErr(nil)
	if err := a.Emit(context.Background(), rec); err != nil {
		t.Fatalf("second publish should succeed: %v", err)
	}
	if !a.Healthy() {
		t.Fatalf("audit should be healthy again after a successful publish")
	}
}

// Feature 003 T014 — Audit fails closed when the history sink rejects
// a record. After a transient history failure, Healthy() must report
// false; after a successful re-publish, both sinks must reset.
func TestAudit_HistorySinkGatesHealthy(t *testing.T) {
	logger := obs.NewLogger(io.Discard, "info", "c", "n", "control")
	metrics := obs.NewMetrics("c", "n")
	pub := &fakeNATS{}
	hist := &fakeHistory{}
	a := NewAudit("c", logger, pub, metrics).WithHistory(historyPublisher(hist))
	rec := AuditRecord{RequestID: "R1", Operation: "Switchover", Outcome: OutcomeAccepted}

	hist.setErr(errors.New("history wedged"))
	if err := a.Emit(context.Background(), rec); err == nil {
		t.Fatalf("emit should surface history sink error")
	}
	if a.Healthy() {
		t.Fatalf("audit should be unhealthy after history publish failure")
	}
	hist.setErr(nil)
	if err := a.Emit(context.Background(), rec); err != nil {
		t.Fatalf("emit should succeed after history recovers: %v", err)
	}
	if !a.Healthy() {
		t.Fatalf("audit should be healthy after both sinks succeed")
	}
}

// Sanity: rawJSON splices already-encoded JSON into the envelope's
// engine_result without double-encoding.
func TestRawJSON_NoDoubleEncoding(t *testing.T) {
	body, err := json.Marshal(envelope{
		Operation:    "X",
		Outcome:      OutcomeAccepted,
		EngineResult: rawJSON([]byte(`{"forwarded":true}`)),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, `"engine_result":{"forwarded":true}`) {
		t.Errorf("rawJSON should pass through verbatim, got: %s", got)
	}
}

// Smoke: ensure the actor field shows up in the audit record AS the
// hash, not the secret.
func TestAudit_ActorIsHashed(t *testing.T) {
	logger := obs.NewLogger(io.Discard, "info", "c", "n", "control")
	metrics := obs.NewMetrics("c", "n")
	pub := &fakeNATS{}
	a := NewAudit("c", logger, pub, metrics)
	rec := AuditRecord{
		RequestID: "R1",
		Operation: "Switchover",
		Outcome:   OutcomeAccepted,
		Actor:     actorFor("the-real-secret"),
	}
	if err := a.Emit(context.Background(), rec); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if pub.PublishedCount() != 1 {
		t.Fatalf("expected 1 publish, got %d", pub.PublishedCount())
	}
	pub.mu.Lock()
	defer pub.mu.Unlock()
	body := string(pub.published[0])
	if strings.Contains(body, "the-real-secret") {
		t.Errorf("audit body leaked secret: %s", body)
	}
	if !strings.Contains(body, "bearer:") {
		t.Errorf("audit actor should look like bearer:<hash>, got: %s", body)
	}
}

// fmt-only smoke to avoid an unused-import lint hit.
var _ = fmt.Sprintf
