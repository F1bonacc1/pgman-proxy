// Per-peer status aggregator for /v1/status (feature 003).
//
// pg-manager's Manager.Status() returns per-peer scalars (LocalRole,
// LocalState, LeaderNodeID) but does NOT populate the cluster-wide
// Instances slice or PrimaryNodeID — that data isn't reconciled by
// pg-manager from its peers. The aggregator fans out a SliceStatus
// request to every peer via internal/fanout and stitches the replies
// into the snapshot returned by GET /v1/status.
//
// Wire payload is `peerSliceStatus` (NodeID + Role + State + Reachable
// + Timestamp). It is intentionally minimal so a single fan-out call
// has a tight RTT budget; LSN / lag fields stay zero until pg-manager
// exposes them in a stable cross-peer API.

package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	pgmanager "github.com/f1bonacc1/pg-manager"

	"github.com/f1bonacc1/pgman-proxy/internal/embedded"
	"github.com/f1bonacc1/pgman-proxy/internal/fanout"
	"github.com/f1bonacc1/pgman-proxy/internal/obs"
)

// peerSliceStatus is the per-peer payload carried by fan-out
// SliceStatus replies. Stable as long as feature 003 stays MINOR.
type peerSliceStatus struct {
	NodeID    string    `json:"node_id"`
	Role      int       `json:"role"`
	State     int       `json:"state"`
	Reachable bool      `json:"reachable"`
	Timestamp time.Time `json:"timestamp"`
}

// statusAggregator implements control.PeerAggregator using
// internal/fanout. Construct via newStatusAggregator and pass into
// control.Config.Aggregator.
type statusAggregator struct {
	client  *fanout.Client
	peers   []string
	timeout time.Duration
	logger  *obs.Logger
}

func newStatusAggregator(client *fanout.Client, peers []string, timeout time.Duration, logger *obs.Logger) *statusAggregator {
	if timeout <= 0 {
		timeout = 750 * time.Millisecond
	}
	return &statusAggregator{
		client:  client,
		peers:   peers,
		timeout: timeout,
		logger:  logger,
	}
}

// EnrichStatus implements control.PeerAggregator. On any internal
// failure it returns the input unchanged — the /v1/status handler
// then ships pg-manager's per-peer scalar snapshot, the same shape
// callers got before this enrichment existed.
func (a *statusAggregator) EnrichStatus(ctx context.Context, local pgmanager.Status) pgmanager.Status {
	if a == nil || a.client == nil || len(a.peers) == 0 {
		return local
	}
	callCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	replies, err := a.client.Broadcast(callCtx, fanout.SliceStatus, nil, fanout.CallOptions{
		PerSliceTimeout: a.timeout,
		ExpectedNodes:   a.peers,
		OperatorActor:   "system:status_aggregator",
	})
	if err != nil {
		if a.logger != nil {
			a.logger.Warn("status aggregator broadcast failed",
				pgmanager.Field{Key: "error", Value: err.Error()})
		}
		return local
	}

	out := local
	out.Instances = make([]pgmanager.InstanceStatus, 0, len(replies))
	for _, r := range replies {
		inst := pgmanager.InstanceStatus{
			NodeID:     pgmanager.NodeID(r.NodeID),
			LastSeenAt: r.RespondedAt,
		}
		if r.Status == fanout.StatusOK && r.Data != nil {
			var p peerSliceStatus
			b, _ := json.Marshal(r.Data)
			if jerr := json.Unmarshal(b, &p); jerr == nil {
				if p.NodeID != "" {
					inst.NodeID = pgmanager.NodeID(p.NodeID)
				}
				inst.Role = pgmanager.Role(p.Role)
				inst.State = pgmanager.State(p.State)
				inst.PostgresUp = p.Reachable &&
					pgmanager.State(p.State) != pgmanager.StateFailed &&
					pgmanager.State(p.State) != pgmanager.StateStopped &&
					pgmanager.State(p.State) != pgmanager.StateUnknown
				if !p.Timestamp.IsZero() {
					inst.LastSeenAt = p.Timestamp
				}
				if pgmanager.Role(p.Role) == pgmanager.RolePrimary && out.PrimaryNodeID == "" {
					out.PrimaryNodeID = pgmanager.NodeID(p.NodeID)
				}
			}
		}
		out.Instances = append(out.Instances, inst)
	}

	// pg-manager's Manager.Status returns SyncStandbys as the RAW
	// policy pool (no primary-exclusion) because the engine has no
	// cluster-wide PrimaryNodeID at hand. After fan-out has stitched
	// PrimaryNodeID into `out`, this is the right layer to apply the
	// pool-minus-primary filter — every peer's reply would carry the
	// same raw pool, so the answer is cluster-wide consistent regardless
	// of which node served the /v1/status call.
	if out.PrimaryNodeID != "" && len(out.SyncStandbys) > 0 {
		filtered := make([]pgmanager.NodeID, 0, len(out.SyncStandbys))
		for _, n := range out.SyncStandbys {
			if n != out.PrimaryNodeID {
				filtered = append(filtered, n)
			}
		}
		if len(filtered) == 0 {
			filtered = nil
		}
		out.SyncStandbys = filtered
	}
	return out
}

// statusResponderHandler returns the fanout.Handler that answers a
// SliceStatus request from a peer. It calls the local
// Manager.Status() and projects the cluster-relevant per-peer fields
// onto the wire payload.
func statusResponderHandler(status func(ctx context.Context) (pgmanager.Status, error)) fanout.Handler {
	return func(ctx context.Context, _ map[string]any, _ string) (any, error) {
		st, err := status(ctx)
		if err != nil {
			return nil, err
		}
		return peerSliceStatus{
			NodeID:    string(st.LocalNodeID),
			Role:      int(st.LocalRole),
			State:     int(st.LocalState),
			Reachable: true,
			Timestamp: time.Now().UTC(),
		}, nil
	}
}

// natsMeshResponderHandler returns the fanout.Handler that answers a
// SliceNATSMesh request. It captures the embedded server's snapshot at
// reply time — every peer responds with its own view of the mesh.
func natsMeshResponderHandler(srv *embedded.Server) fanout.Handler {
	return func(_ context.Context, _ map[string]any, _ string) (any, error) {
		if srv == nil {
			return nil, fmt.Errorf("nats_mesh: embedded server unavailable")
		}
		return srv.Snapshot(), nil
	}
}

// sliceNotImplementedHandler returns a fanout.Handler that always
// fails with a documented "slice not implemented in this build" error.
// Used for SliceConfig + SliceDoctor until US3/US6 land the real
// implementations. The peer-aggregation path treats `status: failed`
// replies with a structured `_error` placeholder per
// fanout-protocol.md § Aggregation rules.
func sliceNotImplementedHandler(slice fanout.Slice) fanout.Handler {
	return func(_ context.Context, _ map[string]any, _ string) (any, error) {
		return nil, fmt.Errorf("slice %q not implemented in this build (003 follow-up)", slice)
	}
}
