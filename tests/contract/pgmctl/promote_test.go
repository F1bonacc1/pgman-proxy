package pgmctl_contract

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/cmd"
)

// pg-manager B-025: `pgmctl promote NODE --accept-data-loss --reason`
// ends a join window whose publisher is lost. It replaced a command that
// printed a request id and promoted nothing.

// Incomplete invocations refuse with EX_USAGE before contacting any
// peer: no node, no data-loss acknowledgment, no reason.
func TestPromote_UsageRefusedBeforeAnyRequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	for args, want := range map[string]string{
		"promote":                                    "NODE is required",
		"promote node-a":                             "--accept-data-loss is required",
		"promote node-a --reason x":                  "--accept-data-loss is required",
		"promote node-a --accept-data-loss":          "--reason is required",
		"promote --force --cluster pgman-pc":         "NODE is required",
		"promote node-a --cancel":                    "--cancel takes no NODE",
		"promote node-a --accept-data-loss --reason": "flag needs an argument",
	} {
		stdout, _, err := runFenceCmd(t, srv.URL, strings.Fields(args)...)
		if err == nil {
			t.Fatalf("%q: must error; stdout=%s", args, stdout)
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %q, want it to contain %q", args, err, want)
		}
		if want != "flag needs an argument" {
			if got := cmd.ExitCodeFromError(err); got != cmd.ExitUsage {
				t.Errorf("%q: ExitCode = %d, want %d (EX_USAGE)", args, got, cmd.ExitUsage)
			}
		}
		if strings.Contains(stdout, "request_id=") {
			t.Errorf("%q: printed a request id, implying something was submitted: %s", args, stdout)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("server received %d requests; usage errors must refuse before contacting any peer", n)
	}
}

const promoteStatusJoinWindow = `{"operation":"Status","request_id":"r0","outcome":"accepted","engine_result":{
 "ClusterID":"pgman-pc","LocalNodeID":"node-b",
 "JoinWindow":{"Publisher":"node-c","RecordRevision":34,"PublisherSilentFor":312000000000,
  "Peers":[
   {"NodeID":"node-a","DataState":"complete","SystemIdentifier":"7431","LSN":67362072,"HasLSN":true},
   {"NodeID":"node-b","DataState":"complete","SystemIdentifier":"7431","LSN":67362032,"HasLSN":true},
   {"NodeID":"node-c","DataState":"complete","SystemIdentifier":"7431","LSN":67362072,"HasLSN":true,"Fenced":false}]}}}`

const promoteStatusNoJoinWindow = `{"operation":"Status","request_id":"r0","outcome":"accepted","engine_result":{
 "ClusterID":"pgman-pc","LocalNodeID":"node-b","LeaderNodeID":"node-a"}}`

// The happy path: preflight from /v1/status, typed confirmation via
// --force --cluster, then one POST carrying the acknowledgment.
func TestPromote_AuthorizesWithAcknowledgment(t *testing.T) {
	var (
		mu   sync.Mutex
		body map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/status":
			_, _ = io.WriteString(w, promoteStatusJoinWindow)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/promotion-authorization":
			mu.Lock()
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Unlock()
			_, _ = io.WriteString(w, `{"operation":"AuthorizePromotion","request_id":"01JBREQ","outcome":"accepted",
			 "engine_result":{"target":"node-a","abandoned_publisher":"node-c","request_id":"01JBREQ",
			  "requested_at":"2026-10-25T08:31:10Z","expires_at":"2026-10-25T08:33:10Z"}}`)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)

	stdout, _, err := runFenceCmd(t, srv.URL, "promote", "node-a", "--accept-data-loss",
		"--reason", "node-c volume lost", "--no-wait", "--force", "--cluster", "pgman-pc")
	if err != nil {
		t.Fatalf("promote: %v\n%s", err, stdout)
	}
	for _, want := range []string{
		"published by node-c", "<- target", "<- publisher", "0 bytes of WAL node-c published",
		"request_id=01JBREQ", "authorized: node-a", "node-c is fenced",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if body["target"] != "node-a" || body["accept_data_loss"] != true || body["reason"] != "node-c volume lost" {
		t.Errorf("POST body = %v", body)
	}
}

// Outside a join window there is nothing to override: exit 4 (subject
// N/A) pointing at switchover and failover, and nothing is posted.
func TestPromote_NoJoinWindow(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		_, _ = io.WriteString(w, promoteStatusNoJoinWindow)
	}))
	t.Cleanup(srv.Close)

	_, _, err := runFenceCmd(t, srv.URL, "promote", "node-a", "--accept-data-loss", "--reason", "x",
		"--force", "--cluster", "pgman-pc")
	if got := cmd.ExitCodeFromError(err); got != cmd.ExitSubjectNA {
		t.Fatalf("ExitCode = %d (%v), want %d", got, err, cmd.ExitSubjectNA)
	}
	for _, want := range []string{"not in a join window", "pgmctl switchover --target", "pgmctl failover"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	if posts.Load() != 0 {
		t.Error("nothing may be posted outside a join window")
	}
}
