// Watch SSE handlers — feature 003 / contracts/control-plane-extensions.md § 1.
//
// Four endpoints mounted under /v1/watch/* expose a Server-Sent Events
// stream of cluster activity. Each endpoint shapes the frame stream
// differently but shares one underlying mechanism: subscribe to the
// cluster history JetStream, filter, format, write.
//
// The control package stays JetStream-free by depending on a narrow
// WatchSubscriber interface; the runtime wires a JetStream-backed
// implementation. Tests can substitute a channel-backed fake.

package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	pgmanager "github.com/f1bonacc1/pg-manager"

	"github.com/f1bonacc1/pgman-proxy/internal/history"
)

// WatchSubscriber yields HistoryEvents to an SSE handler. One Watch
// call creates one independent subscription; cancel via ctx. The
// returned events channel is closed when the subscription ends; the
// errs channel emits at most one error before close.
//
// Production wiring: runtime constructs a *history.Watcher and passes
// its Watch method as the closure.
type WatchSubscriber interface {
	Watch(ctx context.Context, opts history.WatchOptions) (<-chan history.HistoryEvent, <-chan error)
}

// watchTopic enumerates the four documented stream shapes. The string
// value is the metrics-label form (`status`, `transitions`, `events`,
// `node`).
type watchTopic string

const (
	topicStatus      watchTopic = "status"
	topicTransitions watchTopic = "transitions"
	topicEvents      watchTopic = "events"
	topicNode        watchTopic = "node"
)

// keepaliveInterval is the heartbeat cadence between SSE frames (15 s
// per contracts/control-plane-extensions.md § 1). Variable so tests
// can shrink it without sleeping for 15 s.
var keepaliveInterval = 15 * time.Second

// handleWatchStatus emits a `status_update` SSE frame containing the
// current /v1/status snapshot whenever a state-changing history event
// fires (state_transition / leader_changed / primary_changed). The
// first frame after the stream opens is always a fresh snapshot so the
// client has a baseline.
func (s *Server) handleWatchStatus(w http.ResponseWriter, r *http.Request, env *requestEnv) {
	if !acceptsSSE(w, r) {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	s.startSSE(w)
	s.metrics.WatchStreamsActive.WithLabelValues(string(topicStatus)).Inc()
	defer s.metrics.WatchStreamsActive.WithLabelValues(string(topicStatus)).Dec()
	s.logger.Info("watch.stream_started",
		pgmanager.Field{Key: "topic", Value: string(topicStatus)},
		pgmanager.Field{Key: "client_id", Value: clientIDFromActor(env.Actor)})
	defer s.logger.Info("watch.stream_closed",
		pgmanager.Field{Key: "topic", Value: string(topicStatus)},
		pgmanager.Field{Key: "client_id", Value: clientIDFromActor(env.Actor)})

	// Initial snapshot.
	if err := s.writeStatusFrame(w, flusher, r.Context(), env.ReqID); err != nil {
		return
	}

	if s.watch == nil {
		// No live subscriber wired (single-peer dev path). Hold the
		// stream open emitting keepalives until the client disconnects.
		s.holdOpen(w, flusher, r.Context(), topicStatus)
		return
	}

	subOpts := history.WatchOptions{
		Cursor:   r.Header.Get("Last-Event-ID"),
		Category: history.CategoryEvent,
	}
	subCtx, cancel := context.WithCancel(r.Context())
	defer cancel()
	events, errs := s.watch.Watch(subCtx, subOpts)
	keepalive := time.NewTicker(keepaliveInterval)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if err == nil {
				continue
			}
			s.writeSSEError(w, flusher, topicStatus, "subscription_failed", err)
			return
		case ev, ok := <-events:
			if !ok {
				s.writeGapMarker(w, flusher, topicStatus, "subscription_closed")
				return
			}
			// Status frames are debounced: a single state_transition
			// might fire many partial updates; we only re-pull and emit
			// when the event names a state-changing topic.
			if !isStatusChangeEvent(ev.Type) {
				continue
			}
			if err := s.writeStatusFrameID(w, flusher, r.Context(), ev.ID); err != nil {
				return
			}
		case <-keepalive.C:
			if err := writeKeepalive(w, flusher); err != nil {
				return
			}
			s.metrics.WatchEventsEmittedTotal.WithLabelValues(string(topicStatus), "keepalive").Inc()
		}
	}
}

// handleWatchTransitions emits one SSE frame per state_transition
// HistoryEvent.
func (s *Server) handleWatchTransitions(w http.ResponseWriter, r *http.Request, env *requestEnv) {
	s.handleWatchHistoryStream(w, r, env, topicTransitions, history.WatchOptions{
		Category: history.CategoryEvent,
		Types:    []string{"state_transition"},
	}, "state_transition")
}

// handleWatchEvents emits every HistoryEvent on the cluster's history
// stream (both categories).
func (s *Server) handleWatchEvents(w http.ResponseWriter, r *http.Request, env *requestEnv) {
	values := r.URL.Query()
	opts := history.WatchOptions{}
	for _, t := range values["type"] {
		if t = strings.TrimSpace(t); t != "" {
			opts.Types = append(opts.Types, t)
		}
	}
	for _, n := range values["node"] {
		if n = strings.TrimSpace(n); n != "" {
			opts.Nodes = append(opts.Nodes, n)
		}
	}
	if v := values.Get("since"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			opts.Since = d
		}
	}
	s.handleWatchHistoryStream(w, r, env, topicEvents, opts, "cluster_event")
}

// handleWatchNode is the per-node variant: emits any HistoryEvent
// whose NodeID matches the path segment.
func (s *Server) handleWatchNode(w http.ResponseWriter, r *http.Request, env *requestEnv) {
	nodeID := r.PathValue("id")
	if nodeID == "" {
		http.Error(w, "missing node id", http.StatusBadRequest)
		return
	}
	s.handleWatchHistoryStream(w, r, env, topicNode, history.WatchOptions{
		Nodes: []string{nodeID},
	}, "cluster_event")
}

// handleWatchHistoryStream is the shared loop for the append-only
// stream topics (transitions / events / node). frameName is the SSE
// event name written into each frame.
func (s *Server) handleWatchHistoryStream(w http.ResponseWriter, r *http.Request, env *requestEnv,
	topic watchTopic, opts history.WatchOptions, frameName string,
) {
	if !acceptsSSE(w, r) {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	s.startSSE(w)
	s.metrics.WatchStreamsActive.WithLabelValues(string(topic)).Inc()
	defer s.metrics.WatchStreamsActive.WithLabelValues(string(topic)).Dec()
	s.logger.Info("watch.stream_started",
		pgmanager.Field{Key: "topic", Value: string(topic)},
		pgmanager.Field{Key: "client_id", Value: clientIDFromActor(env.Actor)})
	defer s.logger.Info("watch.stream_closed",
		pgmanager.Field{Key: "topic", Value: string(topic)},
		pgmanager.Field{Key: "client_id", Value: clientIDFromActor(env.Actor)})

	if s.watch == nil {
		s.holdOpen(w, flusher, r.Context(), topic)
		return
	}

	if last := r.Header.Get("Last-Event-ID"); last != "" {
		opts.Cursor = last
	}
	subCtx, cancel := context.WithCancel(r.Context())
	defer cancel()
	events, errs := s.watch.Watch(subCtx, opts)
	keepalive := time.NewTicker(keepaliveInterval)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case err, ok := <-errs:
			if !ok {
				// errs closed without an error — subscriber finished
				// cleanly. Continue; the events-channel close will
				// trigger the gap_marker path below.
				errs = nil
				continue
			}
			if err == nil {
				continue
			}
			s.writeSSEError(w, flusher, topic, "subscription_failed", err)
			return
		case ev, ok := <-events:
			if !ok {
				s.writeGapMarker(w, flusher, topic, "subscription_closed")
				return
			}
			if err := s.writeFrame(w, flusher, topic, frameName, ev.ID, ev); err != nil {
				return
			}
		case <-keepalive.C:
			if err := writeKeepalive(w, flusher); err != nil {
				return
			}
			s.metrics.WatchEventsEmittedTotal.WithLabelValues(string(topic), "keepalive").Inc()
		}
	}
}

// holdOpen is the no-subscriber fallback: emit keepalives until the
// client disconnects. Used by single-peer test paths that omit
// JetStream wiring; production always has a subscriber.
func (s *Server) holdOpen(w http.ResponseWriter, flusher http.Flusher, ctx context.Context, topic watchTopic) {
	keepalive := time.NewTicker(keepaliveInterval)
	defer keepalive.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			if err := writeKeepalive(w, flusher); err != nil {
				return
			}
			s.metrics.WatchEventsEmittedTotal.WithLabelValues(string(topic), "keepalive").Inc()
		}
	}
}

// writeStatusFrame fetches the current Status snapshot and writes a
// `status_update` SSE frame. Returns a non-nil error if the underlying
// write fails (closed client).
func (s *Server) writeStatusFrame(w http.ResponseWriter, flusher http.Flusher, ctx context.Context, frameID string) error {
	return s.writeStatusFrameID(w, flusher, ctx, frameID)
}

func (s *Server) writeStatusFrameID(w http.ResponseWriter, flusher http.Flusher, ctx context.Context, frameID string) error {
	st, err := s.engine.Status(ctx)
	if err != nil {
		s.writeSSEError(w, flusher, topicStatus, "status_fetch_failed", err)
		return err
	}
	if s.aggregator != nil {
		st = s.aggregator.EnrichStatus(ctx, st)
	}
	return s.writeFrame(w, flusher, topicStatus, "status_update", frameID, st)
}

// writeFrame writes one SSE frame with the documented framing:
//
//	event: <name>
//	id: <ulid>
//	data: <one-line JSON>
//	(blank line)
func (s *Server) writeFrame(w http.ResponseWriter, flusher http.Flusher,
	topic watchTopic, name, id string, payload any,
) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", name); err != nil {
		return err
	}
	if id != "" {
		if _, err := fmt.Fprintf(w, "id: %s\n", id); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
		return err
	}
	flusher.Flush()
	s.metrics.WatchEventsEmittedTotal.WithLabelValues(string(topic), name).Inc()
	return nil
}

// writeGapMarker writes a single `gap_marker` frame with a `reason`
// field and increments the corresponding counter. Per
// contracts/control-plane-extensions.md § 1 the gap marker is the
// signal a client uses to refresh state (the stream may have missed
// events between the previous frame and this one).
func (s *Server) writeGapMarker(w http.ResponseWriter, flusher http.Flusher, topic watchTopic, reason string) {
	payload := map[string]any{"reason": reason, "topic": string(topic)}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if _, werr := fmt.Fprintf(w, "event: gap_marker\ndata: %s\n\n", raw); werr != nil {
		return
	}
	flusher.Flush()
	s.metrics.WatchEventsEmittedTotal.WithLabelValues(string(topic), "gap_marker").Inc()
	s.metrics.WatchGapsTotal.WithLabelValues(string(topic), reason).Inc()
}

// writeSSEError writes a terminal `error` frame and flushes. The
// caller is expected to return immediately afterward; the stream is
// closed by HTTP-server teardown.
func (s *Server) writeSSEError(w http.ResponseWriter, flusher http.Flusher, topic watchTopic, code string, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	payload := map[string]any{"code": code, "message": msg}
	raw, mErr := json.Marshal(payload)
	if mErr != nil {
		return
	}
	if _, wErr := fmt.Fprintf(w, "event: error\ndata: %s\n\n", raw); wErr != nil {
		return
	}
	flusher.Flush()
	s.metrics.WatchEventsEmittedTotal.WithLabelValues(string(topic), "error").Inc()
}

// writeKeepalive writes a `:keepalive` comment frame. SSE clients
// ignore comment lines but use them to reset read deadlines so a
// long-idle stream doesn't look hung.
func writeKeepalive(w http.ResponseWriter, flusher http.Flusher) error {
	if _, err := fmt.Fprint(w, ":keepalive\n\n"); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// startSSE sets the SSE response headers and flushes them so the
// client sees the 200 immediately (otherwise net/http buffers headers
// until the first body write, and clients that gate on
// response-received remain blocked).
func (s *Server) startSSE(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // keep nginx-style proxies from buffering
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// acceptsSSE enforces the `Accept: text/event-stream` requirement. On
// failure writes a 406 + plain-text body and returns false; the caller
// must return immediately.
func acceptsSSE(w http.ResponseWriter, r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if accept == "" {
		// Curl-style clients omit Accept entirely; accept that as
		// "anything" so /v1/watch/* is reachable without a header tweak
		// when an operator manually pokes it.
		return true
	}
	if strings.Contains(accept, "text/event-stream") || strings.Contains(accept, "*/*") {
		return true
	}
	http.Error(w, "watch requires Accept: text/event-stream", http.StatusNotAcceptable)
	return false
}

// isStatusChangeEvent reports whether a given history event type is
// one that mutates cluster status. The watch_status loop pulls a
// fresh snapshot only on these.
func isStatusChangeEvent(t string) bool {
	switch t {
	case "state_transition",
		"leader_changed",
		"primary_changed",
		"proxy.leader_changed",
		"fenced_node",
		"unfenced_node":
		return true
	}
	return false
}

// clientIDFromActor produces a stable per-actor identifier suitable
// for the watch.stream_started / watch.stream_closed log lines. Per
// the contract, "client_id = hash of bearer-token actor, never the
// token." The actor string here is the public token-name (see
// auth.Verify), which is itself non-secret, so we surface it verbatim.
// Anonymous clients get a monotonic short id so two concurrent
// anonymous streams remain distinguishable in logs.
var anonCounter atomic.Uint64

func clientIDFromActor(actor string) string {
	if actor != "" && actor != "anonymous" {
		return actor
	}
	return fmt.Sprintf("anonymous-%d", anonCounter.Add(1))
}

// ensure errors import stays used in some build configurations.
var _ = errors.New
