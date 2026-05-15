package watch

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseSSE_HandlesMultilineFrame(t *testing.T) {
	body := strings.Join([]string{
		":keepalive",
		"",
		"event: cluster_event",
		"id: 01H1",
		`data: {"type":"state_transition","node_id":"node-a"}`,
		"",
		"event: gap_marker",
		`data: {"reason":"subscription_closed"}`,
		"",
	}, "\n")

	frames := collectFrames(t, body)
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2", len(frames))
	}
	if frames[0].Event != "cluster_event" || frames[0].ID != "01H1" {
		t.Errorf("frames[0]=%#v", frames[0])
	}
	if !strings.Contains(frames[0].Data, "state_transition") {
		t.Errorf("frames[0].Data=%q missing state_transition", frames[0].Data)
	}
	if frames[1].Event != "gap_marker" {
		t.Errorf("frames[1].Event=%q, want gap_marker", frames[1].Event)
	}
}

func TestTail_ResumesViaLastEventID(t *testing.T) {
	// Server: writes one event with id=01H-FIRST, hangs up. On
	// reconnect, asserts Last-Event-ID and writes one more event.
	var mu sync.Mutex
	var seenLastEventID []string
	connectionCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenLastEventID = append(seenLastEventID, r.Header.Get("Last-Event-ID"))
		connectionCount++
		c := connectionCount
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f := w.(http.Flusher)
		f.Flush()
		switch c {
		case 1:
			_, _ = w.Write([]byte("event: cluster_event\nid: 01H-FIRST\ndata: {}\n\n"))
			f.Flush()
			// Close connection (handler returns).
		case 2:
			_, _ = w.Write([]byte("event: cluster_event\nid: 01H-SECOND\ndata: {}\n\n"))
			f.Flush()
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cli := &http.Client{}
	streamer := NewStreamer(func(c context.Context, lastID string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(c, "GET", srv.URL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "text/event-stream")
		if lastID != "" {
			req.Header.Set("Last-Event-ID", lastID)
		}
		return cli.Do(req)
	})

	frames, errs := Tail(ctx, streamer, TailOptions{
		Reconnect: ReconnectPolicy{Base: 5 * time.Millisecond, Factor: 1.0, Cap: 10 * time.Millisecond, MaxAttempts: 5},
	})

	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < 2 {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatalf("frames channel closed early; err=%v; seen=%v", <-errs, seen)
			}
			seen[f.ID] = true
		case <-deadline:
			t.Fatalf("timed out; seen=%v", seen)
		}
	}
	if !seen["01H-FIRST"] || !seen["01H-SECOND"] {
		t.Fatalf("missing frames; seen=%v", seen)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seenLastEventID) < 2 || seenLastEventID[1] != "01H-FIRST" {
		t.Errorf("Last-Event-ID never propagated on reconnect: %v", seenLastEventID)
	}
}

func TestTail_ReconnectCeilingExits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Refuse with a 500 each time so the streamer surfaces an error.
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	cli := &http.Client{}
	streamer := NewStreamer(func(c context.Context, _ string) (*http.Response, error) {
		req, _ := http.NewRequestWithContext(c, "GET", srv.URL, nil)
		return cli.Do(req)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	frames, errs := Tail(ctx, streamer, TailOptions{
		Reconnect: ReconnectPolicy{Base: time.Millisecond, Factor: 1.0, Cap: time.Millisecond, MaxAttempts: 3},
	})
	for range frames {
		// drain
	}
	err := <-errs
	if err == nil {
		t.Fatal("expected non-nil terminal error after reconnect ceiling")
	}
	if !strings.Contains(err.Error(), "reconnect ceiling exceeded") {
		t.Errorf("err=%v missing ceiling phrase", err)
	}
}

// collectFrames runs parseSSE directly on body bytes.
func collectFrames(t *testing.T, body string) []Frame {
	t.Helper()
	out := make(chan Frame, 16)
	errs := make(chan error, 1)
	go func() {
		parseSSE(context.Background(), bytes.NewReader([]byte(body)), out, errs)
		close(out)
		close(errs)
	}()
	var frames []Frame
	for f := range out {
		frames = append(frames, f)
	}
	return frames
}
