// Unit tests for the leader-route responder (FR-026 / FR-034).
//
// Uses an in-memory NATS server (mirrors internal/fanout/fanout_test.go)
// plus the package-local fakeEngine / fakeLeader / fakeNATS helpers so
// we can drive every documented envelope shape without booting
// pg-manager.

package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	pgmanager "github.com/f1bonacc1/pg-manager"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/f1bonacc1/pgman-proxy/internal/obs"
)

// responderFixture bundles every dependency a responder needs.
type responderFixture struct {
	nc        *nats.Conn
	engine    *fakeEngine
	leader    *fakeLeader
	pub       *fakeNATS // captures audit publishes
	audit     *Audit
	metrics   *obs.MetricSet
	logger    *obs.Logger
	router    *LeaderRouter
	responder *LeaderRouteResponder
	clusterID string
}

const responderTestCluster = "responder-test"

// newResponderFixture wires a complete responder against an in-memory
// NATS server. timeout governs both the engine ceiling and the
// publisher Forward() call in the tests below.
func newResponderFixture(t *testing.T, isLeader bool, timeout time.Duration) *responderFixture {
	t.Helper()
	nc := startResponderNATS(t)
	engine := &fakeEngine{}
	leader := &fakeLeader{leader: isLeader, id: "test-leader"}
	pub := &fakeNATS{}
	logger := obs.NewLogger(io.Discard, "info", responderTestCluster, "test-node", "control")
	metrics := obs.NewMetrics(responderTestCluster, "test-node")
	audit := NewAudit(responderTestCluster, logger, pub, metrics)
	router := NewLeaderRouter("forward", timeout, responderTestCluster, nc, leader)
	resp := NewLeaderRouteResponder(nc, responderTestCluster, "test-node", engine,
		router, audit, metrics, logger, timeout)
	if err := resp.Serve(); err != nil {
		t.Fatalf("responder.Serve: %v", err)
	}
	t.Cleanup(func() { _ = resp.Close() })
	return &responderFixture{
		nc: nc, engine: engine, leader: leader, pub: pub,
		audit: audit, metrics: metrics, logger: logger,
		router: router, responder: resp, clusterID: responderTestCluster,
	}
}

func startResponderNATS(t *testing.T) *nats.Conn {
	t.Helper()
	dir := t.TempDir()
	opts := &natsserver.Options{
		Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true, StoreDir: dir,
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("new nats server: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatalf("nats server failed to start")
	}
	t.Cleanup(func() {
		srv.Shutdown()
		srv.WaitForShutdown()
		_ = os.RemoveAll(dir)
	})
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// decodeReply pulls the envelope out of a Forward() reply.
func decodeReply(t *testing.T, raw []byte) envelope {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode reply: %v (raw: %s)", err, raw)
	}
	return env
}

func TestResponder_Failover_Happy(t *testing.T) {
	fx := newResponderFixture(t, true, 2*time.Second)
	var called bool
	fx.engine.failoverFn = func(_ context.Context) error { called = true; return nil }

	reply, err := fx.router.Forward(context.Background(), "Failover",
		"req-1", "operator-a", "", nil)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	env := decodeReply(t, reply)
	if env.Outcome != OutcomeAccepted {
		t.Errorf("outcome=%q want %q (env=%+v)", env.Outcome, OutcomeAccepted, env)
	}
	if env.RequestID != "req-1" {
		t.Errorf("request_id=%q want %q", env.RequestID, "req-1")
	}
	if !called {
		t.Error("engine.Failover was not called")
	}
}

func TestResponder_Fence_HappyWithTarget(t *testing.T) {
	fx := newResponderFixture(t, true, 2*time.Second)
	var seen pgmanager.NodeID
	fx.engine.fenceFn = func(_ context.Context, n pgmanager.NodeID) error {
		seen = n
		return nil
	}

	body, _ := json.Marshal(fenceReq{Target: "node-x"})
	reply, err := fx.router.Forward(context.Background(), "Fence",
		"req-fence", "op", "", body)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	env := decodeReply(t, reply)
	if env.Outcome != OutcomeAccepted {
		t.Fatalf("outcome=%q want accepted (env=%+v)", env.Outcome, env)
	}
	if string(seen) != "node-x" {
		t.Errorf("engine.Fence target=%q want %q", seen, "node-x")
	}
}

func TestResponder_EngineError_MapsToEngineError(t *testing.T) {
	fx := newResponderFixture(t, true, 2*time.Second)
	fx.engine.failoverFn = func(_ context.Context) error {
		return errors.New("primary down")
	}

	reply, err := fx.router.Forward(context.Background(), "Failover",
		"req-err", "op", "", nil)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	env := decodeReply(t, reply)
	if env.Outcome != OutcomeFailed {
		t.Fatalf("outcome=%q want failed (env=%+v)", env.Outcome, env)
	}
	if env.Error == nil || env.Error.Code != CodeEngineError {
		t.Errorf("error code=%v want %q", env.Error, CodeEngineError)
	}
}

func TestResponder_TriggerBackup_ReturnsBackupID(t *testing.T) {
	fx := newResponderFixture(t, true, 2*time.Second)
	fx.engine.triggerBackupFn = func(_ context.Context) (pgmanager.BackupID, error) {
		return "backup-123", nil
	}

	reply, err := fx.router.Forward(context.Background(), "TriggerBackup",
		"req-bk", "op", "", nil)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	env := decodeReply(t, reply)
	if env.Outcome != OutcomeAccepted {
		t.Fatalf("outcome=%q want accepted (env=%+v)", env.Outcome, env)
	}
	// engine_result is decoded into `any`; re-marshal to inspect.
	raw, _ := json.Marshal(env.EngineResult)
	var got triggerBackupResp
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode engine_result: %v (raw: %s)", err, raw)
	}
	if got.BackupID != "backup-123" {
		t.Errorf("backup_id=%q want backup-123", got.BackupID)
	}
}

func TestResponder_TriggerBackup_NotConfigured_MapsToExecutorMissing(t *testing.T) {
	fx := newResponderFixture(t, true, 2*time.Second)
	fx.engine.triggerBackupFn = func(_ context.Context) (pgmanager.BackupID, error) {
		return "", pgmanager.ErrBackupNotConfigured
	}

	reply, err := fx.router.Forward(context.Background(), "TriggerBackup",
		"req-bk2", "op", "", nil)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	env := decodeReply(t, reply)
	if env.Outcome != OutcomeFailed {
		t.Fatalf("outcome=%q want failed (env=%+v)", env.Outcome, env)
	}
	if env.Error == nil || env.Error.Code != CodeBackupExecutorMissing {
		t.Errorf("error code=%v want %q", env.Error, CodeBackupExecutorMissing)
	}
}

func TestResponder_NonLeader_TimesOut(t *testing.T) {
	// Short publisher timeout — we expect the responder to stay silent
	// and the Forward call to hit ErrLeaderRouteTimeout.
	fx := newResponderFixture(t, false, 250*time.Millisecond)

	_, err := fx.router.Forward(context.Background(), "Failover",
		"req-nleader", "op", "", nil)
	if !errors.Is(err, ErrLeaderRouteTimeout) {
		t.Fatalf("want ErrLeaderRouteTimeout, got %v", err)
	}
}

func TestResponder_MalformedBody_MapsToInvalidArgument(t *testing.T) {
	fx := newResponderFixture(t, true, 2*time.Second)

	reply, err := fx.router.Forward(context.Background(), "Fence",
		"req-mal", "op", "", []byte("not-json"))
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	env := decodeReply(t, reply)
	if env.Outcome != OutcomeRejected {
		t.Fatalf("outcome=%q want rejected (env=%+v)", env.Outcome, env)
	}
	if env.Error == nil || env.Error.Code != CodeInvalidArgument {
		t.Errorf("error code=%v want %q", env.Error, CodeInvalidArgument)
	}
}

func TestResponder_AuditUnhealthy_RefusesWithAuditUnavailable(t *testing.T) {
	fx := newResponderFixture(t, true, 2*time.Second)
	// Trip the audit fail-closed gate by making the NATS audit sink
	// reject the next publish — Audit.Healthy() flips to false after a
	// failed Emit. Drive a no-op emit to set the counter, then attempt
	// a forward.
	fx.pub.setErr(errors.New("audit down"))
	_ = fx.audit.Emit(context.Background(), AuditRecord{Operation: "warm-up"})
	if fx.audit.Healthy() {
		t.Fatal("audit should be unhealthy after a publish error")
	}

	reply, err := fx.router.Forward(context.Background(), "Failover",
		"req-ah", "op", "", nil)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	env := decodeReply(t, reply)
	if env.Outcome != OutcomeRejected {
		t.Fatalf("outcome=%q want rejected (env=%+v)", env.Outcome, env)
	}
	if env.Error == nil || env.Error.Code != CodeAuditUnavailable {
		t.Errorf("error code=%v want %q", env.Error, CodeAuditUnavailable)
	}
}

func TestResponder_HeadersFlowThroughToAudit(t *testing.T) {
	fx := newResponderFixture(t, true, 2*time.Second)

	const (
		wantReqID = "req-correlated"
		wantActor = "operator-alice"
		// W3C traceparent: version-traceID-spanID-flags.
		wantTP = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	)
	_, err := fx.router.Forward(context.Background(), "Failover",
		wantReqID, wantActor, wantTP, nil)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	// fakeNATS captures the audit JSON published on the audit subject.
	// The single Emit happens synchronously inside the responder before
	// it Responds, so by the time Forward returns the publish is done.
	fx.pub.mu.Lock()
	got := append([][]byte(nil), fx.pub.published...)
	fx.pub.mu.Unlock()
	if len(got) == 0 {
		t.Fatal("no audit records published")
	}
	var rec AuditRecord
	if err := json.Unmarshal(got[len(got)-1], &rec); err != nil {
		t.Fatalf("decode audit: %v", err)
	}
	if rec.RequestID != wantReqID {
		t.Errorf("audit request_id=%q want %q", rec.RequestID, wantReqID)
	}
	if rec.Actor != wantActor {
		t.Errorf("audit actor=%q want %q", rec.Actor, wantActor)
	}
	if rec.TraceID != "0af7651916cd43dd8448eb211c80319c" {
		t.Errorf("audit trace_id=%q want %q", rec.TraceID, "0af7651916cd43dd8448eb211c80319c")
	}
	if rec.SpanID != "b7ad6b7169203331" {
		t.Errorf("audit span_id=%q want %q", rec.SpanID, "b7ad6b7169203331")
	}
}

func TestResponder_AbsentHeaders_DefaultsApplied(t *testing.T) {
	fx := newResponderFixture(t, true, 2*time.Second)

	reply, err := fx.router.Forward(context.Background(), "Failover",
		"", "", "", nil)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	env := decodeReply(t, reply)
	if env.RequestID == "" {
		t.Error("reply request_id is empty — responder should mint a fallback ULID")
	}
	fx.pub.mu.Lock()
	got := append([][]byte(nil), fx.pub.published...)
	fx.pub.mu.Unlock()
	if len(got) == 0 {
		t.Fatal("no audit records published")
	}
	var rec AuditRecord
	if err := json.Unmarshal(got[len(got)-1], &rec); err != nil {
		t.Fatalf("decode audit: %v", err)
	}
	if rec.Actor != responderActorFallback {
		t.Errorf("audit actor=%q want %q", rec.Actor, responderActorFallback)
	}
}

func TestResponder_ConcurrentDispatches_DontSerialize(t *testing.T) {
	fx := newResponderFixture(t, true, 5*time.Second)
	var (
		mu        sync.Mutex
		callTimes []time.Time
	)
	// Each engine call holds for 200ms so serialization would push two
	// concurrent calls to ~400ms apart.
	fx.engine.failoverFn = func(_ context.Context) error {
		mu.Lock()
		callTimes = append(callTimes, time.Now())
		mu.Unlock()
		time.Sleep(200 * time.Millisecond)
		return nil
	}

	const n = 2
	var wg sync.WaitGroup
	wg.Add(n)
	start := time.Now()
	for range n {
		go func() {
			defer wg.Done()
			_, _ = fx.router.Forward(context.Background(), "Failover",
				"", "", "", nil)
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	if elapsed > 350*time.Millisecond {
		t.Errorf("two concurrent forwards took %v — looks serialized (want < 350ms)", elapsed)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(callTimes) != n {
		t.Fatalf("engine called %d times, want %d", len(callTimes), n)
	}
	gap := callTimes[1].Sub(callTimes[0])
	if gap < 0 {
		gap = -gap
	}
	if gap > 100*time.Millisecond {
		t.Errorf("engine entries %v apart — dispatches not parallel", gap)
	}
}

func TestResponder_UnknownOp_IgnoredSilently(t *testing.T) {
	fx := newResponderFixture(t, true, 250*time.Millisecond)

	// Publish on a syntactically-valid subject with an unknown op
	// suffix. Responder should not respond; publisher times out.
	_, err := fx.router.Forward(context.Background(), "NotARealOp",
		"req-unk", "op", "", nil)
	if !errors.Is(err, ErrLeaderRouteTimeout) {
		t.Fatalf("want ErrLeaderRouteTimeout, got %v", err)
	}
}

func TestResponder_CloseIsIdempotent(t *testing.T) {
	fx := newResponderFixture(t, true, 250*time.Millisecond)
	if err := fx.responder.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := fx.responder.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
