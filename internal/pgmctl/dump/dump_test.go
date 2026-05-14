package dump

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// fakeFetcher dispatches per-path canned responses.
type fakeFetcher struct {
	resp map[string]json.RawMessage
	err  map[string]error
}

func (f *fakeFetcher) GetJSON(_ context.Context, path string) (json.RawMessage, error) {
	if e, ok := f.err[path]; ok && e != nil {
		return nil, e
	}
	if r, ok := f.resp[path]; ok {
		return r, nil
	}
	return nil, errors.New("404 " + path)
}

func TestCollector_RunCapturesOutcomes(t *testing.T) {
	f := &fakeFetcher{
		resp: map[string]json.RawMessage{
			"/v1/status": json.RawMessage(`{"cluster_id":"c","leader_node_id":"a"}`),
		},
		err: map[string]error{
			"/v1/history?category=event":  errors.New("history unreachable"),
		},
	}
	specs := []SliceSpec{
		{Name: "status", Fetch: HTTPSliceFetcher(f, "/v1/status")},
		{Name: "history-events", Fetch: HTTPSliceFetcher(f, "/v1/history?category=event")},
	}
	results := NewCollector(2 * time.Second).Run(context.Background(), specs)
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if results[0].Outcome != OutcomeOK {
		t.Errorf("status outcome = %s, want ok", results[0].Outcome)
	}
	if results[1].Outcome != OutcomeFailed {
		t.Errorf("history-events outcome = %s, want failed", results[1].Outcome)
	}
}

func TestCollector_PerSliceTimeoutFiresWithoutFailingTheWhole(t *testing.T) {
	stalled := SliceSpec{
		Name: "stalled",
		Fetch: func(ctx context.Context) (any, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	fast := SliceSpec{
		Name: "fast",
		Fetch: func(_ context.Context) (any, error) { return map[string]any{"ok": true}, nil },
	}
	results := NewCollector(50 * time.Millisecond).Run(context.Background(), []SliceSpec{stalled, fast})
	if results[0].Outcome != OutcomeFailed {
		t.Errorf("stalled slice should fail: %+v", results[0])
	}
	if results[1].Outcome != OutcomeOK {
		t.Errorf("fast slice should succeed: %+v", results[1])
	}
}

func TestRedactor_NormalScrubsBearerTokensAndPasswords(t *testing.T) {
	r := NewRedactor(RedactNormal)
	in := map[string]any{
		"cluster_id": "prod-east",
		"node_id":    "node-a",
		"password":   "hunter2",
		"log":        "got header Authorization: Bearer abc123_token_value",
	}
	got := r.Apply(in).(map[string]any)
	if got["cluster_id"] != "prod-east" {
		t.Errorf("normal mode should NOT redact cluster_id, got %q", got["cluster_id"])
	}
	if got["node_id"] != "node-a" {
		t.Errorf("normal mode should NOT redact node_id, got %q", got["node_id"])
	}
	if got["password"] != "[REDACTED]" {
		t.Errorf("password field should be redacted, got %q", got["password"])
	}
	if !strings.Contains(got["log"].(string), "Bearer [REDACTED]") {
		t.Errorf("bearer literal should be scrubbed, got %q", got["log"])
	}
}

func TestRedactor_StrictReplacesIdentitiesWithStablePlaceholders(t *testing.T) {
	r := NewRedactor(RedactStrict)
	in := map[string]any{
		"cluster_id": "prod-east",
		"node_id":    "node-a",
		"peer_node_id": "node-a",
		"primary_node_id": "node-b",
		"host":       "10.0.0.5",
	}
	got := r.Apply(in).(map[string]any)
	for k, v := range got {
		if v == in[k] {
			t.Errorf("%s: strict mode should have replaced %q", k, v)
		}
	}
	if got["node_id"] != got["peer_node_id"] {
		t.Errorf("same input should map to same placeholder: %v vs %v", got["node_id"], got["peer_node_id"])
	}
	if got["node_id"] == got["primary_node_id"] {
		t.Errorf("different node ids should map to different placeholders")
	}
	table := r.CorrelationTable()
	if len(table) < 4 {
		t.Errorf("correlation table should record every distinct identity, got %d entries", len(table))
	}
}

func TestWriter_WritesManifestAndSlices(t *testing.T) {
	var buf bytes.Buffer
	m := NewManifest(BuildInfo{Version: "test", Commit: "abc"}, "normal", "c", "127.0.0.1", time.Now())
	w, err := NewWriter(&buf, true, m)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.WriteSlice(SliceResult{Name: "status", Outcome: OutcomeOK, Data: map[string]any{"ok": true}, Duration: 5 * time.Millisecond}); err != nil {
		t.Fatalf("WriteSlice ok: %v", err)
	}
	if err := w.WriteSlice(SliceResult{Name: "doctor", Outcome: OutcomeFailed, Err: errors.New("not implemented"), Duration: time.Millisecond}); err != nil {
		t.Fatalf("WriteSlice failed: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	gzr, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	tr := tar.NewReader(gzr)
	wantFiles := map[string]bool{
		"slices/status.json": false,
		"slices/doctor.json": false,
		"manifest.json":      false,
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		if _, ok := wantFiles[hdr.Name]; ok {
			wantFiles[hdr.Name] = true
		}
		if hdr.Name == "manifest.json" {
			body, _ := io.ReadAll(tr)
			var got Manifest
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("decode manifest: %v", err)
			}
			if len(got.Slices) != 2 {
				t.Errorf("manifest should record 2 slices, got %d", len(got.Slices))
			}
			if got.Outcome() != OutcomePartial {
				t.Errorf("manifest outcome should be partial when one slice failed, got %s", got.Outcome())
			}
		}
	}
	for name, found := range wantFiles {
		if !found {
			t.Errorf("expected tar entry %s, missing", name)
		}
	}
}

func TestWriter_RawTarToStdoutOmitsGzip(t *testing.T) {
	var buf bytes.Buffer
	m := NewManifest(BuildInfo{Version: "test"}, "normal", "c", "", time.Now())
	w, err := NewWriter(&buf, false, m)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.WriteSlice(SliceResult{Name: "x", Outcome: OutcomeOK, Data: 1, Duration: 0}); err != nil {
		t.Fatalf("WriteSlice: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	tr := tar.NewReader(&buf)
	if _, err := tr.Next(); err != nil {
		t.Fatalf("raw tar should parse without gzip wrapping: %v", err)
	}
}

func TestParseRedactLevel(t *testing.T) {
	cases := map[string]RedactLevel{
		"":       RedactNormal,
		"normal": RedactNormal,
		"strict": RedactStrict,
	}
	for in, want := range cases {
		got, err := ParseRedactLevel(in)
		if err != nil {
			t.Errorf("ParseRedactLevel(%q) error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseRedactLevel(%q) = %s, want %s", in, got, want)
		}
	}
	if _, err := ParseRedactLevel("xyz"); err == nil {
		t.Errorf("ParseRedactLevel(xyz) should error")
	}
}

func TestDefaultSpecs_IncludesUnimplementedSlicesAsFailed(t *testing.T) {
	f := &fakeFetcher{
		resp: map[string]json.RawMessage{
			"/v1/status":                 json.RawMessage(`{"ok":true}`),
			"/v1/history?category=event": json.RawMessage(`{"events":[]}`),
			"/v1/history?category=audit": json.RawMessage(`{"events":[]}`),
		},
	}
	specs := DefaultSpecs(f, 0)
	names := make(map[string]bool, len(specs))
	for _, s := range specs {
		names[s.Name] = true
	}
	for _, n := range []string{"status", "topology", "history-events", "history-audit", "doctor", "clock-skew", "config"} {
		if !names[n] {
			t.Errorf("DefaultSpecs should include slice %q", n)
		}
	}
	results := NewCollector(time.Second).Run(context.Background(), specs)
	failed := 0
	for _, r := range results {
		if r.Outcome == OutcomeFailed {
			failed++
		}
	}
	// doctor + clock-skew + config are the three "advertise & fail" slices.
	if failed != 3 {
		t.Errorf("expected 3 failed slices (doctor + clock-skew + config), got %d", failed)
	}
}
