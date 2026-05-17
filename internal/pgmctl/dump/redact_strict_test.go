// T135 — strict-redact removes hosts/IPs/node-ids from dump artifacts.
//
// The Apply walk produces a redacted copy; this test drives the
// strict-mode walk over a representative dump-slice payload and
// asserts no IP, host, node id, or cluster id survives the round
// trip. Distinct from TestRedactor_StrictReplacesIdentitiesWithStablePlaceholders
// (dump_test.go) which checks the placeholder stability invariant;
// this one is the negative-space check — "the original strings MUST
// NOT appear anywhere in the output."

package dump

import (
	"encoding/json"
	"strings"
	"testing"
)

// realisticStatusPayload mirrors the shape of a real /v1/status
// envelope_result a dump would capture: nested struct, mixed
// identifiers across keys, an embedded NATS block.
func realisticStatusPayload() map[string]any {
	return map[string]any{
		"cluster_id":      "prod-east-1",
		"local_role":      "primary",
		"local_state":     "running",
		"leader_node_id":  "node-a",
		"primary_node_id": "node-a",
		"instances": []any{
			map[string]any{
				"node_id": "node-a",
				"host":    "10.0.0.5",
				"addr":    "10.0.0.5:5432",
				"role":    "primary",
				"state":   "running",
			},
			map[string]any{
				"node_id": "node-b",
				"host":    "10.0.0.6",
				"addr":    "10.0.0.6:5432",
				"role":    "standby",
				"state":   "running",
			},
		},
		"embedded_nats": map[string]any{
			"up":                 true,
			"server_name":        "pgman-proxy-node-a",
			"listen_addr":        "127.0.0.1:14222",
			"client_listen_addr": "127.0.0.1:14111",
			"routes_listen_addr": "0.0.0.0:14122",
			"routes_meshed":      2,
		},
		"sync_standbys": "node-b",
		// Free-text log field — strict mode does NOT scrub IPs hiding
		// inside arbitrary strings (it only redacts well-known keys).
		// We assert this explicitly below to make the contract clear.
		"last_log_line": "promoted by 10.0.0.7",
	}
}

func TestStrictRedact_RemovesIPsHostsNodeIDsFromKnownKeys(t *testing.T) {
	r := NewRedactor(RedactStrict)
	in := realisticStatusPayload()
	out := r.Apply(in)
	asJSON, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	dump := string(asJSON)

	// Every identity value used by the input MUST be absent from the
	// strict-redacted output.
	sensitive := []string{
		"prod-east-1",
		"node-a",
		"node-b",
		"10.0.0.5",
		"10.0.0.6",
		"pgman-proxy-node-a",
		"127.0.0.1:14222",
		"127.0.0.1:14111",
		"0.0.0.0:14122",
		"10.0.0.5:5432",
		"10.0.0.6:5432",
	}
	for _, s := range sensitive {
		if strings.Contains(dump, s) {
			t.Errorf("strict-redact leaked %q in output:\n%s", s, dump)
		}
	}

	// The correlation table MUST record every distinct identity
	// originally present so the operator can reverse-map offline.
	table := r.CorrelationTable()
	wantReverse := map[string]bool{
		"prod-east-1":        true,
		"node-a":             true,
		"node-b":             true,
		"10.0.0.5":           true,
		"10.0.0.6":           true,
		"pgman-proxy-node-a": true,
		"127.0.0.1:14222":    true,
		"127.0.0.1:14111":    true,
		"0.0.0.0:14122":      true,
		"10.0.0.5:5432":      true,
		"10.0.0.6:5432":      true,
	}
	have := map[string]bool{}
	for _, original := range table {
		have[original] = true
	}
	for v := range wantReverse {
		if !have[v] {
			t.Errorf("correlation table missing entry for %q (have %d entries)", v, len(table))
		}
	}
}

// Documents a known limitation: free-text fields are NOT scrubbed
// for embedded identities. Operators preparing a dump for external
// sharing should review free-text logs separately. The test pins
// this so a future change that DOES scrub free text doesn't catch
// downstream consumers by surprise.
func TestStrictRedact_FreeTextNotScrubbed_DocumentedLimitation(t *testing.T) {
	r := NewRedactor(RedactStrict)
	in := map[string]any{
		"some_log_message": "node-a promoted at 2026-05-15T00:08:28Z",
	}
	out := r.Apply(in).(map[string]any)
	got := out["some_log_message"].(string)
	if !strings.Contains(got, "node-a") {
		t.Errorf("strict-redact unexpectedly scrubbed free-text 'node-a'; if intentional, update the public-API redaction doc + bump MINOR")
	}
}

// Normal mode preserves identity values; redacts only credentials.
func TestNormalRedact_PreservesIdentitiesScrubsCredentials(t *testing.T) {
	r := NewRedactor(RedactNormal)
	in := map[string]any{
		"cluster_id": "prod-east-1",
		"node_id":    "node-a",
		"password":   "should-be-redacted",
		"api_key":    "should-be-redacted",
	}
	out := r.Apply(in).(map[string]any)
	if out["cluster_id"] != "prod-east-1" {
		t.Errorf("normal mode dropped cluster_id: %q", out["cluster_id"])
	}
	if out["node_id"] != "node-a" {
		t.Errorf("normal mode dropped node_id: %q", out["node_id"])
	}
	if out["password"] != "[REDACTED]" {
		t.Errorf("password not scrubbed: %q", out["password"])
	}
	if out["api_key"] != "[REDACTED]" {
		t.Errorf("api_key not scrubbed: %q", out["api_key"])
	}
}

// Cross-slice references stay correlated: the same node id in two
// different slices maps to the same placeholder so the dump remains
// readable after redaction.
func TestStrictRedact_CrossSliceStability(t *testing.T) {
	r := NewRedactor(RedactStrict)
	a := r.Apply(map[string]any{"node_id": "node-a"}).(map[string]any)
	b := r.Apply(map[string]any{"node_id": "node-a"}).(map[string]any)
	if a["node_id"] != b["node_id"] {
		t.Errorf("same input mapped to different placeholders across slices: %q vs %q",
			a["node_id"], b["node_id"])
	}
}
