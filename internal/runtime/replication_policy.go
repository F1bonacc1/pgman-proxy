// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

package runtime

import (
	"fmt"

	pgmanager "github.com/f1bonacc1/pg-manager"

	"github.com/f1bonacc1/pgman-proxy/internal/config"
)

// replicationPolicy derives the pg-manager replication policy from the
// configured peer set. A single peer cannot have a synchronous standby,
// so it runs AsyncOnly. QuorumSync with a pool of one renders a
// self-referential synchronous_standby_names that hangs every commit,
// and never publishes the quorum snapshot that promotion needs, so a
// restarted single node would stay read-only forever (spec 005, F1/F2).
//
// The mode is derived rather than configured, the same way replicas
// are derived from cluster.declared_size: there is no working
// alternative for one peer (Constitution VII, research R-001).
func replicationPolicy(cfg config.Config) pgmanager.ReplicationPolicy {
	if len(cfg.Peers) == 1 {
		return pgmanager.AsyncOnly{}
	}
	return pgmanager.QuorumSync{
		MinSync: cfg.Policy.QuorumSync.MinSync,
		Pool:    nodeIDs(cfg.Peers),
	}
}

// policyLabel is the stable `replication_policy` log value
// (contracts/observability.md): "async" or "quorum(min_sync=N)".
func policyLabel(p pgmanager.ReplicationPolicy) string {
	switch v := p.(type) {
	case pgmanager.AsyncOnly:
		return "async"
	case pgmanager.QuorumSync:
		return fmt.Sprintf("quorum(min_sync=%d)", v.MinSync)
	case pgmanager.FirstSync:
		return fmt.Sprintf("first(min_sync=%d)", v.MinSync)
	default:
		return fmt.Sprintf("%T", p)
	}
}
