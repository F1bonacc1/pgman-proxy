// FR-009 leak audit — verifies that no code path the control plane
// touches at request time emits the live bearer token to log,
// envelope, or audit record. Test design: drive the server with a
// distinctive token, capture stdout + structured logs + audit-sink
// publishes, assert the literal string never appears.
//
// Spec ref: spec.md § Requirements / FR-009 — "System MUST NOT log,
// print, dump, or otherwise emit the bearer token."

package control

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/f1bonacc1/pgman-proxy/internal/obs"
)

// captureLogger builds a Logger that writes structured records into
// a bytes.Buffer so the test can grep the output afterward.
func captureLogger(t *testing.T, buf *bytes.Buffer) *obs.Logger {
	t.Helper()
	return obs.NewLogger(buf, "debug", "test-cluster", "test-node", "control")
}

// capturedAuditPub is a fakeNATS that exposes its publishes via the
// existing test helper (mu / published / setErr). One token-search
// helper covers both the slog output and the audit JSON.
func newCapturedAuditPub() *fakeNATS { return &fakeNATS{} }

// const distinctive enough not to collide with anything legitimate
// in the audit format (`bearer:<sha256-prefix>` etc.).
const fr009Token = "SUPER-SECRET-OPERATOR-TOKEN-DO-NOT-LEAK-12345-67890"

// newLeakAuditServer builds a Server with capture sinks and a token
// value identical to fr009Token. The Authenticator's env hook is
// replaced so we don't depend on real env vars.
func newLeakAuditServer(t *testing.T, logBuf *bytes.Buffer, pub *fakeNATS) *Server {
	t.Helper()
	logger := captureLogger(t, logBuf)
	metrics := obs.NewMetrics("test-cluster", "test-node")
	audit := NewAudit("test-cluster", logger, pub, metrics)
	auth := NewAuthenticator("PGMAN_PROXY_TEST_TOKEN", "", false)
	auth.getEnv = func(_ string) string { return fr009Token }
	srv, err := NewServer(Config{
		Addr:      "127.0.0.1:0",
		Auth:      auth,
		Audit:     audit,
		Router:    NewLeaderRouter("redirect", 0, "test-cluster", nil, &fakeLeader{leader: true}),
		Engine:    &fakeEngine{},
		Logger:    logger,
		Metrics:   metrics,
		ClusterID: "test-cluster",
		NodeID:    "test-node",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv
}

// auditPayloadDump returns every audit payload pub captured,
// concatenated with newlines so the test can grep across them.
func auditPayloadDump(pub *fakeNATS) string {
	pub.mu.Lock()
	defer pub.mu.Unlock()
	parts := make([]string, len(pub.published))
	for i, p := range pub.published {
		parts[i] = string(p)
	}
	return strings.Join(parts, "\n")
}

// assertNoTokenLeak asserts the literal token never appears in any
// of the captured outputs. Fails the test with a precise excerpt
// if it does.
func assertNoTokenLeak(t *testing.T, where string, captured string) {
	t.Helper()
	if !strings.Contains(captured, fr009Token) {
		return
	}
	// Excerpt the surrounding context so the test failure points at
	// the offending line without dumping the whole buffer.
	idx := strings.Index(captured, fr009Token)
	start := idx - 80
	if start < 0 {
		start = 0
	}
	end := idx + len(fr009Token) + 80
	if end > len(captured) {
		end = len(captured)
	}
	t.Fatalf("FR-009 leak in %s: token literal found at offset %d\ncontext: ...%s...",
		where, idx, captured[start:end])
}

// 1. Happy path — Authorization: Bearer <token> succeeds, no leak in
// logs or audit. Should never reveal the token even though both
// pipelines actively log on success.
func TestFR009_NoLeakOnAcceptedRequest(t *testing.T) {
	var logBuf bytes.Buffer
	pub := newCapturedAuditPub()
	srv := newLeakAuditServer(t, &logBuf, pub)

	req, _ := http.NewRequest(http.MethodGet, "/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+fr009Token)
	w := newCapturingResponseWriter()
	srv.Handler().ServeHTTP(w, req)
	if w.code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.code, w.body.String())
	}
	assertNoTokenLeak(t, "log", logBuf.String())
	assertNoTokenLeak(t, "envelope", w.body.String())
	assertNoTokenLeak(t, "audit", auditPayloadDump(pub))
}

// 2. Refused path — wrong token; the supplied invalid value MUST NOT
// land in logs / audit / envelope either, even though the failure
// path is the most common place credentials get accidentally
// surfaced (operators want to "log what they tried").
func TestFR009_NoLeakOnRefusedRequest(t *testing.T) {
	var logBuf bytes.Buffer
	pub := newCapturedAuditPub()
	srv := newLeakAuditServer(t, &logBuf, pub)

	wrongToken := "ANOTHER-DISTINCTIVE-CRED-VALUE-9999-NOPE"
	req, _ := http.NewRequest(http.MethodGet, "/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+wrongToken)
	w := newCapturingResponseWriter()
	srv.Handler().ServeHTTP(w, req)
	if w.code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403; body=%s", w.code, w.body.String())
	}

	combined := strings.Join([]string{logBuf.String(), w.body.String(), auditPayloadDump(pub)}, "\n")
	if strings.Contains(combined, wrongToken) {
		t.Fatalf("FR-009 leak: wrong-token literal found in log / envelope / audit:\n%s", combined)
	}
	// Also verify the EXPECTED token didn't leak (in case Verify's
	// constant-time compare accidentally surfaces the truth on
	// mismatch via a log line).
	assertNoTokenLeak(t, "combined", combined)
}

// 3. Mutating path — Fence carries a body and produces a full audit
// record. Same invariant.
func TestFR009_NoLeakOnMutatingRequest(t *testing.T) {
	var logBuf bytes.Buffer
	pub := newCapturedAuditPub()
	srv := newLeakAuditServer(t, &logBuf, pub)

	req, _ := http.NewRequest(http.MethodPost, "/v1/fence", strings.NewReader(`{"target":"node-x"}`))
	req.Header.Set("Authorization", "Bearer "+fr009Token)
	w := newCapturingResponseWriter()
	srv.Handler().ServeHTTP(w, req)
	if w.code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.code, w.body.String())
	}
	assertNoTokenLeak(t, "log (mutating)", logBuf.String())
	assertNoTokenLeak(t, "envelope (mutating)", w.body.String())
	assertNoTokenLeak(t, "audit (mutating)", auditPayloadDump(pub))
}

// 4. Sink-failure path — audit pipeline broken. Even when both
// sinks publish an error log line, neither leaks the token.
func TestFR009_NoLeakOnAuditSinkFailure(t *testing.T) {
	var logBuf bytes.Buffer
	pub := newCapturedAuditPub()
	pub.setErr(errors.New("nats unavailable"))
	srv := newLeakAuditServer(t, &logBuf, pub)

	req, _ := http.NewRequest(http.MethodGet, "/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+fr009Token)
	w := newCapturingResponseWriter()
	srv.Handler().ServeHTTP(w, req)
	// Read endpoints don't gate on the audit pipeline; just verify
	// the response came back and nothing leaked.
	assertNoTokenLeak(t, "log (sink-fail)", logBuf.String())
	assertNoTokenLeak(t, "envelope (sink-fail)", w.body.String())
	assertNoTokenLeak(t, "audit (sink-fail)", auditPayloadDump(pub))
}

// capturingResponseWriter records body + headers for the test.
type capturingResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	code   int
	once   sync.Once
}

func newCapturingResponseWriter() *capturingResponseWriter {
	return &capturingResponseWriter{header: http.Header{}}
}

func (c *capturingResponseWriter) Header() http.Header        { return c.header }
func (c *capturingResponseWriter) Write(b []byte) (int, error) { return c.body.Write(b) }
func (c *capturingResponseWriter) WriteHeader(code int) {
	c.once.Do(func() { c.code = code })
}

// Ensure compile-time satisfaction.
var (
	_ http.ResponseWriter = (*capturingResponseWriter)(nil)
	_ io.Writer           = (*bytes.Buffer)(nil)
	_ atomic.Bool         = atomic.Bool{}
)
