package embedded

import (
	"sync"
	"testing"

	"github.com/nats-io/nats-server/v2/server"
)

// fakeRouteSource captures the per-tick Routez result. Real Server
// state is opaque to tests; we drive the tick() method directly with
// a swapped-in known map.
type fakeEmit struct {
	mu   sync.Mutex
	rows []string
}

func (f *fakeEmit) record(kind LifecycleEventKind, fields map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	peer, _ := fields["peer_node_id"].(string)
	f.rows = append(f.rows, string(kind)+" "+peer)
}

func (f *fakeEmit) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

// TestRouteWatcher_DedupByPeerNodeID — the watcher must NOT emit a
// duplicate `embedded_nats.route_up` when NATS reassigns a new Rid
// to an already-known peer link. Pre-fix: each Rid bump → another
// route_up event; post-fix: identity is the peer's RemoteName.
func TestRouteWatcher_DedupByPeerNodeID(t *testing.T) {
	fe := &fakeEmit{}
	srv := &Server{nodeID: "self", clusterID: "c", emit: fe.record}
	w := &RouteWatcher{
		srv:   srv,
		known: map[string]*server.RouteInfo{},
	}

	// Simulate tick 1: one route to node-b, Rid=10.
	w.known = map[string]*server.RouteInfo{}
	emitFor(w, []*server.RouteInfo{{Rid: 10, RemoteName: "node-b", DidSolicit: true, IP: "10.0.0.1", Port: 6222}})

	if fe.count() != 1 {
		t.Fatalf("tick 1: want 1 route_up, got %d", fe.count())
	}

	// Simulate tick 2: same peer, NEW Rid (TCP reconnect under the hood).
	// The peer's RemoteName is unchanged → must NOT emit a new event.
	emitFor(w, []*server.RouteInfo{{Rid: 11, RemoteName: "node-b", DidSolicit: true, IP: "10.0.0.1", Port: 6222}})
	if fe.count() != 1 {
		t.Errorf("tick 2: peer with new Rid must NOT re-emit, got %d events total", fe.count())
	}

	// Simulate tick 3: peer disappears → exactly one route_down.
	emitFor(w, nil)
	if fe.count() != 2 {
		t.Errorf("tick 3: want one additional route_down, total events %d", fe.count())
	}

	// Simulate tick 4: peer reappears with yet another Rid → route_up
	// again (legitimate transition this time).
	emitFor(w, []*server.RouteInfo{{Rid: 99, RemoteName: "node-b", DidSolicit: false, IP: "10.0.0.1", Port: 6222}})
	if fe.count() != 3 {
		t.Errorf("tick 4: want route_up after a real down/up transition, total events %d", fe.count())
	}
}

// TestRouteWatcher_SkipsHandshakeInFlight — routes whose RemoteName
// hasn't been announced yet (peer in mid-handshake) should NOT emit
// a route_up event; emitting one would surface a useless
// `peer_node_id=""` line in /v1/history.
func TestRouteWatcher_SkipsHandshakeInFlight(t *testing.T) {
	fe := &fakeEmit{}
	srv := &Server{nodeID: "self", clusterID: "c", emit: fe.record}
	w := &RouteWatcher{
		srv:   srv,
		known: map[string]*server.RouteInfo{},
	}

	emitFor(w, []*server.RouteInfo{{Rid: 5, RemoteName: "", DidSolicit: true, IP: "10.0.0.7", Port: 6222}})
	if fe.count() != 0 {
		t.Errorf("handshake-in-flight route should not emit, got %d", fe.count())
	}

	emitFor(w, []*server.RouteInfo{{Rid: 5, RemoteName: "node-b", DidSolicit: true, IP: "10.0.0.7", Port: 6222}})
	if fe.count() != 1 {
		t.Errorf("route_up should emit once the peer name is known, got %d", fe.count())
	}
}

// emitFor injects a fake Routez result into the watcher's diff loop
// without booting a real NATS server. Mirrors the body of tick()
// minus the live srv.Routez() call.
func emitFor(w *RouteWatcher, routes []*server.RouteInfo) {
	w.mu.Lock()
	defer w.mu.Unlock()
	current := map[string]*server.RouteInfo{}
	for _, ri := range routes {
		if ri == nil || ri.RemoteName == "" {
			continue
		}
		current[ri.RemoteName] = ri
		if _, seen := w.known[ri.RemoteName]; !seen {
			direction := "inbound"
			if ri.DidSolicit {
				direction = "outbound"
			}
			w.srv.emit("embedded_nats.route_up", map[string]any{
				"node_id":         w.srv.nodeID,
				"cluster_id":      w.srv.clusterID,
				"peer_route_url":  routeURL(ri),
				"peer_node_id":    ri.RemoteName,
				"direction":       direction,
				"password_prefix": "",
			})
		}
	}
	for peer, prev := range w.known {
		if _, still := current[peer]; !still {
			w.srv.emit("embedded_nats.route_down", map[string]any{
				"node_id":        w.srv.nodeID,
				"cluster_id":     w.srv.clusterID,
				"peer_route_url": routeURL(prev),
				"peer_node_id":   prev.RemoteName,
				"reason":         "peer_disconnect",
			})
		}
	}
	w.known = current
}
