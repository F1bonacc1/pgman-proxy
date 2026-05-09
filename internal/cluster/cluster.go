// Package cluster wires the NATS coordination plane that pg-manager's
// Manager consumes. The new code in this repository is strictly limited
// to wiring (Constitution IV — Thin Scaffold over pg-manager); leadership
// election, state-store semantics, and event-bus delivery are entirely
// owned by github.com/f1bonacc1/pg-manager/adapters/nats.
package cluster

import (
	"context"
	"fmt"
	"time"

	pgmanager "github.com/f1bonacc1/pg-manager"
	natsadapter "github.com/f1bonacc1/pg-manager/adapters/nats"
	"github.com/nats-io/nats.go"

	"github.com/f1bonacc1/pgman-proxy/internal/config"
	"github.com/f1bonacc1/pgman-proxy/internal/obs"
)

// Handles bundles the NATS-backed adapter handles that pg-manager needs.
// Constructed once at startup and passed into manager.Config; never
// re-constructed at runtime (data-model.md § ClusterHandles).
type Handles struct {
	Conn       *nats.Conn
	Leadership *natsadapter.Leadership
	State      pgmanager.StateStore
	Bus        pgmanager.EventBus
}

// Close drains the NATS connection and closes the leadership lease.
// Best-effort — errors are logged but not returned, since this runs at
// shutdown.
func (h *Handles) Close(logger *obs.Logger) {
	if h == nil {
		return
	}
	if h.Leadership != nil {
		h.Leadership.Close()
	}
	if h.Conn != nil {
		// Drain returns immediately; the connection completes draining
		// asynchronously up to the configured FlusherTimeout.
		if err := h.Conn.Drain(); err != nil {
			logger.Warn("nats drain failed", pgmanager.Field{Key: "error", Value: err.Error()})
		}
	}
}

// Connect dials NATS within the configured connect-timeout budget. On
// failure the caller MUST exit fail-closed (FR-010, EX_DEPS).
func Connect(ctx context.Context, cfg config.NATSConfig, nodeID string, logger *obs.Logger) (*nats.Conn, error) {
	connectCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()

	opts := []nats.Option{
		nats.Name(fmt.Sprintf("pgman-proxy-%s", nodeID)),
		nats.Timeout(5 * time.Second),
		nats.ReconnectWait(cfg.ReconnectWait),
		nats.MaxReconnects(cfg.MaxReconnects),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				logger.Warn("nats disconnected",
					pgmanager.Field{Key: "url", Value: cfg.URL},
					pgmanager.Field{Key: "reason", Value: err.Error()})
			}
		}),
		nats.ReconnectHandler(func(c *nats.Conn) {
			logger.Warn("nats reconnected", pgmanager.Field{Key: "url", Value: c.ConnectedUrl()})
		}),
	}
	if cfg.CredsFile != "" {
		opts = append(opts, nats.UserCredentials(cfg.CredsFile))
	}

	connCh := make(chan *nats.Conn, 1)
	errCh := make(chan error, 1)
	go func() {
		c, err := nats.Connect(cfg.URL, opts...)
		if err != nil {
			errCh <- err
			return
		}
		connCh <- c
	}()
	select {
	case <-connectCtx.Done():
		return nil, fmt.Errorf("nats connect timeout %s exceeded: %w", cfg.ConnectTimeout, connectCtx.Err())
	case err := <-errCh:
		return nil, fmt.Errorf("nats connect: %w", err)
	case c := <-connCh:
		logger.Info("nats connected", pgmanager.Field{Key: "url", Value: cfg.URL})
		return c, nil
	}
}

// BuildHandles constructs the leadership, state-store, and event-bus
// adapters on top of an existing connection. Failure of any adapter
// returns a wrapped error and the caller is responsible for draining
// the connection.
func BuildHandles(ctx context.Context, conn *nats.Conn, clusterID, nodeID string, logger *obs.Logger) (*Handles, error) {
	leadership, err := natsadapter.NewLeadership(
		ctx, conn, clusterID, pgmanager.NodeID(nodeID),
		natsadapter.WithLogger(logger),
	)
	if err != nil {
		return nil, fmt.Errorf("leadership: %w", err)
	}
	state, err := natsadapter.NewStateStore(ctx, conn, clusterID)
	if err != nil {
		leadership.Close()
		return nil, fmt.Errorf("state store: %w", err)
	}
	bus, err := natsadapter.NewEventBus(conn)
	if err != nil {
		leadership.Close()
		return nil, fmt.Errorf("event bus: %w", err)
	}
	return &Handles{Conn: conn, Leadership: leadership, State: state, Bus: bus}, nil
}
