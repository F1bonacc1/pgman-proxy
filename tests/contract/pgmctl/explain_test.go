package pgmctl_contract

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/cmd"
)

// explainMux wires the four endpoints `pgmctl explain` calls. Each
// per-subject test supplies a custom statusBody / doctorBody /
// historyBody so the same scaffolding can drive the happy + NA paths.
func explainMux(t *testing.T, statusBody, doctorBody, historyBody map[string]any) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/version", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r, t) {
			return
		}
		writeEnvelope(w, "Version", map[string]string{"version": "1.0.0", "commit": "abc1234"})
	})
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r, t) {
			return
		}
		writeEnvelope(w, "Status", statusBody)
	})
	mux.HandleFunc("/v1/diagnose", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r, t) {
			return
		}
		writeEnvelope(w, "Diagnose", map[string]any{"Healthy": true})
	})
	mux.HandleFunc("/v1/doctor/run", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r, t) {
			return
		}
		writeEnvelope(w, "DoctorRun", doctorBody)
	})
	mux.HandleFunc("/v1/history", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r, t) {
			return
		}
		writeEnvelope(w, "HistoryQuery", historyBody)
	})
	return mux
}

// TestExplain_CurrentState_Healthy — current-state always applies;
// the diagnosis line should describe a nominal cluster.
func TestExplain_CurrentState_Healthy(t *testing.T) {
	mux := explainMux(t,
		statusHealthy(),
		map[string]any{
			"apiVersion": "pgman-proxy/v1", "kind": "DoctorReport",
			"summary": map[string]int{"pass": 8, "fail": 0, "warn": 0, "unknown": 0, "info": 0},
			"checks":  []any{},
		},
		map[string]any{
			"apiVersion": "pgman-proxy/v1", "kind": "HistoryQueryResult",
			"events": []any{},
		},
	)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	out := runRoot(t, srv.URL, "explain", "current-state", "--no-color")
	for _, want := range []string{"DIAGNOSIS", "cluster nominal", "EVIDENCE", "SUGGESTED NEXT STEPS"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("explain current-state missing %q\n%s", want, out)
		}
	}
}

// TestExplain_FailoverStuck_HealthyCluster_ExitsSubjectNA — the
// subject's premise fails when the cluster is healthy. The CLI must
// exit 4 (EX_SUBJECT_NA) per cli-commands.md § explain.
func TestExplain_FailoverStuck_HealthyCluster_ExitsSubjectNA(t *testing.T) {
	mux := explainMux(t,
		statusHealthy(),
		map[string]any{
			"apiVersion": "pgman-proxy/v1", "kind": "DoctorReport",
			"summary": map[string]int{"pass": 8, "fail": 0},
			"checks":  []any{},
		},
		map[string]any{
			"apiVersion": "pgman-proxy/v1", "kind": "HistoryQueryResult",
			"events": []any{},
		},
	)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	err := execRoot(t, srv.URL, "explain", "failover-stuck", "--no-color")
	if err == nil {
		t.Fatalf("expected non-nil error for not-applicable subject")
	}
	if code := cmd.ExitCodeFromError(err); code != cmd.ExitSubjectNA {
		t.Errorf("exit code = %d, want %d (EX_SUBJECT_NA)", code, cmd.ExitSubjectNA)
	}
}

// TestExplain_FailoverStuck_NoPrimary — when the cluster has no
// primary, the subject applies. Diagnosis must call out the missing
// primary; evidence should cite the status fact.
func TestExplain_FailoverStuck_NoPrimary(t *testing.T) {
	status := statusHealthy()
	engine := status["engine"].(map[string]any)
	engine["PrimaryNodeID"] = ""
	mux := explainMux(t,
		status,
		map[string]any{
			"apiVersion": "pgman-proxy/v1", "kind": "DoctorReport",
			"summary": map[string]int{"pass": 0, "fail": 1, "warn": 0, "unknown": 0, "info": 0},
			"checks": []any{
				map[string]any{
					"name":    "cluster.has-primary",
					"status":  "FAIL",
					"message": "no primary observed",
				},
			},
		},
		map[string]any{
			"apiVersion": "pgman-proxy/v1", "kind": "HistoryQueryResult",
			"events": []any{},
		},
	)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	out := runRoot(t, srv.URL, "explain", "failover-stuck", "--no-color")
	for _, want := range []string{"DIAGNOSIS", "no node has been promoted to primary", "[FAIL]", "cluster.has-primary"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("explain failover-stuck missing %q\n%s", want, out)
		}
	}
}

// TestExplain_JSONOutput — -o json emits the documented three-section
// shape.
func TestExplain_JSONOutput(t *testing.T) {
	mux := explainMux(t,
		statusHealthy(),
		map[string]any{
			"apiVersion": "pgman-proxy/v1", "kind": "DoctorReport",
			"summary": map[string]int{}, "checks": []any{},
		},
		map[string]any{
			"apiVersion": "pgman-proxy/v1", "kind": "HistoryQueryResult",
			"events": []any{},
		},
	)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	out := runRoot(t, srv.URL, "explain", "current-state", "-o", "json")
	var got struct {
		APIVersion string         `json:"apiVersion"`
		Kind       string         `json:"kind"`
		Payload    map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out)
	}
	if got.Kind != "Explain" {
		t.Errorf("kind = %v, want Explain", got.Kind)
	}
	for _, key := range []string{"diagnosis", "evidence", "suggested_next_steps"} {
		if _, ok := got.Payload[key]; !ok {
			t.Errorf("payload missing field %q (got keys: %v)", key, mapKeys(got.Payload))
		}
	}
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestExplain_UnknownSubject — gibberish subject must error (not
// EX_SUBJECT_NA — that's reserved for "premise didn't match cluster").
func TestExplain_UnknownSubject(t *testing.T) {
	mux := explainMux(t, statusHealthy(), map[string]any{}, map[string]any{})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	err := execRoot(t, srv.URL, "explain", "bogus-subject")
	if err == nil {
		t.Fatalf("expected error for unknown subject")
	}
	if code := cmd.ExitCodeFromError(err); code == cmd.ExitSubjectNA {
		t.Errorf("unknown subject should NOT exit %d (EX_SUBJECT_NA)", code)
	}
}
