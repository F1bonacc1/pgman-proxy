package history

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// TestEnsureHistoryStream_RetriesUnansweredCreate: same failure mode
// as embedded.TestPreCreateClusterKV_RetriesUnansweredLookup, one boot
// gate later. A single unanswered $JS.API.STREAM.CREATE must cost one
// attempt, not the whole 30 s budget.
func TestEnsureHistoryStream_RetriesUnansweredCreate(t *testing.T) {
	orig := ensureAttemptTimeout
	ensureAttemptTimeout = time.Second
	t.Cleanup(func() { ensureAttemptTimeout = orig })

	// Core NATS only: the fake responder below stands in for the JS API.
	srv, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
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
	})
	responder, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(responder.Close)
	client, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(client.Close)

	// Echo the requested config back so reconcile sees no drift.
	var reqs atomic.Int32
	if _, err := responder.Subscribe("$JS.API.STREAM.CREATE."+StreamName("it"), func(m *nats.Msg) {
		if reqs.Add(1) == 1 {
			return // the dropped reply
		}
		_ = m.Respond(fmt.Appendf(nil,
			`{"type":"io.nats.jetstream.api.v1.stream_create_response","config":%s,"created":"2026-09-26T08:36:34Z","state":{}}`,
			m.Data))
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := responder.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	js, err := jetstream.New(client)
	if err != nil {
		t.Fatalf("jetstream.New: %v", err)
	}
	start := time.Now()
	_, err = EnsureHistoryStream(context.Background(), js, "it", DefaultStreamOptions(3))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("EnsureHistoryStream after one dropped reply: %v (elapsed %s)", err, elapsed)
	}
	if got := reqs.Load(); got < 2 {
		t.Errorf("STREAM.CREATE requests = %d, want a retry after the dropped one", got)
	}
	if limit := 5 * ensureAttemptTimeout; elapsed > limit {
		t.Errorf("elapsed %s, want < %s: a dropped reply must cost one attempt, not the budget", elapsed, limit)
	}
}
