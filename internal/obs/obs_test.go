package obs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pgmanager "github.com/f1bonacc1/pg-manager"
)

func TestLogger_EmitsRequiredFields(t *testing.T) {
	var buf SafeBuffer
	l := NewLogger(&buf, "info", "demo", "node-a", "test")
	l.Info("config loaded", pgmanager.Field{Key: "source", Value: "yaml"})
	got := buf.String()
	for _, want := range []string{
		`"cluster_id":"demo"`,
		`"node_id":"node-a"`,
		`"component":"test"`,
		`"msg":"config loaded"`,
		`"source":"yaml"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log missing %q\nfull:\n%s", want, got)
		}
	}
}

func TestLogger_LevelFiltering(t *testing.T) {
	var buf SafeBuffer
	l := NewLogger(&buf, "warn", "demo", "node-a", "test")
	l.Info("should be filtered out")
	l.Warn("should appear")
	got := buf.String()
	if strings.Contains(got, "should be filtered out") {
		t.Errorf("INFO leaked through WARN-level filter:\n%s", got)
	}
	if !strings.Contains(got, "should appear") {
		t.Errorf("WARN missing:\n%s", got)
	}
}

func TestHealth_ReadinessStateMachine(t *testing.T) {
	h := NewHealth()
	if h.Ready() {
		t.Fatal("fresh health should not be ready")
	}
	h.SetNATSUp(true)
	h.SetListenerUp(true)
	if h.Ready() {
		t.Error("ready should require manager readiness too")
	}
	h.SetManagerReady(true)
	if !h.Ready() {
		t.Error("all three signals true → ready=true")
	}
	h.SetNATSUp(false)
	if h.Ready() {
		t.Error("losing NATS should flip /readyz to 503")
	}
}

func TestServer_HealthEndpoints(t *testing.T) {
	h := NewHealth()
	m := NewMetrics("demo", "node-a")
	s := NewServer(":0", h, m.Registry)

	mux := s.srv.Handler

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("/healthz code = %d, want 200", rr.Code)
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz before signals = %d, want 503", rr.Code)
	}
	if rr.Header().Get("Retry-After") != "1" {
		t.Errorf("/readyz missing Retry-After header")
	}

	h.SetNATSUp(true)
	h.SetListenerUp(true)
	h.SetManagerReady(true)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("/readyz after signals = %d, want 200", rr.Code)
	}
}

func TestMetrics_RequiredMetricsRegistered(t *testing.T) {
	m := NewMetrics("demo", "node-a")
	if m.ConnectionsOpen == nil || m.LCMRequestsTotal == nil ||
		m.LeadershipState == nil || m.NATSRoundTrip == nil {
		t.Fatal("expected metrics to be constructed")
	}
	gathered, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	// At minimum the process collector should be present.
	if len(gathered) == 0 {
		t.Error("expected at least one metric family registered")
	}
}

func TestNoopTracer(t *testing.T) {
	tr := NewNoopTracer()
	span, end := tr.StartSpan("op")
	defer end()
	if span.TraceID() != "" || span.SpanID() != "" {
		t.Error("noop tracer should return empty IDs")
	}
}

func TestParseTraceParent(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool // HasTrace
	}{
		{"empty", "", false},
		{"valid_sampled", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01", true},
		{"valid_unsampled", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-00", true},
		{"wrong_version", "01-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01", false},
		{"all_zero_trace", "00-00000000000000000000000000000000-b7ad6b7169203331-01", false},
		{"all_zero_span", "00-0af7651916cd43dd8448eb211c80319c-0000000000000000-01", false},
		{"too_short", "00-0af7651916cd43dd8448eb211c80319c", false},
		{"non_hex", "00-0af7651916cd43dd8448eb211c8031zz-b7ad6b7169203331-01", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseTraceParent(c.in)
			if got.HasTrace() != c.want {
				t.Errorf("HasTrace() = %v, want %v (parsed: %+v)", got.HasTrace(), c.want, got)
			}
		})
	}
}

func TestTraceContext_HeaderRoundTrip(t *testing.T) {
	in := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	tp := ParseTraceParent(in)
	if tp.Header() != in {
		t.Errorf("round-trip mismatch: got %q, want %q", tp.Header(), in)
	}
}

func TestObsServer_TraceparentEcho(t *testing.T) {
	h := NewHealth()
	h.SetNATSUp(true)
	h.SetListenerUp(true)
	h.SetManagerReady(true)
	m := NewMetrics("demo", "node-a")
	s := NewServer(":0", h, m.Registry)

	tp := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("traceparent", tp)
		rr := httptest.NewRecorder()
		s.srv.Handler.ServeHTTP(rr, req)
		if got := rr.Header().Get("traceparent"); got != tp {
			t.Errorf("%s should echo traceparent, got %q want %q", path, got, tp)
		}
	}
}
