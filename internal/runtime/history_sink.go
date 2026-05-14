// Tiny adapter that lets obs.Logger publish to history.Publisher
// without obs importing internal/history (which would create a cycle
// once control/audit also imports both).

package runtime

import (
	"context"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/f1bonacc1/pgman-proxy/internal/history"
)

// historyLoggerSink wires a *history.Publisher to obs.HistorySink. The
// concrete category strings used by obs.Logger.Event ("event", "audit")
// are projected onto history.Category here.
type historyLoggerSink struct {
	p *history.Publisher
}

// PublishEvent implements obs.HistorySink.
func (s historyLoggerSink) PublishEvent(ctx context.Context, category, evType, nodeID string, details map[string]any) (string, error) {
	c := history.Category(category)
	return s.p.PublishEvent(ctx, c, evType, nodeID, details)
}

// historyRunner adapts a (JetStream context + cluster id) pair to the
// control.HistoryQuerier interface so GET /v1/history can call into
// history.Run without the control package taking a JetStream dep.
type historyRunner struct {
	js        jetstream.JetStream
	clusterID string
}

// Query implements control.HistoryQuerier.
func (r *historyRunner) Query(ctx context.Context, q history.Query) (history.Result, error) {
	return history.Run(ctx, r.js, r.clusterID, q)
}
