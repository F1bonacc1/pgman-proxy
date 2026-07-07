// Feature 004 (specs/004-upgrade-tests) — contract tier for the
// upgrade surface: POST /v1/upgrade/prepare and /v1/upgrade/execute.
//
// Per Constitution VI, fakes here are confined to fault injection the
// real engine cannot produce deterministically through the HTTP
// surface (pre-swap failure, post-swap version mismatch, forward
// timeout); the real-server happy path lives in
// tests/integration/lcm_upgrade_test.go. Contract reference:
// specs/004-upgrade-tests/contracts/upgrade-endpoints.md.

package control

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	pgmanager "github.com/f1bonacc1/pg-manager"
	"github.com/f1bonacc1/pg-manager/upgrade"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// upgradeEnvelope mirrors the response envelope with engine_result
// kept raw so forwarding tests can compare the relayed leader
// envelope byte-for-byte.
type upgradeEnvelope struct {
	Operation    string          `json:"operation"`
	RequestID    string          `json:"request_id"`
	Outcome      string          `json:"outcome"`
	EngineResult json.RawMessage `json:"engine_result"`
	Error        *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// upgradeRecorder captures what the engine hooks receive so tests can
// assert pass-through fidelity and not-invoked guarantees. Guarded by
// a mutex for -race.
type upgradeRecorder struct {
	mu            sync.Mutex
	prepareCalls  int
	executeCalls  int
	lastPlan      pgmanager.UpgradePlan
	lastPreSwap   upgrade.PreSwap
	prepareResult error
	executeResult error
}

func (rec *upgradeRecorder) engine() *fakeEngine {
	return &fakeEngine{
		prepareUpgradeFn: func(_ context.Context, plan pgmanager.UpgradePlan) error {
			rec.mu.Lock()
			defer rec.mu.Unlock()
			rec.prepareCalls++
			rec.lastPlan = plan
			return rec.prepareResult
		},
		executeUpgradeFn: func(_ context.Context, plan pgmanager.UpgradePlan, pre upgrade.PreSwap) error {
			rec.mu.Lock()
			defer rec.mu.Unlock()
			rec.executeCalls++
			rec.lastPlan = plan
			rec.lastPreSwap = pre
			return rec.executeResult
		},
	}
}

func (rec *upgradeRecorder) snapshot() (int, int, pgmanager.UpgradePlan, upgrade.PreSwap) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.prepareCalls, rec.executeCalls, rec.lastPlan, rec.lastPreSwap
}

// postUpgrade POSTs body to path on a server built around engine and
// decodes the envelope.
func postUpgrade(t *testing.T, srv *Server, path, body string) (int, upgradeEnvelope) {
	t.Helper()
	w := doAuthed(t, srv.Handler(), http.MethodPost, path, body)
	var env upgradeEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope from %q: %v\nbody: %s", path, err, w.Body.String())
	}
	return w.Code, env
}

func upgradePlanJSON(t *testing.T, plan pgmanager.UpgradePlan) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"plan": plan})
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	return string(b)
}

// T009 — the decoded plan reaches the engine field-for-field intact
// and execute supplies a non-nil pre-swap (v1 no-op). FR-013 boundary;
// research.md R5.
func TestUpgradeExecute_PlanPassthrough(t *testing.T) {
	rec := &upgradeRecorder{}
	srv := newTestServer(t, rec.engine(), &fakeLeader{leader: true}, &fakeNATS{}, "")

	want := pgmanager.UpgradePlan{
		Strategy:      pgmanager.UpgradeMinor,
		TargetMajor:   17,
		TargetMinor:   6,
		NodeOrder:     []pgmanager.NodeID{"node-b", "node-c", "node-a"},
		PerNodeBudget: 90 * time.Second,
	}

	code, env := postUpgrade(t, srv, "/v1/upgrade/execute", upgradePlanJSON(t, want))
	if code != http.StatusOK || env.Outcome != OutcomeAccepted {
		t.Fatalf("execute: status=%d outcome=%q, want 200/accepted", code, env.Outcome)
	}
	if env.Operation != "ExecuteUpgrade" {
		t.Errorf("operation=%q, want ExecuteUpgrade", env.Operation)
	}
	_, execCalls, got, preSwap := rec.snapshot()
	if execCalls != 1 {
		t.Fatalf("ExecuteUpgrade called %d times, want 1", execCalls)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("plan mutated in transit:\n got:  %+v\n want: %+v", got, want)
	}
	if preSwap == nil {
		t.Error("engine received a nil PreSwap; handler must supply the v1 no-op")
	}

	// Prepare: same pass-through guarantee (plan only).
	code, env = postUpgrade(t, srv, "/v1/upgrade/prepare", upgradePlanJSON(t, want))
	if code != http.StatusOK || env.Outcome != OutcomeAccepted {
		t.Fatalf("prepare: status=%d outcome=%q, want 200/accepted", code, env.Outcome)
	}
	if env.Operation != "PrepareUpgrade" {
		t.Errorf("operation=%q, want PrepareUpgrade", env.Operation)
	}
	prepCalls, _, got, _ := rec.snapshot()
	if prepCalls != 1 {
		t.Fatalf("PrepareUpgrade called %d times, want 1", prepCalls)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("prepare plan mutated in transit:\n got:  %+v\n want: %+v", got, want)
	}
}

// T010 — a pre-swap failure inside the engine surfaces as
// failed/engine_error with the message preserved verbatim (FR-003 /
// US2-AS3). The abort-before-restart behavior itself is owned and
// tested upstream in pg-manager/upgrade; the proxy's contract is
// faithful surfacing.
func TestUpgradeExecute_SwapFailureSurfaced(t *testing.T) {
	const swapErr = "upgrade: pre-swap: flip symlink /usr/lib/postgresql: permission denied"
	rec := &upgradeRecorder{executeResult: errFromString(swapErr)}
	srv := newTestServer(t, rec.engine(), &fakeLeader{leader: true}, &fakeNATS{}, "")

	plan := pgmanager.UpgradePlan{Strategy: pgmanager.UpgradeMinor, TargetMajor: 17, TargetMinor: 6}
	code, env := postUpgrade(t, srv, "/v1/upgrade/execute", upgradePlanJSON(t, plan))

	if code != http.StatusInternalServerError {
		t.Errorf("status=%d, want 500", code)
	}
	if env.Outcome != OutcomeFailed {
		t.Errorf("outcome=%q, want failed", env.Outcome)
	}
	if env.Error == nil || env.Error.Code != CodeEngineError {
		t.Fatalf("error=%+v, want code %q", env.Error, CodeEngineError)
	}
	if env.Error.Message != swapErr {
		t.Errorf("message not preserved verbatim:\n got:  %q\n want: %q", env.Error.Message, swapErr)
	}
}

// T011 — a post-swap version-mismatch failure surfaces the same way
// (FR-004 / US2-AS4).
func TestUpgradeExecute_VersionMismatchSurfaced(t *testing.T) {
	const mismatchErr = "upgrade: post-swap version probe: running major 16, plan targets 17"
	rec := &upgradeRecorder{executeResult: errFromString(mismatchErr)}
	srv := newTestServer(t, rec.engine(), &fakeLeader{leader: true}, &fakeNATS{}, "")

	plan := pgmanager.UpgradePlan{Strategy: pgmanager.UpgradeMinor, TargetMajor: 17, TargetMinor: 6}
	code, env := postUpgrade(t, srv, "/v1/upgrade/execute", upgradePlanJSON(t, plan))

	if code != http.StatusInternalServerError || env.Outcome != OutcomeFailed {
		t.Errorf("status=%d outcome=%q, want 500/failed", code, env.Outcome)
	}
	if env.Error == nil || env.Error.Code != CodeEngineError {
		t.Fatalf("error=%+v, want code %q", env.Error, CodeEngineError)
	}
	if !strings.Contains(env.Error.Message, "version probe") {
		t.Errorf("message %q lost the engine's mismatch reason", env.Error.Message)
	}
}

// T012 — malformed and empty bodies are rejected as invalid_argument
// before the engine is ever consulted (FR-009 / US2-AS5), on both
// endpoints.
func TestUpgrade_MalformedBodyRejected(t *testing.T) {
	for _, path := range []string{"/v1/upgrade/prepare", "/v1/upgrade/execute"} {
		for name, body := range map[string]string{
			"garbage": `{"plan": {"Strategy": "not-an-int"`,
			"empty":   "",
		} {
			t.Run(path+"/"+name, func(t *testing.T) {
				rec := &upgradeRecorder{}
				srv := newTestServer(t, rec.engine(), &fakeLeader{leader: true}, &fakeNATS{}, "")

				code, env := postUpgrade(t, srv, path, body)
				if code != http.StatusBadRequest {
					t.Errorf("status=%d, want 400", code)
				}
				if env.Outcome != OutcomeRejected {
					t.Errorf("outcome=%q, want rejected", env.Outcome)
				}
				if env.Error == nil || env.Error.Code != CodeInvalidArgument {
					t.Fatalf("error=%+v, want code %q", env.Error, CodeInvalidArgument)
				}
				if env.Error.Message == "" {
					t.Error("rejection carries no descriptive message")
				}
				prep, exec, _, _ := rec.snapshot()
				if prep != 0 || exec != 0 {
					t.Errorf("engine reached despite invalid body: prepare=%d execute=%d", prep, exec)
				}
			})
		}
	}
}

// startUpgradeTestNATS boots a real in-process NATS server (same
// pattern as internal/fanout tests) so forwarding runs over an actual
// request/reply round trip rather than a mocked router.
func startUpgradeTestNATS(t *testing.T) *nats.Conn {
	t.Helper()
	dir := t.TempDir()
	opts := &natsserver.Options{
		Host:     "127.0.0.1",
		Port:     -1,
		NoLog:    true,
		NoSigs:   true,
		StoreDir: dir,
	}
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("new nats server: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatalf("nats server failed to start")
	}
	t.Cleanup(func() {
		ns.Shutdown()
		ns.WaitForShutdown()
		_ = os.RemoveAll(dir)
	})
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// forwardModeServer returns a non-leader server in `forward` mode
// wired to nc, so upgrade requests are published on the LCM request
// subject and the reply is relayed.
func forwardModeServer(t *testing.T, nc *nats.Conn, timeout time.Duration) *Server {
	t.Helper()
	srv := newTestServer(t, &fakeEngine{}, &fakeLeader{leader: true}, &fakeNATS{}, "")
	srv.router = NewLeaderRouter("forward", timeout, "test-cluster",
		nc, &fakeLeader{leader: false, id: "node-b", addr: "http://node-b:9091"})
	return srv
}

// T013 — forwarding (FR-010 / US2-AS6, edge case: unreachable peer).
// A non-leader in forward mode publishes the raw body to the leader's
// LCM request subject and splices the leader's envelope — success or
// failure — verbatim into engine_result; a dead leader maps to
// leader_route_timeout / 504.
func TestUpgrade_ForwardedToLeader(t *testing.T) {
	plan := pgmanager.UpgradePlan{Strategy: pgmanager.UpgradeMinor, TargetMajor: 17, TargetMinor: 6}
	body := upgradePlanJSON(t, plan)

	t.Run("leader success envelope relayed verbatim", func(t *testing.T) {
		nc := startUpgradeTestNATS(t)
		leaderReply := `{"operation":"ExecuteUpgrade","request_id":"leader-req-1","outcome":"accepted"}`
		sub, err := nc.Subscribe("pgman_proxy.test-cluster.lcm.request.ExecuteUpgrade",
			func(m *nats.Msg) { _ = m.Respond([]byte(leaderReply)) })
		if err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		defer func() { _ = sub.Unsubscribe() }()

		srv := forwardModeServer(t, nc, 2*time.Second)
		code, env := postUpgrade(t, srv, "/v1/upgrade/execute", body)
		if code != http.StatusOK || env.Outcome != OutcomeAccepted {
			t.Fatalf("status=%d outcome=%q, want 200/accepted", code, env.Outcome)
		}
		if string(env.EngineResult) != leaderReply {
			t.Errorf("leader envelope not relayed verbatim:\n got:  %s\n want: %s",
				env.EngineResult, leaderReply)
		}
	})

	t.Run("leader failure envelope relayed verbatim", func(t *testing.T) {
		nc := startUpgradeTestNATS(t)
		leaderReply := `{"operation":"ExecuteUpgrade","request_id":"leader-req-2",` +
			`"outcome":"failed","error":{"code":"engine_error",` +
			`"message":"upgrade: major strategies wired but gated on v0.7.0"}}`
		sub, err := nc.Subscribe("pgman_proxy.test-cluster.lcm.request.ExecuteUpgrade",
			func(m *nats.Msg) { _ = m.Respond([]byte(leaderReply)) })
		if err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		defer func() { _ = sub.Unsubscribe() }()

		srv := forwardModeServer(t, nc, 2*time.Second)
		code, env := postUpgrade(t, srv, "/v1/upgrade/execute", body)
		if code != http.StatusOK {
			t.Fatalf("status=%d, want 200 (forward itself succeeded)", code)
		}
		if string(env.EngineResult) != leaderReply {
			t.Errorf("leader failure envelope not relayed verbatim:\n got:  %s\n want: %s",
				env.EngineResult, leaderReply)
		}
	})

	t.Run("silent leader maps to leader_route_timeout", func(t *testing.T) {
		nc := startUpgradeTestNATS(t)
		// A responder that never replies: the forward waits out the
		// router timeout, exercising the deadline → 504 mapping.
		sub, err := nc.Subscribe("pgman_proxy.test-cluster.lcm.request.ExecuteUpgrade",
			func(_ *nats.Msg) {})
		if err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		defer func() { _ = sub.Unsubscribe() }()

		srv := forwardModeServer(t, nc, 250*time.Millisecond)
		code, env := postUpgrade(t, srv, "/v1/upgrade/execute", body)
		if code != http.StatusGatewayTimeout {
			t.Errorf("status=%d, want 504", code)
		}
		if env.Outcome != OutcomeFailed {
			t.Errorf("outcome=%q, want failed", env.Outcome)
		}
		if env.Error == nil || env.Error.Code != CodeLeaderRouteTimeout {
			t.Fatalf("error=%+v, want code %q", env.Error, CodeLeaderRouteTimeout)
		}
	})

	t.Run("dead leader (no responders) surfaces as engine_error", func(t *testing.T) {
		// Nobody subscribed on the subject: NATS fails fast with
		// ErrNoResponders rather than timing out. Pin the actual
		// mapping (failed/engine_error) so the "unreachable peer"
		// edge case is a documented routing error, not a hang.
		nc := startUpgradeTestNATS(t)
		srv := forwardModeServer(t, nc, 2*time.Second)
		code, env := postUpgrade(t, srv, "/v1/upgrade/execute", body)
		if code != http.StatusInternalServerError {
			t.Errorf("status=%d, want 500", code)
		}
		if env.Outcome != OutcomeFailed {
			t.Errorf("outcome=%q, want failed", env.Outcome)
		}
		if env.Error == nil || env.Error.Code != CodeEngineError {
			t.Fatalf("error=%+v, want code %q", env.Error, CodeEngineError)
		}
	})
}

// errFromString keeps injected engine errors one-line at call sites.
func errFromString(s string) error { return &stringError{s} }

type stringError struct{ s string }

func (e *stringError) Error() string { return e.s }
