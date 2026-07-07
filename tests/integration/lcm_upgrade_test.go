// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build integration

// Feature 004 (specs/004-upgrade-tests) — integration tier for the
// upgrade surface: real engine (pg-manager v0.4.1), real PostgreSQL,
// real embedded NATS, via the shared compose harness. Contract
// reference: specs/004-upgrade-tests/contracts/upgrade-endpoints.md.
//
// Forwarding shape: the compose topology runs leader routing in
// `forward` mode, so a request landing on a non-leader peer returns
// an OUTER 200/accepted envelope whose engine_result carries the
// LEADER's envelope verbatim (the relay behavior pinned by the
// contract tier in internal/control/handlers_upgrade_test.go). Every
// assertion here therefore unwraps to the EFFECTIVE envelope first.
//
// The minor happy path uses a same-version plan: v1 wires a no-op
// pre-swap through HTTP (binary swaps are a host concern), so
// targeting the live major.minor exercises the engine's full
// stop → swap → start → version-probe sequence without needing two
// installed PostgreSQL versions (research.md R2). Proof that a real
// restart happened comes from pg_postmaster_start_time() moving on
// exactly one node (the leader's local PostgreSQL).
//
// TestUpgradeMajorExecute_GateSentinel pins the exact upstream gate
// string. When a pg-manager bump lifts or rewords the gate this test
// MUST fail — that is the sentinel firing, not a pgman-proxy
// regression. Follow contracts/upgrade-endpoints.md § "Sentinel
// obligations"; do not loosen the assertion in place.

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	pgmanager "github.com/f1bonacc1/pg-manager"
	"github.com/jackc/pgx/v5"
)

// upgradeGateSentinel is the pinned upstream refusal for
// major-strategy execution (pg-manager v0.4.1
// manager/backup_upgrade.go:102).
const upgradeGateSentinel = "upgrade: major strategies wired but gated on v0.7.0"

// upgradePlanBody marshals the request body for the upgrade endpoints
// using the typed plan so field names can never drift from the module
// (research.md R8).
func upgradePlanBody(t *testing.T, plan pgmanager.UpgradePlan) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"plan": plan})
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	return string(b)
}

// isForwardRelay reports whether outer is a forward-mode relay whose
// engine_result carries the leader's envelope (i.e. the receiving
// peer was not the leader).
func isForwardRelay(outer lcmResponse) bool {
	if len(outer.EngineResult) == 0 {
		return false
	}
	var inner lcmResponse
	if err := json.Unmarshal(outer.EngineResult, &inner); err != nil {
		return false
	}
	return inner.Operation != "" && inner.Outcome != ""
}

// unwrapForwarded returns the leader's envelope when outer is a
// forward-mode relay (engine_result holding a nested envelope), else
// outer itself.
func unwrapForwarded(outer lcmResponse) lcmResponse {
	if !isForwardRelay(outer) {
		return outer
	}
	var inner lcmResponse
	_ = json.Unmarshal(outer.EngineResult, &inner)
	return inner
}

// transientLCM reports whether the effective envelope is a retryable
// startup-window condition rather than a terminal outcome (FR-029
// retryable codes plus the forward-timeout race while leadership
// settles).
func transientLCM(env lcmResponse) bool {
	if env.Error == nil {
		return false
	}
	switch env.Error.Code {
	case "cluster_bootstrapping", "leadership_in_transition",
		"audit_unavailable", "leader_route_timeout":
		return true
	}
	return false
}

// upgradeCall POSTs body to path on the first peer and returns the
// EFFECTIVE envelope (unwrapped from a forward relay), retrying
// transport errors and transient startup conditions until budget
// expires. Terminal accepted/failed envelopes are returned as-is so
// callers assert the operator-visible outcome (FR-013).
func lcmCall(t *testing.T, ctx context.Context, path, body string, budget time.Duration) lcmResponse {
	t.Helper()
	deadline := time.Now().Add(budget)
	last := "no attempt completed"
	for {
		code, raw, err := callLCM(ctx, Peers()[0].Name, "POST", path, integrationToken, body)
		switch {
		case err != nil:
			last = fmt.Sprintf("transport: %v", err)
		default:
			var outer lcmResponse
			if jerr := json.Unmarshal(raw, &outer); jerr != nil {
				last = fmt.Sprintf("decode (status %d): %v body=%s", code, jerr, raw)
				break
			}
			env := unwrapForwarded(outer)
			if !transientLCM(env) {
				return env
			}
			last = fmt.Sprintf("transient: status=%d error=%+v", code, env.Error)
		}
		if time.Now().After(deadline) {
			t.Fatalf("POST %s never reached a terminal envelope within %s; last: %s",
				path, budget, last)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("POST %s: context ended: %v (last: %s)", path, ctx.Err(), last)
		case <-time.After(2 * time.Second):
		}
	}
}

// requireFailedWith asserts the effective envelope is the engine's
// refusal surface: outcome failed, code engine_error, message naming
// the reason.
func requireFailedWith(t *testing.T, env lcmResponse, wantMsg string) {
	t.Helper()
	if env.Outcome != "failed" {
		t.Fatalf("outcome=%q, want failed; %s", env.Outcome, lcmErrString(env))
	}
	if env.Error == nil || env.Error.Code != "engine_error" {
		t.Fatalf("error=%+v, want code engine_error", env.Error)
	}
	if !strings.Contains(env.Error.Message, wantMsg) {
		t.Errorf("message %q missing %q", env.Error.Message, wantMsg)
	}
}

// engineVersion reads the live server_version_num through a proxy
// data-plane listener and decomposes it EXACTLY the way the engine
// does (pg-manager v0.4.1 pgproto.Executor.Version): major =
// num/10000, minor = (num/100)%100 — the pre-PostgreSQL-10 scheme.
// For PostgreSQL ≥ 10 that means the engine reports Minor=0 and the
// real minor release lands in Patch, so upgrade plans MUST be
// expressed in this scheme to validate and to pass the post-swap
// confirm (a same-version plan for 17.x is {TargetMajor:17,
// TargetMinor:0}). Pinned in contracts/upgrade-endpoints.md
// § "Version semantics"; flagged for an upstream fix. num is returned
// for exact before/after comparisons.
func engineVersion(t *testing.T, ctx context.Context) (major, minor, num int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	var lastErr error
	for time.Now().Before(deadline) {
		err := withConn(ctx, Peers()[0].DSN(), func(conn *pgx.Conn) error {
			return conn.QueryRow(ctx,
				"SELECT current_setting('server_version_num')::int").Scan(&num)
		})
		if err == nil {
			return num / 10000, (num / 100) % 100, num
		}
		lastErr = err
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("server version unavailable after 2m: %v", lastErr)
	return 0, 0, 0
}

// localPostmasterStartTimes reads pg_postmaster_start_time() from
// every peer's LOCAL PostgreSQL over its in-container unix socket
// (the LOCAL_DSN path from docker-compose.test.yml). A changed value
// is ground truth that that node's postmaster really restarted.
// Retries while a postmaster is still starting up (crash-recovery /
// streaming catch-up window right after topology boot).
func localPostmasterStartTimes(t *testing.T, ctx context.Context) map[string]string {
	t.Helper()
	out := make(map[string]string, 3)
	deadline := time.Now().Add(2 * time.Minute)
	for _, p := range Peers() {
		for {
			raw, err := dockerComposeOutput(ctx, "exec", "-T", p.Name,
				"psql", "-U", "postgres", "-h", "/var/run/postgresql", "-tA",
				"-c", "SELECT pg_postmaster_start_time()")
			if err == nil {
				out[p.Name] = strings.TrimSpace(string(raw))
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("postmaster start time on %s: %v", p.Name, err)
			}
			time.Sleep(2 * time.Second)
		}
	}
	return out
}

// lcmErrString renders the envelope's error for failure messages.
func lcmErrString(env lcmResponse) string {
	if env.Error == nil {
		return "<no error>"
	}
	return fmt.Sprintf("code=%s message=%q", env.Error.Code, env.Error.Message)
}

// waitForDataPlane polls every peer's proxy listener until a trivial
// query succeeds on all of them (or the budget expires).
func waitForDataPlane(t *testing.T, ctx context.Context, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for _, p := range Peers() {
		for {
			var one int
			err := withConn(ctx, p.DSN(), func(conn *pgx.Conn) error {
				return conn.QueryRow(ctx, "SELECT 1").Scan(&one)
			})
			if err == nil && one == 1 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("data plane on %s not serving within budget: %v", p.Name, err)
			}
			time.Sleep(2 * time.Second)
		}
	}
}

// awaitClusterReady rides out bootstrap until the control plane
// answers status on the first peer.
func awaitClusterReady(t *testing.T, ctx context.Context) {
	t.Helper()
	_, _ = retryLCM(t, ctx, Peers()[0].Name, "GET", "/v1/status", "", 200, 2*time.Minute)
}

// T006 / FR-001 / US1-AS1 — a well-formed minor plan (same major,
// current minor) passes the dry-run with no rejection reasons.
func TestUpgradePrepare_MinorAccepted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	awaitClusterReady(t, ctx)
	major, minor, _ := engineVersion(t, ctx)

	plan := pgmanager.UpgradePlan{
		Strategy:    pgmanager.UpgradeMinor,
		TargetMajor: major,
		TargetMinor: minor,
	}
	env := lcmCall(t, ctx, "/v1/upgrade/prepare", upgradePlanBody(t, plan), 90*time.Second)
	if env.Outcome != "accepted" {
		t.Fatalf("outcome=%q, want accepted; %s", env.Outcome, lcmErrString(env))
	}
	if env.Operation != "PrepareUpgrade" {
		t.Errorf("operation: got %q, want PrepareUpgrade", env.Operation)
	}
	if env.Error != nil {
		t.Errorf("accepted prepare carries an error: %+v", env.Error)
	}
}

// T007 / FR-002, FR-011 / US1-AS2..AS4 — executing the same-version
// minor plan drives the engine's real stop → (no-op) swap → start →
// version-confirm sequence on the leader's local PostgreSQL and
// returns accepted. Proof of the real restart: exactly one node's
// pg_postmaster_start_time() moves; afterwards the whole data plane
// serves queries and the version is unchanged.
func TestUpgradeExecute_MinorHappyPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	awaitClusterReady(t, ctx)
	major, minor, preNum := engineVersion(t, ctx)
	before := localPostmasterStartTimes(t, ctx)

	plan := pgmanager.UpgradePlan{
		Strategy:      pgmanager.UpgradeMinor,
		TargetMajor:   major,
		TargetMinor:   minor,
		PerNodeBudget: 90 * time.Second,
	}
	// Any peer works: forward mode ferries the request to the leader,
	// whose local PostgreSQL restarts during execution.
	env := lcmCall(t, ctx, "/v1/upgrade/execute", upgradePlanBody(t, plan), 3*time.Minute)
	if env.Outcome != "accepted" {
		t.Fatalf("execute outcome=%q, want accepted; %s", env.Outcome, lcmErrString(env))
	}
	if env.Operation != "ExecuteUpgrade" {
		t.Errorf("operation: got %q, want ExecuteUpgrade", env.Operation)
	}

	// US1-AS2: the stop → start sequence really ran, on exactly one
	// node (the leader's local PostgreSQL).
	waitForDataPlane(t, ctx, 3*time.Minute)
	after := localPostmasterStartTimes(t, ctx)
	var restarted []string
	for name, pre := range before {
		if after[name] != pre {
			restarted = append(restarted, name)
		}
	}
	if len(restarted) != 1 {
		t.Errorf("postmaster restarts on %v, want exactly one node (before=%v after=%v)",
			restarted, before, after)
	}

	// US1-AS3: the database reports the expected version.
	_, _, postNum := engineVersion(t, ctx)
	if postNum != preNum {
		t.Errorf("post-upgrade server_version_num %d, want %d", postNum, preNum)
	}
}

// T014 / FR-005 / US2-AS1 — a plan targeting a lower major is refused
// by the dry-run AND by execution (the engine re-validates), naming
// the downgrade; no postmaster restarts (nothing executed).
//
// Scope note: the engine's Validate rejects downgrades at major
// granularity. A same-major lower-minor plan is caught later by the
// post-swap version probe instead — that surfacing path is pinned in
// the contract tier (TestUpgradeExecute_VersionMismatchSurfaced) and
// the probe itself is owned/tested upstream in pg-manager.
func TestUpgradePrepare_RejectsDowngrade(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	awaitClusterReady(t, ctx)
	major, _, _ := engineVersion(t, ctx)
	before := localPostmasterStartTimes(t, ctx)

	plan := pgmanager.UpgradePlan{
		Strategy:    pgmanager.UpgradeMinor,
		TargetMajor: major - 1,
	}
	body := upgradePlanBody(t, plan)

	for _, path := range []string{"/v1/upgrade/prepare", "/v1/upgrade/execute"} {
		env := lcmCall(t, ctx, path, body, 90*time.Second)
		requireFailedWith(t, env, "cannot downgrade")
	}

	// Nothing executed: every postmaster kept its start time.
	after := localPostmasterStartTimes(t, ctx)
	for name, pre := range before {
		if after[name] != pre {
			t.Errorf("postmaster on %s restarted during a refused downgrade", name)
		}
	}
}

// T015 / FR-006 / US2-AS2 + data-model validation rules — a
// minor-strategy plan crossing a major boundary and a plan with an
// unknown strategy value are both refused by the dry-run.
func TestUpgradePrepare_RejectsCrossMajorMinorAndUnknownStrategy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	awaitClusterReady(t, ctx)
	major, _, _ := engineVersion(t, ctx)

	cases := []struct {
		name    string
		plan    pgmanager.UpgradePlan
		wantMsg string
	}{
		{
			name: "minor crossing major boundary",
			plan: pgmanager.UpgradePlan{
				Strategy:    pgmanager.UpgradeMinor,
				TargetMajor: major + 1,
			},
			wantMsg: "minor strategy requires same major",
		},
		{
			name: "unknown strategy value",
			plan: pgmanager.UpgradePlan{
				Strategy:    pgmanager.UpgradeStrategy(9),
				TargetMajor: major,
			},
			wantMsg: "unknown strategy",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := lcmCall(t, ctx, "/v1/upgrade/prepare",
				upgradePlanBody(t, tc.plan), 90*time.Second)
			requireFailedWith(t, env, tc.wantMsg)
		})
	}
}

// T017 / FR-007 / US3-AS1 — a well-formed major-in-place plan
// (strictly higher target major) passes the dry-run today.
func TestUpgradeMajorPrepare_Accepted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	awaitClusterReady(t, ctx)
	major, _, _ := engineVersion(t, ctx)

	plan := pgmanager.UpgradePlan{
		Strategy:    pgmanager.UpgradeMajorInPlace,
		TargetMajor: major + 1,
	}
	env := lcmCall(t, ctx, "/v1/upgrade/prepare", upgradePlanBody(t, plan), 90*time.Second)
	if env.Outcome != "accepted" {
		t.Fatalf("outcome=%q, want accepted; %s", env.Outcome, lcmErrString(env))
	}
	if env.Operation != "PrepareUpgrade" {
		t.Errorf("operation: got %q, want PrepareUpgrade", env.Operation)
	}
}

// T018 / US3-AS2 — major plans whose target is not strictly higher
// are refused.
func TestUpgradeMajorPrepare_RejectsNonHigherMajor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	awaitClusterReady(t, ctx)
	major, _, _ := engineVersion(t, ctx)

	cases := []struct {
		name    string
		target  int
		wantMsg string
	}{
		{"equal major", major, "major strategy requires a higher major"},
		{"lower major", major - 1, "cannot downgrade"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := pgmanager.UpgradePlan{
				Strategy:    pgmanager.UpgradeMajorInPlace,
				TargetMajor: tc.target,
			}
			env := lcmCall(t, ctx, "/v1/upgrade/prepare",
				upgradePlanBody(t, plan), 90*time.Second)
			requireFailedWith(t, env, tc.wantMsg)
		})
	}
}

// T019 / FR-008 / US3-AS3..AS4 — SENTINEL. Executing either major
// strategy fails with the exact upstream gate string and changes
// nothing. If this test fails after a pg-manager bump, the gate has
// lifted (or been reworded): follow
// specs/004-upgrade-tests/contracts/upgrade-endpoints.md § "Sentinel
// obligations" — decide the new contract deliberately; never loosen
// this assertion in place.
func TestUpgradeMajorExecute_GateSentinel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	awaitClusterReady(t, ctx)
	major, _, preNum := engineVersion(t, ctx)
	before := localPostmasterStartTimes(t, ctx)

	for _, tc := range []struct {
		name     string
		strategy pgmanager.UpgradeStrategy
	}{
		{"major in-place", pgmanager.UpgradeMajorInPlace},
		{"major logical-bridge", pgmanager.UpgradeMajorLogicalBridge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := pgmanager.UpgradePlan{
				Strategy:    tc.strategy,
				TargetMajor: major + 1,
			}
			env := lcmCall(t, ctx, "/v1/upgrade/execute",
				upgradePlanBody(t, plan), 90*time.Second)
			if env.Outcome != "failed" {
				t.Fatalf("outcome=%q, want failed; %s", env.Outcome, lcmErrString(env))
			}
			if env.Error == nil || env.Error.Code != "engine_error" {
				t.Fatalf("error=%+v, want code engine_error", env.Error)
			}
			if !strings.Contains(env.Error.Message, upgradeGateSentinel) {
				t.Errorf("SENTINEL: gate message changed upstream.\n got:  %q\n want substring: %q\n"+
					"See contracts/upgrade-endpoints.md § Sentinel obligations before touching this.",
					env.Error.Message, upgradeGateSentinel)
			}
		})
	}

	// US3-AS3: nothing changed — no postmaster restarted, version intact.
	after := localPostmasterStartTimes(t, ctx)
	for name, pre := range before {
		if after[name] != pre {
			t.Errorf("postmaster on %s restarted during gated major execute", name)
		}
	}
	_, _, postNum := engineVersion(t, ctx)
	if postNum != preNum {
		t.Errorf("version changed by gated execute: server_version_num %d, want %d",
			postNum, preNum)
	}
}
