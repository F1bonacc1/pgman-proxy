package history

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// startEmbeddedNATS spins up an in-process NATS server with JetStream
// enabled. Storage is on a fresh temp directory; teardown is deferred
// via t.Cleanup. Returns a connected nats.Conn.
//
// Mirrors the test harness used in internal/embedded.
func startEmbeddedNATS(t *testing.T) *nats.Conn {
	t.Helper()
	dir := t.TempDir()
	opts := &natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1, // ask the OS for a free port
		JetStream: true,
		StoreDir:  dir,
		NoLog:     true,
		NoSigs:    true,
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("new server: %v", err)
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

// TestStreamLifecycle_EnsureAndReconfigure asserts EnsureHistoryStream
// is idempotent and tolerant of operator-tuned retention values.
func TestStreamLifecycle_EnsureAndReconfigure(t *testing.T) {
	nc := startEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	opts := DefaultStreamOptions(1)
	opts.Storage = jetstream.MemoryStorage // for fast unit-test teardown
	opts.MaxAge = 5 * time.Minute
	opts.MaxBytes = 1 << 20 // 1 MiB

	stream, err := EnsureHistoryStream(ctx, js, "unit-test", opts)
	if err != nil {
		t.Fatalf("Ensure 1: %v", err)
	}
	if stream.CachedInfo().Config.MaxAge != opts.MaxAge {
		t.Errorf("MaxAge = %s, want %s", stream.CachedInfo().Config.MaxAge, opts.MaxAge)
	}

	// Second call with same opts must be a no-op (no error).
	if _, err := EnsureHistoryStream(ctx, js, "unit-test", opts); err != nil {
		t.Fatalf("Ensure 2 (idempotent): %v", err)
	}

	// Tweak retention; Ensure should reconcile in place.
	opts.MaxAge = 10 * time.Minute
	stream2, err := EnsureHistoryStream(ctx, js, "unit-test", opts)
	if err != nil {
		t.Fatalf("Ensure 3 (update): %v", err)
	}
	if got := stream2.CachedInfo().Config.MaxAge; got != opts.MaxAge {
		t.Errorf("After update, MaxAge = %s, want %s", got, opts.MaxAge)
	}
}

// TestPublishAndQuery_RoundTrip asserts a full publish→query loop
// recovers every emitted record in chronological order with correct
// type / category / node filters.
func TestPublishAndQuery_RoundTrip(t *testing.T) {
	nc := startEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	opts := DefaultStreamOptions(1)
	opts.Storage = jetstream.MemoryStorage
	if _, err := EnsureHistoryStream(ctx, js, "rt-test", opts); err != nil {
		t.Fatal(err)
	}

	pub := NewPublisher(js, "rt-test", "node-a")
	pub.SetPublishTimeout(2 * time.Second)

	type rec struct {
		category Category
		typ      string
		node     string
		details  map[string]any
	}
	records := []rec{
		{CategoryEvent, "state_transition", "node-a", map[string]any{"from": "running", "to": "demoting"}},
		{CategoryEvent, "leader_change", "node-b", map[string]any{"new_leader": "node-b"}},
		{CategoryAudit, "lcm_audit", "node-a", map[string]any{"operation": "Failover", "outcome": "accepted"}},
		{CategoryEvent, "route_up", "node-c", map[string]any{"peer": "node-a"}},
	}
	ids := make([]string, 0, len(records))
	for _, r := range records {
		id, err := pub.PublishEvent(ctx, r.category, r.typ, r.node, r.details)
		if err != nil {
			t.Fatalf("publish %s/%s: %v", r.category, r.typ, err)
		}
		ids = append(ids, id)
	}
	if pub.PublishFailures() != 0 {
		t.Errorf("PublishFailures = %d, want 0", pub.PublishFailures())
	}

	// Tiny pause to make sure JetStream has the messages indexed.
	time.Sleep(100 * time.Millisecond)

	// 1. Query everything since 1m ago.
	got, err := Run(ctx, js, "rt-test", Query{Since: time.Minute, Limit: 100})
	if err != nil {
		t.Fatalf("Run (all): %v", err)
	}
	if len(got.Events) != len(records) {
		t.Fatalf("Run(all): got %d events, want %d\n%+v", len(got.Events), len(records), got.Events)
	}
	for i, ev := range got.Events {
		if ev.ID != ids[i] {
			t.Errorf("ev[%d].ID = %s, want %s", i, ev.ID, ids[i])
		}
		if ev.Type != records[i].typ {
			t.Errorf("ev[%d].Type = %s, want %s", i, ev.Type, records[i].typ)
		}
	}

	// 2. Category filter — audit only.
	auditOnly, err := Run(ctx, js, "rt-test", Query{Since: time.Minute, Category: CategoryAudit, Limit: 100})
	if err != nil {
		t.Fatalf("Run (audit): %v", err)
	}
	if len(auditOnly.Events) != 1 || auditOnly.Events[0].Type != "lcm_audit" {
		t.Errorf("audit filter returned %+v", auditOnly.Events)
	}

	// 3. Node filter — node-b only.
	nodeOnly, err := Run(ctx, js, "rt-test", Query{Since: time.Minute, Nodes: []string{"node-b"}, Limit: 100})
	if err != nil {
		t.Fatalf("Run (node-b): %v", err)
	}
	if len(nodeOnly.Events) != 1 || nodeOnly.Events[0].NodeID != "node-b" {
		t.Errorf("node filter returned %+v", nodeOnly.Events)
	}

	// 4. Type filter.
	typed, err := Run(ctx, js, "rt-test", Query{Since: time.Minute, Types: []string{"route_up", "state_transition"}, Limit: 100})
	if err != nil {
		t.Fatalf("Run (types): %v", err)
	}
	if len(typed.Events) != 2 {
		t.Errorf("type filter returned %d events, want 2", len(typed.Events))
	}
	for _, ev := range typed.Events {
		if ev.Type != "route_up" && ev.Type != "state_transition" {
			t.Errorf("type filter returned wrong type %s", ev.Type)
		}
	}
}

// TestPublishLatency_UnderSpec asserts a single publish completes well
// within the contracts/history-stream.md 250ms budget on an idle
// in-memory NATS server.
func TestPublishLatency_UnderSpec(t *testing.T) {
	nc := startEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	opts := DefaultStreamOptions(1)
	opts.Storage = jetstream.MemoryStorage
	if _, err := EnsureHistoryStream(ctx, js, "lat-test", opts); err != nil {
		t.Fatal(err)
	}

	pub := NewPublisher(js, "lat-test", "node-a")
	start := time.Now()
	if _, err := pub.PublishEvent(ctx, CategoryEvent, "state_transition", "node-a", nil); err != nil {
		t.Fatal(err)
	}
	d := time.Since(start)
	if d > 250*time.Millisecond {
		t.Errorf("publish took %s, want < 250ms", d)
	}
}

// TestStreamOptions_Validate covers the documented refusals.
func TestStreamOptions_Validate(t *testing.T) {
	good := DefaultStreamOptions(3)
	if err := good.Validate(); err != nil {
		t.Errorf("defaults invalid: %v", err)
	}

	for _, bad := range []StreamOptions{
		{Replicas: 0, MaxAge: time.Hour},
		{Replicas: 6, MaxAge: time.Hour},
		{Replicas: 1, MaxAge: 0, MaxBytes: 0}, // unbounded
		{Replicas: 1, MaxAge: -1},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("Validate accepted bad %+v", bad)
		}
	}
}

// TestSubjectShape asserts the documented subject form is stable.
func TestSubjectShape(t *testing.T) {
	if got := EventSubject("prod-east", CategoryEvent, "state_transition"); got != "pgman_proxy.prod-east.history.event.state_transition" {
		t.Errorf("subject = %q", got)
	}
	if got := SubjectFilterCategory("Prod East!", CategoryAudit); !strings.Contains(got, ".history.audit.>") {
		t.Errorf("category filter = %q", got)
	}
	if got := StreamName("prod-east"); got != "PGMAN_PROXY_HISTORY_PROD-EAST" {
		t.Errorf("stream name = %q", got)
	}
}
