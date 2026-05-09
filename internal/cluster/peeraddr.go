// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

package cluster

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	pgmanager "github.com/f1bonacc1/pg-manager"
	"github.com/nats-io/nats.go/jetstream"
)

// peerAddrKeyPrefix namespaces our keys inside the shared cluster KV
// bucket so they don't collide with pg-manager's own state__* keys.
// NATS KV keys must match [a-zA-Z0-9_=.-]+ — no slashes — so we
// separate segments with `.`.
const peerAddrKeyPrefix = "peer."

// PeerAddrKey returns the KV key under which `nodeID` advertises its
// externally-reachable Postgres replication address. Exported so tests
// can introspect.
func PeerAddrKey(nodeID string) string {
	return peerAddrKeyPrefix + sanitizePeerKey(nodeID) + ".pg-replication-addr"
}

// PublishPeerAddress writes "<host>:<port>" to the cluster KV under
// PeerAddrKey(nodeID). Idempotent (Put, not Create) so a process
// restart with the same address is a no-op write.
func PublishPeerAddress(ctx context.Context, kv jetstream.KeyValue, nodeID, addr string) error {
	if kv == nil {
		return errors.New("PublishPeerAddress: kv is nil")
	}
	if nodeID == "" {
		return errors.New("PublishPeerAddress: nodeID is empty")
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return fmt.Errorf("PublishPeerAddress: addr %q must be host:port: %w", addr, err)
	}
	putCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := kv.Put(putCtx, PeerAddrKey(nodeID), []byte(addr)); err != nil {
		return fmt.Errorf("publish peer address for %q: %w", nodeID, err)
	}
	return nil
}

// PeerAddressResolver returns a pg-manager-compatible
// Topology.PeerDSNResolver closure that reads
// `peer.<peer-id>.pg-replication-addr` from the cluster KV and
// templates a libpq conninfo around it.
//
// On miss (the peer hasn't published yet), retries 5 times at 500ms
// intervals before returning an error. This covers the cold-start
// race where peer X's pg-manager attempts basebackup before peer Y
// has finished writing its address into the KV. pg-manager has its
// own basebackup retry around the call so total resilience is the
// product of the two.
//
// The DSN format matches peerDSNsForConfig in start.go so behaviour is
// identical to the static-map path when the lookup succeeds.
func PeerAddressResolver(kv jetstream.KeyValue) func(ctx context.Context, peer pgmanager.NodeID) (string, error) {
	return func(ctx context.Context, peer pgmanager.NodeID) (string, error) {
		if kv == nil {
			return "", errors.New("PeerAddressResolver: kv is nil")
		}
		const attempts = 5
		const backoff = 500 * time.Millisecond
		var lastErr error
		key := PeerAddrKey(string(peer))
		for i := 0; i < attempts; i++ {
			lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			entry, err := kv.Get(lookupCtx, key)
			cancel()
			if err == nil {
				addr := string(entry.Value())
				host, port, splitErr := net.SplitHostPort(addr)
				if splitErr != nil {
					return "", fmt.Errorf("peer %s addr %q: %w", peer, addr, splitErr)
				}
				return fmt.Sprintf(
					"host=%s port=%s user=postgres dbname=postgres sslmode=disable",
					host, port,
				), nil
			}
			if !errors.Is(err, jetstream.ErrKeyNotFound) {
				return "", fmt.Errorf("peer %s KV lookup: %w", peer, err)
			}
			lastErr = err
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(backoff):
			}
		}
		return "", fmt.Errorf("peer %s address not in KV after %d attempts: %w", peer, attempts, lastErr)
	}
}

// sanitizePeerKey lower-cases and replaces NATS-KV-illegal characters
// in a node ID so it can safely appear inside a key. Same character
// class as embedded.bucketName but as a key segment, not a bucket
// name.
func sanitizePeerKey(nodeID string) string {
	var b strings.Builder
	b.Grow(len(nodeID))
	for _, r := range nodeID {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-', r == '=':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}
