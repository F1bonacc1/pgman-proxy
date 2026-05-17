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

// WaitForJetStreamResponsive blocks until the embedded JetStream
// subsystem can answer a trivial API call (`js.AccountInfo`) — i.e. the
// meta-cluster Raft node has elected a leader and is serving requests.
// Necessary because the JS client's internal per-request timeout is
// only 5 s; without this gate, the first JS RPC issued by
// pg-manager's NewLeadership (in cluster.BuildHandles) hits that
// timeout on a fresh cold start before the meta cluster is up.
//
// Each probe attempt has a 3 s deadline; on failure the function backs
// off `interval` (defaulting to 500 ms) and retries within the parent
// ctx budget.
func WaitForJetStreamResponsive(ctx context.Context, conn *nats.Conn, interval time.Duration) error {
	if conn == nil {
		return errors.New("WaitForJetStreamResponsive: nats connection is nil")
	}
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	js, err := jetstream.New(conn)
	if err != nil {
		return fmt.Errorf("jetstream context: %w", err)
	}
	for {
		probeCtx, probeCancel := context.WithTimeout(ctx, 3*time.Second)
		_, err := js.AccountInfo(probeCtx)
		probeCancel()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// PreCreateClusterKV ensures the JetStream KV bucket that pg-manager's
// `adapters/nats` uses (for leadership lease + state-store) exists with
// the operator-specified Replicas count, BEFORE pg-manager's adapter
// touches it. pg-manager's ensureBucket creates with the default
// Replicas=1 if absent; calling this first guarantees the adapter sees
// an existing bucket with the desired replication factor on its first
// lookup. RD-002 fallback for the not-yet-upstreamed `WithReplicas(int)`
// option (T007 of tasks.md); once T007 lands this collapses to a no-op
// forwarder.
//
// Idempotent and safe to call concurrently from every peer:
//
//   - Bucket missing → create with Replicas=N. If another peer wins the
//     create race we get ErrBucketExists / ErrStreamNameAlreadyInUse;
//     re-fetch and continue (matches pg-manager's upstream race-tolerance
//     pattern in adapters/nats/bucket.go).
//   - Bucket present with matching Replicas → no-op.
//   - Bucket present with smaller Replicas → upgrade via
//     UpdateKeyValue. Defensive: with WaitForRouteMesh +
//     WaitForJetStreamResponsive ahead of this call and pg-manager's
//     ensureBucket only running afterward, this branch is unreachable
//     in steady state — but it keeps the function safe if call ordering
//     changes upstream.
//   - Bucket present with larger Replicas → refuse. Shrinking would
//     require an operator-driven mass-resync.
//
// The caller MUST gate this behind WaitForRouteMesh +
// WaitForJetStreamResponsive: a peer running alone with no quorate
// meta cluster cannot place N>1 replicas, leaving a degraded bucket
// that subsequent peers reject.
//
// The bucket name format is replicated from
// `github.com/f1bonacc1/pg-manager/adapters/nats/bucket.go` —
// `pgmgr_<sanitized-cluster-id>` with non-[A-Za-z0-9_-] runes mapped
// to `_`. This coupling is the Constitution-IV exception logged in
// plan.md.
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
	// 60 s — phase-1 cold-start contention (all peers racing to land
	// meta-cluster RAFT + provision this bucket + tune AllowDirect on
	// the underlying stream) can blow through a 30 s budget while a
	// peer waits for the meta-cluster leader to settle. Bumping the
	// budget keeps a transient slow election from killing the boot
	// (chaos-rig RCA, 2026-05-16). The function is still bounded.
	callCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	kv, err := js.KeyValue(callCtx, name)
	if err != nil {
		if !errors.Is(err, jetstream.ErrBucketNotFound) {
			return fmt.Errorf("look up cluster KV bucket %q: %w", name, err)
		}
		kv, err = js.CreateKeyValue(callCtx, jetstream.KeyValueConfig{
			Bucket:   name,
			History:  8,
			Replicas: replicas,
		})
		if err != nil {
			if !errors.Is(err, jetstream.ErrBucketExists) && !errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) {
				return fmt.Errorf("create cluster KV bucket %q with replicas=%d: %w", name, replicas, err)
			}
			// Race lost: another peer created the bucket between our
			// lookup and create. Re-fetch and verify Replicas.
			kv, err = js.KeyValue(callCtx, name)
			if err != nil {
				return fmt.Errorf("re-fetch cluster KV bucket %q after create-race: %w", name, err)
			}
		}
	}

	status, err := kv.Status(callCtx)
	if err != nil {
		return fmt.Errorf("read cluster KV bucket %q status: %w", name, err)
	}
	current := status.Config().Replicas
	if current > replicas {
		return fmt.Errorf(
			"cluster KV bucket %q has Replicas=%d, target %d (would shrink — FR-011a / RD-004); "+
				"drop the bucket via the operator's recovery procedure to reduce the cluster's declared size",
			name, current, replicas,
		)
	}
	if current < replicas {
		if _, updErr := js.UpdateKeyValue(callCtx, jetstream.KeyValueConfig{
			Bucket:   name,
			History:  8,
			Replicas: replicas,
		}); updErr != nil {
			return fmt.Errorf(
				"upgrade cluster KV bucket %q Replicas %d -> %d: %w",
				name, current, replicas, updErr,
			)
		}
	}

	// Force read-your-writes consistency by disabling AllowDirect on the
	// underlying JetStream stream. `js.CreateKeyValue` hardcodes
	// AllowDirect=true on the stream it provisions (nats.go v1.51.0,
	// jetstream/kv.go:684), which routes KV Get requests to *any*
	// replica via the `$JS.API.DIRECT.GET.*` subject — fast, but no
	// read-your-writes guarantee (NATS KV docs explicitly note this).
	//
	// For coordination data (pg-manager leadership lease + failover
	// quorum snapshot) we need stricter consistency: a survivor that
	// polls the leader key must observe the current leader's just-
	// committed renewal, not a lagging replica's older view. Without
	// this, the Leadership tickOnce stale-observation counter
	// (staleThreshold=3) flips on a *live* leader during a partition
	// because the local replica is slow to receive the leader's CAS
	// Update, producing false evictions and multi-tens-of-seconds of
	// failover delay (chaos-rig RCA, 2026-05-16).
	//
	// Setting AllowDirect=false forces KV Gets through the stream
	// leader (`$JS.API.STREAM.MSG.GET.*`) — strictly slower, but
	// linearisable. Idempotent: skip the UpdateStream when the field
	// already matches.
	streamName := bucketStreamName(name)
	stream, err := js.Stream(callCtx, streamName)
	if err != nil {
		return fmt.Errorf("fetch KV stream %q to tune AllowDirect: %w", streamName, err)
	}
	scfg := stream.CachedInfo().Config
	if scfg.AllowDirect {
		// On a freshly-created replicated stream, the underlying RAFT
		// group can take several seconds to elect a leader. UpdateStream
		// blocks waiting for the stream leader; without this wait the
		// 60 s callCtx budget gets burned on a single hung call during
		// cold-start contention (chaos-rig RCA, 2026-05-16).
		if err := WaitForStreamReady(callCtx, js, streamName, 30*time.Second, 500*time.Millisecond); err != nil {
			return fmt.Errorf("wait for KV stream %q leader before AllowDirect tune: %w", streamName, err)
		}
		scfg.AllowDirect = false
		if _, err := js.UpdateStream(callCtx, scfg); err != nil {
			return fmt.Errorf("disable AllowDirect on KV stream %q: %w", streamName, err)
		}
	}
	return nil
}

// bucketStreamName returns the JetStream stream name that backs a KV
// bucket. Mirrors nats.go's kvBucketNameTmpl ("KV_%s"). Hard-coupled to
// the SDK's naming convention by design — pgman-proxy needs to reach
// underneath the KV abstraction to override AllowDirect, which the KV
// config does not expose. If the SDK ever changes this template the
// bucket-creation path here will start failing loudly (Stream lookup
// returns ErrStreamNotFound), which is the surface we want.
func bucketStreamName(bucket string) string {
	return "KV_" + bucket
}

// WaitForStreamReady blocks until the named stream's RAFT group has
// elected a leader, or budget elapses. Necessary before any boot-time
// "verify-the-stream-works" probe: a freshly-created replicated stream
// often has no leader for several seconds while the underlying RAFT
// group forms quorum, and any API call against the stream
// (UpdateStream, Publish, etc.) returns `nats: no response from
// stream` / a 503 during that window. If a caller treats that
// transient failure as fatal, cold-start cluster wedges in a boot loop
// (chaos-rig RCA, 2026-05-16).
//
// Used in two boot paths:
//
//   - history stream readiness, gating the FR-027 audit emit (Gate
//     #11a in runtime/start.go).
//   - cluster KV stream readiness, gating UpdateStream(AllowDirect)
//     inside PreCreateClusterKV.
//
// Single-replica streams have no real election (Cluster == nil or
// Leader is already self) and the wait returns immediately. Each
// probe has a 3 s deadline; on failure the function sleeps interval
// (defaulting to 500 ms) and retries within the budget.
func WaitForStreamReady(ctx context.Context, js jetstream.JetStream, streamName string, budget, interval time.Duration) error {
	if js == nil {
		return errors.New("WaitForStreamReady: nil JetStream context")
	}
	if streamName == "" {
		return errors.New("WaitForStreamReady: empty stream name")
	}
	if budget <= 0 {
		budget = 30 * time.Second
	}
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	deadline := time.Now().Add(budget)
	var lastErr error
	for {
		probeCtx, probeCancel := context.WithTimeout(ctx, 3*time.Second)
		stream, err := js.Stream(probeCtx, streamName)
		var info *jetstream.StreamInfo
		if err == nil {
			info, err = stream.Info(probeCtx)
		}
		probeCancel()
		if err == nil && info != nil {
			if info.Cluster == nil || info.Cluster.Leader != "" {
				return nil
			}
			lastErr = errors.New("WaitForStreamReady: stream has no elected leader")
		} else if err != nil {
			lastErr = err
		}
		if !time.Now().Before(deadline) {
			if lastErr == nil {
				lastErr = errors.New("WaitForStreamReady: probe returned no result")
			}
			return fmt.Errorf("WaitForStreamReady: stream %q not ready within %s: %w", streamName, budget, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// OpenClusterKV returns a handle to the cluster KV bucket
// (`pgmgr_<sanitized-cluster-id>`), assumed to already exist via a
// prior `PreCreateClusterKV` call. Used by host-level wiring that
// needs to publish/read non-pg-manager keys (e.g. peer reachability)
// in the shared bucket.
func OpenClusterKV(ctx context.Context, conn *nats.Conn, clusterID string) (jetstream.KeyValue, error) {
	if conn == nil {
		return nil, errors.New("OpenClusterKV: nats connection is nil")
	}
	if clusterID == "" {
		return nil, errors.New("OpenClusterKV: clusterID is empty")
	}
	js, err := jetstream.New(conn)
	if err != nil {
		return nil, fmt.Errorf("jetstream context: %w", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	kv, err := js.KeyValue(callCtx, bucketName(clusterID))
	if err != nil {
		return nil, fmt.Errorf("open cluster KV bucket %q: %w", bucketName(clusterID), err)
	}
	return kv, nil
}

// bucketName mirrors pg-manager/adapters/nats/bucket.go:bucketName.
// MUST stay in sync with the upstream until T007's WithReplicas option
// lands and UpgradeBucketReplicas is retired.
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
