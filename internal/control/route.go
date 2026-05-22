// Leader-routing helper (T056 / FR-026 / FR-034).
//
// Two modes:
//
//   * forward — receiver publishes the request body on
//     `pgman_proxy.<cluster_id>.lcm.request.<op>` and awaits the
//     leader's reply on a private inbox. Bounded by
//     `control.leader_route_timeout` (FR-034). On timeout return
//     `leader_route_timeout` / HTTP 504; audit `outcome=failed`.
//
//   * redirect — receiver returns HTTP 307 with `Location` set to the
//     leader's control-plane address. Used when operators want clients
//     to chase the leader themselves.
//
// `Promote` is local-only and never routes (its semantics are "promote
// THIS node"). The caller decides which ops are leader-only and passes
// `leaderOnly=true` only for those.

package control

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

// LeaderRouter encapsulates the routing decision plus the NATS
// req/reply transport. Construct via NewLeaderRouter; one instance per
// LCM server.
type LeaderRouter struct {
	mode      string // "forward" | "redirect"
	timeout   time.Duration
	clusterID string
	conn      *nats.Conn
	state     LeaderState
}

// NewLeaderRouter constructs a router. `conn` may be nil in `redirect`
// mode (we never publish in that path) but MUST be non-nil in `forward`
// mode.
func NewLeaderRouter(mode string, timeout time.Duration, clusterID string, conn *nats.Conn, state LeaderState) *LeaderRouter {
	return &LeaderRouter{
		mode:      mode,
		timeout:   timeout,
		clusterID: clusterID,
		conn:      conn,
		state:     state,
	}
}

// IsLeader reports whether the local peer is the cluster leader.
func (r *LeaderRouter) IsLeader() bool { return r.state.IsLeader() }

// Mode returns the configured mode ("forward" or "redirect").
func (r *LeaderRouter) Mode() string { return r.mode }

// LeaderAddr returns the leader's control-plane address, or "" when
// unknown.
func (r *LeaderRouter) LeaderAddr() string { return r.state.LeaderAddr() }

// LeaderID returns the leader's NodeID, or "" when unknown.
func (r *LeaderRouter) LeaderID() string { return r.state.LeaderID() }

// Forward publishes payload on the documented subject for op and
// awaits a reply within the configured timeout. Returns the reply body
// on success; ErrLeaderRouteTimeout on deadline; any underlying NATS
// error otherwise.
//
// Optional headers (`X-Request-Id`, `X-Actor`, `traceparent`) propagate
// the originating client's identity and trace context to the leader so
// the leader's audit record correlates with the forwarder's. Older
// publishers (or this method called with empty values) omit the header,
// and the responder falls back to defaults — wire-compatible.
func (r *LeaderRouter) Forward(ctx context.Context, op, requestID, actor, traceparent string, payload []byte) ([]byte, error) {
	if r.conn == nil {
		return nil, errors.New("forward: nil NATS connection")
	}
	subject := fmt.Sprintf("pgman_proxy.%s.lcm.request.%s", r.clusterID, op)
	timeoutCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	msg := &nats.Msg{Subject: subject, Data: payload}
	if requestID != "" || actor != "" || traceparent != "" {
		msg.Header = nats.Header{}
		if requestID != "" {
			msg.Header.Set("X-Request-Id", requestID)
		}
		if actor != "" {
			msg.Header.Set("X-Actor", actor)
		}
		if traceparent != "" {
			msg.Header.Set("traceparent", traceparent)
		}
	}
	reply, err := r.conn.RequestMsgWithContext(timeoutCtx, msg)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, nats.ErrTimeout) {
			return nil, ErrLeaderRouteTimeout
		}
		return nil, err
	}
	return reply.Data, nil
}

// ErrLeaderRouteTimeout is the typed sentinel handlers map to the
// `leader_route_timeout` error code (FR-034).
var ErrLeaderRouteTimeout = errors.New("leader route: forward timed out")
