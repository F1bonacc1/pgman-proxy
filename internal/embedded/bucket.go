package embedded

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// PreCreateClusterKV is the RD-002 fallback path. It pre-creates the
// JetStream KV bucket that pg-manager's `adapters/nats` uses (for
// leadership lease + state-store), with the desired Replicas count
// derived from the FR-011a / RD-004 table.
//
// IMPORTANT (Constitution-IV exception logged in plan.md): the bucket
// name format is replicated here from
// `github.com/f1bonacc1/pg-manager/adapters/nats/bucket.go` —
// `pgmgr_<sanitized-cluster-id>` with non-[A-Za-z0-9_-] runes mapped
// to `_`. This is a temporary coupling. When pg-manager exposes a
// `WithReplicas(int)` adapter option upstream (T007 of tasks.md),
// this function collapses to a no-op forwarder.
//
// The function is idempotent: if the bucket already exists with a
// compatible configuration, the existing bucket is returned without
// re-creation. If it exists with the wrong Replicas count, a clear
// error is returned — the operator must drop the bucket before
// re-running (this is a one-time bootstrap concern, not a steady-
// state code path).
func PreCreateClusterKV(ctx context.Context, conn *nats.Conn, clusterID string, replicas int) error {
	if conn == nil {
		return errors.New("PreCreateClusterKV: nats connection is nil")
	}
	if clusterID == "" {
		return errors.New("PreCreateClusterKV: clusterID is empty")
	}
	if replicas < 1 {
		return fmt.Errorf("PreCreateClusterKV: replicas must be >= 1, got %d", replicas)
	}

	js, err := jetstream.New(conn)
	if err != nil {
		return fmt.Errorf("jetstream context: %w", err)
	}

	name := bucketName(clusterID)

	// Bucket already exists?
	kv, err := js.KeyValue(ctx, name)
	if err == nil {
		// Verify the existing bucket's Replicas matches what we want
		// to commit to. If it doesn't, the operator must reconcile —
		// we don't silently mutate an existing bucket's replication
		// factor (mutation is a mass-resync event).
		status, err := kv.Status(ctx)
		if err != nil {
			return fmt.Errorf("read existing bucket status: %w", err)
		}
		if existing := replicasFromStatus(status); existing != replicas {
			return fmt.Errorf(
				"cluster KV bucket %q already exists with Replicas=%d, expected %d (FR-011a / RD-004); "+
					"drop the bucket via the operator's recovery procedure before changing the cluster's declared size",
				name, existing, replicas,
			)
		}
		return nil
	}
	if !errors.Is(err, jetstream.ErrBucketNotFound) {
		return fmt.Errorf("look up existing bucket %q: %w", name, err)
	}

	// Bucket does not exist — create it with the operator-specified
	// Replicas. History matches the value pg-manager uses today (8) so
	// the adapter sees an indistinguishable bucket. If pg-manager
	// later changes its History default, this function MUST be kept
	// in sync (the coupling that motivates T007's upstream PR).
	createCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = js.CreateKeyValue(createCtx, jetstream.KeyValueConfig{
		Bucket:   name,
		History:  8,
		Replicas: replicas,
	})
	if err != nil {
		return fmt.Errorf("create cluster KV bucket %q with replicas=%d: %w", name, replicas, err)
	}
	return nil
}

// bucketName mirrors pg-manager/adapters/nats/bucket.go:bucketName.
// MUST stay in sync with the upstream until T007's WithReplicas option
// lands and PreCreateClusterKV is retired.
func bucketName(clusterID string) string {
	var b strings.Builder
	b.WriteString("pgmgr_")
	for _, r := range clusterID {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// replicasFromStatus reads the Replicas field out of a KV status.
// jetstream.KeyValueStatus is an interface; the concrete returned
// value carries Replicas via the underlying StreamConfig. The
// function isolates the field access so future API changes only
// require an update here.
func replicasFromStatus(status jetstream.KeyValueStatus) int {
	type replicasReporter interface {
		Replicas() int
	}
	if rr, ok := status.(replicasReporter); ok {
		return rr.Replicas()
	}
	// Fall back to inspecting the raw stream info if the interface
	// changes shape. Returning 0 here means "unknown" — the caller's
	// equality check below will then refuse to proceed, which is the
	// correct conservative behaviour.
	return 0
}
