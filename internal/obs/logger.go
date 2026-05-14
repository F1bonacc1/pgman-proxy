// Package obs holds observability primitives for pgman-proxy: a slog
// JSON logger that doubles as a pgmanager.Logger, a Prometheus registry
// for the documented metric set, and the /healthz / /readyz / /metrics
// HTTP surface. Stable field/metric names per
// specs/001-active-active-pg-proxy/contracts/observability.md.
package obs

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	pgmanager "github.com/f1bonacc1/pg-manager"
)

// HistorySink is the narrow contract obs.Logger needs to dual-publish
// event-shaped log lines to the cluster history stream (003 FR-007 /
// T013). It mirrors history.Publisher.PublishEvent so the concrete
// publisher can be plugged in without obs importing internal/history
// (which would create a cycle through control → obs).
type HistorySink interface {
	PublishEvent(ctx context.Context, category, evType, nodeID string, details map[string]any) (string, error)
}

// sinkHolder wraps an atomic.Pointer to a HistorySink so the field can
// live behind a *holder shared by all Logger derivatives (With /
// WithComponent return new *Logger values sharing the same holder).
// Pointers can't be safely copied past the first atomic op, so the
// shared-pointer indirection is the canonical workaround.
type sinkHolder struct {
	ptr atomic.Pointer[HistorySink]
}

// Logger is a slog-backed JSON logger that also satisfies
// pgmanager.Logger. The same instance is passed to pg-manager so all
// engine logs flow through the same handler.
type Logger struct {
	slog   *slog.Logger
	nodeID string
	sinks  *sinkHolder
}

// NewLogger constructs a Logger that writes to w (typically os.Stderr)
// at the requested level, with the standard fields (cluster_id, node_id,
// component) attached to every record.
func NewLogger(w io.Writer, level, clusterID, nodeID, component string) *Logger {
	if w == nil {
		w = os.Stderr
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: parseLevel(level),
	})
	l := slog.New(h).With(
		slog.String("cluster_id", clusterID),
		slog.String("node_id", nodeID),
		slog.String("component", component),
	)
	return &Logger{slog: l, nodeID: nodeID, sinks: &sinkHolder{}}
}

// SetHistorySink attaches (or replaces) the history publisher used by
// Logger.Event for the cluster-wide audit/event trail. Safe to call at
// any time; nil clears the sink. All loggers derived via With /
// WithComponent share the same sink — the wiring is set once at
// startup after the cluster handles + history stream are ready.
func (l *Logger) SetHistorySink(s HistorySink) {
	if l == nil || l.sinks == nil {
		return
	}
	if s == nil {
		l.sinks.ptr.Store(nil)
		return
	}
	l.sinks.ptr.Store(&s)
}

func (l *Logger) historySink() HistorySink {
	if l == nil || l.sinks == nil {
		return nil
	}
	p := l.sinks.ptr.Load()
	if p == nil {
		return nil
	}
	return *p
}

// With returns a new Logger that adds the given key/value pairs to
// every record. Useful for per-component sub-loggers without losing
// the cluster/node/component context.
func (l *Logger) With(kv ...any) *Logger {
	return &Logger{slog: l.slog.With(kv...), nodeID: l.nodeID, sinks: l.sinks}
}

// WithComponent returns a new Logger whose `component` field is replaced.
func (l *Logger) WithComponent(name string) *Logger {
	return &Logger{slog: l.slog.With(slog.String("component", name)), nodeID: l.nodeID, sinks: l.sinks}
}

// SLog returns the underlying *slog.Logger for direct use.
func (l *Logger) SLog() *slog.Logger { return l.slog }

// pgmanager.Logger interface implementation — Debug/Info/Warn/Error.

// Debug logs at DEBUG level with pg-manager-style fields.
func (l *Logger) Debug(msg string, fields ...pgmanager.Field) {
	l.slog.LogAttrs(context.Background(), slog.LevelDebug, msg, slogAttrs(fields)...)
}

// Info logs at INFO level.
func (l *Logger) Info(msg string, fields ...pgmanager.Field) {
	l.slog.LogAttrs(context.Background(), slog.LevelInfo, msg, slogAttrs(fields)...)
}

// Warn logs at WARN level.
func (l *Logger) Warn(msg string, fields ...pgmanager.Field) {
	l.slog.LogAttrs(context.Background(), slog.LevelWarn, msg, slogAttrs(fields)...)
}

// Error logs at ERROR level.
func (l *Logger) Error(msg string, fields ...pgmanager.Field) {
	l.slog.LogAttrs(context.Background(), slog.LevelError, msg, slogAttrs(fields)...)
}

// Event emits a structured event at INFO level AND, when a HistorySink
// is attached, publishes the same record to the cluster history stream
// (003 FR-007 / T013). The slog line is unchanged in format — only the
// history sink is the new behaviour.
//
//   - evType is the canonical dotted event identifier the project
//     already uses for slog (e.g. "embedded_nats.route_up",
//     "proxy.history_stream_ready"). It is recorded VERBATIM in slog
//     and used as the history record's `type`.
//   - nodeID overrides the default node id stamped on the history
//     record (empty = use the Logger's own node id).
//
// Publish failures are recorded as an event-shaped slog line at WARN
// but never propagated to the caller — the in-process slog line is
// always authoritative.
func (l *Logger) Event(evType, nodeID string, fields ...pgmanager.Field) {
	l.slog.LogAttrs(context.Background(), slog.LevelInfo, evType, slogAttrs(fields)...)
	sink := l.historySink()
	if sink == nil {
		return
	}
	details := make(map[string]any, len(fields))
	for _, f := range fields {
		details[f.Key] = f.Value
	}
	if _, err := sink.PublishEvent(context.Background(), "event", evType, nodeID, details); err != nil {
		l.slog.LogAttrs(context.Background(), slog.LevelWarn, "obs.history_publish_failed",
			slog.String("event_type", evType),
			slog.String("error", err.Error()))
	}
}

func slogAttrs(fs []pgmanager.Field) []slog.Attr {
	out := make([]slog.Attr, len(fs))
	for i, f := range fs {
		out[i] = slog.Any(f.Key, f.Value)
	}
	return out
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Compile-time check: *Logger satisfies pgmanager.Logger.
var _ pgmanager.Logger = (*Logger)(nil)

// SafeBuffer is a small concurrency-safe io.Writer used by tests to
// capture log output. Lives here so tests don't need to reinvent it.
type SafeBuffer struct {
	mu  sync.Mutex
	buf []byte
}

// Write appends bytes; safe for concurrent callers.
func (b *SafeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

// String returns the buffered content as a string.
func (b *SafeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
