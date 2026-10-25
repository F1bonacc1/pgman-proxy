package pgmctl_contract

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/cmd"
)

// TestPromote_RefusedBeforeAnyRequest locks the fix for a silent no-op:
// `pgmctl promote` used to POST /v1/promote and print a request id,
// while pg-manager's Promote never promoted anything. It must now
// refuse with EX_USAGE in every form, contact no peer (so it also
// protects against servers that still accept the call), and point at
// the commands that work.
func TestPromote_RefusedBeforeAnyRequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	for _, args := range [][]string{
		{"promote"},
		{"promote", "node-a"},
		{"promote", "--force", "--cluster", "pgman-pc"},
	} {
		stdout, _, err := runFenceCmd(t, srv.URL, args...)
		if err == nil {
			t.Fatalf("%v: must error; stdout=%s", args, stdout)
		}
		if got := cmd.ExitCodeFromError(err); got != cmd.ExitUsage {
			t.Errorf("%v: ExitCode = %d, want %d (EX_USAGE)", args, got, cmd.ExitUsage)
		}
		for _, want := range []string{"not supported", "pgmctl switchover --target", "pgmctl failover"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%v: refusal missing %q; got: %v", args, want, err)
			}
		}
		if strings.Contains(stdout, "request_id=") {
			t.Errorf("%v: printed a request id, implying something was submitted: %s", args, stdout)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("server received %d requests; promote must refuse before contacting any peer", n)
	}
}
