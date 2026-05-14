package fanout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/oklog/ulid/v2"
)

// Client publishes fan-out requests and aggregates per-sibling
// replies. One Client per pgman-proxy peer; safe to reuse across many
// concurrent requests.
type Client struct {
	conn      *nats.Conn
	clusterID string
}

// NewClient constructs a Client.
func NewClient(conn *nats.Conn, clusterID string) *Client {
	return &Client{conn: conn, clusterID: clusterID}
}

// CallOptions controls one fan-out invocation.
type CallOptions struct {
	// PerSliceTimeout is the per-call deadline applied by the
	// originator. Defaults to 5s.
	PerSliceTimeout time.Duration

	// ExpectedRespondents is the number of replies we expect.
	// Synthesized `sibling_unreachable` entries are added for any
	// missing nodes when ExpectedNodes is non-empty.
	ExpectedNodes []string

	// OperatorActor is the bearer-token-derived actor identifier
	// recorded in every sibling's audit log per
	// contracts/fanout-protocol.md § Authorization.
	OperatorActor string

	// TraceID is propagated to every responder. Optional.
	TraceID string
}

// Broadcast publishes the request to every peer (subject = `.*`) and
// waits up to PerSliceTimeout for replies. Returns the aggregated
// replies, in order of arrival.
//
// Per FR-006a, a sibling-level failure NEVER fails the broadcast as a
// whole; it appears as a `failed` reply in the result slice.
func (c *Client) Broadcast(ctx context.Context, slice Slice, args map[string]any, opts CallOptions) ([]Reply, error) {
	if c == nil || c.conn == nil {
		return nil, errors.New("fanout: nil Client")
	}
	if !IsValidSlice(slice) {
		return nil, fmt.Errorf("fanout: unknown slice %q", slice)
	}
	deadline := opts.PerSliceTimeout
	if deadline <= 0 {
		deadline = 5 * time.Second
	}

	req := Request{
		Version:       1,
		RequestID:     ulid.Make().String(),
		OperatorActor: opts.OperatorActor,
		TraceID:       opts.TraceID,
		DeadlineMS:    int(deadline.Milliseconds()),
		Slice:         slice,
		Args:          args,
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	inbox := nats.NewInbox()
	sub, err := c.conn.SubscribeSync(inbox)
	if err != nil {
		return nil, fmt.Errorf("inbox subscribe: %w", err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	if err := c.conn.PublishRequest(RequestSubject(c.clusterID, slice, "*"), inbox, raw); err != nil {
		return nil, fmt.Errorf("publish request: %w", err)
	}

	// Drain replies until deadline.
	var (
		mu      sync.Mutex
		got     []Reply
		seenIDs = make(map[string]bool)
	)
	overall, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	for {
		// SubscribeSync uses a polling NextMsg with a per-call
		// timeout; this is the simplest correct shape for the
		// "wait for N replies until deadline" pattern.
		remaining := time.Until(overallDeadline(overall))
		if remaining <= 0 {
			break
		}
		msg, err := sub.NextMsg(remaining)
		if err != nil {
			break
		}
		var r Reply
		if jerr := json.Unmarshal(msg.Data, &r); jerr != nil {
			continue
		}
		mu.Lock()
		if !seenIDs[r.NodeID] { // dedup by node id (broadcast can race)
			seenIDs[r.NodeID] = true
			got = append(got, r)
		}
		mu.Unlock()

		if expected := len(opts.ExpectedNodes); expected > 0 && len(got) >= expected {
			break
		}
	}

	// Synthesize sibling_unreachable entries for missing expected
	// nodes (FR-006a).
	if len(opts.ExpectedNodes) > 0 {
		for _, n := range opts.ExpectedNodes {
			if seenIDs[n] {
				continue
			}
			got = append(got, Reply{
				Version:     1,
				RequestID:   req.RequestID,
				NodeID:      n,
				Status:      StatusFailed,
				Error:       &Error{Code: CodeSiblingUnreachable, Message: "no reply within " + deadline.String()},
				RespondedAt: time.Now().UTC(),
			})
		}
	}

	return got, nil
}

// Unicast publishes the request to a specific node and waits for its
// single reply. Returns an `unreachable` synthetic reply if the peer
// doesn't answer within the deadline.
func (c *Client) Unicast(ctx context.Context, slice Slice, target string, args map[string]any, opts CallOptions) (Reply, error) {
	if c == nil || c.conn == nil {
		return Reply{}, errors.New("fanout: nil Client")
	}
	deadline := opts.PerSliceTimeout
	if deadline <= 0 {
		deadline = 5 * time.Second
	}
	req := Request{
		Version:       1,
		RequestID:     ulid.Make().String(),
		OperatorActor: opts.OperatorActor,
		TraceID:       opts.TraceID,
		DeadlineMS:    int(deadline.Milliseconds()),
		Slice:         slice,
		Args:          args,
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return Reply{}, fmt.Errorf("marshal request: %w", err)
	}

	callCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	msg, err := c.conn.RequestWithContext(callCtx, RequestSubject(c.clusterID, slice, target), raw)
	if err != nil {
		return Reply{
			Version:     1,
			RequestID:   req.RequestID,
			NodeID:      target,
			Status:      StatusFailed,
			Error:       &Error{Code: CodeSiblingUnreachable, Message: err.Error()},
			RespondedAt: time.Now().UTC(),
		}, nil
	}
	var r Reply
	if jerr := json.Unmarshal(msg.Data, &r); jerr != nil {
		return Reply{
			Version:     1,
			RequestID:   req.RequestID,
			NodeID:      target,
			Status:      StatusFailed,
			Error:       &Error{Code: CodeSliceInternal, Message: "malformed reply: " + jerr.Error()},
			RespondedAt: time.Now().UTC(),
		}, nil
	}
	return r, nil
}

func overallDeadline(ctx context.Context) time.Time {
	if t, ok := ctx.Deadline(); ok {
		return t
	}
	return time.Now().Add(5 * time.Second)
}
