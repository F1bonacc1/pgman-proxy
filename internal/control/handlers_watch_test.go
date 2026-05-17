// Tests for /v1/watch/* SSE endpoints (T089/T090 — contract-level).
// Verifies framing, Last-Event-ID resumption, keepalive cadence, and
// gap_marker emission. Uses a real httptest.Server because
// httptest.ResponseRecorder does not implement http.Flusher.

package control

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/f1bonacc1/pgman-proxy/internal/history"
)

// fakeWatch is a channel-backed WatchSubscriber. Tests drive the
// `feed` channel; the subscriber forwards events to the handler.
// closeAll closes all live subscriber channels so the handler's
// gap_marker / drained-stream path fires.
//
// lastOpts is written by the server goroutine inside Watch() and read
// by test goroutines via getLastOpts(); the mutex makes that ordering
// safe under `go test -race`.
type fakeWatch struct {
	feed       chan history.HistoryEvent
	stopCh     chan struct{}
	subscribed chan struct{}

	mu       sync.Mutex
	lastOpts history.WatchOptions
}

func newFakeWatch() *fakeWatch {
	return &fakeWatch{
		feed:       make(chan history.HistoryEvent, 8),
		stopCh:     make(chan struct{}),
		subscribed: make(chan struct{}, 1),
	}
}

func (f *fakeWatch) getLastOpts() history.WatchOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastOpts
}

func (f *fakeWatch) Watch(ctx context.Context, opts history.WatchOptions) (<-chan history.HistoryEvent, <-chan error) {
	f.mu.Lock()
	f.lastOpts = opts
	f.mu.Unlock()
	select {
	case f.subscribed <- struct{}{}:
	default:
	}
	out := make(chan history.HistoryEvent, 8)
	errs := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errs)
		for {
			select {
			case <-ctx.Done():
				return
			case <-f.stopCh:
				return
			case ev, ok := <-f.feed:
				if !ok {
					return
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, errs
}

func (f *fakeWatch) closeAll() { close(f.stopCh) }

// newWatchTestServer builds a Server wired with allow_unauth_reads
// (so the SSE tests skip the bearer-auth dance) and the supplied
// watch subscriber.
func newWatchTestServer(t *testing.T, watch WatchSubscriber, engine Engine) *Server {
	t.Helper()
	srv := newTestServer(t, engine, &fakeLeader{leader: true}, &fakeNATS{}, "")
	srv.auth = NewAuthenticator("", "", true) // allow_unauth_reads
	srv.watch = watch
	return srv
}

// readSSEFrames reads up to n complete frames (event/id/data triples)
// from the body. Returns the parsed frames.
type sseFrame struct {
	event string
	id    string
	data  string
}

func readSSEFrames(t *testing.T, body *bufio.Reader, n int, timeout time.Duration) []sseFrame {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var frames []sseFrame
	var cur sseFrame
	for len(frames) < n {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d frames (got %d)", n, len(frames))
		}
		line, err := body.ReadString('\n')
		if err != nil {
			t.Fatalf("read frame: %v (frames so far: %d)", err, len(frames))
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, ":"):
			// comment / keepalive; skip
			continue
		case line == "":
			if cur.event != "" || cur.data != "" {
				frames = append(frames, cur)
				cur = sseFrame{}
			}
		case strings.HasPrefix(line, "event:"):
			cur.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "id:"):
			cur.id = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		case strings.HasPrefix(line, "data:"):
			cur.data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	return frames
}

func TestWatchEvents_StreamsFramesWithIDAndData(t *testing.T) {
	watch := newFakeWatch()
	srv := newWatchTestServer(t, watch, &fakeEngine{})
	ts := newSSEServer(t, srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/v1/watch/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	resp := doStreamingRequest(t, req)
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("Content-Type=%q, want text/event-stream", got)
	}

	watch.feed <- history.HistoryEvent{ID: "01H1", Type: "state_transition", NodeID: "node-a"}
	watch.feed <- history.HistoryEvent{ID: "01H2", Type: "leader_changed", NodeID: "node-b"}

	frames := readSSEFrames(t, bufio.NewReader(resp.Body), 2, 3*time.Second)
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2", len(frames))
	}
	if frames[0].event != "cluster_event" || frames[0].id != "01H1" {
		t.Errorf("frame[0]: event=%q id=%q", frames[0].event, frames[0].id)
	}
	if !strings.Contains(frames[0].data, `"type":"state_transition"`) {
		t.Errorf("frame[0].data missing type: %q", frames[0].data)
	}
	if frames[1].id != "01H2" {
		t.Errorf("frame[1].id=%q, want 01H2", frames[1].id)
	}
}

func TestWatchTransitions_FiltersToStateTransitionsOnly(t *testing.T) {
	watch := newFakeWatch()
	srv := newWatchTestServer(t, watch, &fakeEngine{})
	ts := newSSEServer(t, srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/v1/watch/transitions", nil)
	req.Header.Set("Accept", "text/event-stream")
	resp := doStreamingRequest(t, req)
	defer resp.Body.Close()

	// Block until the handler has subscribed (i.e. lastOpts is set).
	waitFor(t, func() bool {
		opts := watch.getLastOpts()
		return len(opts.Types) == 1 && opts.Types[0] == "state_transition"
	}, 2*time.Second, "watch subscriber never received state_transition type filter")
}

func TestWatchEvents_LastEventIDPropagatesAsCursor(t *testing.T) {
	watch := newFakeWatch()
	srv := newWatchTestServer(t, watch, &fakeEngine{})
	ts := newSSEServer(t, srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/v1/watch/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Last-Event-ID", "01H-RESUME")
	resp := doStreamingRequest(t, req)
	defer resp.Body.Close()

	waitFor(t, func() bool {
		return watch.getLastOpts().Cursor == "01H-RESUME"
	}, 2*time.Second, "Last-Event-ID never propagated as Cursor")
}

func TestWatch_AcceptHeaderEnforced_406(t *testing.T) {
	srv := newWatchTestServer(t, newFakeWatch(), &fakeEngine{})
	ts := newSSEServer(t, srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/v1/watch/events", nil)
	req.Header.Set("Accept", "application/json")
	resp := doStreamingRequest(t, req)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotAcceptable {
		t.Fatalf("status=%d, want 406", resp.StatusCode)
	}
}

func TestWatch_GapMarker_OnSubscriptionClosed(t *testing.T) {
	watch := newFakeWatch()
	srv := newWatchTestServer(t, watch, &fakeEngine{})
	ts := newSSEServer(t, srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/v1/watch/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	resp := doStreamingRequest(t, req)
	defer resp.Body.Close()

	// Wait for the subscription to be wired before closing it.
	select {
	case <-watch.subscribed:
	case <-time.After(2 * time.Second):
		t.Fatal("watch never subscribed")
	}
	watch.closeAll()

	br := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(2 * time.Second)
	var collected []string
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if line != "" {
			collected = append(collected, line)
		}
		if err != nil {
			break
		}
		if strings.HasPrefix(strings.TrimSpace(line), "event: gap_marker") {
			return
		}
	}
	t.Fatalf("never received gap_marker frame; body so far:\n%s", strings.Join(collected, "|"))
}

func TestWatch_KeepaliveCadence_FastTuned(t *testing.T) {
	prev := keepaliveInterval
	keepaliveInterval = 50 * time.Millisecond
	t.Cleanup(func() { keepaliveInterval = prev })

	srv := newWatchTestServer(t, newFakeWatch(), &fakeEngine{})
	ts := newSSEServer(t, srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/v1/watch/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	resp := doStreamingRequest(t, req)
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			break
		}
		if strings.HasPrefix(line, ":keepalive") {
			return
		}
	}
	t.Fatalf("never received :keepalive comment")
}

// newSSEServer wraps an http.Handler in an httptest.Server with the
// per-request timeout disabled. SSE needs long-lived connections.
func newSSEServer(t *testing.T, h http.Handler) *sseServer {
	t.Helper()
	ts := &sseServer{Server: http.Server{Handler: h}}
	ts.start(t)
	return ts
}

type sseServer struct {
	http.Server
	URL string
}

func (s *sseServer) start(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	s.URL = "http://" + ln.Addr().String()
	go func() {
		_ = s.Serve(ln)
	}()
}

func (s *sseServer) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = s.Shutdown(ctx)
}

// doStreamingRequest issues an HTTP request with response-body
// streaming preserved (no buffered drain). The caller MUST close
// resp.Body.
func doStreamingRequest(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	tr := &http.Transport{DisableCompression: true}
	cli := &http.Client{Transport: tr, Timeout: 0}
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatalf("do streaming request: %v", err)
	}
	return resp
}

// waitFor polls until cond returns true or timeout elapses.
func waitFor(t *testing.T, cond func() bool, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}
