package fanout

import (
	"strings"
	"time"
)

// Slice is the per-peer data kind the originator asks for.
// Matches contracts/fanout-protocol.md.
type Slice string

const (
	SliceStatus   Slice = "status"
	SliceConfig   Slice = "config"
	SliceNATSMesh Slice = "nats_mesh"
	SliceDoctor   Slice = "doctor"
)

// Request is the request envelope published by the originator on the
// fan-out subject hierarchy. Wire form per
// contracts/fanout-protocol.md § Request envelope.
type Request struct {
	Version       int            `json:"version"` // always 1 in v1
	RequestID     string         `json:"request_id"`
	OperatorActor string         `json:"operator_actor"`
	TraceID       string         `json:"trace_id,omitempty"`
	DeadlineMS    int            `json:"deadline_ms"`
	Slice         Slice          `json:"slice"`
	Args          map[string]any `json:"args,omitempty"`
}

// Reply is what each responder sends back. status is one of:
//
//	ok      — request succeeded; data carries the payload.
//	partial — succeeded with caveats; data carries the payload and
//	          error explains the caveats.
//	failed  — the responder cannot satisfy the request; error explains.
type Reply struct {
	Version     int       `json:"version"` // always 1 in v1
	RequestID   string    `json:"request_id"`
	NodeID      string    `json:"node_id"`
	Status      string    `json:"status"`
	Data        any       `json:"data,omitempty"`
	Error       *Error    `json:"error,omitempty"`
	RespondedAt time.Time `json:"responded_at"`
}

// Error is the optional error block.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Reply status constants. Wire values are stable.
const (
	StatusOK      = "ok"
	StatusPartial = "partial"
	StatusFailed  = "failed"
)

// Reply error codes; renames are MINOR-version events.
const (
	CodeSiblingUnreachable = "sibling_unreachable"
	CodeDeadlineExceeded   = "deadline_exceeded"
	CodeAuthFailed         = "auth_failed"
	CodeSliceInternal      = "slice_internal"
)

// SubjectPrefix is the per-cluster prefix used for fan-out subjects.
func SubjectPrefix(clusterID string) string {
	return "pgman_proxy." + sanitize(clusterID) + ".fanout."
}

// RequestSubject builds the publish subject for a (slice, target).
// target may be "*" for broadcast.
func RequestSubject(clusterID string, slice Slice, target string) string {
	return SubjectPrefix(clusterID) + string(slice) + "." + sanitize(target)
}

// ResponderUnicast builds the per-peer subscription subject.
func ResponderUnicast(clusterID string, slice Slice, selfNodeID string) string {
	return SubjectPrefix(clusterID) + string(slice) + "." + sanitize(selfNodeID)
}

// ResponderWildcard builds the wildcard subscription subject (for
// broadcasts).
func ResponderWildcard(clusterID string, slice Slice) string {
	return SubjectPrefix(clusterID) + string(slice) + ".*"
}

// SubjectAllSlicesAllTargets is the broadest fan-out subject pattern
// — used by the audit hook that records every fan-out request.
func SubjectAllSlicesAllTargets(clusterID string) string {
	return SubjectPrefix(clusterID) + ">"
}

// IsValidSlice reports whether s is one of the documented slices.
func IsValidSlice(s Slice) bool {
	switch s {
	case SliceStatus, SliceConfig, SliceNATSMesh, SliceDoctor:
		return true
	}
	return false
}

// sanitize mirrors history.sanitize — replace NATS-illegal subject
// runes with underscore. The wildcard token "*" is preserved as-is.
func sanitize(s string) string {
	if s == "*" {
		return "*"
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '_', c == '-':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "_"
	}
	return string(out)
}

// trim shortens long error messages to keep audit lines tractable.
func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Silence unused import in clean test builds.
var _ = strings.TrimSpace
