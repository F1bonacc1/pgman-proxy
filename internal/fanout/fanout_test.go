package fanout

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// startEmbeddedNATS spins up a no-JetStream NATS server for fan-out
// tests. Fan-out uses bare pub/sub + req/reply; JetStream is not
// required.
func startEmbeddedNATS(t *testing.T) *nats.Conn {
	t.Helper()
	dir := t.TempDir()
	opts := &natsserver.Options{
		Host:     "127.0.0.1",
		Port:     -1,
		NoLog:    true,
		NoSigs:   true,
		StoreDir: dir,
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

// TestBroadcast_ThreeRespondersAllOK exercises the happy path: three
// peers register a status handler, originator broadcasts, all three
// reply, aggregated payload contains every node id.
func TestBroadcast_ThreeRespondersAllOK(t *testing.T) {
	nc := startEmbeddedNATS(t)
	const cluster = "fan-test"

	for _, peer := range []string{"node-a", "node-b", "node-c"} {
		p := peer
		s := NewServer(nc, cluster, p)
		if err := s.Register(SliceStatus, func(_ context.Context, _ map[string]any, _ string) (any, error) {
			return map[string]string{"node_id": p, "role": "running"}, nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.Serve(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
	}

	c := NewClient(nc, cluster)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	replies, err := c.Broadcast(ctx, SliceStatus, nil, CallOptions{
		PerSliceTimeout: 2 * time.Second,
		ExpectedNodes:   []string{"node-a", "node-b", "node-c"},
		OperatorActor:   "bearer:test",
	})
	if err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	if len(replies) != 3 {
		t.Fatalf("got %d replies, want 3:\n%+v", len(replies), replies)
	}

	seen := map[string]bool{}
	for _, r := range replies {
		if r.Status != StatusOK {
			t.Errorf("node %s status = %s, want ok", r.NodeID, r.Status)
		}
		seen[r.NodeID] = true
	}
	for _, n := range []string{"node-a", "node-b", "node-c"} {
		if !seen[n] {
			t.Errorf("missing reply from %s", n)
		}
	}
}

// TestBroadcast_OneSiblingUnreachable_SynthesizesEntry asserts FR-006a:
// a missing responder appears as a synthesized `failed` reply, never
// causes the broadcast to fail.
func TestBroadcast_OneSiblingUnreachable_SynthesizesEntry(t *testing.T) {
	nc := startEmbeddedNATS(t)
	const cluster = "fan-miss"

	// Only register node-a and node-b; node-c never starts.
	for _, peer := range []string{"node-a", "node-b"} {
		p := peer
		s := NewServer(nc, cluster, p)
		_ = s.Register(SliceStatus, func(_ context.Context, _ map[string]any, _ string) (any, error) {
			return map[string]string{"node_id": p}, nil
		})
		if err := s.Serve(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
	}

	c := NewClient(nc, cluster)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	replies, err := c.Broadcast(ctx, SliceStatus, nil, CallOptions{
		PerSliceTimeout: 750 * time.Millisecond,
		ExpectedNodes:   []string{"node-a", "node-b", "node-c"},
	})
	if err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	if len(replies) != 3 {
		t.Fatalf("got %d replies, want 3:\n%+v", len(replies), replies)
	}

	var missing Reply
	for _, r := range replies {
		if r.NodeID == "node-c" {
			missing = r
			break
		}
	}
	if missing.Status != StatusFailed {
		t.Errorf("node-c.status = %q, want %q", missing.Status, StatusFailed)
	}
	if missing.Error == nil || missing.Error.Code != CodeSiblingUnreachable {
		t.Errorf("node-c.error = %+v, want code=%s", missing.Error, CodeSiblingUnreachable)
	}
}

// TestUnicast_HappyPath asserts a single peer Unicast returns the
// responder's payload.
func TestUnicast_HappyPath(t *testing.T) {
	nc := startEmbeddedNATS(t)
	const cluster = "fan-uni"

	s := NewServer(nc, cluster, "node-a")
	_ = s.Register(SliceConfig, func(_ context.Context, _ map[string]any, _ string) (any, error) {
		return map[string]string{"redacted": "yes"}, nil
	})
	if err := s.Serve(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	c := NewClient(nc, cluster)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, err := c.Unicast(ctx, SliceConfig, "node-a", nil, CallOptions{PerSliceTimeout: time.Second})
	if err != nil {
		t.Fatalf("Unicast: %v", err)
	}
	if r.Status != StatusOK || r.NodeID != "node-a" {
		t.Errorf("reply = %+v", r)
	}
}

// TestUnicast_NoResponder_ReturnsSyntheticUnreachable asserts that a
// unicast to a non-existent node yields a failed reply (not an error).
func TestUnicast_NoResponder_ReturnsSyntheticUnreachable(t *testing.T) {
	nc := startEmbeddedNATS(t)
	c := NewClient(nc, "fan-empty")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r, err := c.Unicast(ctx, SliceStatus, "ghost", nil, CallOptions{PerSliceTimeout: 250 * time.Millisecond})
	if err != nil {
		t.Fatalf("Unicast returned error (want synthetic reply): %v", err)
	}
	if r.Status != StatusFailed {
		t.Errorf("status = %q, want %q", r.Status, StatusFailed)
	}
	if r.Error == nil || r.Error.Code != CodeSiblingUnreachable {
		t.Errorf("error = %+v, want code=%s", r.Error, CodeSiblingUnreachable)
	}
}

// TestServer_HandlerError_ReturnsFailed asserts that a handler's
// error is wrapped into a Reply.Error block.
func TestServer_HandlerError_ReturnsFailed(t *testing.T) {
	nc := startEmbeddedNATS(t)
	const cluster = "fan-err"

	s := NewServer(nc, cluster, "node-a")
	_ = s.Register(SliceDoctor, func(_ context.Context, _ map[string]any, _ string) (any, error) {
		return nil, errors.New("synthetic doctor failure")
	})
	if err := s.Serve(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	c := NewClient(nc, cluster)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, err := c.Unicast(ctx, SliceDoctor, "node-a", nil, CallOptions{PerSliceTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusFailed {
		t.Errorf("status = %q, want %q", r.Status, StatusFailed)
	}
	if r.Error == nil || !strings.Contains(r.Error.Message, "synthetic doctor failure") {
		t.Errorf("error = %+v, want synthetic message", r.Error)
	}
}

// TestSubjectShape asserts the documented subject scheme.
func TestSubjectShape(t *testing.T) {
	if got := SubjectPrefix("prod-east"); got != "pgman_proxy.prod-east.fanout." {
		t.Errorf("prefix = %q", got)
	}
	if got := RequestSubject("prod-east", SliceStatus, "node-2"); got != "pgman_proxy.prod-east.fanout.status.node-2" {
		t.Errorf("request subject = %q", got)
	}
	if got := RequestSubject("prod-east", SliceStatus, "*"); got != "pgman_proxy.prod-east.fanout.status.*" {
		t.Errorf("broadcast subject = %q", got)
	}
}
