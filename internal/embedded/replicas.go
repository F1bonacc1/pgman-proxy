// Package embedded owns the in-process NATS server that pgman-proxy
// boots alongside its data-plane and control-plane listeners. It
// replaces feature 001's external-NATS dependency per
// specs/002-embedded-nats-cluster/spec.md.
//
// The package is strictly a scaffold around the upstream
// github.com/nats-io/nats-server/v2 module: configuration assembly
// (options.go), credential plumbing (nkey.go), replication-factor
// derivation (this file), pre-creation of the cluster KV bucket
// (bucket.go), and process-lifecycle wiring (server.go). No NATS
// internals are reimplemented or forked.
package embedded

import "fmt"

// DeriveReplicas implements the FR-011a / RD-004 derivation table:
//
//	declared_size  | replicas | peer-loss tolerance
//	---------------+----------+--------------------
//	          == 1 |        1 | 0
//	          == 2 |        2 | 0 (degraded; emit warning)
//	          >= 3 |        3 | 1 (capped — JetStream's safe maximum is 5
//	                            but a coordination plane gains nothing
//	                            from R=4 / R=5)
//
// declaredSize <= 0 is an invariant violation (validation should have
// rejected it); the function panics so the bug surfaces immediately
// rather than silently degrading to R=1.
//
// The returned warning is non-empty for the degraded R=2 case; callers
// MUST log it at startup so operators can see the topology is below
// production-recommended size.
func DeriveReplicas(declaredSize int) (replicas int, warning string) {
	switch {
	case declaredSize <= 0:
		panic(fmt.Sprintf("embedded.DeriveReplicas: declared cluster size must be >= 1, got %d", declaredSize))
	case declaredSize == 1:
		return 1, ""
	case declaredSize == 2:
		return 2, "two-peer cluster has zero peer-loss tolerance; scale to 3 ASAP"
	default:
		return 3, ""
	}
}

// ReplicaDecision bundles the derived replica count and the override
// state so callers can emit a single startup log line / metric set
// covering both pieces of state.
type ReplicaDecision struct {
	DeclaredSize int
	Derived      int
	Override     int    // 0 = no override, otherwise the operator-supplied value
	Warning      string // populated for the degraded R=2 case (or by override audit, below)
}

// Effective returns the replica count actually used for stream/bucket
// creation: the override if non-zero, otherwise the derived value.
func (d ReplicaDecision) Effective() int {
	if d.Override > 0 {
		return d.Override
	}
	return d.Derived
}

// Overridden reports whether an operator-supplied replication-factor
// override is in effect (drives the `replicas_overridden` metric label
// per contracts/observability.md).
func (d ReplicaDecision) Overridden() bool { return d.Override > 0 }

// DecideReplicas wraps DeriveReplicas with operator-override handling.
// When override is > 0 it wins, and an audit-line warning is appended
// to the decision so the override is logged loudly at every startup
// (FR-011a).
func DecideReplicas(declaredSize, override int) ReplicaDecision {
	derived, warn := DeriveReplicas(declaredSize)
	d := ReplicaDecision{DeclaredSize: declaredSize, Derived: derived, Warning: warn}
	if override > 0 {
		d.Override = override
		audit := fmt.Sprintf("cluster.replication_factor_override=%d in effect (derived would have been %d for declared_size=%d)",
			override, derived, declaredSize)
		if d.Warning != "" {
			d.Warning = d.Warning + "; " + audit
		} else {
			d.Warning = audit
		}
	}
	return d
}
