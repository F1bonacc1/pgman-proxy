package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"testing"
	"time"

	pgmanager "github.com/f1bonacc1/pg-manager"
	"github.com/f1bonacc1/pg-manager/upgrade"

	"github.com/f1bonacc1/pgman-proxy/internal/obs"
)

// readReportFromEnvelope unwraps the LCM envelope written by
// handleDoctorRun into a DoctorReport for assertion.
func readReportFromEnvelope(t *testing.T, raw []byte) DoctorReport {
	t.Helper()
	var env struct {
		Outcome      string          `json:"outcome"`
		EngineResult json.RawMessage `json:"engine_result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Outcome != OutcomeAccepted {
		t.Fatalf("envelope outcome = %q, want accepted; body=%s", env.Outcome, raw)
	}
	var rep DoctorReport
	if err := json.Unmarshal(env.EngineResult, &rep); err != nil {
		t.Fatalf("decode engine_result: %v", err)
	}
	return rep
}

func TestDoctorChecks_ServesCatalogue(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, &fakeLeader{leader: true}, &fakeNATS{}, "")
	w := doAuthed(t, srv.Handler(), http.MethodGet, "/v1/doctor/checks", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var env struct {
		EngineResult DoctorChecksResponse `json:"engine_result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.EngineResult.Kind != "DoctorChecks" {
		t.Errorf("Kind = %q, want DoctorChecks", env.EngineResult.Kind)
	}
	if len(env.EngineResult.Checks) == 0 {
		t.Errorf("Checks is empty; the v1 catalog must return at least one entry")
	}
	for _, c := range env.EngineResult.Checks {
		if c.Name == "" {
			t.Errorf("Check with empty name in catalog: %+v", c)
		}
		if c.Description == "" {
			t.Errorf("Check %s: empty description", c.Name)
		}
	}
}

func TestDoctorRun_AllChecks_HealthyFixture(t *testing.T) {
	engine := &fakeEngine{
		statusFn: func(_ context.Context) (pgmanager.Status, error) {
			return pgmanager.Status{
				ClusterID:    "test-cluster",
				LeaderNodeID: "node-a",
				Instances: []pgmanager.InstanceStatus{
					{NodeID: "node-a", Role: pgmanager.RolePrimary, State: pgmanager.StateRunning, PostgresUp: true},
					{NodeID: "node-b", Role: pgmanager.RoleStandby, State: pgmanager.StateRunning, PostgresUp: true, LagBytes: 1024},
					{NodeID: "node-c", Role: pgmanager.RoleStandby, State: pgmanager.StateRunning, PostgresUp: true, LagBytes: 2048},
				},
			}, nil
		},
		diagnoseFn: func(_ context.Context) (pgmanager.Diagnosis, error) {
			return pgmanager.Diagnosis{Healthy: true}, nil
		},
	}
	srv := newTestServer(t, engine, &fakeLeader{leader: true}, &fakeNATS{}, "")
	w := doAuthed(t, srv.Handler(), http.MethodPost, "/v1/doctor/run", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	rep := readReportFromEnvelope(t, w.Body.Bytes())
	if rep.Kind != "DoctorReport" {
		t.Errorf("Kind = %q, want DoctorReport", rep.Kind)
	}
	if rep.Summary.Fail != 0 {
		t.Errorf("healthy fixture should produce zero FAILs; summary=%+v", rep.Summary)
	}
	if rep.Summary.Pass+rep.Summary.Info+rep.Summary.Warn != len(rep.Checks) {
		t.Errorf("summary doesn't sum to len(checks): %+v vs %d", rep.Summary, len(rep.Checks))
	}
}

func TestDoctorRun_OneCheck_NameRouting(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, &fakeLeader{leader: true}, &fakeNATS{}, "")
	w := doAuthed(t, srv.Handler(), http.MethodPost, "/v1/doctor/run", `{"check":"cluster.has-leader"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	rep := readReportFromEnvelope(t, w.Body.Bytes())
	if len(rep.Checks) != 1 {
		t.Fatalf("expected single check result, got %d", len(rep.Checks))
	}
	if rep.Checks[0].Name != "cluster.has-leader" {
		t.Errorf("ran %q, want cluster.has-leader", rep.Checks[0].Name)
	}
}

func TestDoctorRun_UnknownCheck_Rejected(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, &fakeLeader{leader: true}, &fakeNATS{}, "")
	w := doAuthed(t, srv.Handler(), http.MethodPost, "/v1/doctor/run", `{"check":"bogus"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestDoctorFix_Always_412_AdvisoryOnly(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, &fakeLeader{leader: true}, &fakeNATS{}, "")
	w := doAuthed(t, srv.Handler(), http.MethodPost, "/v1/doctor/fix", `{"fix":"kick-replication","args":{"node":"node-c"}}`)
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412; body=%s", w.Code, w.Body.String())
	}
	var env struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error == nil || env.Error.Code != CodeAdvisoryOnly {
		t.Errorf("error.code = %v, want %s", env.Error, CodeAdvisoryOnly)
	}
}

// TestDoctorChecks_ReadOnly_Invariant (T075) — runs every registered
// check against a fake engine whose Status / Diagnose snapshots are
// captured before AND after, then asserts the two snapshots are
// byte-identical. Any check that called a mutator would have to go
// through one of the fake's *Fn hooks; we deliberately leave them nil
// so a mutator call returns the type's zero value but doesn't fail the
// test on its own. The invariant is: no observable state change.
// fakeAggregator mirrors a PeerAggregator that stitches a fake
// cluster-wide Instances slice onto the local scalar Status. Used to
// assert handleDoctorRun goes through the aggregator (and is therefore
// consistent with /v1/status / pgmctl health).
type fakeAggregator struct {
	stitched []pgmanager.InstanceStatus
	primary  pgmanager.NodeID
}

func (f *fakeAggregator) EnrichStatus(_ context.Context, local pgmanager.Status) pgmanager.Status {
	local.Instances = f.stitched
	if f.primary != "" {
		local.PrimaryNodeID = f.primary
	}
	return local
}

// TestDoctorRun_UsesPeerAggregator — regression for the "pgmctl health
// vs pgmctl doctor disagree" bug observed in the live fixture. The
// engine's raw Status returns only the per-peer scalar view (Instances
// empty); the aggregator stitches the cluster-wide slice. Without
// routing through the aggregator, every cross-peer check degenerated
// to a single-node lens.
func TestDoctorRun_UsesPeerAggregator(t *testing.T) {
	rawEngine := &fakeEngine{
		statusFn: func(_ context.Context) (pgmanager.Status, error) {
			// Raw per-peer view: scalar fields populated, Instances empty.
			return pgmanager.Status{
				ClusterID:    "test-cluster",
				LeaderNodeID: "node-b",
				LocalNodeID:  "node-a",
				LocalRole:    pgmanager.RoleStandby,
				LocalState:   pgmanager.StateRunning,
			}, nil
		},
		diagnoseFn: func(_ context.Context) (pgmanager.Diagnosis, error) {
			return pgmanager.Diagnosis{Healthy: true}, nil
		},
	}
	agg := &fakeAggregator{
		primary: "node-b",
		stitched: []pgmanager.InstanceStatus{
			{NodeID: "node-a", Role: pgmanager.RoleStandby, State: pgmanager.StateRunning, PostgresUp: true},
			{NodeID: "node-b", Role: pgmanager.RolePrimary, State: pgmanager.StateRunning, PostgresUp: true},
			{NodeID: "node-c", Role: pgmanager.RoleStandby, State: pgmanager.StateRunning, PostgresUp: true},
		},
	}

	logger := obs.NewLogger(io.Discard, "info", "test-cluster", "test-node", "control")
	metrics := obs.NewMetrics("test-cluster", "test-node")
	audit := NewAudit("test-cluster", logger, &fakeNATS{}, metrics)
	auth := NewAuthenticator("PGMAN_PROXY_TEST_TOKEN", "", false)
	auth.getEnv = func(_ string) string { return "secret-token" }
	router := NewLeaderRouter("redirect", time.Second, "test-cluster", nil, &fakeLeader{leader: true})
	srv, err := NewServer(Config{
		Addr:       "127.0.0.1:0",
		Auth:       auth,
		Audit:      audit,
		Router:     router,
		Engine:     rawEngine,
		Aggregator: agg,
		Logger:     logger,
		Metrics:    metrics,
		ClusterID:  "test-cluster",
		NodeID:     "test-node",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	w := doAuthed(t, srv.Handler(), http.MethodPost, "/v1/doctor/run", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	rep := readReportFromEnvelope(t, w.Body.Bytes())
	if rep.Summary.Fail != 0 {
		t.Errorf("aggregator-enriched healthy fixture must produce zero FAILs; summary=%+v", rep.Summary)
	}
	for _, c := range rep.Checks {
		if c.Name == "cluster.has-primary" && c.Status != SeverityPass {
			t.Errorf("cluster.has-primary should PASS once aggregator runs; got %s (%q)", c.Status, c.Message)
		}
		if c.Name == "cluster.quorum" && c.Status == SeverityUnknown {
			t.Errorf("cluster.quorum should NOT be UNKNOWN when aggregator stitches Instances; got %s", c.Message)
		}
	}
}

// TestCheckClusterHasPrimary_IgnoresFailedExPrimary is the regression
// test for the doctor false-positive observed in the chaos rig after
// CR-009c (2026-05-16): once a SIGKILLed primary lands in StateFailed
// with PG down, its Role label stays "primary" until rebootstrap
// drives it back through Standby. The check used to count
// Role==Primary regardless of state, so a healthy failover (one new
// active primary + one ex-primary in Failed/StateFailed) was
// reported as a 2-primary split-brain. The active-primary count must
// require State==Running AND PostgresUp.
func TestCheckClusterHasPrimary_IgnoresFailedExPrimary(t *testing.T) {
	engine := &fakeEngine{
		statusFn: func(_ context.Context) (pgmanager.Status, error) {
			return pgmanager.Status{
				ClusterID:    "test-cluster",
				LeaderNodeID: "node-a",
				Instances: []pgmanager.InstanceStatus{
					// Newly-elected active primary.
					{NodeID: "node-a", Role: pgmanager.RolePrimary, State: pgmanager.StateRunning, PostgresUp: true},
					// Ex-primary: PG SIGKILLed, role label persists, state
					// is Failed, PostgresUp=false. Must NOT count as a
					// second primary.
					{NodeID: "node-b", Role: pgmanager.RolePrimary, State: pgmanager.StateFailed, PostgresUp: false},
					// Healthy standby.
					{NodeID: "node-c", Role: pgmanager.RoleStandby, State: pgmanager.StateRunning, PostgresUp: true},
				},
			}, nil
		},
	}
	sev, msg, ev := checkClusterHasPrimary(context.Background(), engine)
	if sev != SeverityPass {
		t.Errorf("severity = %s, want PASS; msg=%q", sev, msg)
	}
	if got := ev["primary_count"]; got != 1 {
		t.Errorf("primary_count = %v, want 1", got)
	}
	if got := ev["primary_node_id"]; got != "node-a" {
		t.Errorf("primary_node_id = %v, want node-a", got)
	}
	if got := ev["stale_primary_role_ct"]; got != 1 {
		t.Errorf("stale_primary_role_ct = %v, want 1 (node-b's stale role label)", got)
	}
}

// TestCheckClusterHasPrimary_RealSplitBrain confirms a true
// split-brain (two peers both running + PG up + role=primary) still
// FAILs. Guards the regression test above against being so permissive
// that it masks a real two-primary condition.
func TestCheckClusterHasPrimary_RealSplitBrain(t *testing.T) {
	engine := &fakeEngine{
		statusFn: func(_ context.Context) (pgmanager.Status, error) {
			return pgmanager.Status{
				ClusterID: "test-cluster",
				Instances: []pgmanager.InstanceStatus{
					{NodeID: "node-a", Role: pgmanager.RolePrimary, State: pgmanager.StateRunning, PostgresUp: true},
					{NodeID: "node-b", Role: pgmanager.RolePrimary, State: pgmanager.StateRunning, PostgresUp: true},
				},
			}, nil
		},
	}
	sev, msg, _ := checkClusterHasPrimary(context.Background(), engine)
	if sev != SeverityFail {
		t.Errorf("severity = %s, want FAIL; msg=%q", sev, msg)
	}
	if msg != "2 primaries observed (split-brain)" {
		t.Errorf("msg = %q, want %q", msg, "2 primaries observed (split-brain)")
	}
}

func TestDoctorChecks_ReadOnly_Invariant(t *testing.T) {
	baseline := pgmanager.Status{
		ClusterID:    "test-cluster",
		LeaderNodeID: "node-a",
		Instances: []pgmanager.InstanceStatus{
			{NodeID: "node-a", Role: pgmanager.RolePrimary, State: pgmanager.StateRunning, PostgresUp: true},
			{NodeID: "node-b", Role: pgmanager.RoleStandby, State: pgmanager.StateRunning, PostgresUp: true, LagBytes: 512},
		},
	}
	baselineDiagnose := pgmanager.Diagnosis{Healthy: true}

	calls := struct {
		switchover, failover, fence, unfence, updateTopo,
		backup, prepareUpgrade, executeUpgrade int
	}{}

	engine := &fakeEngine{
		statusFn:   func(_ context.Context) (pgmanager.Status, error) { return baseline, nil },
		diagnoseFn: func(_ context.Context) (pgmanager.Diagnosis, error) { return baselineDiagnose, nil },
		switchoverFn: func(_ context.Context, _ pgmanager.NodeID) error {
			calls.switchover++
			return nil
		},
		failoverFn: func(_ context.Context) error { calls.failover++; return nil },
		fenceFn: func(_ context.Context, _ pgmanager.NodeID) error {
			calls.fence++
			return nil
		},
		unfenceFn: func(_ context.Context, _ pgmanager.NodeID) error {
			calls.unfence++
			return nil
		},
		updateTopologyFn: func(_ context.Context, _ pgmanager.Topology, _ pgmanager.Policy) error {
			calls.updateTopo++
			return nil
		},
		triggerBackupFn: func(_ context.Context) (pgmanager.BackupID, error) {
			calls.backup++
			return "", nil
		},
		prepareUpgradeFn: func(_ context.Context, _ pgmanager.UpgradePlan) error {
			calls.prepareUpgrade++
			return nil
		},
		executeUpgradeFn: func(_ context.Context, _ pgmanager.UpgradePlan, _ upgrade.PreSwap) error {
			calls.executeUpgrade++
			return nil
		},
	}

	results := runChecks(context.Background(), engine, DefaultDoctorChecks())
	if len(results) == 0 {
		t.Fatalf("DefaultDoctorChecks returned an empty registry")
	}

	zero := struct {
		switchover, failover, fence, unfence, updateTopo,
		backup, prepareUpgrade, executeUpgrade int
	}{}
	if !reflect.DeepEqual(calls, zero) {
		t.Errorf("FR-027 read-only invariant violated: a check called an LCM mutator. Counts: %+v", calls)
	}
}

// TestCheckClusterIdentity pins feature 005 WI-C4 (FR-016): the check
// fails with both identities as evidence while pg-manager reports an
// identity mismatch, and passes otherwise. fakeEngine injects the
// mismatch (Constitution VI fault-injection carve-out); a real-server
// run sees it absent in tests/scaleout.
func TestCheckClusterIdentity(t *testing.T) {
	ctx := context.Background()
	mismatch := &fakeEngine{statusFn: func(context.Context) (pgmanager.Status, error) {
		return pgmanager.Status{IdentityMismatch: &pgmanager.IdentityMismatch{
			LocalSystemIdentifier: "111", LeaderSystemIdentifier: "222", LeaderNodeID: "node-a",
		}}, nil
	}}
	sev, msg, ev := checkClusterIdentity(ctx, mismatch)
	if sev != SeverityFail {
		t.Fatalf("severity = %v (%s), want fail", sev, msg)
	}
	if ev["local_system_identifier"] != "111" || ev["leader_system_identifier"] != "222" || ev["leader_node_id"] != "node-a" {
		t.Fatalf("evidence = %v", ev)
	}

	if sev, _, _ := checkClusterIdentity(ctx, &fakeEngine{}); sev != SeverityPass {
		t.Fatalf("no mismatch: severity = %v, want pass", sev)
	}
	failing := &fakeEngine{statusFn: func(context.Context) (pgmanager.Status, error) {
		return pgmanager.Status{}, errors.New("engine down")
	}}
	if sev, _, _ := checkClusterIdentity(ctx, failing); sev != SeverityUnknown {
		t.Fatalf("status error: severity = %v, want unknown", sev)
	}

	found := false
	for _, c := range DefaultDoctorChecks() {
		if c.Name == "cluster.identity" {
			found = c.SuggestedFix != nil && c.SuggestedFix.BlastRadius == BlastAdvisory
		}
	}
	if !found {
		t.Fatal("cluster.identity must be registered with an advisory suggested fix")
	}
}

// timelineAggregator is a fakeAggregator that also answers peer
// timelines, as the runtime status aggregator does.
type timelineAggregator struct {
	fakeAggregator
	peers []PeerTimeline
	err   error
}

func (a *timelineAggregator) PeerTimelines(context.Context) ([]PeerTimeline, error) {
	return a.peers, a.err
}

// issue12Divergence is the state pgman-proxy issue #12 observed on
// node-a: timeline 2 WAL to 0/5063878, while timeline 2 ended at
// 0/50637C8 in the history of node-c's timeline 3.
func issue12Divergence() *pgmanager.TimelineDivergence {
	return &pgmanager.TimelineDivergence{
		LocalTimeline:   2,
		LocalEndLSN:     0x5063878,
		ClusterTimeline: 3,
		SwitchLSN:       0x50637C8,
		Evidence:        pgmanager.DivergenceEvidenceSource,
		SourceNodeID:    "node-c",
	}
}

// TestCheckTimelineConsistent pins pgman-proxy issue #12: the check
// fails while any node reports a timeline divergence, from whichever
// node serves it, and lists peers that did not answer without failing
// on them. fakeEngine injects the divergence (Constitution VI
// fault-injection carve-out); scaleout S10 produces a real one.
func TestCheckTimelineConsistent(t *testing.T) {
	ctx := context.Background()
	statusOf := func(st pgmanager.Status, err error) *fakeEngine {
		return &fakeEngine{statusFn: func(context.Context) (pgmanager.Status, error) { return st, err }}
	}

	// A plain engine reports only this node.
	sev, msg, ev := checkTimelineConsistent(ctx, statusOf(pgmanager.Status{LocalNodeID: "node-a", TimelineDivergence: issue12Divergence()}, nil))
	if sev != SeverityFail {
		t.Fatalf("diverged local node: severity = %v (%s), want fail", sev, msg)
	}
	if want := "node-a holds timeline 2 WAL to 0/5063878, past its end at 0/50637C8 in the cluster's timeline 3"; msg != want {
		t.Errorf("message = %q, want %q", msg, want)
	}
	d := ev["diverged"].([]map[string]any)
	if len(d) != 1 || d[0]["node_id"] != "node-a" || d[0]["local_end_lsn"] != "0/5063878" ||
		d[0]["switch_lsn"] != "0/50637C8" || d[0]["evidence"] != "source" || d[0]["source_node_id"] != "node-c" {
		t.Errorf("evidence = %v", ev)
	}
	if sev, msg, _ := checkTimelineConsistent(ctx, statusOf(pgmanager.Status{LocalNodeID: "node-a"}, nil)); sev != SeverityPass {
		t.Errorf("clean local node: severity = %v (%s), want pass", sev, msg)
	}
	if sev, _, _ := checkTimelineConsistent(ctx, statusOf(pgmanager.Status{}, errors.New("engine down"))); sev != SeverityUnknown {
		t.Errorf("status error: severity = %v, want unknown", sev)
	}

	// The doctor handler's engine asks every peer, so a healthy serving
	// node still fails the check for a diverged peer.
	notAncestor := &pgmanager.TimelineDivergence{LocalTimeline: 4, LocalEndLSN: 0x6000000, ClusterTimeline: 3, Evidence: pgmanager.DivergenceEvidenceForkRecord}
	healthy := statusOf(pgmanager.Status{LocalNodeID: "node-b"}, nil)
	cluster := &enrichedEngine{Engine: healthy, agg: &timelineAggregator{peers: []PeerTimeline{
		{NodeID: "node-c", Answered: false},
		{NodeID: "node-b", Answered: true},
		{NodeID: "node-a", Answered: true, Divergence: issue12Divergence()},
		{NodeID: "node-d", Answered: true, Divergence: notAncestor},
	}}}
	sev, msg, ev = checkTimelineConsistent(ctx, cluster)
	if sev != SeverityFail {
		t.Fatalf("diverged peers: severity = %v (%s), want fail", sev, msg)
	}
	if want := "node-a holds timeline 2 WAL to 0/5063878, past its end at 0/50637C8 in the cluster's timeline 3; " +
		"node-d is on timeline 4, which is not in the history of the cluster's timeline 3"; msg != want {
		t.Errorf("message = %q, want %q", msg, want)
	}
	if ev["checked"] != 3 || !reflect.DeepEqual(ev["unanswered"], []string{"node-c"}) || len(ev["diverged"].([]map[string]any)) != 2 {
		t.Errorf("evidence = %v", ev)
	}

	cleanPeers := &enrichedEngine{Engine: healthy, agg: &timelineAggregator{peers: []PeerTimeline{
		{NodeID: "node-a", Answered: true}, {NodeID: "node-b", Answered: true}, {NodeID: "node-c"},
	}}}
	if sev, msg, _ := checkTimelineConsistent(ctx, cleanPeers); sev != SeverityPass || msg != "no divergence reported by 2 node(s); not answering: node-c" {
		t.Errorf("clean peers: %v %q, want pass naming node-c as not answering", sev, msg)
	}
	failing := &enrichedEngine{Engine: healthy, agg: &timelineAggregator{err: errors.New("no responders")}}
	if sev, _, _ := checkTimelineConsistent(ctx, failing); sev != SeverityUnknown {
		t.Errorf("fan-out error: severity = %v, want unknown", sev)
	}

	catalog := DefaultDoctorChecks()
	last := catalog[len(catalog)-1]
	if last.Name != "replication.timeline-consistent" || last.SuggestedFix == nil || last.SuggestedFix.BlastRadius != BlastAdvisory {
		t.Fatalf("replication.timeline-consistent must be registered last with an advisory suggested fix, got %+v", last)
	}
}

// TestDoctorRun_TimelineConsistentAsksPeers pins that POST
// /v1/doctor/run reaches the aggregator's peer timelines, so `pgmctl
// doctor` against any node reports a diverged peer (issue #12).
func TestDoctorRun_TimelineConsistentAsksPeers(t *testing.T) {
	rawEngine := &fakeEngine{statusFn: func(context.Context) (pgmanager.Status, error) {
		return pgmanager.Status{ClusterID: "test-cluster", LocalNodeID: "node-c", LocalRole: pgmanager.RolePrimary}, nil
	}}
	agg := &timelineAggregator{peers: []PeerTimeline{
		{NodeID: "node-a", Answered: true, Divergence: issue12Divergence()},
		{NodeID: "node-c", Answered: true},
	}}
	logger := obs.NewLogger(io.Discard, "info", "test-cluster", "test-node", "control")
	metrics := obs.NewMetrics("test-cluster", "test-node")
	auth := NewAuthenticator("PGMAN_PROXY_TEST_TOKEN", "", false)
	auth.getEnv = func(_ string) string { return "secret-token" }
	srv, err := NewServer(Config{
		Addr:       "127.0.0.1:0",
		Auth:       auth,
		Audit:      NewAudit("test-cluster", logger, &fakeNATS{}, metrics),
		Router:     NewLeaderRouter("redirect", time.Second, "test-cluster", nil, &fakeLeader{leader: true}),
		Engine:     rawEngine,
		Aggregator: agg,
		Logger:     logger,
		Metrics:    metrics,
		ClusterID:  "test-cluster",
		NodeID:     "test-node",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	w := doAuthed(t, srv.Handler(), http.MethodPost, "/v1/doctor/run", `{"check":"replication.timeline-consistent"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	rep := readReportFromEnvelope(t, w.Body.Bytes())
	if len(rep.Checks) != 1 || rep.Checks[0].Status != SeverityFail || rep.Checks[0].SuggestedFix == nil {
		t.Fatalf("report = %+v, want one failing check with its suggested fix", rep.Checks)
	}
}
