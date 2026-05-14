package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Query parameters mirror GET /v1/history (control-plane-extensions.md
// § 4). Zero values mean "no filter".
type Query struct {
	Since    time.Duration // events newer than now-Since (when non-zero)
	Until    time.Time     // RFC3339 cutoff; zero = no upper bound
	Category Category      // empty means both categories
	Types    []string      // type filter; empty = any
	Nodes    []string      // node filter; empty = any
	Limit    int           // max records; <=0 = 1000
	Cursor   string        // resume after this ULID; empty = start from Since
}

// Result is the typed shape of an HTTP /v1/history JSON response.
type Result struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Events     []HistoryEvent `json:"events"`
	NextCursor string         `json:"next_cursor,omitempty"`
	Truncated  bool           `json:"truncated"`
}

// Run runs the query against the given cluster's history stream and
// returns a Result. The default per-call deadline is 15s.
func Run(ctx context.Context, js jetstream.JetStream, clusterID string, q Query) (Result, error) {
	if js == nil {
		return Result{}, errors.New("history: nil JetStream context")
	}
	if clusterID == "" {
		return Result{}, errors.New("history: empty clusterID")
	}
	if q.Limit <= 0 {
		q.Limit = 1000
	}
	subject := buildSubjectFilter(clusterID, q.Category)

	stream, err := js.Stream(ctx, StreamName(clusterID))
	if err != nil {
		return Result{}, fmt.Errorf("open history stream: %w", err)
	}

	// Build the OrderedConsumer with the chosen start point.
	cc := jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{subject},
	}
	switch {
	case q.Cursor != "":
		// Resume strictly after this id. We rely on per-record `Nats-Msg-Id`
		// equality (publisher uses ULID as the dedup id, never reuses), but
		// JetStream consumers can't filter by msg-id at the server. Instead
		// start from a sequence past the cursor's timestamp.
		cc.DeliverPolicy = jetstream.DeliverAllPolicy
	case q.Since > 0:
		cc.DeliverPolicy = jetstream.DeliverByStartTimePolicy
		t := time.Now().Add(-q.Since)
		cc.OptStartTime = &t
	default:
		cc.DeliverPolicy = jetstream.DeliverAllPolicy
	}

	consumer, err := stream.OrderedConsumer(ctx, cc)
	if err != nil {
		return Result{}, fmt.Errorf("history ordered consumer: %w", err)
	}

	deadline, _ := ctx.Deadline()
	if deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
	}

	out := Result{APIVersion: "pgman-proxy/v1", Kind: "HistoryQueryResult"}

	// JetStream's Fetch returns up to Batch messages within the
	// per-fetch deadline. We loop until the limit is hit, the cursor
	// resumption skips records, or the deadline elapses.
	cursorSeen := q.Cursor == ""
	batchSize := q.Limit
	if batchSize > 256 {
		batchSize = 256
	}
	for len(out.Events) < q.Limit {
		fetchCtx, fcancel := context.WithTimeout(ctx, 1*time.Second)
		msgs, err := consumer.Fetch(batchSize, jetstream.FetchMaxWait(750*time.Millisecond))
		_ = fetchCtx
		fcancel()
		if err != nil {
			// FetchNoWait variants would be cleaner; the ordered consumer
			// API only exposes Fetch with FetchMaxWait. Treat ErrTimeout
			// as "no more messages in window".
			if errors.Is(err, context.DeadlineExceeded) {
				break
			}
			return out, fmt.Errorf("history fetch: %w", err)
		}
		any := false
		for msg := range msgs.Messages() {
			any = true
			var ev HistoryEvent
			if jerr := json.Unmarshal(msg.Data(), &ev); jerr != nil {
				_ = msg.Ack()
				continue
			}
			_ = msg.Ack()

			if !q.Until.IsZero() && ev.Time.After(q.Until) {
				continue
			}
			if !cursorSeen {
				if ev.ID == q.Cursor {
					cursorSeen = true
				}
				continue
			}
			if !typeMatches(q.Types, ev.Type) {
				continue
			}
			if !nodeMatches(q.Nodes, ev.NodeID) {
				continue
			}
			out.Events = append(out.Events, ev)
			if len(out.Events) >= q.Limit {
				break
			}
		}
		if !any {
			break
		}
	}

	if len(out.Events) > 0 {
		out.NextCursor = out.Events[len(out.Events)-1].ID
	}
	out.Truncated = len(out.Events) == q.Limit
	return out, nil
}

func buildSubjectFilter(clusterID string, c Category) string {
	if c == "" {
		return SubjectFilterAll(clusterID)
	}
	return SubjectFilterCategory(clusterID, c)
}

func typeMatches(filter []string, t string) bool {
	if len(filter) == 0 {
		return true
	}
	for _, f := range filter {
		if strings.EqualFold(t, f) {
			return true
		}
	}
	return false
}

func nodeMatches(filter []string, id string) bool {
	if len(filter) == 0 {
		return true
	}
	for _, f := range filter {
		if f == id {
			return true
		}
	}
	return false
}
