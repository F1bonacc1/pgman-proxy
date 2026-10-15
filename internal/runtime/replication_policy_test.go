// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

package runtime

import (
	"reflect"
	"testing"

	pgmanager "github.com/f1bonacc1/pg-manager"

	"github.com/f1bonacc1/pgman-proxy/internal/config"
)

// TestReplicationPolicy pins WI-A (spec 005 FR-001..FR-003): one peer
// runs AsyncOnly, because QuorumSync with a pool of one renders a
// self-referential synchronous_standby_names that hangs every commit
// (F1) and never publishes the quorum snapshot promotion needs (F2).
// Two or more peers keep today's QuorumSync literal unchanged.
func TestReplicationPolicy(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		peers     []string
		minSync   int
		want      pgmanager.ReplicationPolicy
		wantLabel string
	}{
		{
			name:      "single peer runs async",
			peers:     []string{"node-a"},
			minSync:   1,
			want:      pgmanager.AsyncOnly{},
			wantLabel: "async",
		},
		{
			name:      "two peers keep quorum sync",
			peers:     []string{"node-a", "node-b"},
			minSync:   1,
			want:      pgmanager.QuorumSync{MinSync: 1, Pool: []pgmanager.NodeID{"node-a", "node-b"}},
			wantLabel: "quorum(min_sync=1)",
		},
		{
			name:      "three peers forward min_sync and pool",
			peers:     []string{"node-a", "node-b", "node-c"},
			minSync:   2,
			want:      pgmanager.QuorumSync{MinSync: 2, Pool: []pgmanager.NodeID{"node-a", "node-b", "node-c"}},
			wantLabel: "quorum(min_sync=2)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := config.Config{Peers: tc.peers}
			cfg.Policy.QuorumSync.MinSync = tc.minSync

			got := replicationPolicy(cfg)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("replicationPolicy = %#v, want %#v", got, tc.want)
			}
			if fromPolicy := policyFromConfig(cfg).Replication; !reflect.DeepEqual(fromPolicy, tc.want) {
				t.Fatalf("policyFromConfig.Replication = %#v, want %#v", fromPolicy, tc.want)
			}
			if label := policyLabel(got); label != tc.wantLabel {
				t.Fatalf("policyLabel = %q, want %q", label, tc.wantLabel)
			}
		})
	}
}
