// T136 follow-up — pin the flag plumbing for `pgmctl get audit`
// and `pgmctl get events`. The quickstart documents these flags;
// the regression target is the kind of stale-syntax drift that
// surfaced during the original T136 sweep.

package pgmctl_contract

import (
	"bytes"
	"strings"
	"testing"

	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/cmd"
)

// flagAvailable returns whether the named subcommand surfaces a
// given flag in its --help output.
func flagAvailable(t *testing.T, subcommand, flag string) bool {
	t.Helper()
	root := cmd.NewRoot(cmd.BuildInfo{Version: "test", Commit: "test"})
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs(strings.Fields(subcommand + " --help"))
	if err := root.Execute(); err != nil {
		t.Fatalf("%s --help: %v\nstderr=%s", subcommand, err, stderr.String())
	}
	return strings.Contains(stdout.String(), "--"+flag)
}

func TestGetAudit_AcceptsHistoryFlags(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"since", "until", "type", "node", "limit", "cursor", "list-types"} {
		if !flagAvailable(t, "get", flag) {
			t.Errorf("pgmctl get is missing --%s flag (quickstart relies on it)", flag)
		}
	}
}

func TestList_AcceptsHistoryFlags(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"since", "type", "limit"} {
		if !flagAvailable(t, "list", flag) {
			t.Errorf("pgmctl list is missing --%s flag", flag)
		}
	}
}

func TestDescribe_AcceptsHistoryFlags(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"since", "type", "limit"} {
		if !flagAvailable(t, "describe", flag) {
			t.Errorf("pgmctl describe is missing --%s flag", flag)
		}
	}
}
