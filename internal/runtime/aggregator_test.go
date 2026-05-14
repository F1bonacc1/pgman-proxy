package runtime

import (
	"context"
	"os"
	"sort"
	"testing"
	"time"

	pgmanager "github.com/f1bonacc1/pg-manager"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/f1bonacc1/pgman-proxy/internal/fanout"
)

const aggTestCluster = "agg-test"

// startEmbeddedNATSForAgg spins up a bare NATS server (no JetStream
// needed — fanout uses pub/sub + req/reply only). Mirrors the helper in
// internal/fanout's tests; duplicated here to keep packages independent.
func startEmbeddedNATSForAgg(t *testing.T) *nats.Conn {
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

// registerPeerResponder spins up a fanout.Server bound to one peer ID,
// registering a SliceStatus handler that returns a snapshot built from
// the supplied role/state. Cleaned up via t.Cleanup.
func registerPeerResponder(t *testing.T, nc *nats.Conn, nodeID string, role pgmanager.Role, state pgmanager.State) {
	t.Helper()
	srv := fanout.NewServer(nc, aggTestCluster, nodeID)
	stub := func(_ context.Context) (pgmanager.Status, error) {
		return pgmanager.Status{
			ClusterID:   aggTestCluster,
			LocalNodeID: pgmanager.NodeID(nodeID),
			LocalRole:   role,
			LocalState:  state,
		}, nil
	}
	if err := srv.Register(fanout.SliceStatus, statusResponderHandler(stub)); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := srv.Serve(); err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
}

// TestEnrichStatus_StitchesInstancesAndInfersPrimary is the happy path:
// three peers respond with their local snapshots; the aggregator
// stitches them into Instances and picks PrimaryNodeID from whichever
// peer reports RolePrimary.
func TestEnrichStatus_StitchesInstancesAndInfersPrimary(t *testing.T) {
	nc := startEmbeddedNATSForAgg(t)

	registerPeerResponder(t, nc, "node-a", pgmanager.RoleStandby, pgmanager.StateRunning)
	registerPeerResponder(t, nc, "node-b", pgmanager.RolePrimary, pgmanager.StateRunning)
	registerPeerResponder(t, nc, "node-c", pgmanager.RoleStandby, pgmanager.StateFailed)

	agg := newStatusAggregator(
		fanout.NewClient(nc, aggTestCluster),
		[]string{"node-a", "node-b", "node-c"},
		2*time.Second,
		nil,
	)

	local := pgmanager.Status{
		ClusterID:    aggTestCluster,
		LeaderNodeID: "node-b",
		LocalNodeID:  "node-a",
		LocalRole:    pgmanager.RoleStandby,
		LocalState:   pgmanager.StateRunning,
		// pg-manager never fills these; the aggregator should populate them.
		PrimaryNodeID: "",
		Instances:     nil,
	}

	got := agg.EnrichStatus(context.Background(), local)

	if got.PrimaryNodeID != "node-b" {
		t.Errorf("PrimaryNodeID = %q, want node-b", got.PrimaryNodeID)
	}
	if len(got.Instances) != 3 {
		t.Fatalf("len(Instances) = %d, want 3 — got %+v", len(got.Instances), got.Instances)
	}

	sort.Slice(got.Instances, func(i, j int) bool { return got.Instances[i].NodeID < got.Instances[j].NodeID })

	cases := []struct {
		idx        int
		nodeID     pgmanager.NodeID
		role       pgmanager.Role
		state      pgmanager.State
		postgresUp bool
	}{
		{0, "node-a", pgmanager.RoleStandby, pgmanager.StateRunning, true},
		{1, "node-b", pgmanager.RolePrimary, pgmanager.StateRunning, true},
		{2, "node-c", pgmanager.RoleStandby, pgmanager.StateFailed, false},
	}
	for _, c := range cases {
		inst := got.Instances[c.idx]
		if inst.NodeID != c.nodeID {
			t.Errorf("instances[%d].NodeID = %q, want %q", c.idx, inst.NodeID, c.nodeID)
		}
		if inst.Role != c.role {
			t.Errorf("instances[%d].Role = %v, want %v", c.idx, inst.Role, c.role)
		}
		if inst.State != c.state {
			t.Errorf("instances[%d].State = %v, want %v", c.idx, inst.State, c.state)
		}
		if inst.PostgresUp != c.postgresUp {
			t.Errorf("instances[%d].PostgresUp = %v, want %v", c.idx, inst.PostgresUp, c.postgresUp)
		}
		if inst.LastSeenAt.IsZero() {
			t.Errorf("instances[%d].LastSeenAt is zero", c.idx)
		}
	}

	// LeaderNodeID is the input's field — must NOT be mutated.
	if got.LeaderNodeID != "node-b" {
		t.Errorf("LeaderNodeID mutated to %q", got.LeaderNodeID)
	}
}

// TestEnrichStatus_MissingPeerSynthesizesUnknownRow validates FR-006a:
// if a peer doesn't reply within the budget, the aggregator emits a
// fan-out `sibling_unreachable` reply, which surfaces as an
// Instance row with Role=RoleUnknown / State=StateUnknown.
func TestEnrichStatus_MissingPeerSynthesizesUnknownRow(t *testing.T) {
	nc := startEmbeddedNATSForAgg(t)

	registerPeerResponder(t, nc, "node-a", pgmanager.RoleStandby, pgmanager.StateRunning)
	registerPeerResponder(t, nc, "node-b", pgmanager.RolePrimary, pgmanager.StateRunning)
	// node-c is deliberately not registered.

	agg := newStatusAggregator(
		fanout.NewClient(nc, aggTestCluster),
		[]string{"node-a", "node-b", "node-c"},
		300*time.Millisecond,
		nil,
	)

	got := agg.EnrichStatus(context.Background(), pgmanager.Status{
		ClusterID:   aggTestCluster,
		LocalNodeID: "node-a",
	})

	if len(got.Instances) != 3 {
		t.Fatalf("len(Instances) = %d, want 3", len(got.Instances))
	}

	var sawUnknown bool
	for _, inst := range got.Instances {
		if inst.NodeID == "node-c" {
			if inst.Role != pgmanager.RoleUnknown {
				t.Errorf("node-c Role = %v, want RoleUnknown", inst.Role)
			}
			if inst.State != pgmanager.StateUnknown {
				t.Errorf("node-c State = %v, want StateUnknown", inst.State)
			}
			if inst.PostgresUp {
				t.Errorf("node-c PostgresUp = true, want false (no reply)")
			}
			sawUnknown = true
		}
	}
	if !sawUnknown {
		t.Errorf("no node-c row present in Instances: %+v", got.Instances)
	}
	if got.PrimaryNodeID != "node-b" {
		t.Errorf("PrimaryNodeID = %q, want node-b (still picked up from node-b's reply)", got.PrimaryNodeID)
	}
}

// TestEnrichStatus_FiltersPrimaryFromSyncStandbys asserts that the
// pool-minus-primary filter runs after PrimaryNodeID is stitched. The
// engine reports the RAW pool (cluster-wide consistent); the aggregator
// is the only layer that knows who is primary, so this is where the
// filter lives.
func TestEnrichStatus_FiltersPrimaryFromSyncStandbys(t *testing.T) {
	nc := startEmbeddedNATSForAgg(t)

	registerPeerResponder(t, nc, "node-a", pgmanager.RoleStandby, pgmanager.StateRunning)
	registerPeerResponder(t, nc, "node-b", pgmanager.RolePrimary, pgmanager.StateRunning)
	registerPeerResponder(t, nc, "node-c", pgmanager.RoleStandby, pgmanager.StateRunning)

	agg := newStatusAggregator(
		fanout.NewClient(nc, aggTestCluster),
		[]string{"node-a", "node-b", "node-c"},
		2*time.Second,
		nil,
	)

	// Local Status comes from the queried peer (node-a here). pg-manager
	// returns the RAW policy pool — includes the primary. The aggregator
	// must strip PrimaryNodeID after stitching.
	local := pgmanager.Status{
		ClusterID:    aggTestCluster,
		LocalNodeID:  "node-a",
		SyncStandbys: []pgmanager.NodeID{"node-a", "node-b", "node-c"},
	}

	got := agg.EnrichStatus(context.Background(), local)

	if got.PrimaryNodeID != "node-b" {
		t.Fatalf("PrimaryNodeID = %q, want node-b (precondition)", got.PrimaryNodeID)
	}
	want := []pgmanager.NodeID{"node-a", "node-c"}
	if len(got.SyncStandbys) != len(want) {
		t.Fatalf("SyncStandbys = %v, want %v (primary node-b must be filtered out)", got.SyncStandbys, want)
	}
	for i := range want {
		if got.SyncStandbys[i] != want[i] {
			t.Errorf("SyncStandbys[%d] = %q, want %q", i, got.SyncStandbys[i], want[i])
		}
	}
}

// TestEnrichStatus_AsyncOnlyPolicyKeepsNilSyncStandbys asserts that
// when the engine reports nil SyncStandbys (AsyncOnly), the aggregator
// leaves it nil — even after PrimaryNodeID is stitched, there's nothing
// to filter.
func TestEnrichStatus_AsyncOnlyPolicyKeepsNilSyncStandbys(t *testing.T) {
	nc := startEmbeddedNATSForAgg(t)
	registerPeerResponder(t, nc, "node-a", pgmanager.RolePrimary, pgmanager.StateRunning)
	registerPeerResponder(t, nc, "node-b", pgmanager.RoleStandby, pgmanager.StateRunning)
	agg := newStatusAggregator(
		fanout.NewClient(nc, aggTestCluster),
		[]string{"node-a", "node-b"},
		2*time.Second,
		nil,
	)
	got := agg.EnrichStatus(context.Background(), pgmanager.Status{
		ClusterID: aggTestCluster, LocalNodeID: "node-a",
		SyncStandbys: nil, // AsyncOnly
	})
	if got.SyncStandbys != nil {
		t.Errorf("SyncStandbys = %v, want nil (AsyncOnly)", got.SyncStandbys)
	}
}

// TestEnrichStatus_NilClientReturnsInputUnchanged ensures single-peer
// test paths (no fan-out wiring) pass through pg-manager's per-peer
// scalar snapshot verbatim.
func TestEnrichStatus_NilClientReturnsInputUnchanged(t *testing.T) {
	agg := newStatusAggregator(nil, []string{"node-a"}, time.Second, nil)
	local := pgmanager.Status{ClusterID: "x", LocalNodeID: "node-a"}
	got := agg.EnrichStatus(context.Background(), local)
	if got.LocalNodeID != "node-a" || got.ClusterID != "x" {
		t.Errorf("input mutated: %+v", got)
	}
	if got.Instances != nil {
		t.Errorf("Instances should remain nil, got %+v", got.Instances)
	}
}
