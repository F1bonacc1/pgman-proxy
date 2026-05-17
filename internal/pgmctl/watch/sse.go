package watch

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Frame is one parsed SSE frame.
type Frame struct {
	Event string
	ID    string
	Data  string
}

// Streamer opens an SSE connection and yields parsed frames via the
// returned channel. It does NOT implement reconnect — see Tail() for
// that. The channel closes when ctx is cancelled, the server closes
// the connection, or a network error occurs (in which case the err
// channel emits exactly one error before close).
//
// Used directly by the contract tests and by Tail() under the hood.
type Streamer struct {
	open func(ctx context.Context, lastEventID string) (*http.Response, error)
}

// NewStreamer builds a Streamer from an opener function. The opener
// is called every time a new HTTP connection is established and MUST
// pass `Last-Event-ID: <lastEventID>` to the server when non-empty so
// the resumption guarantee documented in contracts/control-plane-
// extensions.md § 1 holds across reconnects.
func NewStreamer(open func(ctx context.Context, lastEventID string) (*http.Response, error)) *Streamer {
	return &Streamer{open: open}
}

// Stream runs one SSE connection. Returns (frames, errs); both are
// closed when the connection ends. Caller iterates until both close.
func (s *Streamer) Stream(ctx context.Context, lastEventID string) (<-chan Frame, <-chan error) {
	frames := make(chan Frame, 16)
	errs := make(chan error, 1)

	go func() {
		defer close(frames)
		defer close(errs)
		resp, err := s.open(ctx, lastEventID)
		if err != nil {
			errs <- err
			return
		}
		defer func() { _ = resp.Body.Close() }()
		parseSSE(ctx, resp.Body, frames, errs)
	}()

	return frames, errs
}

// parseSSE reads SSE-framed bytes from r and emits Frame values on
// out. Stops cleanly on ctx-cancel / io.EOF; surfaces other errors on
// errs.
func parseSSE(ctx context.Context, r io.Reader, out chan<- Frame, errs chan<- error) {
	br := bufio.NewReaderSize(r, 64*1024)
	cur := Frame{}
	for {
		if ctx.Err() != nil {
			return
		}
		line, err := br.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, ":"):
				// comment / keepalive — skip but use as a "we're alive"
				// liveness signal upstream renderers may surface.
			case line == "":
				if cur.Event != "" || cur.Data != "" {
					select {
					case out <- cur:
					case <-ctx.Done():
						return
					}
					cur = Frame{}
				}
			case strings.HasPrefix(line, "event:"):
				cur.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "id:"):
				cur.ID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
			case strings.HasPrefix(line, "data:"):
				cur.Data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				// Flush any in-progress frame so a stream that ends
				// without a trailing blank line (server-side abort,
				// curl --max-time, etc.) doesn't silently lose its
				// last event.
				if cur.Event != "" || cur.Data != "" {
					select {
					case out <- cur:
					case <-ctx.Done():
					}
				}
				return
			}
			errs <- fmt.Errorf("sse parse: %w", err)
			return
		}
	}
}

// ReconnectPolicy controls Tail()'s reconnection behaviour. Defaults
// (per RD-010 / contracts/cli-commands.md § watch):
//   - Base 250 ms, Factor 2.0, Cap 10 s, Max 30 attempts.
type ReconnectPolicy struct {
	Base        time.Duration
	Factor      float64
	Cap         time.Duration
	MaxAttempts int
}

// DefaultReconnectPolicy returns the documented defaults.
func DefaultReconnectPolicy() ReconnectPolicy {
	return ReconnectPolicy{
		Base:        250 * time.Millisecond,
		Factor:      2.0,
		Cap:         10 * time.Second,
		MaxAttempts: 30,
	}
}

// TailOptions controls Tail()'s loop.
type TailOptions struct {
	// LastEventID seeds the first connection's Last-Event-ID header.
	// Subsequent reconnects use the most recently seen frame.ID.
	LastEventID string

	// Reconnect controls backoff. Zero values fall through to the
	// defaults in DefaultReconnectPolicy().
	Reconnect ReconnectPolicy

	// OnReconnect is called before every reconnect with the current
	// attempt count + delay. Renderers use this to surface a status
	// line. Nil = silent.
	OnReconnect func(attempt int, delay time.Duration)

	// OnGap is called when the stream emits a `gap_marker` frame.
	// Renderers use this to print a divider + (for status views)
	// trigger a fresh fetch on next display.
	OnGap func(reason string)
}

// Tail opens an SSE stream and reconnects on transport failure with
// exponential backoff. Frames flow through the returned channel; the
// channel closes when ctx is cancelled or the reconnect ceiling is
// hit. The final err (if any) is delivered on errs before close.
//
// Returning the last successfully seen frame ID in the closed-channel
// handshake lets the caller resume on a fresh process invocation.
func Tail(ctx context.Context, s *Streamer, opts TailOptions) (<-chan Frame, <-chan error) {
	frames := make(chan Frame, 32)
	errs := make(chan error, 1)
	rp := opts.Reconnect
	if rp.Base == 0 {
		rp = DefaultReconnectPolicy()
	}

	go func() {
		defer close(frames)
		defer close(errs)
		lastID := opts.LastEventID
		attempt := 0
		delay := rp.Base
		for {
			if ctx.Err() != nil {
				return
			}
			in, ierr := s.Stream(ctx, lastID)
			handler := newGapInterceptor(opts.OnGap)
			_ = pipeFrames(ctx, frames, in, &lastID, handler)
			// Drain the (now-closed) err channel.
			var connErr error
			for e := range ierr {
				connErr = e
			}
			if ctx.Err() != nil {
				return
			}
			// A clean server-side close (`ok && connErr == nil`) is
			// treated the same as a faulted close: fall through to the
			// reconnect cadence below so the caller can decide what to do.
			attempt++
			if rp.MaxAttempts > 0 && attempt > rp.MaxAttempts {
				errs <- fmt.Errorf("reconnect ceiling exceeded after %d attempts: %w", attempt-1, connErr)
				return
			}
			if opts.OnReconnect != nil {
				opts.OnReconnect(attempt, delay)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			delay = time.Duration(float64(delay) * rp.Factor)
			if delay > rp.Cap {
				delay = rp.Cap
			}
		}
	}()
	return frames, errs
}

// gapInterceptor intercepts gap_marker frames (calling OnGap) before
// passing them through to the caller.
type gapInterceptor struct {
	onGap func(reason string)
}

func newGapInterceptor(onGap func(string)) *gapInterceptor {
	return &gapInterceptor{onGap: onGap}
}

func (g *gapInterceptor) inspect(f Frame) {
	if g.onGap == nil || f.Event != "gap_marker" {
		return
	}
	// Best-effort parse: payload is `{"reason":"...","topic":"..."}`.
	reason := ""
	if idx := strings.Index(f.Data, `"reason":"`); idx >= 0 {
		s := f.Data[idx+len(`"reason":"`):]
		if end := strings.IndexByte(s, '"'); end >= 0 {
			reason = s[:end]
		}
	}
	g.onGap(reason)
}

// pipeFrames forwards frames from in to out, updating lastID as it
// goes. Returns true if the channel closed cleanly (no early ctx
// cancel).
func pipeFrames(ctx context.Context, out chan<- Frame, in <-chan Frame, lastID *string, g *gapInterceptor) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case f, ok := <-in:
			if !ok {
				return true
			}
			if f.ID != "" {
				*lastID = f.ID
			}
			g.inspect(f)
			select {
			case out <- f:
			case <-ctx.Done():
				return false
			}
		}
	}
}
