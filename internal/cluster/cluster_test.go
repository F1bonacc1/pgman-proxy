package cluster

import (
	"context"
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/f1bonacc1/pgman-proxy/internal/obs"
)

func TestClassifyOutcome(t *testing.T) {
	tests := map[string]string{
		"pgmanager.demo.auto_rebootstrap.detected": "delivered",
		"pgmanager.demo.auto_rebootstrap.refused":  "refused",
		"pgmanager.demo.auto_demote.failed":        "failed",
		"pgmanager.demo.divergence.parked":         "delivered",
		"pgmanager.demo.conninfo.reconciled":       "delivered",
	}
	for subject, want := range tests {
		t.Run(subject, func(t *testing.T) {
			if got := classifyOutcome(subject); got != want {
				t.Errorf("classifyOutcome(%q) = %q, want %q", subject, got, want)
			}
		})
	}
}

// TestBuildHandles_CampaignGateControlsAcquisition pins the wiring the
// switchover path depends on: with the gate closed the node never takes
// a free lease, and opening the gate lets it acquire. A BuildHandles
// that drops the gate fails the first half.
func TestBuildHandles_CampaignGateControlsAcquisition(t *testing.T) {
	conn := startJetStreamNATS(t)
	logger := obs.NewLogger(io.Discard, "info", "gate-test", "node-a", "test")
	ctx := context.Background()
	var open atomic.Bool

	h, err := BuildHandles(ctx, conn, "gate-test", "node-a", logger, 200*time.Millisecond, open.Load)
	if err != nil {
		t.Fatalf("BuildHandles: %v", err)
	}
	t.Cleanup(h.Leadership.Close)

	// Ten acquisition cadences (leaseTTL/2) with nobody holding the key.
	time.Sleep(time.Second)
	if leader, _ := h.Leadership.IsLeader(ctx); leader {
		t.Fatal("node acquired the lease while its campaign gate was closed")
	}

	open.Store(true)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if leader, _ := h.Leadership.IsLeader(ctx); leader {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("node did not acquire the lease within 5s of its campaign gate opening")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// startJetStreamNATS spins up an in-process NATS server with JetStream
// on a temp directory and returns a connection to it. Mirrors the
// helper in internal/history.
func startJetStreamNATS(t *testing.T) *nats.Conn {
	t.Helper()
	dir := t.TempDir()
	srv, err := natsserver.NewServer(&natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  dir,
		NoLog:     true,
		NoSigs:    true,
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
		_ = os.RemoveAll(dir)
	})
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}
