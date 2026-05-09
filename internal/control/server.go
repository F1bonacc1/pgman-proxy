// HTTP server for the LCM control plane (T053 / FR-021..FR-034).
//
// Exposes:
//   * The /v1/* routes documented in contracts/lcm.md.
//   * `Server.Start(ctx)` — binds the listener, with TLS when
//     control.tls.{cert_file,key_file} are configured. Loopback binds
//     allow plaintext; non-loopback binds REQUIRE TLS unless
//     control.tls.plaintext_explicit_ack=true (FR-033). The server
//     emits the documented WARN log line on the ack path.
//   * `Server.Stop(ctx)` — graceful shutdown bounded by the supplied
//     context.
//
// Middleware order (outermost first):
//   1. panic-recovery  (translates a runtime panic into 500 + audit failed)
//   2. request-id      (allocates a ULID, attaches it to ctx + response header)
//   3. access-log      (slog access line at debug)
//   4. metrics         (in-flight gauge + per-op latency histogram)
//   5. dispatch        (route + handler)

package control

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"sync"
	"time"

	pgmanager "github.com/f1bonacc1/pg-manager"
	"github.com/oklog/ulid/v2"

	"github.com/f1bonacc1/pgman-proxy/internal/obs"
)

// Server bundles the LCM HTTP listener with its dependencies.
type Server struct {
	addr     string
	tlsCfg   *tls.Config
	plainAck bool

	auth    *Authenticator
	audit   *Audit
	router  *LeaderRouter
	engine  Engine
	logger  *obs.Logger
	metrics *obs.MetricSet

	clusterID string
	nodeID    string

	srv *http.Server
	mu  sync.Mutex // guards srv during Stop

	ulidEntropy *ulidEntropySource
}

// Config carries everything Server needs. Built by runtime/start.go.
type Config struct {
	Addr                 string // control.listen_addr
	TLSCertFile          string
	TLSKeyFile           string
	PlaintextExplicitAck bool

	Auth    *Authenticator
	Audit   *Audit
	Router  *LeaderRouter
	Engine  Engine
	Logger  *obs.Logger
	Metrics *obs.MetricSet

	ClusterID string
	NodeID    string
}

// NewServer wires a Server. Returns an error when the supplied TLS
// material cannot be parsed; loopback-vs-TLS validation is the config
// validator's job (config.Validate).
func NewServer(cfg Config) (*Server, error) {
	s := &Server{
		addr:        cfg.Addr,
		plainAck:    cfg.PlaintextExplicitAck,
		auth:        cfg.Auth,
		audit:       cfg.Audit,
		router:      cfg.Router,
		engine:      cfg.Engine,
		logger:      cfg.Logger,
		metrics:     cfg.Metrics,
		clusterID:   cfg.ClusterID,
		nodeID:      cfg.NodeID,
		ulidEntropy: newULIDEntropy(),
	}
	if cfg.TLSCertFile != "" && cfg.TLSKeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("control plane: load tls cert/key: %w", err)
		}
		s.tlsCfg = &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
		}
	}
	return s, nil
}

// Handler returns an *http.ServeMux wired with every documented route.
// Exported so tests can drive it via httptest.Server without binding a
// TCP listener.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Reads.
	mux.Handle("GET /v1/status", s.wrap("Status", false, false, s.handleStatus))
	mux.Handle("GET /v1/diagnose", s.wrap("Diagnose", false, false, s.handleDiagnose))

	// Membership mutations (leader-only).
	mux.Handle("POST /v1/switchover", s.wrap("Switchover", true, true, s.handleSwitchover))
	mux.Handle("POST /v1/failover", s.wrap("Failover", true, true, s.handleFailover))
	mux.Handle("POST /v1/fence", s.wrap("Fence", true, true, s.handleFence))
	mux.Handle("POST /v1/unfence", s.wrap("Unfence", true, true, s.handleUnfence))

	// Promote: explicitly NOT leader-only — semantics is "promote THIS node"
	// (matches manager.Manager.Promote).
	mux.Handle("POST /v1/promote", s.wrap("Promote", true, false, s.handlePromote))

	// Topology + backup + upgrade (leader-only).
	mux.Handle("POST /v1/topology", s.wrap("UpdateTopology", true, true, s.handleUpdateTopology))
	mux.Handle("POST /v1/backup", s.wrap("TriggerBackup", true, true, s.handleTriggerBackup))
	mux.Handle("POST /v1/upgrade/prepare", s.wrap("PrepareUpgrade", true, true, s.handlePrepareUpgrade))
	mux.Handle("POST /v1/upgrade/execute", s.wrap("ExecuteUpgrade", true, true, s.handleExecuteUpgrade))

	return mux
}

// Start binds the listener and serves until ctx is cancelled or Stop
// is called. Returns nil on graceful shutdown, error on bind failure
// or unexpected close.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	srv := &http.Server{
		Addr:              s.addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig:         s.tlsCfg,
	}
	s.srv = srv
	s.mu.Unlock()

	// Plaintext-ack WARN line (FR-033). Emitted only when TLS is NOT
	// configured AND the operator opted into plaintext on a non-
	// loopback bind via PlaintextExplicitAck.
	if s.tlsCfg == nil && s.plainAck {
		s.logger.Warn("control plane plaintext bind on non-loopback acknowledged",
			pgmanager.Field{Key: "listen_addr", Value: s.addr})
	}

	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("control plane listen %s: %w", s.addr, err)
	}

	go func() {
		<-ctx.Done()
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(stopCtx)
	}()

	if s.tlsCfg != nil {
		err = srv.ServeTLS(ln, "", "")
	} else {
		err = srv.Serve(ln)
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Stop tears the server down, bounded by ctx.
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	srv := s.srv
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

// PlaintextOK reports whether the server is set up to accept plaintext
// HTTP. Used by the readiness audit (FR-033 — "first audit record
// must include the listener mode").
func (s *Server) PlaintextOK() bool {
	return s.tlsCfg == nil
}

// wrap builds the HandlerFunc chain for one route. `mutating` decides
// whether the audit fail-closed gate engages (FR-028); `leaderOnly`
// decides whether leader-routing kicks in (FR-026).
func (s *Server) wrap(op string, mutating, leaderOnly bool, fn handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		reqID := s.newRequestID()
		ctx := withRequestID(r.Context(), reqID)
		// T073 trace-context: read inbound `traceparent`, surface on
		// response, store on ctx so the audit record can carry it.
		if tp := obs.ParseTraceParent(r.Header.Get("traceparent")); tp.HasTrace() {
			ctx = withTrace(ctx, tp)
			w.Header().Set("traceparent", tp.Header())
		}
		r = r.WithContext(ctx)
		w.Header().Set("X-Request-Id", reqID)

		s.metrics.LCMInFlight.WithLabelValues(op).Inc()
		defer s.metrics.LCMInFlight.WithLabelValues(op).Dec()

		// Panic-recovery — translates a runtime panic into a 500 +
		// `internal` audit record so a buggy handler never silently
		// drops a request.
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("control plane: handler panic",
					pgmanager.Field{Key: "operation", Value: op},
					pgmanager.Field{Key: "request_id", Value: reqID},
					pgmanager.Field{Key: "panic", Value: fmt.Sprintf("%v", rec)})
				s.refuse(r.Context(), w, r, op, reqID, start, "anonymous", CodeInternal,
					fmt.Sprintf("handler panic: %v", rec))
			}
		}()

		// 1. Auth (skipped on read ops when unauth is enabled).
		// Mutating ops always need auth; read ops only when
		// allow_unauth_reads is false.
		actor := "anonymous"
		needsAuth := mutating || !s.auth.AllowUnauthReads()
		if needsAuth {
			a, err := s.auth.Verify(r.Header.Get("Authorization"))
			if err != nil {
				code := CodeAuthRequired
				if errors.Is(err, ErrAuthInvalid) {
					code = CodeAuthInvalid
				}
				s.refuse(r.Context(), w, r, op, reqID, start, "anonymous", code, err.Error())
				return
			}
			actor = a
		}

		// 2. Fail-closed audit gate (FR-028) for mutating ops.
		if mutating && !s.audit.Healthy() {
			s.refuse(r.Context(), w, r, op, reqID, start, actor, CodeAuditUnavailable,
				"audit pipeline unavailable; mutating ops refused (FR-028)")
			return
		}

		// 3. Leader routing (FR-026 / FR-034).
		if leaderOnly && !s.router.IsLeader() {
			if s.router.Mode() == "redirect" {
				addr := s.router.LeaderAddr()
				if addr == "" {
					s.refuse(r.Context(), w, r, op, reqID, start, actor,
						CodeLeadershipInTransition, "no known leader; retry shortly")
					return
				}
				s.metrics.LCMLeaderRouteTotal.WithLabelValues(op, "redirected").Inc()
				w.Header().Set("Location", addr+r.URL.Path)
				http.Error(w, "", http.StatusTemporaryRedirect)
				_ = s.audit.Emit(r.Context(), s.recordAt(op, "", actor, reqID, r, start, 0,
					OutcomeRejected, CodeNotLeader, s.router.LeaderID()))
				return
			}
			// `forward` mode — handler runs the forward instead of the
			// engine. The handlerFunc signature exposes a forwarder
			// callback so each handler decides what to forward.
			s.metrics.LCMLeaderRouteTotal.WithLabelValues(op, "forwarded").Inc()
			fn(w, r, &requestEnv{
				Op:        op,
				ReqID:     reqID,
				Actor:     actor,
				Start:     start,
				Forwarded: true,
			})
			return
		}

		// Local execution path. Per contracts/observability.md the
		// disposition label MUST cover all three values: forwarded,
		// redirected, local_executed.
		if leaderOnly {
			s.metrics.LCMLeaderRouteTotal.WithLabelValues(op, "local_executed").Inc()
		}
		fn(w, r, &requestEnv{
			Op:        op,
			ReqID:     reqID,
			Actor:     actor,
			Start:     start,
			Forwarded: false,
		})
	})
}

// requestEnv aggregates per-request state every handler needs.
type requestEnv struct {
	Op        string
	ReqID     string
	Actor     string
	Start     time.Time
	Forwarded bool // true when this peer should forward to the leader instead of execute locally
}

// handlerFunc is the per-route signature.
type handlerFunc func(w http.ResponseWriter, r *http.Request, env *requestEnv)

// refuse writes a non-accepted response + audit record. Used for every
// pre-engine rejection path (auth, leader-routing, fail-closed gate,
// panic-recovery).
func (s *Server) refuse(ctx context.Context, w http.ResponseWriter, r *http.Request,
	op, reqID string, start time.Time, actor, code, msg string,
) {
	rec := s.recordAt(op, "", actor, reqID, r, start, 0, OutcomeRejected, code, "")
	_ = s.audit.Emit(ctx, rec)
	s.metrics.LCMRequestsTotal.WithLabelValues(op, OutcomeRejected).Inc()
	s.metrics.LCMRequestLatency.WithLabelValues(op, OutcomeRejected).
		Observe(time.Since(start).Seconds())
	s.writeEnvelope(w, op, reqID, OutcomeRejected, nil, &errorEnvelope{Code: code, Message: msg})
}

// writeEnvelope encodes a response envelope at the appropriate HTTP
// status. The status comes from the error code (rejected / failed) or
// 200 when accepted.
func (s *Server) writeEnvelope(w http.ResponseWriter, op, reqID, outcome string,
	engineResult any, errEnv *errorEnvelope,
) {
	status := http.StatusOK
	if errEnv != nil {
		status = httpStatusForCode(errEnv.Code)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := envelope{
		Operation:    op,
		RequestID:    reqID,
		Outcome:      outcome,
		EngineResult: engineResult,
		Error:        errEnv,
	}
	encodeJSON(w, body)
}

// recordAt assembles an AuditRecord from per-request state. `target`
// is operation-specific (e.g., the NodeID being switched to).
func (s *Server) recordAt(op, target, actor, reqID string, r *http.Request,
	start time.Time, engineLatency time.Duration, outcome, errCode, leaderAtRequest string,
) AuditRecord {
	tp := traceFromContext(r.Context())
	return AuditRecord{
		Time:            time.Now().UTC().Format(time.RFC3339Nano),
		RequestID:       reqID,
		Operation:       op,
		Target:          target,
		Actor:           actor,
		SourceAddr:      r.RemoteAddr,
		Outcome:         outcome,
		EngineLatencyMS: engineLatency.Milliseconds(),
		TotalLatencyMS:  time.Since(start).Milliseconds(),
		ErrorCode:       errCode,
		ClusterID:       s.clusterID,
		NodeID:          s.nodeID,
		LeaderAtRequest: leaderAtRequest,
		TraceID:         tp.TraceID,
		SpanID:          tp.SpanID,
	}
}

// completeOK reports an accepted engine call.
func (s *Server) completeOK(ctx context.Context, w http.ResponseWriter, r *http.Request, env *requestEnv,
	target string, engineResult any, engineLatency time.Duration,
) {
	rec := s.recordAt(env.Op, target, env.Actor, env.ReqID, r, env.Start, engineLatency,
		OutcomeAccepted, "", "")
	_ = s.audit.Emit(ctx, rec)
	s.metrics.LCMRequestsTotal.WithLabelValues(env.Op, OutcomeAccepted).Inc()
	s.metrics.LCMRequestLatency.WithLabelValues(env.Op, OutcomeAccepted).
		Observe(time.Since(env.Start).Seconds())
	s.metrics.LCMEngineLatency.WithLabelValues(env.Op, OutcomeAccepted).
		Observe(engineLatency.Seconds())
	s.writeEnvelope(w, env.Op, env.ReqID, OutcomeAccepted, engineResult, nil)
}

// completeFail reports an engine error.
func (s *Server) completeFail(ctx context.Context, w http.ResponseWriter, r *http.Request, env *requestEnv,
	target, code, message string, engineLatency time.Duration, leaderAt string,
) {
	rec := s.recordAt(env.Op, target, env.Actor, env.ReqID, r, env.Start, engineLatency,
		OutcomeFailed, code, leaderAt)
	_ = s.audit.Emit(ctx, rec)
	s.metrics.LCMRequestsTotal.WithLabelValues(env.Op, OutcomeFailed).Inc()
	s.metrics.LCMRequestLatency.WithLabelValues(env.Op, OutcomeFailed).
		Observe(time.Since(env.Start).Seconds())
	s.metrics.LCMEngineLatency.WithLabelValues(env.Op, OutcomeFailed).
		Observe(engineLatency.Seconds())
	s.writeEnvelope(w, env.Op, env.ReqID, OutcomeFailed,
		nil, &errorEnvelope{Code: code, Message: message})
}

// rejectInvalid encodes a 400 invalid_argument response + audit. Used
// when the request body fails to decode or carries an unknown enum
// value.
func (s *Server) rejectInvalid(ctx context.Context, w http.ResponseWriter, r *http.Request,
	env *requestEnv, message string,
) {
	s.refuse(ctx, w, r, env.Op, env.ReqID, env.Start, env.Actor, CodeInvalidArgument, message)
}

// newRequestID returns a fresh ULID for the request. Monotonic
// entropy under sync.Mutex so concurrent requests still produce
// strictly-increasing IDs (within a millisecond).
func (s *Server) newRequestID() string {
	return s.ulidEntropy.New().String()
}

// withRequestID stores reqID in ctx so downstream middleware/handlers
// can recover it for log correlation.
type ridCtxKey struct{}
type traceCtxKey struct{}

func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ridCtxKey{}, id)
}

// withTrace stores the parsed trace-context on ctx so audit records
// can copy it into the `trace_id` / `span_id` fields.
func withTrace(ctx context.Context, tp obs.TraceContext) context.Context {
	return context.WithValue(ctx, traceCtxKey{}, tp)
}

// traceFromContext returns the stored trace-context, or the zero value.
func traceFromContext(ctx context.Context) obs.TraceContext {
	v, _ := ctx.Value(traceCtxKey{}).(obs.TraceContext)
	return v
}

// reqIDFromContext returns the request-ID stored in ctx, or "" if none.
func reqIDFromContext(ctx context.Context) string { //nolint:unused // utility for handlers
	v, _ := ctx.Value(ridCtxKey{}).(string)
	return v
}

// ulidEntropySource is a goroutine-safe ULID generator. ulid.Monotonic
// is NOT thread-safe so we wrap it.
type ulidEntropySource struct {
	mu  sync.Mutex
	src *ulid.MonotonicEntropy
}

func newULIDEntropy() *ulidEntropySource {
	//nolint:gosec // ULID entropy doesn't need a CSPRNG; monotonic is enough.
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	return &ulidEntropySource{src: ulid.Monotonic(r, 0)}
}

func (u *ulidEntropySource) New() ulid.ULID {
	u.mu.Lock()
	defer u.mu.Unlock()
	return ulid.MustNew(ulid.Timestamp(time.Now()), u.src)
}
