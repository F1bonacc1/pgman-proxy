package fanout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// Handler is the per-slice responder function. It receives the
// request's args and the operator actor (for audit attribution) and
// returns the slice payload or an error.
type Handler func(ctx context.Context, args map[string]any, operatorActor string) (any, error)

// Server is the per-peer responder for fan-out requests. One instance
// per peer; subscribe once at startup, Close at shutdown.
type Server struct {
	conn       *nats.Conn
	clusterID  string
	selfNodeID string

	mu       sync.Mutex
	handlers map[Slice]Handler
	subs     []*nats.Subscription
	closed   bool
}

// NewServer constructs a Server. The caller is responsible for
// registering at least one Handler before Serve().
func NewServer(conn *nats.Conn, clusterID, selfNodeID string) *Server {
	return &Server{
		conn:       conn,
		clusterID:  clusterID,
		selfNodeID: selfNodeID,
		handlers:   make(map[Slice]Handler),
	}
}

// Register attaches a Handler for the given slice. Calling Register
// twice for the same slice replaces the previous handler.
func (s *Server) Register(slice Slice, h Handler) error {
	if !IsValidSlice(slice) {
		return fmt.Errorf("fanout: unknown slice %q", slice)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[slice] = h
	return nil
}

// Serve subscribes to the unicast + wildcard subjects for each
// registered slice. Call once after all Register() calls; subsequent
// calls are no-ops.
func (s *Server) Serve() error {
	if s.conn == nil {
		return errors.New("fanout: nil NATS connection")
	}
	if s.clusterID == "" || s.selfNodeID == "" {
		return errors.New("fanout: empty clusterID or selfNodeID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("fanout: server is closed")
	}
	if len(s.subs) > 0 {
		return nil
	}
	for slice := range s.handlers {
		for _, subj := range []string{
			ResponderUnicast(s.clusterID, slice, s.selfNodeID),
			ResponderWildcard(s.clusterID, slice),
		} {
			currentSlice := slice
			sub, err := s.conn.Subscribe(subj, func(m *nats.Msg) {
				s.handle(currentSlice, m)
			})
			if err != nil {
				return fmt.Errorf("subscribe %s: %w", subj, err)
			}
			s.subs = append(s.subs, sub)
		}
	}
	return nil
}

// Close drains every subscription. Idempotent.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	for _, sub := range s.subs {
		_ = sub.Drain()
	}
	s.subs = nil
	return nil
}

// handle is the dispatch path for one inbound fan-out request.
// All responses go through this method; we never write directly to
// m.Respond from a handler.
func (s *Server) handle(slice Slice, m *nats.Msg) {
	var req Request
	if err := json.Unmarshal(m.Data, &req); err != nil {
		_ = m.Respond(replyBytes(s.selfNodeID, req.RequestID, StatusFailed, nil, &Error{
			Code:    CodeSliceInternal,
			Message: "malformed request: " + trim(err.Error(), 256),
		}))
		return
	}
	if req.Version != 1 {
		_ = m.Respond(replyBytes(s.selfNodeID, req.RequestID, StatusFailed, nil, &Error{
			Code:    CodeSliceInternal,
			Message: fmt.Sprintf("unsupported version %d", req.Version),
		}))
		return
	}

	s.mu.Lock()
	h, ok := s.handlers[slice]
	s.mu.Unlock()
	if !ok || h == nil {
		_ = m.Respond(replyBytes(s.selfNodeID, req.RequestID, StatusFailed, nil, &Error{
			Code:    CodeSliceInternal,
			Message: "no handler for slice " + string(slice),
		}))
		return
	}

	deadline := 5 * time.Second
	if req.DeadlineMS > 0 {
		deadline = time.Duration(req.DeadlineMS) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	data, err := h(ctx, req.Args, req.OperatorActor)
	if err != nil {
		code := CodeSliceInternal
		if errors.Is(err, context.DeadlineExceeded) {
			code = CodeDeadlineExceeded
		}
		_ = m.Respond(replyBytes(s.selfNodeID, req.RequestID, StatusFailed, nil, &Error{
			Code:    code,
			Message: trim(err.Error(), 512),
		}))
		return
	}
	_ = m.Respond(replyBytes(s.selfNodeID, req.RequestID, StatusOK, data, nil))
}

func replyBytes(nodeID, reqID, status string, data any, errBlk *Error) []byte {
	r := Reply{
		Version:     1,
		RequestID:   reqID,
		NodeID:      nodeID,
		Status:      status,
		Data:        data,
		Error:       errBlk,
		RespondedAt: time.Now().UTC(),
	}
	b, _ := json.Marshal(r)
	return b
}
