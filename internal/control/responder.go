// Leader-side responder for forwarded LCM requests (FR-026 / FR-034).
//
// Counterpart to LeaderRouter.Forward. Every peer subscribes once to the
// wildcard subject `pgman_proxy.<cluster>.lcm.request.>`. The callback
// gates on LeaderState.IsLeader(): non-leaders return silently (the
// forwarder's leader_route_timeout governs), the leader dispatches the
// op against the local engine and replies with the LCM envelope.
//
// Without this responder, a forwarded request returns
// `nats: no responders available for request` immediately.
//
// Subjects this file reads:
//
//	pgman_proxy.<cluster_id>.lcm.request.<operation>
//
// Subjects this file writes: the request's reply inbox (handled by
// nats.Msg.Respond — no explicit subject naming).

package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	pgmanager "github.com/f1bonacc1/pg-manager"
	"github.com/nats-io/nats.go"

	"github.com/f1bonacc1/pgman-proxy/internal/obs"
)

// responderActorFallback is the actor stamped on the leader's audit
// record when the forwarder did not propagate `X-Actor` (older publisher
// during a rolling restart).
const responderActorFallback = "leader-route-forward"

// responderMaxParallel caps concurrent in-flight engine calls per peer.
// NATS dispatches subscription callbacks on a single goroutine, so each
// accepted message MUST be handed off; the cap bounds resource use under
// a storm.
const responderMaxParallel = 8

// LeaderRouteResponder is the per-peer subscriber for forwarded LCM
// requests. Construct via NewLeaderRouteResponder; Serve once at
// startup; Close on shutdown. Safe for concurrent use; Close is
// idempotent.
type LeaderRouteResponder struct {
	conn      *nats.Conn
	clusterID string
	nodeID    string
	engine    Engine
	router    *LeaderRouter
	audit     *Audit
	metrics   *obs.MetricSet
	logger    *obs.Logger
	timeout   time.Duration

	mu     sync.Mutex
	sub    *nats.Subscription
	closed bool

	// inFlight bounds concurrent engine dispatches.
	inFlight chan struct{}
	wg       sync.WaitGroup

	// ulidEntropy supplies request IDs when the forwarder omitted one.
	ulidEntropy *ulidEntropySource
}

// NewLeaderRouteResponder constructs a responder. None of the
// dependencies may be nil except `conn` in test paths that exercise the
// no-op Serve guard.
func NewLeaderRouteResponder(
	conn *nats.Conn,
	clusterID, nodeID string,
	engine Engine,
	router *LeaderRouter,
	audit *Audit,
	metrics *obs.MetricSet,
	logger *obs.Logger,
	timeout time.Duration,
) *LeaderRouteResponder {
	return &LeaderRouteResponder{
		conn:        conn,
		clusterID:   clusterID,
		nodeID:      nodeID,
		engine:      engine,
		router:      router,
		audit:       audit,
		metrics:     metrics,
		logger:      logger,
		timeout:     timeout,
		inFlight:    make(chan struct{}, responderMaxParallel),
		ulidEntropy: newULIDEntropy(),
	}
}

// Serve subscribes to the wildcard request subject. Idempotent;
// subsequent calls are no-ops.
func (r *LeaderRouteResponder) Serve() error {
	if r.conn == nil {
		return errors.New("leader-route responder: nil NATS connection")
	}
	if r.clusterID == "" {
		return errors.New("leader-route responder: empty clusterID")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("leader-route responder: already closed")
	}
	if r.sub != nil {
		return nil
	}
	subject := fmt.Sprintf("pgman_proxy.%s.lcm.request.>", r.clusterID)
	sub, err := r.conn.Subscribe(subject, r.dispatch)
	if err != nil {
		return fmt.Errorf("subscribe %q: %w", subject, err)
	}
	r.sub = sub
	return nil
}

// Close drains the subscription and waits for in-flight dispatches to
// finish. Idempotent.
func (r *LeaderRouteResponder) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	sub := r.sub
	r.sub = nil
	r.mu.Unlock()
	if sub != nil {
		_ = sub.Drain()
	}
	r.wg.Wait()
	return nil
}

// dispatch is the NATS subscription callback. It runs on the
// subscription's single goroutine; engine work is handed off to a
// worker so concurrent requests don't serialize.
func (r *LeaderRouteResponder) dispatch(m *nats.Msg) {
	op := opFromSubject(m.Subject)
	if op == "" {
		return
	}
	if !r.router.IsLeader() {
		// Non-leader: stay silent so the forwarder's leader_route_timeout
		// governs the wait. Replying with `not_leader` here would race
		// leadership transitions and could mask a leader that just
		// acquired the role.
		return
	}
	if !isKnownLeaderOnlyOp(op) {
		// Unknown op (future or typo): drop silently. Forward-compat.
		return
	}
	r.inFlight <- struct{}{}
	r.wg.Go(func() {
		defer func() { <-r.inFlight }()
		r.handle(op, m)
	})
}

// handle runs one forwarded op end-to-end: parse body, dispatch to
// engine, audit, reply.
func (r *LeaderRouteResponder) handle(op string, m *nats.Msg) {
	start := time.Now()
	reqID, actor, traceparent := r.headersOrDefaults(m)

	// FR-028 fail-closed gate. Every leader-only op is mutating.
	if !r.audit.Healthy() {
		rec := r.makeRecord(op, "", actor, reqID, traceparent, start, 0,
			OutcomeRejected, CodeAuditUnavailable)
		_ = r.audit.Emit(context.Background(), rec)
		r.bumpMetrics(op, OutcomeRejected, start, 0)
		r.respondEnvelope(m, op, reqID, OutcomeRejected, nil,
			&errorEnvelope{Code: CodeAuditUnavailable,
				Message: "audit pipeline unavailable; mutating ops refused (FR-028)"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()

	engineStart := time.Now()
	result, target, code, message, err := r.dispatchEngine(ctx, op, m.Data)
	engineLatency := time.Since(engineStart)

	if err != nil {
		outcome := OutcomeFailed
		if code == CodeInvalidArgument {
			outcome = OutcomeRejected
		}
		rec := r.makeRecord(op, target, actor, reqID, traceparent, start, engineLatency, outcome, code)
		_ = r.audit.Emit(ctx, rec)
		r.bumpMetrics(op, outcome, start, engineLatency)
		r.respondEnvelope(m, op, reqID, outcome, nil,
			&errorEnvelope{Code: code, Message: message})
		return
	}

	rec := r.makeRecord(op, target, actor, reqID, traceparent, start, engineLatency, OutcomeAccepted, "")
	_ = r.audit.Emit(ctx, rec)
	r.bumpMetrics(op, OutcomeAccepted, start, engineLatency)
	r.respondEnvelope(m, op, reqID, OutcomeAccepted, result, nil)
}

// dispatchEngine maps an op name to the corresponding Engine call.
// Returns (engineResult, target, errorCode, message, err) — when err is
// nil the call succeeded and code/message are empty.
func (r *LeaderRouteResponder) dispatchEngine(ctx context.Context, op string, body []byte) (any, string, string, string, error) {
	switch op {
	case "Failover":
		if err := r.engine.Failover(ctx); err != nil {
			return nil, "", CodeEngineError, err.Error(), err
		}
		return nil, "", "", "", nil

	case "Switchover":
		var b switchoverReq
		if err := json.Unmarshal(body, &b); err != nil {
			return nil, "", CodeInvalidArgument, "decode body: " + err.Error(), err
		}
		if b.Target == "" {
			return nil, "", CodeInvalidArgument, "target is required", errors.New("target is required")
		}
		if err := r.engine.Switchover(ctx, pgmanager.NodeID(b.Target)); err != nil {
			return nil, b.Target, CodeEngineError, err.Error(), err
		}
		return nil, b.Target, "", "", nil

	case "Fence":
		var b fenceReq
		if err := json.Unmarshal(body, &b); err != nil {
			return nil, "", CodeInvalidArgument, "decode body: " + err.Error(), err
		}
		if b.Target == "" {
			return nil, "", CodeInvalidArgument, "target is required", errors.New("target is required")
		}
		if err := r.engine.Fence(ctx, pgmanager.NodeID(b.Target)); err != nil {
			return nil, b.Target, CodeEngineError, err.Error(), err
		}
		return nil, b.Target, "", "", nil

	case "Unfence":
		var b fenceReq
		if err := json.Unmarshal(body, &b); err != nil {
			return nil, "", CodeInvalidArgument, "decode body: " + err.Error(), err
		}
		if b.Target == "" {
			return nil, "", CodeInvalidArgument, "target is required", errors.New("target is required")
		}
		if err := r.engine.Unfence(ctx, pgmanager.NodeID(b.Target)); err != nil {
			return nil, b.Target, CodeEngineError, err.Error(), err
		}
		return nil, b.Target, "", "", nil

	case "UpdateTopology":
		var b updateTopologyReq
		if err := json.Unmarshal(body, &b); err != nil {
			return nil, "", CodeInvalidArgument, "decode body: " + err.Error(), err
		}
		target := string(b.Topology.NodeID)
		if err := r.engine.UpdateTopology(ctx, b.Topology, b.Policy); err != nil {
			return nil, target, CodeEngineError, err.Error(), err
		}
		return nil, target, "", "", nil

	case "TriggerBackup":
		id, err := r.engine.TriggerBackup(ctx)
		if err != nil {
			if errors.Is(err, pgmanager.ErrBackupNotConfigured) {
				return nil, "", CodeBackupExecutorMissing,
					"no BackupExecutor wired; configure backup.driver per FR-030", err
			}
			return nil, "", CodeEngineError, err.Error(), err
		}
		return triggerBackupResp{BackupID: string(id)}, "", "", "", nil

	case "PrepareUpgrade":
		var b upgradeReq
		if err := json.Unmarshal(body, &b); err != nil {
			return nil, "", CodeInvalidArgument, "decode body: " + err.Error(), err
		}
		if err := r.engine.PrepareUpgrade(ctx, b.Plan); err != nil {
			return nil, "", CodeEngineError, err.Error(), err
		}
		return nil, "", "", "", nil

	case "ExecuteUpgrade":
		var b upgradeReq
		if err := json.Unmarshal(body, &b); err != nil {
			return nil, "", CodeInvalidArgument, "decode body: " + err.Error(), err
		}
		preSwap := func(_ context.Context) error { return nil }
		if err := r.engine.ExecuteUpgrade(ctx, b.Plan, preSwap); err != nil {
			return nil, "", CodeEngineError, err.Error(), err
		}
		return nil, "", "", "", nil
	}
	// dispatch() already gates on isKnownLeaderOnlyOp so this is
	// defensive; an unknown op would have been dropped silently before
	// reaching here.
	return nil, "", CodeInternal, "unknown op: " + op, fmt.Errorf("unknown op %q", op)
}

// headersOrDefaults extracts X-Request-Id / X-Actor / traceparent from
// the inbound message. Falls back to a fresh ULID + sentinel actor
// when the forwarder didn't propagate headers (older publishers).
func (r *LeaderRouteResponder) headersOrDefaults(m *nats.Msg) (reqID, actor, traceparent string) {
	if m.Header != nil {
		reqID = m.Header.Get("X-Request-Id")
		actor = m.Header.Get("X-Actor")
		traceparent = m.Header.Get("traceparent")
	}
	if reqID == "" {
		reqID = r.ulidEntropy.New().String()
	}
	if actor == "" {
		actor = responderActorFallback
	}
	return
}

// makeRecord assembles an AuditRecord for the leader's audit emit.
// Mirrors server.recordAt without an *http.Request.
func (r *LeaderRouteResponder) makeRecord(op, target, actor, reqID, traceparent string,
	start time.Time, engineLatency time.Duration, outcome, errCode string,
) AuditRecord {
	tp := obs.ParseTraceParent(traceparent)
	return AuditRecord{
		Time:            time.Now().UTC().Format(time.RFC3339Nano),
		RequestID:       reqID,
		Operation:       op,
		Target:          target,
		Actor:           actor,
		SourceAddr:      "", // forwarded via NATS — no client IP visible on leader
		Outcome:         outcome,
		EngineLatencyMS: engineLatency.Milliseconds(),
		TotalLatencyMS:  time.Since(start).Milliseconds(),
		ErrorCode:       errCode,
		ClusterID:       r.clusterID,
		NodeID:          r.nodeID,
		TraceID:         tp.TraceID,
		SpanID:          tp.SpanID,
		// LeaderAtRequest stays empty: we ARE the leader for this record.
	}
}

// bumpMetrics mirrors the metric writes from completeOK / completeFail
// so the leader's view of LCM requests is symmetric with the
// HTTP-served path.
func (r *LeaderRouteResponder) bumpMetrics(op, outcome string, start time.Time, engineLatency time.Duration) {
	r.metrics.LCMRequestsTotal.WithLabelValues(op, outcome).Inc()
	r.metrics.LCMRequestLatency.WithLabelValues(op, outcome).Observe(time.Since(start).Seconds())
	r.metrics.LCMEngineLatency.WithLabelValues(op, outcome).Observe(engineLatency.Seconds())
	r.metrics.LCMLeaderResponderTotal.WithLabelValues(op, outcome).Inc()
}

// respondEnvelope marshals the LCM envelope and replies on the NATS
// inbox carried by the request message.
func (r *LeaderRouteResponder) respondEnvelope(m *nats.Msg, op, reqID, outcome string,
	engineResult any, errEnv *errorEnvelope,
) {
	body := envelope{
		Operation:    op,
		RequestID:    reqID,
		Outcome:      outcome,
		EngineResult: engineResult,
		Error:        errEnv,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		// Defensive: the envelope contents we build here are always
		// marshallable, so a failure here means a programming bug.
		r.logger.Error("leader-route responder: envelope marshal failed",
			pgmanager.Field{Key: "operation", Value: op},
			pgmanager.Field{Key: "request_id", Value: reqID},
			pgmanager.Field{Key: "error", Value: err.Error()})
		return
	}
	if err := m.Respond(payload); err != nil {
		r.logger.Error("leader-route responder: respond failed",
			pgmanager.Field{Key: "operation", Value: op},
			pgmanager.Field{Key: "request_id", Value: reqID},
			pgmanager.Field{Key: "error", Value: err.Error()})
	}
}

// opFromSubject returns the trailing op token from a subject of the
// form `pgman_proxy.<cluster>.lcm.request.<Op>`. Returns "" when the
// subject doesn't match the expected shape.
func opFromSubject(subject string) string {
	idx := strings.LastIndexByte(subject, '.')
	if idx < 0 || idx == len(subject)-1 {
		return ""
	}
	return subject[idx+1:]
}

// isKnownLeaderOnlyOp returns true for the ops the responder dispatches.
// Future ops added to the LCM contract must be added here to be
// reachable via the forward path.
func isKnownLeaderOnlyOp(op string) bool {
	switch op {
	case "Failover", "Switchover", "Fence", "Unfence",
		"UpdateTopology", "TriggerBackup", "PrepareUpgrade", "ExecuteUpgrade":
		return true
	}
	return false
}
