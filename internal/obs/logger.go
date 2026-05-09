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

	pgmanager "github.com/f1bonacc1/pg-manager"
)

// Logger is a slog-backed JSON logger that also satisfies
// pgmanager.Logger. The same instance is passed to pg-manager so all
// engine logs flow through the same handler.
type Logger struct {
	slog *slog.Logger
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
	return &Logger{slog: l}
}

// With returns a new Logger that adds the given key/value pairs to
// every record. Useful for per-component sub-loggers without losing
// the cluster/node/component context.
func (l *Logger) With(kv ...any) *Logger {
	return &Logger{slog: l.slog.With(kv...)}
}

// WithComponent returns a new Logger whose `component` field is replaced.
func (l *Logger) WithComponent(name string) *Logger {
	return &Logger{slog: l.slog.With(slog.String("component", name))}
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
