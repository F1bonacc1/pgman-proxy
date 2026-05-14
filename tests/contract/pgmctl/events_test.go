package pgmctl_contract

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/cmd"
)

// TestEvents_Tail_RendersTable asserts that `pgmctl events` renders
// the documented one-line-per-event form (contracts/cli-commands.md
// § events) and queries /v1/history with the right parameters.
func TestEvents_Tail_RendersTable(t *testing.T) {
	var (
		lastSince    string
		lastCategory string
		lastNode     string
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/version", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r, t) {
			return
		}
		writeEnvelope(w, "Version", map[string]string{"version": "1.0.0", "commit": "abc1234"})
	})
	mux.HandleFunc("/v1/history", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r, t) {
			return
		}
		q := r.URL.Query()
		lastSince = q.Get("since")
		lastCategory = q.Get("category")
		lastNode = q.Get("node")
		body := map[string]any{
			"apiVersion": "pgman-proxy/v1",
			"kind":       "HistoryQueryResult",
			"events": []map[string]any{
				{
					"apiVersion": "pgman-proxy/v1",
					"kind":       "HistoryEvent",
					"id":         "01hxy",
					"time":       time.Now().UTC().Format(time.RFC3339Nano),
					"category":   "event",
					"type":       "state_transition",
					"cluster_id": "pgman-pc",
					"node_id":    "node-a",
					"details":    map[string]any{"from": "running", "to": "demoting"},
				},
			},
			"truncated": false,
		}
		writeEnvelope(w, "HistoryQuery", body)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	out := runRoot(t, srv.URL, "events", "--since", "15m", "--node", "node-a", "--no-color")
	for _, want := range []string{"state_transition", "node-a", "from=running", "to=demoting"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("events output missing %q\n%s", want, out)
		}
	}
	if lastSince != "15m0s" && lastSince != "15m" {
		t.Errorf("server saw since=%q, want 15m", lastSince)
	}
	if lastCategory != "event" {
		t.Errorf("server saw category=%q, want event", lastCategory)
	}
	if lastNode != "node-a" {
		t.Errorf("server saw node=%q, want node-a", lastNode)
	}
}

// TestEvents_Empty_RendersNoEventsLine asserts the empty case prints
// a human-friendly line rather than an empty table.
func TestEvents_Empty_RendersNoEventsLine(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/history", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r, t) {
			return
		}
		writeEnvelope(w, "HistoryQuery", map[string]any{
			"apiVersion": "pgman-proxy/v1",
			"kind":       "HistoryQueryResult",
			"events":     []any{},
			"truncated":  false,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	out := runRoot(t, srv.URL, "events", "--no-color")
	if !bytes.Contains(out, []byte("no events in window")) {
		t.Errorf("events empty output unexpected:\n%s", out)
	}
}

// TestGetAudit_RoutesToHistory asserts `pgmctl get audit` sets
// category=audit on the upstream request.
func TestGetAudit_RoutesToHistory(t *testing.T) {
	var capturedCategory string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/history", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r, t) {
			return
		}
		capturedCategory = r.URL.Query().Get("category")
		writeEnvelope(w, "HistoryQuery", map[string]any{
			"apiVersion": "pgman-proxy/v1",
			"kind":       "HistoryQueryResult",
			"events":     []any{},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_ = runRoot(t, srv.URL, "get", "audit", "--no-color")
	if capturedCategory != "audit" {
		t.Errorf("category = %q, want audit", capturedCategory)
	}
}

// TestEvents_JSON_SchemaVersioned asserts -o json wraps the history
// response in the pgmctl/v1 envelope.
func TestEvents_JSON_SchemaVersioned(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/history", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r, t) {
			return
		}
		writeEnvelope(w, "HistoryQuery", map[string]any{
			"apiVersion": "pgman-proxy/v1",
			"kind":       "HistoryQueryResult",
			"events":     []any{},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	out := runRoot(t, srv.URL, "events", "-o", "json")
	var doc struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if doc.APIVersion != "pgmctl/v1" || doc.Kind != "HistoryQueryResult" {
		t.Errorf("doc.APIVersion=%q kind=%q", doc.APIVersion, doc.Kind)
	}
}

// TestEvents_NetworkError_ExitsEX_NETWORK asserts that an unreachable
// /v1/history surfaces as EX_NETWORK (65), not a generic failure.
func TestEvents_NetworkError_ExitsEX_NETWORK(t *testing.T) {
	err := execRoot(t, "http://127.0.0.1:1", "events", "--since", "1m")
	if got := cmd.ExitCodeFromError(err); got != cmd.ExitNetwork {
		t.Errorf("ExitCode = %d, want %d; err=%v", got, cmd.ExitNetwork, err)
	}
}

// silence unused-imports linting in alternative builds.
var (
	_ = url.QueryEscape
	_ = strings.NewReader
)
