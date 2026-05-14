package history

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// StreamOptions controls how the history JetStream is created /
// reconciled. Defaults match contracts/history-stream.md.
type StreamOptions struct {
	// Replicas is the JetStream R factor derived per 002 FR-011a.
	Replicas int

	// MaxAge bounds retention by wall time. Zero disables age-based
	// retention.
	MaxAge time.Duration

	// MaxBytes bounds retention by stream-on-disk size. Zero disables
	// size-based retention.
	MaxBytes int64

	// MaxMsgSize caps the wire size of a single record. Defaults to
	// 1 MiB. Records exceeding this are rejected at publish time.
	MaxMsgSize int32

	// Storage is the JetStream storage backend. Defaults to
	// FileStorage (durable). Tests MAY pass MemoryStorage for
	// in-process unit tests.
	Storage jetstream.StorageType
}

// DefaultStreamOptions returns the contract defaults.
func DefaultStreamOptions(replicas int) StreamOptions {
	return StreamOptions{
		Replicas:   replicas,
		MaxAge:     24 * time.Hour,
		MaxBytes:   256 << 20, // 256 MiB
		MaxMsgSize: 1 << 20,   // 1 MiB
		Storage:    jetstream.FileStorage,
	}
}

// ErrRetentionUnbounded is returned by Validate when both MaxAge and
// MaxBytes are zero — infinite retention is a foot-gun on a
// database-adjacent service.
var ErrRetentionUnbounded = errors.New("history: at least one of MaxAge / MaxBytes must be set (infinite retention refused)")

// Validate enforces the documented invariants.
func (o StreamOptions) Validate() error {
	if o.Replicas < 1 || o.Replicas > 5 {
		return fmt.Errorf("history: Replicas must be in [1,5], got %d", o.Replicas)
	}
	if o.MaxAge < 0 {
		return fmt.Errorf("history: MaxAge must be >= 0, got %s", o.MaxAge)
	}
	if o.MaxBytes < 0 {
		return fmt.Errorf("history: MaxBytes must be >= 0, got %d", o.MaxBytes)
	}
	if o.MaxAge == 0 && o.MaxBytes == 0 {
		return ErrRetentionUnbounded
	}
	return nil
}

// EnsureHistoryStream creates or reconciles the cluster's history
// JetStream. Idempotent and safe to call concurrently from every
// peer; if another peer wins the create race we receive
// ErrStreamNameAlreadyInUse and re-fetch.
//
// Returns the live stream on success.
func EnsureHistoryStream(ctx context.Context, js jetstream.JetStream, clusterID string, opts StreamOptions) (jetstream.Stream, error) {
	if js == nil {
		return nil, errors.New("history: nil JetStream context")
	}
	if clusterID == "" {
		return nil, errors.New("history: empty clusterID")
	}
	if err := opts.Validate(); err != nil {
		return nil, err
	}

	name := StreamName(clusterID)
	cfg := jetstream.StreamConfig{
		Name:       name,
		Subjects:   []string{SubjectFilterAll(clusterID)},
		Storage:    opts.Storage,
		Retention:  jetstream.LimitsPolicy,
		Discard:    jetstream.DiscardOld,
		MaxAge:     opts.MaxAge,
		MaxBytes:   opts.MaxBytes,
		MaxMsgSize: opts.MaxMsgSize,
		Replicas:   opts.Replicas,
	}

	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	stream, err := js.CreateStream(callCtx, cfg)
	if err != nil {
		if !errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) {
			// Even if "in use" wasn't the exact error, JetStream may
			// surface stream-exists as a generic API error. Try one
			// more time via Stream().
			fallback, fallbackErr := js.Stream(callCtx, name)
			if fallbackErr == nil {
				return reconcile(callCtx, js, fallback, cfg)
			}
			return nil, fmt.Errorf("create history stream %q: %w", name, err)
		}
		stream, err = js.Stream(callCtx, name)
		if err != nil {
			return nil, fmt.Errorf("fetch history stream %q after create race: %w", name, err)
		}
	}
	return reconcile(callCtx, js, stream, cfg)
}

// reconcile updates a stream's mutable fields when they drift from
// the desired config. The JetStream library is strict about field
// equality so we only update when at least one bounded field
// disagrees; that keeps node-restart noise low.
func reconcile(ctx context.Context, js jetstream.JetStream, stream jetstream.Stream, desired jetstream.StreamConfig) (jetstream.Stream, error) {
	current := stream.CachedInfo().Config
	if current.MaxAge == desired.MaxAge &&
		current.MaxBytes == desired.MaxBytes &&
		current.MaxMsgSize == desired.MaxMsgSize &&
		current.Replicas == desired.Replicas &&
		current.Storage == desired.Storage &&
		current.Discard == desired.Discard &&
		current.Retention == desired.Retention {
		return stream, nil
	}
	updated, err := js.UpdateStream(ctx, desired)
	if err != nil {
		return nil, fmt.Errorf("reconcile history stream %q: %w", desired.Name, err)
	}
	return updated, nil
}
