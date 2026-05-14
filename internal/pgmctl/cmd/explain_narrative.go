// Per-subject narrative evaluators (T101).
//
// Each function composes ExplainOutput from the live cluster's
// Status / Diagnose / Doctor / History responses. The pattern: probe
// for the subject's premise, return ErrSubjectNotApplicable if the
// cluster shape says the subject is moot, otherwise interpolate the
// observed facts into the documented Diagnosis / Evidence / Suggested
// Next Steps templates.

package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/doctor"
)

// explainFailoverStuck inspects whether a recent leader election has
// completed end-to-end: there should be a primary AND a leader, both
// non-empty, and the engine.diagnose-clean check should be reporting
// healthy on the path. The subject is "not applicable" when the
// cluster is already healthy.
func explainFailoverStuck(ctx context.Context, app *AppContext) (ExplainOutput, error) {
	engine, _, err := fetchStatus(ctx, app)
	if err != nil {
		return ExplainOutput{}, err
	}
	rep, repErr := fetchDoctorReport(ctx, app)
	// Doctor report failure isn't fatal — we proceed with partial evidence.

	// Premise check: a healthy cluster has no failover to be "stuck" on.
	if engine.PrimaryNodeID != "" && engine.LeaderNodeID != "" &&
		(repErr != nil || rep.Summary.Fail == 0) {
		return ExplainOutput{}, fmt.Errorf("%w: cluster has a primary (%s) and leader (%s); no failover in progress",
			ErrSubjectNotApplicable, engine.PrimaryNodeID, engine.LeaderNodeID)
	}

	var diag string
	switch {
	case engine.LeaderNodeID == "" && engine.PrimaryNodeID == "":
		diag = "no leader and no primary — leader-election has not converged"
	case engine.LeaderNodeID != "" && engine.PrimaryNodeID == "":
		diag = fmt.Sprintf("leader=%s is elected but no node has been promoted to primary", engine.LeaderNodeID)
	case engine.LeaderNodeID == "" && engine.PrimaryNodeID != "":
		diag = fmt.Sprintf("primary=%s exists but no leadership lease is held", engine.PrimaryNodeID)
	default:
		diag = "primary + leader present but the doctor battery reports outstanding failures"
	}

	out := ExplainOutput{
		APIVersion: "pgmctl/v1",
		Kind:       "Explain",
		Subject:    subjFailoverStuck,
		Diagnosis:  diag,
	}
	out.Evidence = append(out.Evidence, ExplainEvidence{
		Source: "status",
		Detail: fmt.Sprintf("primary_node_id=%q leader_node_id=%q", engine.PrimaryNodeID, engine.LeaderNodeID),
	})
	for _, inst := range engine.Instances {
		state := strings.ToLower(string(inst.State))
		if state == "promoting" || state == "demoting" || state == "failed" || state == "fenced" {
			out.Evidence = append(out.Evidence, ExplainEvidence{
				Source: "status",
				Detail: fmt.Sprintf("%s state=%s role=%s postgres_up=%v",
					inst.NodeID, state, strings.ToLower(string(inst.Role)), inst.PostgresUp),
			})
		}
	}
	appendDoctorFailures(&out, rep)
	appendRecentTransitions(ctx, app, &out, 5)

	out.SuggestedNextSteps = []string{
		"pgmctl watch transitions     # tail leader-election + promote/demote",
		"pgmctl doctor                # cross-check what the catalogue thinks is failing",
		"pgmctl events --type LeaderElection --since 10m",
	}
	return out, nil
}

// explainNodeNotPromoting investigates a specific node that should be
// (but isn't yet) the primary. "Not applicable" when the node is
// already primary or the cluster has no in-flight promotion.
func explainNodeNotPromoting(ctx context.Context, app *AppContext, nodeArg string) (ExplainOutput, error) {
	engine, _, err := fetchStatus(ctx, app)
	if err != nil {
		return ExplainOutput{}, err
	}
	if engine.PrimaryNodeID == nodeArg {
		return ExplainOutput{}, fmt.Errorf("%w: %s is already the primary", ErrSubjectNotApplicable, nodeArg)
	}
	var target *instanceStatus
	for i := range engine.Instances {
		if engine.Instances[i].NodeID == nodeArg {
			target = &engine.Instances[i]
			break
		}
	}
	if target == nil {
		return ExplainOutput{}, fmt.Errorf("%w: node %q is not in the cluster Instances slice", ErrSubjectNotApplicable, nodeArg)
	}
	state := strings.ToLower(string(target.State))
	role := strings.ToLower(string(target.Role))

	var diag string
	switch {
	case state == "failed":
		diag = fmt.Sprintf("%s is in StateFailed (operator-sticky); promotion cannot proceed", nodeArg)
	case state == "fenced":
		diag = fmt.Sprintf("%s is fenced; the leader cannot promote a fenced node", nodeArg)
	case !target.PostgresUp:
		diag = fmt.Sprintf("%s's PostgreSQL is not reachable (postgres_up=false)", nodeArg)
	case engine.LeaderNodeID != nodeArg:
		diag = fmt.Sprintf("%s does not currently hold the leadership lease (leader=%s); promotion requires the leader", nodeArg, engine.LeaderNodeID)
	case state == "promoting":
		diag = fmt.Sprintf("%s is mid-promotion; pg_promote may still be running", nodeArg)
	default:
		diag = fmt.Sprintf("%s state=%s role=%s — no obvious blocker; consult the doctor battery", nodeArg, state, role)
	}

	out := ExplainOutput{
		APIVersion: "pgmctl/v1",
		Kind:       "Explain",
		Subject:    subjNodeNotPromoting,
		Diagnosis:  diag,
	}
	out.Evidence = append(out.Evidence, ExplainEvidence{
		Source: "status",
		Detail: fmt.Sprintf("%s role=%s state=%s postgres_up=%v lag_bytes=%d",
			nodeArg, role, state, target.PostgresUp, target.LagBytes),
	})
	if engine.LeaderNodeID != "" {
		out.Evidence = append(out.Evidence, ExplainEvidence{
			Source: "status",
			Detail: fmt.Sprintf("current leader: %s", engine.LeaderNodeID),
		})
	}
	rep, _ := fetchDoctorReport(ctx, app)
	appendDoctorFailures(&out, rep)
	appendNodeTransitions(ctx, app, &out, nodeArg, 5)

	out.SuggestedNextSteps = []string{
		fmt.Sprintf("pgmctl watch node %s         # tail this peer's state transitions", nodeArg),
		fmt.Sprintf("pgmctl events --node %s --since 10m", nodeArg),
		"pgmctl doctor                          # confirm what's blocking promotion",
	}
	return out, nil
}

// explainReplicationBroken investigates a specific standby whose
// replication is failing. "Not applicable" when the standby is
// streaming and within lag thresholds.
func explainReplicationBroken(ctx context.Context, app *AppContext, nodeArg string) (ExplainOutput, error) {
	engine, _, err := fetchStatus(ctx, app)
	if err != nil {
		return ExplainOutput{}, err
	}
	var target *instanceStatus
	for i := range engine.Instances {
		if engine.Instances[i].NodeID == nodeArg {
			target = &engine.Instances[i]
			break
		}
	}
	if target == nil {
		return ExplainOutput{}, fmt.Errorf("%w: node %q is not in the cluster Instances slice", ErrSubjectNotApplicable, nodeArg)
	}
	role := strings.ToLower(string(target.Role))
	state := strings.ToLower(string(target.State))
	if role != "standby" {
		return ExplainOutput{}, fmt.Errorf("%w: %s is role=%s; replication-broken applies to standbys", ErrSubjectNotApplicable, nodeArg, role)
	}

	const fail = int64(1 << 30) // 1 GiB mirrors doctor's replicationLagFailBytes
	const warn = int64(1 << 20) // 1 MiB

	var diag string
	switch {
	case !target.PostgresUp:
		diag = fmt.Sprintf("%s's PostgreSQL is not reachable; WAL receiver cannot run", nodeArg)
	case state == "failed":
		diag = fmt.Sprintf("%s is in StateFailed; replication is halted until the operator investigates", nodeArg)
	case state == "fenced":
		diag = fmt.Sprintf("%s is fenced; replication slot is intentionally paused", nodeArg)
	case target.LagBytes >= fail:
		diag = fmt.Sprintf("%s lag=%d bytes (≥ %d) — replay has stalled or fallen too far behind", nodeArg, target.LagBytes, fail)
	case target.LagBytes >= warn:
		diag = fmt.Sprintf("%s lag=%d bytes (≥ %d) — degraded but not yet broken", nodeArg, target.LagBytes, warn)
	default:
		return ExplainOutput{}, fmt.Errorf("%w: %s state=%s lag=%d bytes — streaming nominally",
			ErrSubjectNotApplicable, nodeArg, state, target.LagBytes)
	}

	out := ExplainOutput{
		APIVersion: "pgmctl/v1",
		Kind:       "Explain",
		Subject:    subjReplicationBroken,
		Diagnosis:  diag,
	}
	out.Evidence = append(out.Evidence, ExplainEvidence{
		Source: "status",
		Detail: fmt.Sprintf("%s state=%s postgres_up=%v lag_bytes=%d replay_lsn=%d",
			nodeArg, state, target.PostgresUp, target.LagBytes, target.ReplayLSN),
	})
	rep, _ := fetchDoctorReport(ctx, app)
	appendDoctorFailures(&out, rep)
	appendNodeTransitions(ctx, app, &out, nodeArg, 5)

	out.SuggestedNextSteps = []string{
		fmt.Sprintf("pgmctl lag --node %s        # current lag breakdown", nodeArg),
		fmt.Sprintf("pgmctl events --node %s --type StateTransition --since 30m", nodeArg),
		"pgmctl doctor                                # full check battery + suggested fixes",
	}
	return out, nil
}

// explainLeaderElection narrates the most recent leader-election
// sequence from the history stream. "Not applicable" when the
// history stream has no leader-election records in the recent window.
func explainLeaderElection(ctx context.Context, app *AppContext) (ExplainOutput, error) {
	engine, _, err := fetchStatus(ctx, app)
	if err != nil {
		return ExplainOutput{}, err
	}
	// Pull recent leader-election + state-transition records.
	events, _ := fetchHistory(ctx, app, "event", []string{"LeaderElection", "StateTransition"}, 50)
	leaderEvents := filterLeaderEvents(events)
	if len(leaderEvents) == 0 {
		return ExplainOutput{}, fmt.Errorf("%w: no leader-election records in the last 30m of the history stream",
			ErrSubjectNotApplicable)
	}

	out := ExplainOutput{
		APIVersion: "pgmctl/v1",
		Kind:       "Explain",
		Subject:    subjLeaderElection,
		Diagnosis: fmt.Sprintf("current leader=%s; %d leader-related event(s) in the recent window",
			engine.LeaderNodeID, len(leaderEvents)),
	}
	for _, ev := range leaderEvents {
		out.Evidence = append(out.Evidence, ExplainEvidence{
			Source:    "history",
			Timestamp: ev.Time.UTC().Format(time.RFC3339Nano),
			Detail:    fmt.Sprintf("%s %s %s", ev.Type, ev.NodeID, summariseDetails(ev.Details)),
			Reference: ev.ID,
		})
	}

	out.SuggestedNextSteps = []string{
		"pgmctl watch transitions    # live state-transition stream",
		"pgmctl status               # current cluster shape",
		"pgmctl get audit --since 30m # operator-initiated LCM during the window",
	}
	return out, nil
}

// explainCurrentState is a "where am I" one-shot rollup. Always
// applies; never returns ErrSubjectNotApplicable.
func explainCurrentState(ctx context.Context, app *AppContext) (ExplainOutput, error) {
	engine, embedded, err := fetchStatus(ctx, app)
	if err != nil {
		return ExplainOutput{}, err
	}
	rep, _ := fetchDoctorReport(ctx, app)

	responding, healthy, total := peerCounts(engine)
	var diag string
	switch {
	case healthy == total:
		diag = fmt.Sprintf("cluster nominal: %d/%d peers healthy, primary=%s, leader=%s",
			healthy, total, engine.PrimaryNodeID, engine.LeaderNodeID)
	case responding < total/2+1:
		diag = fmt.Sprintf("QUORUM AT RISK: only %d/%d peers responding (need %d for majority)",
			responding, total, total/2+1)
	default:
		diag = fmt.Sprintf("degraded: %d/%d healthy / %d/%d responding; primary=%s leader=%s",
			healthy, total, responding, total, engine.PrimaryNodeID, engine.LeaderNodeID)
	}

	out := ExplainOutput{
		APIVersion: "pgmctl/v1",
		Kind:       "Explain",
		Subject:    subjCurrentState,
		Diagnosis:  diag,
	}
	for _, inst := range engine.Instances {
		out.Evidence = append(out.Evidence, ExplainEvidence{
			Source: "status",
			Detail: fmt.Sprintf("%s role=%s state=%s postgres_up=%v lag=%d",
				inst.NodeID, strings.ToLower(string(inst.Role)),
				strings.ToLower(string(inst.State)), inst.PostgresUp, inst.LagBytes),
		})
	}
	if embedded != nil {
		out.Evidence = append(out.Evidence, ExplainEvidence{
			Source: "status",
			Detail: fmt.Sprintf("embedded_nats: ready=%v routes=%d",
				embedded.Ready, embedded.RoutesMeshed),
		})
	}
	appendDoctorFailures(&out, rep)

	out.SuggestedNextSteps = []string{
		"pgmctl topology     # tree view of the cluster shape",
		"pgmctl health       # one-line-per-component rollup",
		"pgmctl doctor       # diagnostic battery",
	}
	return out, nil
}

// explainLastEvent surfaces the most recent record from the history
// stream and surrounds it with context (the doctor's view, the
// preceding 3 records). "Not applicable" only when the history
// stream is empty.
func explainLastEvent(ctx context.Context, app *AppContext) (ExplainOutput, error) {
	events, err := fetchHistory(ctx, app, "", nil, 5)
	if err != nil {
		return ExplainOutput{}, err
	}
	if len(events) == 0 {
		return ExplainOutput{}, fmt.Errorf("%w: history stream is empty in the queried window", ErrSubjectNotApplicable)
	}
	// History list ordering is oldest → newest; pick the tail.
	last := events[len(events)-1]
	out := ExplainOutput{
		APIVersion: "pgmctl/v1",
		Kind:       "Explain",
		Subject:    subjLastEvent,
		Diagnosis: fmt.Sprintf("most recent history record: %s on %s at %s",
			last.Type, last.NodeID, last.Time.UTC().Format(time.RFC3339Nano)),
	}
	out.Evidence = append(out.Evidence, ExplainEvidence{
		Source:    "history",
		Timestamp: last.Time.UTC().Format(time.RFC3339Nano),
		Detail:    fmt.Sprintf("%s %s %s", last.Type, last.NodeID, summariseDetails(last.Details)),
		Reference: last.ID,
	})
	// Preceding context.
	for _, ev := range events[:len(events)-1] {
		out.Evidence = append(out.Evidence, ExplainEvidence{
			Source:    "history",
			Timestamp: ev.Time.UTC().Format(time.RFC3339Nano),
			Detail:    fmt.Sprintf("%s %s", ev.Type, ev.NodeID),
			Reference: ev.ID,
		})
	}

	out.SuggestedNextSteps = []string{
		"pgmctl events --since 30m   # broader window of recent records",
		"pgmctl watch events         # live tail",
		"pgmctl get audit --since 30m # operator-initiated changes",
	}
	return out, nil
}

// --- helpers ---

// appendDoctorFailures cites every FAIL / UNKNOWN check from the
// supplied doctor report. PASS / INFO are omitted to keep the
// evidence section focused on actionable signal.
func appendDoctorFailures(out *ExplainOutput, rep doctor.Report) {
	for _, c := range rep.Checks {
		if c.Status == "FAIL" || c.Status == "WARN" || c.Status == "UNKNOWN" {
			out.Evidence = append(out.Evidence, ExplainEvidence{
				Source:    "doctor",
				Detail:    fmt.Sprintf("%s [%s] %s", c.Name, c.Status, c.Message),
				Reference: c.Name,
			})
		}
	}
}

// appendRecentTransitions pulls the last N StateTransition records
// (any node) into the Evidence list. Best-effort — a history fetch
// failure is absorbed silently so the narrative still renders the
// other evidence sources.
func appendRecentTransitions(ctx context.Context, app *AppContext, out *ExplainOutput, n int) {
	events, err := fetchHistory(ctx, app, "event", []string{"StateTransition"}, n)
	if err != nil {
		return
	}
	for _, ev := range events {
		out.Evidence = append(out.Evidence, ExplainEvidence{
			Source:    "history",
			Timestamp: ev.Time.UTC().Format(time.RFC3339Nano),
			Detail:    fmt.Sprintf("%s %s %s", ev.Type, ev.NodeID, summariseDetails(ev.Details)),
			Reference: ev.ID,
		})
	}
}

// appendNodeTransitions is the per-node sibling of
// appendRecentTransitions: filters history to records whose NodeID
// matches the supplied node argument.
func appendNodeTransitions(ctx context.Context, app *AppContext, out *ExplainOutput, node string, n int) {
	events, err := fetchHistory(ctx, app, "event", []string{"StateTransition"}, n*4)
	if err != nil {
		return
	}
	count := 0
	for _, ev := range events {
		if ev.NodeID != node {
			continue
		}
		out.Evidence = append(out.Evidence, ExplainEvidence{
			Source:    "history",
			Timestamp: ev.Time.UTC().Format(time.RFC3339Nano),
			Detail:    fmt.Sprintf("%s %s", ev.Type, summariseDetails(ev.Details)),
			Reference: ev.ID,
		})
		count++
		if count >= n {
			return
		}
	}
}

// filterLeaderEvents returns the subset of records whose Type matches
// one of the leader-election / promotion family.
func filterLeaderEvents(events []eventRow) []eventRow {
	out := make([]eventRow, 0, len(events))
	for _, ev := range events {
		t := ev.Type
		if t == "LeaderElection" || t == "LeaderChange" ||
			(t == "StateTransition" && (strings.Contains(strings.ToLower(summariseDetails(ev.Details)), "promot") ||
				strings.Contains(strings.ToLower(summariseDetails(ev.Details)), "demot") ||
				strings.Contains(strings.ToLower(summariseDetails(ev.Details)), "leader"))) {
			out = append(out, ev)
		}
	}
	return out
}
