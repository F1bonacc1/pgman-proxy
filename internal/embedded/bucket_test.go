package embedded

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// startPlainNATS spins up an in-process core-NATS server WITHOUT
// JetStream so a test can stand in for the JS API with its own
// responder. Returns one connection for the code under test and one
// for the fake responder.
func startPlainNATS(t *testing.T) (client, responder *nats.Conn) {
	t.Helper()
	srv, err := natsserver.NewServer(&natsserver.Options{
		Host:   "127.0.0.1",
		Port:   -1,
		NoLog:  true,
		NoSigs: true,
	})
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
	for _, nc := range []**nats.Conn{&client, &responder} {
		c, err := nats.Connect(srv.ClientURL())
		if err != nil {
			t.Fatalf("nats.Connect: %v", err)
		}
		t.Cleanup(c.Close)
		*nc = c
	}
	return client, responder
}

// TestPreCreateClusterKV_RetriesUnansweredLookup pins the
// nightly-integration failure of run 36230204614: nats-server leaves
// some $JS.API.STREAM.INFO requests unanswered while a freshly created
// stream's Raft group elects a leader, and nats.go applies its 5 s
// request default only when the ctx has no deadline. Without a
// per-attempt bound, one dropped reply burned the whole 60 s budget
// with zero retries and the peer exited 75.
func TestPreCreateClusterKV_RetriesUnansweredLookup(t *testing.T) {
	orig := preCreateAttemptTimeout
	preCreateAttemptTimeout = time.Second
	t.Cleanup(func() { preCreateAttemptTimeout = orig })

	client, responder := startPlainNATS(t)

	// A ready R3 KV stream with AllowDirect already off, so a
	// successful attempt is lookup + status + stream fetch only.
	const info = `{"type":"io.nats.jetstream.api.v1.stream_info_response",
	 "config":{"name":"KV_pgmgr_it","subjects":["$KV.pgmgr_it.>"],"retention":"limits",
	  "max_msgs_per_subject":8,"storage":"file","num_replicas":3,"allow_direct":false,
	  "discard":"new","allow_rollup_hdrs":true,"deny_delete":true},
	 "created":"2026-09-26T08:36:33Z","state":{},"cluster":{"leader":"node-a"}}`
	var reqs atomic.Int32
	if _, err := responder.Subscribe("$JS.API.STREAM.INFO.KV_pgmgr_it", func(m *nats.Msg) {
		if reqs.Add(1) == 1 {
			return // the dropped reply
		}
		_ = m.Respond([]byte(info))
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := responder.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	start := time.Now()
	err := PreCreateClusterKV(context.Background(), client, "it", 3)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("PreCreateClusterKV after one dropped reply: %v (elapsed %s)", err, elapsed)
	}
	if got := reqs.Load(); got < 2 {
		t.Errorf("STREAM.INFO requests = %d, want a retry after the dropped one", got)
	}
	if limit := 5 * preCreateAttemptTimeout; elapsed > limit {
		t.Errorf("elapsed %s, want < %s: a dropped reply must cost one attempt, not the budget", elapsed, limit)
	}
}
