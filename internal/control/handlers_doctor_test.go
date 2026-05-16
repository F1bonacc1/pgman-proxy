package control

import (
	"context"
	"encoding/json"
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
		switchover, failover, fence, unfence, promote, updateTopo,
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
		promoteFn: func(_ context.Context) error { calls.promote++; return nil },
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
		switchover, failover, fence, unfence, promote, updateTopo,
		backup, prepareUpgrade, executeUpgrade int
	}{}
	if !reflect.DeepEqual(calls, zero) {
		t.Errorf("FR-027 read-only invariant violated: a check called an LCM mutator. Counts: %+v", calls)
	}
}
