package obs

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Health tracks the three readiness signals required by
// contracts/lifecycle.md § Health-endpoint state machine:
// NATS up, listener up, manager past singleton claim. Liveness
// (`/healthz`) returns 200 once the process is past arg parsing.
type Health struct {
	natsUp       atomic.Bool
	listenerUp   atomic.Bool
	managerReady atomic.Bool
}

// NewHealth returns a fresh Health instance with all three signals false.
func NewHealth() *Health { return &Health{} }

// SetNATSUp marks the NATS connection healthy/unhealthy.
func (h *Health) SetNATSUp(v bool) { h.natsUp.Store(v) }

// SetListenerUp marks the data-plane listener accepting/closed.
func (h *Health) SetListenerUp(v bool) { h.listenerUp.Store(v) }

// SetManagerReady marks the manager past its singleton-claim phase.
func (h *Health) SetManagerReady(v bool) { h.managerReady.Store(v) }

// Ready returns true only when all three signals are healthy.
func (h *Health) Ready() bool {
	return h.natsUp.Load() && h.listenerUp.Load() && h.managerReady.Load()
}

// Server is the HTTP server hosting /healthz, /readyz, /metrics. The
// instance is bound at startup gate #3 (contracts/lifecycle.md) and
// torn down during shutdown.
type Server struct {
	addr   string
	srv    *http.Server
	health *Health
	reg    prometheus.Gatherer
}

// NewServer constructs an HTTP server. The caller is responsible for
// invoking Start in a goroutine and Stop on shutdown.
func NewServer(addr string, h *Health, reg prometheus.Gatherer) *Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", traceparent(func(w http.ResponseWriter, _ *http.Request) {
		// Liveness: 200 once init is past arg parsing — i.e., always once
		// this server is up.
		_, _ = w.Write([]byte("ok\n"))
	}))
	mux.HandleFunc("/readyz", traceparent(func(w http.ResponseWriter, _ *http.Request) {
		if h.Ready() {
			_, _ = w.Write([]byte("ready\n"))
			return
		}
		w.Header().Set("Retry-After", "1")
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	mux.Handle("/metrics", traceparent(promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError,
	}).ServeHTTP))

	return &Server{
		addr:   addr,
		health: h,
		reg:    reg,
		srv: &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		},
	}
}

// Start binds the listen address and serves until ctx is cancelled or
// Stop is called. Returns nil on graceful shutdown, error on bind
// failure or unexpected close.
func (s *Server) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		// The parent ctx is already cancelled here, so it cannot drive
		// Shutdown's grace window. Bound the wait to 5s — obs requests
		// are tiny (curl /metrics) and should never need longer.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(stopCtx)
	}()
	if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Stop tears the server down, bounded by the supplied context.
func (s *Server) Stop(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

// traceparent wraps an http.HandlerFunc to read the inbound
// `traceparent` header and echo it on the response so chained
// observability hops can correlate. This is the minimum viable T073
// wiring for the obs surface; the control plane has its own copy
// because it also stores the trace fields on the audit record.
func traceparent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if tp := r.Header.Get("traceparent"); tp != "" {
			w.Header().Set("traceparent", tp)
		}
		next(w, r)
	}
}
