package history

import (
	"strings"
	"time"
)

// Category enumerates the two top-level event categories carried by
// the history stream. Wire form is lowercase.
type Category string

const (
	CategoryEvent Category = "event"
	CategoryAudit Category = "audit"
)

// HistoryEvent is the JSON wire shape persisted on the JetStream
// stream and returned by the GET /v1/history endpoint.
// Schema-versioned per FR-038; renames are MINOR-version events.
type HistoryEvent struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	ID         string         `json:"id"` // ULID
	Time       time.Time      `json:"time"`
	Category   Category       `json:"category"`
	Type       string         `json:"type"`
	ClusterID  string         `json:"cluster_id"`
	NodeID     string         `json:"node_id,omitempty"`
	Details    map[string]any `json:"details,omitempty"`
	TraceID    string         `json:"trace_id,omitempty"`
	SpanID     string         `json:"span_id,omitempty"`
}

// SubjectPrefix is the per-cluster prefix used for both publish and
// subscribe operations. Subjects of the form
// `pgman_proxy.<cluster_id>.history.<category>.<type>` carry one
// HistoryEvent per message.
func SubjectPrefix(clusterID string) string {
	return "pgman_proxy." + sanitize(clusterID) + ".history."
}

// EventSubject builds the subject for one event publish.
func EventSubject(clusterID string, c Category, evType string) string {
	return SubjectPrefix(clusterID) + string(c) + "." + sanitizeSubjectPart(evType)
}

// SubjectFilterAll subscribes to every history record in a cluster.
func SubjectFilterAll(clusterID string) string {
	return SubjectPrefix(clusterID) + ">"
}

// SubjectFilterCategory subscribes to one category (event/audit).
func SubjectFilterCategory(clusterID string, c Category) string {
	return SubjectPrefix(clusterID) + string(c) + ".>"
}

// StreamName returns the JetStream stream name for the given cluster.
// Upper-cased so the stream name is stable across operator-supplied
// cluster ids (which may use any case).
func StreamName(clusterID string) string {
	return "PGMAN_PROXY_HISTORY_" + strings.ToUpper(sanitize(clusterID))
}

// sanitize replaces NATS-illegal subject characters with underscores
// so subjects remain wildcard-safe. Matches the rule the 002 KV
// bucket-naming code uses.
func sanitize(s string) string {
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
	return string(out)
}

// sanitizeSubjectPart turns the user-facing type string ("route_up",
// "embedded_nats.server_started", "lcm_audit") into a NATS-safe
// subject token. Dots are preserved so multi-segment types remain
// queryable by wildcard.
func sanitizeSubjectPart(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '_', c == '-', c == '.':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
