package embedded

import (
	"context"
	"sync"
	"time"

	"github.com/nats-io/nats-server/v2/server"
)

// RouteWatcher polls the embedded server's `Routez()` snapshot at a
// fixed interval and emits `embedded_nats.route_up` /
// `embedded_nats.route_down` events on transitions
// (contracts/observability.md). Per-peer identity is taken from the
// sibling's `RemoteName` field, which is set to the pgman-proxy node
// ID at startup (RD-001a per-peer audit identity).
//
// Polling is the pragmatic choice for v1: NATS server v2 does not
// expose a public route-event hook, but `Routez()` is cheap (in-
// process method call, no I/O) and a 2-second poll covers the SC-002
// failover budget with margin.
type RouteWatcher struct {
	srv      *Server
	interval time.Duration

	mu      sync.Mutex
	known   map[uint64]*server.RouteInfo // by Rid (route id)
	stopped bool
}

// NewRouteWatcher constructs a route watcher. Returns nil if the
// supplied server is nil (no-op for tests that don't boot the NATS
// server).
func NewRouteWatcher(srv *Server, interval time.Duration) *RouteWatcher {
	if srv == nil {
		return nil
	}
	if interval == 0 {
		interval = 2 * time.Second
	}
	return &RouteWatcher{
		srv:      srv,
		interval: interval,
		known:    map[uint64]*server.RouteInfo{},
	}
}

// Start launches the polling loop until ctx is cancelled. Safe to
// call on a nil receiver.
func (w *RouteWatcher) Start(ctx context.Context) {
	if w == nil {
		return
	}
	go w.run(ctx)
}

func (w *RouteWatcher) run(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.tick()
		}
	}
}

// tick is the per-poll diff: compare the current Routez snapshot to
// the known set and emit route_up / route_down events for the delta.
// Exposed as an internal method for testability.
func (w *RouteWatcher) tick() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped || w.srv == nil || w.srv.srv == nil {
		return
	}
	rz, err := w.srv.srv.Routez(nil)
	if err != nil || rz == nil {
		return
	}
	current := map[uint64]*server.RouteInfo{}
	for _, ri := range rz.Routes {
		if ri == nil {
			continue
		}
		current[ri.Rid] = ri
		if _, seen := w.known[ri.Rid]; !seen {
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
				"password_prefix": "", // populated by future pp work
			})
		}
	}
	for rid, prev := range w.known {
		if _, still := current[rid]; !still {
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

// Stop marks the watcher stopped. The polling goroutine exits on the
// next ctx-cancel; this is mainly a safety hatch for tests.
func (w *RouteWatcher) Stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.stopped = true
	w.mu.Unlock()
}

// routeURL renders the IP:port pair from a RouteInfo into the
// canonical `nats-route://` URL form used elsewhere in the package.
func routeURL(ri *server.RouteInfo) string {
	if ri == nil {
		return ""
	}
	return "nats-route://" + ri.IP + ":" + portString(ri.Port)
}

func portString(p int) string {
	// Avoid strconv import in this tight file; simple positive-int
	// format is sufficient.
	const digits = "0123456789"
	if p == 0 {
		return "0"
	}
	buf := [11]byte{}
	i := len(buf)
	for p > 0 {
		i--
		buf[i] = digits[p%10]
		p /= 10
	}
	return string(buf[i:])
}
