package history

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/oklog/ulid/v2"
)

// Publisher emits HistoryEvents on the cluster's history stream.
// Publishes are best-effort: failures are counted (PublishFailures)
// and surfaced via the err return, but the caller MUST treat the
// in-process structured log as authoritative for the event.
//
// All public methods are goroutine-safe.
type Publisher struct {
	js              jetstream.JetStream
	clusterID       string
	nodeID          string
	publishTimeout  time.Duration
	publishFailures atomic.Uint64
}

// NewPublisher constructs a Publisher. js MUST be non-nil. nodeID and
// clusterID become the default identifiers stamped on every
// HistoryEvent.
func NewPublisher(js jetstream.JetStream, clusterID, nodeID string) *Publisher {
	return &Publisher{
		js:             js,
		clusterID:      clusterID,
		nodeID:         nodeID,
		publishTimeout: 5 * time.Second,
	}
}

// SetPublishTimeout overrides the per-publish JetStream timeout
// (default 5s). Used by tests that want fast deadlines.
func (p *Publisher) SetPublishTimeout(d time.Duration) {
	if d > 0 {
		p.publishTimeout = d
	}
}

// PublishFailures returns the count of best-effort publish failures
// since process start. Wired into Prometheus as
// `pgman_proxy_history_publish_failures_total`.
func (p *Publisher) PublishFailures() uint64 {
	return p.publishFailures.Load()
}

// PublishEvent publishes one HistoryEvent. Returns the ULID assigned
// to the record on success; "" on error.
//
//   - category MUST be CategoryEvent or CategoryAudit.
//   - evType is the user-visible kind (e.g. "state_transition",
//     "lcm_audit"). Sanitized into the subject.
//   - nodeID, when empty, defaults to the Publisher's own node id.
//   - details is the type-specific payload; nil is fine.
func (p *Publisher) PublishEvent(ctx context.Context, category Category, evType, nodeID string, details map[string]any) (string, error) {
	if p == nil || p.js == nil {
		return "", errors.New("history: nil Publisher")
	}
	if category != CategoryEvent && category != CategoryAudit {
		return "", errors.New("history: invalid category (want event or audit)")
	}
	if evType == "" {
		return "", errors.New("history: empty event type")
	}

	if nodeID == "" {
		nodeID = p.nodeID
	}
	rec := HistoryEvent{
		APIVersion: "pgman-proxy/v1",
		Kind:       "HistoryEvent",
		ID:         strings.ToLower(ulid.Make().String()),
		Time:       time.Now().UTC(),
		Category:   category,
		Type:       evType,
		ClusterID:  p.clusterID,
		NodeID:     nodeID,
		Details:    details,
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		p.publishFailures.Add(1)
		return "", err
	}

	callCtx, cancel := context.WithTimeout(ctx, p.publishTimeout)
	defer cancel()

	if _, err := p.js.Publish(callCtx, EventSubject(p.clusterID, category, evType), raw, jetstream.WithMsgID(rec.ID)); err != nil {
		p.publishFailures.Add(1)
		return "", err
	}
	return rec.ID, nil
}
