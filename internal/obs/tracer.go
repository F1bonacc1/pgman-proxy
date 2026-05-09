package obs

import "strings"

// Tracer is the minimal trace-context interface this codebase needs in
// v1. The full OTel SDK integration (OTLP exporter wiring) is deferred
// to Phase 6 (US3 observability completeness) per research R4 and the
// "Open follow-ups" section of research.md. For v1 we keep the surface
// trivial so callers don't have to choose an exporter at startup.
//
// When the OTel endpoint is unset (the default), traces are no-ops:
// inbound `traceparent` headers are still parsed and propagated as
// fields on log records, but no span is emitted.
type Tracer interface {
	// StartSpan returns a span handle and a function to end it.
	// The handle exposes Inject / Extract for header propagation.
	StartSpan(name string) (Span, func())
}

// Span is a single in-flight trace span.
type Span interface {
	// TraceID returns the W3C trace id (32 hex chars) or "" for no-op.
	TraceID() string
	// SpanID returns the W3C span id (16 hex chars) or "" for no-op.
	SpanID() string
}

// noopTracer is the default tracer when no OTel endpoint is configured.
type noopTracer struct{}

// NewNoopTracer returns a tracer that produces no spans.
func NewNoopTracer() Tracer { return noopTracer{} }

// StartSpan returns an empty span and a no-op end function.
func (noopTracer) StartSpan(name string) (Span, func()) {
	return noopSpan{}, func() {}
}

type noopSpan struct{}

// TraceID returns the empty string for the no-op tracer.
func (noopSpan) TraceID() string { return "" }

// SpanID returns the empty string for the no-op tracer.
func (noopSpan) SpanID() string { return "" }

// TraceContext is the W3C trace-context fields extracted from a
// `traceparent` header. The control plane and event subscriber treat
// these as opaque strings — the noop tracer doesn't manipulate them,
// it just propagates.
type TraceContext struct {
	TraceID string // 32 hex chars
	SpanID  string // 16 hex chars
	Flags   string // 2 hex chars (e.g. "01" for sampled)
}

// HasTrace reports whether the context carries a populated traceparent.
func (t TraceContext) HasTrace() bool { return t.TraceID != "" && t.SpanID != "" }

// Header returns the `traceparent` header value to forward downstream.
func (t TraceContext) Header() string {
	if !t.HasTrace() {
		return ""
	}
	flags := t.Flags
	if flags == "" {
		flags = "00"
	}
	return "00-" + t.TraceID + "-" + t.SpanID + "-" + flags
}

// ParseTraceParent extracts the W3C trace-context from a `traceparent`
// header value. Returns the zero TraceContext when the header is empty
// or malformed; callers MUST always check HasTrace().
//
// Format: `<version>-<trace-id>-<parent-id>-<trace-flags>` where
// version=`00`, trace-id=32 hex, parent-id=16 hex, flags=2 hex
// (W3C Trace Context, level 1).
func ParseTraceParent(value string) TraceContext {
	if len(value) != 55 { // 2+1+32+1+16+1+2
		return TraceContext{}
	}
	parts := strings.Split(value, "-")
	if len(parts) != 4 {
		return TraceContext{}
	}
	if parts[0] != "00" {
		return TraceContext{}
	}
	if !isHex(parts[1], 32) || !isHex(parts[2], 16) || !isHex(parts[3], 2) {
		return TraceContext{}
	}
	if isAllZero(parts[1]) || isAllZero(parts[2]) {
		// W3C says all-zero is the "invalid" sentinel; treat as missing.
		return TraceContext{}
	}
	return TraceContext{TraceID: parts[1], SpanID: parts[2], Flags: parts[3]}
}

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

func isAllZero(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			return false
		}
	}
	return true
}
