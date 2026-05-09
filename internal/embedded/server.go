package embedded

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats-server/v2/server"
)

// LifecycleEventKind enumerates the embedded-NATS lifecycle event names
// emitted via the host's structured logger
// (contracts/observability.md § Structured-log events).
type LifecycleEventKind string

const (
	EventServerStarted   LifecycleEventKind = "embedded_nats.server_started"
	EventServerReady     LifecycleEventKind = "embedded_nats.server_ready"
	EventServerStopped   LifecycleEventKind = "embedded_nats.server_stopped"
	EventStorageDegraded LifecycleEventKind = "embedded_nats.storage_degraded"
	EventReloadApplied   LifecycleEventKind = "embedded_nats.reload_applied"
)

// EventEmitter is the host-supplied callback used to surface
// lifecycle events into the project's structured-log pipeline. Keeping
// it as a callback (rather than importing internal/obs) preserves the
// package's testability and avoids a circular import between
// internal/embedded and internal/obs.
type EventEmitter func(kind LifecycleEventKind, fields map[string]any)

// Server wraps a *nats-server.Server with the lifecycle semantics
// required by feature 002: explicit ready gating, structured event
// emission, clean shutdown that doesn't leak goroutines.
type Server struct {
	nodeID      string
	clusterID   string
	srv         *server.Server
	emit        EventEmitter
	startedAt   time.Time
	readyClosed atomic.Bool
	ready       chan struct{}
	stopped     atomic.Bool
}

// NewServer constructs an embedded server from the supplied options.
// It does NOT start the server; call Start() to bring it online. The
// emit callback is required (use a no-op closure if the host is not
// ready to consume events yet).
func NewServer(opts *server.Options, nodeID, clusterID string, emit EventEmitter) (*Server, error) {
	if opts == nil {
		return nil, errors.New("embedded.NewServer: opts is nil")
	}
	if emit == nil {
		// Treat a nil emitter as a programming error so we surface it
		// at construction rather than crashing inside the goroutine
		// that publishes the first event.
		return nil, errors.New("embedded.NewServer: EventEmitter is required (use a no-op closure if no logger yet)")
	}
	srv, err := server.NewServer(opts)
	if err != nil {
		return nil, fmt.Errorf("server.NewServer: %w", err)
	}
	return &Server{
		nodeID:    nodeID,
		clusterID: clusterID,
		srv:       srv,
		emit:      emit,
		ready:     make(chan struct{}),
	}, nil
}

// Start launches the embedded NATS server in a background goroutine
// and blocks the calling goroutine until the server reports
// ReadyForConnections — or the supplied context is cancelled, or the
// readyTimeout elapses. Returns nil on a successful boot; an error
// (with the appropriate startup-fail-closed semantics from
// contracts/lifecycle.md) otherwise.
func (s *Server) Start(ctx context.Context, readyTimeout time.Duration) error {
	if s.srv == nil {
		return errors.New("embedded.Server.Start: server is nil")
	}
	s.startedAt = time.Now()

	go s.srv.Start()

	s.emit(EventServerStarted, map[string]any{
		"node_id":    s.nodeID,
		"cluster_id": s.clusterID,
	})

	// nats-server's ReadyForConnections is blocking up to its own
	// timeout; we wrap it in a context-cancellable wait so a SIGTERM
	// during startup unblocks the caller.
	readyCh := make(chan bool, 1)
	go func() {
		readyCh <- s.srv.ReadyForConnections(readyTimeout)
	}()

	select {
	case ok := <-readyCh:
		if !ok {
			return fmt.Errorf("embedded NATS not ready within %s", readyTimeout)
		}
	case <-ctx.Done():
		return fmt.Errorf("embedded NATS startup cancelled: %w", ctx.Err())
	}

	if !s.readyClosed.Swap(true) {
		close(s.ready)
	}

	s.emit(EventServerReady, map[string]any{
		"node_id":            s.nodeID,
		"cluster_id":         s.clusterID,
		"client_listen_addr": s.srv.ClientURL(),
		"routes_listen_addr": formatAddr(s.srv.ClusterAddr()),
		"wait_ms":            time.Since(s.startedAt).Milliseconds(),
	})

	return nil
}

// Ready returns a channel that closes when the server reaches the
// ready state. Useful for callers that want to wait without driving
// the Start budget themselves (e.g., the in-process pg-manager
// adapter waiting for the loopback dial to succeed).
func (s *Server) Ready() <-chan struct{} { return s.ready }

// ClientURL returns the loopback URL the in-process pg-manager
// adapters dial. Returns "" before the server is ready.
func (s *Server) ClientURL() string {
	if s == nil || s.srv == nil {
		return ""
	}
	return s.srv.ClientURL()
}

// NumRoutes returns the count of currently-meshed sibling cluster
// routes (excluding self). Powers the
// `pgman_proxy_embedded_nats_routes_meshed` gauge.
func (s *Server) NumRoutes() int {
	if s == nil || s.srv == nil {
		return 0
	}
	return s.srv.NumRoutes()
}

// Shutdown drains the embedded NATS server cleanly. Blocks until the
// server has released its listeners and goroutines. Idempotent — a
// second call after a successful shutdown is a no-op.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.stopped.Swap(true) {
		return nil
	}
	if s.srv == nil {
		return nil
	}

	doneCh := make(chan struct{})
	go func() {
		s.srv.Shutdown()
		close(doneCh)
	}()

	select {
	case <-doneCh:
		s.emit(EventServerStopped, map[string]any{
			"node_id":    s.nodeID,
			"cluster_id": s.clusterID,
			"reason":     "clean_shutdown",
			"uptime_ms":  time.Since(s.startedAt).Milliseconds(),
		})
		return nil
	case <-ctx.Done():
		// Context expired while waiting for clean shutdown. The server
		// is now in an indeterminate state; emit a structured stopped
		// event with the indication and return the ctx error so the
		// caller maps to ExitDrainTimeout.
		s.emit(EventServerStopped, map[string]any{
			"node_id":    s.nodeID,
			"cluster_id": s.clusterID,
			"reason":     "forced",
			"uptime_ms":  time.Since(s.startedAt).Milliseconds(),
			"error":      ctx.Err().Error(),
		})
		return fmt.Errorf("embedded NATS shutdown timeout: %w", ctx.Err())
	}
}

// Reload re-applies the supplied options to the running server (FR-014a).
// Per RD-001a + the contracts/lifecycle.md hot-reload allow-list,
// callers MUST only mutate the route list and the cluster password
// before invoking Reload — every other field is startup-only and
// changes are silently ineffective at the NATS level.
func (s *Server) Reload(opts *server.Options) error {
	if s == nil || s.srv == nil {
		return errors.New("embedded.Server.Reload: server is not running")
	}
	return s.srv.ReloadOptions(opts)
}

// EmitReloadApplied is a helper the host layer calls after a
// successful Reload to surface the diff in the structured-log stream.
// The fields shape matches contracts/observability.md §
// `embedded_nats.reload_applied`.
func (s *Server) EmitReloadApplied(routesAdded, routesRemoved []string, passwordRotated bool, oldPwPrefix, newPwPrefix string, skippedKeys []string, skippedReason string) {
	fields := map[string]any{
		"node_id":          s.nodeID,
		"cluster_id":       s.clusterID,
		"routes_added":     routesAdded,
		"routes_removed":   routesRemoved,
		"password_rotated": passwordRotated,
	}
	if passwordRotated {
		fields["password_old_prefix"] = oldPwPrefix
		fields["password_new_prefix"] = newPwPrefix
	}
	if len(skippedKeys) > 0 {
		fields["skipped_keys"] = skippedKeys
		fields["skipped_reason"] = skippedReason
	}
	s.emit(EventReloadApplied, fields)
}

// EmitStorageDegraded surfaces a JetStream durability failure to the
// host's audit stream. Calling this is the trigger for self-fencing
// per Constitution III (host code maps the event to a leadership-
// release call against the pg-manager adapter handle).
func (s *Server) EmitStorageDegraded(kind, path, errMsg string) {
	s.emit(EventStorageDegraded, map[string]any{
		"node_id":    s.nodeID,
		"cluster_id": s.clusterID,
		"kind":       kind,
		"path":       path,
		"error":      errMsg,
	})
}

// formatAddr renders a net.Addr as host:port; safe for nil input.
func formatAddr(a interface{ String() string }) string {
	if a == nil {
		return ""
	}
	return a.String()
}
