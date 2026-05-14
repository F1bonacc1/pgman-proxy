// Doctor check registry (T071 / FR-022).
//
// Each entry is a NamedCheck whose Run function MUST be read-only — it
// MAY call Engine.Status / Engine.Diagnose and MAY interpret the
// returned data, but MUST NOT invoke any LCM mutator. The read-only
// invariant is enforced by doctor_checks_readonly_test.go (T075).
//
// The v1 catalog is a subset of FR-022's 18-check goal: cluster-shape
// invariants and per-peer reachability that are derivable from the
// existing Status / Diagnose API surface. Checks needing data the
// proxy can't yet retrieve (disk metrics, clock skew, TLS expiry,
// backup recency) are scheduled for follow-up phases; their absence
// here is documented in MISSING_CHECKS.

package control

import (
	"context"
	"fmt"
	"strings"
	"time"

	pgmanager "github.com/f1bonacc1/pg-manager"
)

// CheckFunc runs one named check against the live engine. Returns
// (status, message, evidence). It MUST NOT mutate engine state.
type CheckFunc func(ctx context.Context, e Engine) (Severity, string, map[string]any)

// NamedCheck is one registry entry.
type NamedCheck struct {
	Name           string
	Description    string
	EvidenceSchema string
	SuggestedFix   *SuggestedFix
	Run            CheckFunc
}

// DefaultDoctorChecks returns the v1 catalog. Callers should treat the
// returned slice as immutable — pgmctl's check-by-name lookup relies
// on insertion order being stable across calls.
func DefaultDoctorChecks() []NamedCheck {
	return []NamedCheck{
		{
			Name:           "cluster.has-primary",
			Description:    "Exactly one node is reported as the cluster primary.",
			EvidenceSchema: "doctor.evidence.cluster-shape/v1",
			Run:            checkClusterHasPrimary,
		},
		{
			Name:           "cluster.has-leader",
			Description:    "The leadership KV reports a current leader.",
			EvidenceSchema: "doctor.evidence.cluster-shape/v1",
			Run:            checkClusterHasLeader,
		},
		{
			Name:           "cluster.quorum",
			Description:    "Reachable peers ≥ floor(N/2)+1 for the declared cluster size.",
			EvidenceSchema: "doctor.evidence.cluster-shape/v1",
			Run:            checkClusterQuorum,
		},
		{
			Name:           "nodes.all-reachable",
			Description:    "Every configured peer responded to the per-peer status fan-out.",
			EvidenceSchema: "doctor.evidence.peer-reachability/v1",
			Run:            checkNodesAllReachable,
		},
		{
			Name:           "nodes.no-failed-state",
			Description:    "No peer is in StateFailed (pg-manager's operator-sticky terminal state).",
			EvidenceSchema: "doctor.evidence.peer-state/v1",
			SuggestedFix: &SuggestedFix{
				Name:           "investigate-failed-node",
				Description:    "Inspect the failed peer's PG + pg-manager logs; the failed state is operator-sticky and won't self-clear.",
				BlastRadius:    BlastAdvisory,
				AppliesToCheck: "nodes.no-failed-state",
				ApplyEndpoint:  "/v1/doctor/fix",
			},
			Run: checkNodesNoFailedState,
		},
		{
			Name:           "postgres.responding",
			Description:    "Every peer reports postgres_up = true.",
			EvidenceSchema: "doctor.evidence.peer-state/v1",
			Run:            checkPostgresResponding,
		},
		{
			Name:           "replication.lag-acceptable",
			Description:    "No standby's lag_bytes exceeds the warn / fail thresholds.",
			EvidenceSchema: "doctor.evidence.replication-lag/v1",
			SuggestedFix: &SuggestedFix{
				Name:           "kick-replication",
				Description:    "Restart the lagging standby's replication client to clear a stalled WAL receiver.",
				BlastRadius:    BlastAdvisory, // v1 advisory; apply path lands when /v1/restart wires up
				AppliesToCheck: "replication.lag-acceptable",
				ApplyEndpoint:  "/v1/doctor/fix",
			},
			Run: checkReplicationLag,
		},
		{
			Name:           "engine.diagnose-clean",
			Description:    "pg-manager Diagnose returns Healthy=true.",
			EvidenceSchema: "doctor.evidence.diagnose/v1",
			Run:            checkEngineDiagnose,
		},
	}
}

// MISSING_CHECKS documents the 10 v1-target checks not yet implemented.
// Each entry names the data path that needs to exist before the check
// can return anything but UNKNOWN.
var MISSING_CHECKS = []string{
	"replication.all-streaming     // needs per-standby pg_stat_replication.state on the primary",
	"replication.no-wal-gaps       // needs primary LSN + per-standby restart_lsn",
	"slots.no-orphans              // needs primary's pg_replication_slots vs cluster peers",
	"slots.not-bloated             // needs slot retained_bytes",
	"disk.has-space                // needs node-side disk usage probe",
	"disk.wal-not-filling          // needs PG WAL dir growth rate",
	"clock.skew-acceptable         // needs per-peer NTP-like skew probe",
	"postgres.version-consistent   // needs per-peer SELECT version()",
	"tls.certs-valid               // needs per-peer cert expiry inspection",
	"backups.recent                // needs backup catalog visibility",
}

// --- check implementations ---

func checkClusterHasPrimary(ctx context.Context, e Engine) (Severity, string, map[string]any) {
	st, err := e.Status(ctx)
	if err != nil {
		return SeverityUnknown, "Status: " + err.Error(), nil
	}
	primaries := 0
	primaryID := ""
	for _, inst := range st.Instances {
		if inst.Role == pgmanager.RolePrimary {
			primaries++
			primaryID = string(inst.NodeID)
		}
	}
	ev := map[string]any{
		"primary_count":   primaries,
		"primary_node_id": primaryID,
	}
	switch {
	case primaries == 1:
		return SeverityPass, fmt.Sprintf("primary=%s", primaryID), ev
	case primaries == 0:
		return SeverityFail, "no primary observed", ev
	default:
		return SeverityFail, fmt.Sprintf("%d primaries observed (split-brain)", primaries), ev
	}
}

func checkClusterHasLeader(ctx context.Context, e Engine) (Severity, string, map[string]any) {
	st, err := e.Status(ctx)
	if err != nil {
		return SeverityUnknown, "Status: " + err.Error(), nil
	}
	ev := map[string]any{"leader_node_id": string(st.LeaderNodeID)}
	if st.LeaderNodeID == "" {
		return SeverityFail, "no leader (KV lease unowned)", ev
	}
	return SeverityPass, fmt.Sprintf("leader=%s", st.LeaderNodeID), ev
}

func checkClusterQuorum(ctx context.Context, e Engine) (Severity, string, map[string]any) {
	st, err := e.Status(ctx)
	if err != nil {
		return SeverityUnknown, "Status: " + err.Error(), nil
	}
	total := len(st.Instances)
	reachable := 0
	for _, inst := range st.Instances {
		if inst.State != pgmanager.StateUnknown {
			reachable++
		}
	}
	quorum := total/2 + 1
	ev := map[string]any{
		"reachable_count":  reachable,
		"total_count":      total,
		"required_quorum":  quorum,
	}
	if total == 0 {
		return SeverityUnknown, "no peer information in Status (aggregator unwired?)", ev
	}
	if reachable >= quorum {
		return SeverityPass, fmt.Sprintf("%d/%d reachable (quorum=%d)", reachable, total, quorum), ev
	}
	return SeverityFail, fmt.Sprintf("only %d/%d reachable (need %d for quorum)", reachable, total, quorum), ev
}

func checkNodesAllReachable(ctx context.Context, e Engine) (Severity, string, map[string]any) {
	st, err := e.Status(ctx)
	if err != nil {
		return SeverityUnknown, "Status: " + err.Error(), nil
	}
	unreachable := []string{}
	for _, inst := range st.Instances {
		if inst.State == pgmanager.StateUnknown {
			unreachable = append(unreachable, string(inst.NodeID))
		}
	}
	ev := map[string]any{"unreachable_nodes": unreachable}
	if len(unreachable) == 0 {
		return SeverityPass, fmt.Sprintf("all %d peers reachable", len(st.Instances)), ev
	}
	return SeverityWarn, fmt.Sprintf("unreachable: %s", strings.Join(unreachable, ", ")), ev
}

func checkNodesNoFailedState(ctx context.Context, e Engine) (Severity, string, map[string]any) {
	st, err := e.Status(ctx)
	if err != nil {
		return SeverityUnknown, "Status: " + err.Error(), nil
	}
	failed := []string{}
	for _, inst := range st.Instances {
		if inst.State == pgmanager.StateFailed {
			failed = append(failed, string(inst.NodeID))
		}
	}
	ev := map[string]any{"failed_nodes": failed}
	if len(failed) == 0 {
		return SeverityPass, "no peer in StateFailed", ev
	}
	return SeverityFail, fmt.Sprintf("failed nodes: %s", strings.Join(failed, ", ")), ev
}

func checkPostgresResponding(ctx context.Context, e Engine) (Severity, string, map[string]any) {
	st, err := e.Status(ctx)
	if err != nil {
		return SeverityUnknown, "Status: " + err.Error(), nil
	}
	down := []string{}
	for _, inst := range st.Instances {
		if inst.State == pgmanager.StateUnknown {
			continue // reachability checked elsewhere
		}
		if !inst.PostgresUp {
			down = append(down, string(inst.NodeID))
		}
	}
	ev := map[string]any{"postgres_down_nodes": down}
	if len(down) == 0 {
		return SeverityPass, "all reachable peers report postgres_up=true", ev
	}
	return SeverityFail, fmt.Sprintf("postgres down on: %s", strings.Join(down, ", ")), ev
}

// Lag thresholds — conservative defaults until the config wire form
// for them (RD-007 follow-up) lands. WARN above 1 MiB, FAIL above 1 GiB.
const (
	replicationLagWarnBytes int64 = 1 << 20
	replicationLagFailBytes int64 = 1 << 30
)

func checkReplicationLag(ctx context.Context, e Engine) (Severity, string, map[string]any) {
	st, err := e.Status(ctx)
	if err != nil {
		return SeverityUnknown, "Status: " + err.Error(), nil
	}
	type laggy struct {
		Node  string `json:"node"`
		Bytes int64  `json:"lag_bytes"`
	}
	var warn, fail []laggy
	for _, inst := range st.Instances {
		if inst.Role != pgmanager.RoleStandby {
			continue
		}
		if inst.LagBytes >= replicationLagFailBytes {
			fail = append(fail, laggy{Node: string(inst.NodeID), Bytes: inst.LagBytes})
		} else if inst.LagBytes >= replicationLagWarnBytes {
			warn = append(warn, laggy{Node: string(inst.NodeID), Bytes: inst.LagBytes})
		}
	}
	ev := map[string]any{
		"warn":        warn,
		"fail":        fail,
		"warn_bytes":  replicationLagWarnBytes,
		"fail_bytes":  replicationLagFailBytes,
	}
	switch {
	case len(fail) > 0:
		return SeverityFail, fmt.Sprintf("%d standby(s) above fail threshold", len(fail)), ev
	case len(warn) > 0:
		return SeverityWarn, fmt.Sprintf("%d standby(s) above warn threshold", len(warn)), ev
	default:
		return SeverityPass, "all standbys within lag thresholds", ev
	}
}

func checkEngineDiagnose(ctx context.Context, e Engine) (Severity, string, map[string]any) {
	dg, err := e.Diagnose(ctx)
	if err != nil {
		return SeverityUnknown, "Diagnose: " + err.Error(), nil
	}
	if dg.Healthy {
		return SeverityPass, "Diagnose reports healthy", map[string]any{"issue_count": 0}
	}
	worst := SeverityWarn
	messages := make([]string, 0, len(dg.Issues))
	for _, iss := range dg.Issues {
		messages = append(messages, fmt.Sprintf("[%s] %s: %s", iss.Severity.String(), iss.Component, iss.Message))
		switch iss.Severity {
		case pgmanager.SeverityError, pgmanager.SeverityCritical:
			worst = SeverityFail
		case pgmanager.SeverityWarning:
			if worst != SeverityFail {
				worst = SeverityWarn
			}
		}
	}
	ev := map[string]any{
		"issue_count": len(dg.Issues),
		"issues":      messages,
	}
	return worst, fmt.Sprintf("%d issue(s) reported", len(dg.Issues)), ev
}

// runChecks executes the supplied check set against the engine,
// stamping the captured timestamp on every result. Used by the
// handler and by tests. Single-named-check runs pass a one-element
// slice; "run all" passes the full registry.
func runChecks(ctx context.Context, e Engine, checks []NamedCheck) []CheckResult {
	now := time.Now().UTC()
	results := make([]CheckResult, 0, len(checks))
	for _, c := range checks {
		status, msg, ev := c.Run(ctx, e)
		r := CheckResult{
			Name:       c.Name,
			Status:     status,
			Message:    msg,
			Evidence:   ev,
			ExecutedAt: now,
		}
		if (status == SeverityFail || status == SeverityWarn) && c.SuggestedFix != nil {
			r.SuggestedFix = c.SuggestedFix
		}
		results = append(results, r)
	}
	return results
}

// catalogFromChecks projects the registry into the wire-form
// DoctorCheck slice served on GET /v1/doctor/checks.
func catalogFromChecks(checks []NamedCheck) []DoctorCheck {
	out := make([]DoctorCheck, 0, len(checks))
	for _, c := range checks {
		out = append(out, DoctorCheck{
			Name:           c.Name,
			Description:    c.Description,
			EvidenceSchema: c.EvidenceSchema,
			SuggestedFix:   c.SuggestedFix,
		})
	}
	return out
}
