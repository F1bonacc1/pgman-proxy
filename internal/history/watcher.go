package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// WatchOptions controls a live history-stream subscription. Zero values
// are valid:
//   - Category=="" subscribes to both event and audit subjects.
//   - Cursor=="" starts live-only (DeliverNewPolicy) — events strictly
//     after subscription time.
//   - Cursor!="" replays from immediately after that ULID in the
//     stream (backfill), then continues live without a gap.
//   - Since>0 (Cursor=="" only) replays from now-Since onward.
//
// Type and node filters are applied client-side (the JetStream consumer
// already filters by category subject, but evType is a sanitized subject
// token and a single record can carry e.g. "auto_rebootstrap.detected"
// which the caller may or may not want to subscribe to).
type WatchOptions struct {
	Cursor   string
	Since    time.Duration
	Category Category
	Types    []string
	Nodes    []string
}

// Watcher subscribes to live HistoryEvents on a cluster's history
// JetStream. One Watch call yields one independent subscription; cancel
// via the ctx supplied at Watch time.
type Watcher struct {
	js        jetstream.JetStream
	clusterID string
}

// NewWatcher constructs a Watcher.
func NewWatcher(js jetstream.JetStream, clusterID string) *Watcher {
	return &Watcher{js: js, clusterID: clusterID}
}

// Watch starts a live subscription. The returned events channel emits
// HistoryEvent values; the errors channel emits at most one error
// (consumer failure) before both channels are closed. Cancel via ctx.
//
// Resumption semantics:
//
//   - opts.Cursor != "": the consumer opens with DeliverAllPolicy and
//     skips records up to and including the cursor. This means the
//     first event delivered is the one strictly after the cursor — the
//     resumption guarantee documented in
//     contracts/control-plane-extensions.md § 1.
//   - opts.Cursor == "" && opts.Since > 0: DeliverByStartTimePolicy from
//     now-Since.
//   - else: DeliverNewPolicy (live-only).
func (w *Watcher) Watch(ctx context.Context, opts WatchOptions) (<-chan HistoryEvent, <-chan error) {
	events := make(chan HistoryEvent, 64)
	errs := make(chan error, 1)

	if w == nil || w.js == nil {
		errs <- errors.New("history: nil Watcher")
		close(events)
		close(errs)
		return events, errs
	}
	if w.clusterID == "" {
		errs <- errors.New("history: empty clusterID")
		close(events)
		close(errs)
		return events, errs
	}

	go w.run(ctx, opts, events, errs)
	return events, errs
}

func (w *Watcher) run(ctx context.Context, opts WatchOptions, events chan<- HistoryEvent, errs chan<- error) {
	defer close(events)
	defer close(errs)

	stream, err := w.js.Stream(ctx, StreamName(w.clusterID))
	if err != nil {
		errs <- fmt.Errorf("open history stream: %w", err)
		return
	}

	cc := jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{buildSubjectFilter(w.clusterID, opts.Category)},
	}
	switch {
	case opts.Cursor != "":
		cc.DeliverPolicy = jetstream.DeliverAllPolicy
	case opts.Since > 0:
		cc.DeliverPolicy = jetstream.DeliverByStartTimePolicy
		t := time.Now().Add(-opts.Since)
		cc.OptStartTime = &t
	default:
		cc.DeliverPolicy = jetstream.DeliverNewPolicy
	}

	consumer, err := stream.OrderedConsumer(ctx, cc)
	if err != nil {
		errs <- fmt.Errorf("history ordered consumer: %w", err)
		return
	}

	cursorSeen := opts.Cursor == ""
	iter, err := consumer.Messages()
	if err != nil {
		errs <- fmt.Errorf("history messages: %w", err)
		return
	}
	defer iter.Stop()

	// Bridge the blocking iter.Next() call to ctx cancellation. A
	// goroutine stops the iterator when ctx fires so Next() returns
	// promptly.
	go func() {
		<-ctx.Done()
		iter.Stop()
	}()

	for {
		msg, err := iter.Next()
		if err != nil {
			// Stop() returns ErrMsgIteratorClosed; treat as clean shutdown.
			if errors.Is(err, jetstream.ErrMsgIteratorClosed) || ctx.Err() != nil {
				return
			}
			errs <- fmt.Errorf("history next: %w", err)
			return
		}
		_ = msg.Ack()

		var ev HistoryEvent
		if jerr := json.Unmarshal(msg.Data(), &ev); jerr != nil {
			continue
		}
		if !cursorSeen {
			if ev.ID == opts.Cursor {
				cursorSeen = true
			}
			continue
		}
		if !typeMatches(opts.Types, ev.Type) {
			continue
		}
		if !nodeMatches(opts.Nodes, ev.NodeID) {
			continue
		}
		select {
		case events <- ev:
		case <-ctx.Done():
			return
		}
	}
}
