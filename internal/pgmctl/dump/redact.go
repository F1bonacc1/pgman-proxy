// Redaction for dump artifacts (T064 / FR-033).
//
// Two modes:
//
//	normal — best-effort scrub of bearer tokens, password fields, and
//	         anything that smells like an Authorization header value.
//	         Cluster identity (cluster_id, node_id, host) is preserved.
//
//	strict — every cluster-identity value (cluster_id, node_id, host,
//	         IP, replication slot name) is replaced with a stable
//	         placeholder. The correlation table maps placeholder ←→
//	         original so an operator who can produce the table can
//	         re-correlate the dump after redaction.
//
// Strict mode is the path used when an operator shares a dump
// externally (vendor support, public bug report). Normal mode is the
// default for in-team uploads.

package dump

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// RedactLevel is the user-facing flag value.
type RedactLevel string

const (
	RedactNormal RedactLevel = "normal"
	RedactStrict RedactLevel = "strict"
)

// ParseRedactLevel validates and converts a string. Empty input
// defaults to RedactNormal.
func ParseRedactLevel(s string) (RedactLevel, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "normal":
		return RedactNormal, nil
	case "strict":
		return RedactStrict, nil
	default:
		return "", fmt.Errorf("invalid --redact-level %q (want normal|strict)", s)
	}
}

// Redactor scrubs slice payloads according to the chosen level.
// Strict mode accumulates a correlation table that can be persisted
// alongside the dump for the operator's offline use.
type Redactor struct {
	level RedactLevel
	// strictMap is the {placeholder: original} map populated only in
	// RedactStrict mode. Inverse of typical correlation tables so an
	// operator reading the dump's correlation.json sees the original
	// behind each placeholder.
	strictMap map[string]string
	// reverseMap is the {original: placeholder} cache so identical
	// inputs always map to the same placeholder within one dump.
	reverseMap map[string]string
}

// NewRedactor constructs a Redactor for the requested level.
func NewRedactor(level RedactLevel) *Redactor {
	return &Redactor{
		level:      level,
		strictMap:  make(map[string]string),
		reverseMap: make(map[string]string),
	}
}

// CorrelationTable returns the strict-mode placeholder/original map.
// Empty for RedactNormal.
func (r *Redactor) CorrelationTable() map[string]string {
	out := make(map[string]string, len(r.strictMap))
	for k, v := range r.strictMap {
		out[k] = v
	}
	return out
}

// Apply walks the supplied value and returns a redacted copy. The
// input shape is preserved; only string fields are scrubbed. Numbers,
// booleans, nulls, timestamps pass through.
func (r *Redactor) Apply(v any) any {
	if r == nil {
		return v
	}
	return r.walk(v, "")
}

func (r *Redactor) walk(v any, key string) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			out[k] = r.walk(vv, k)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			out[i] = r.walk(vv, key)
		}
		return out
	case string:
		return r.scrubString(key, x)
	default:
		return v
	}
}

// secretKeyRE matches field names that should be scrubbed regardless
// of value, in both normal and strict modes.
var secretKeyRE = regexp.MustCompile(`(?i)(password|secret|token|bearer|api[_-]?key|credential|private[_-]?key)`)

// identityKeyRE matches field names whose values must be replaced
// with stable placeholders under RedactStrict.
var identityKeyRE = regexp.MustCompile(`(?i)^(cluster_id|node_id|peer_node_id|local_node_id|leader_node_id|primary_node_id|server_name|host|addr|address|listen_addr|client_listen_addr|routes_listen_addr|peer_route_url|replication_slot|sync_standbys?)$`)

// bearerLiteralRE catches "Bearer <token>"-shaped substrings inside
// arbitrary text values (log lines, descriptions, etc.).
var bearerLiteralRE = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9_\-\.=:]{8,}`)

func (r *Redactor) scrubString(key, value string) string {
	if value == "" {
		return value
	}
	if secretKeyRE.MatchString(key) {
		return "[REDACTED]"
	}
	if r.level == RedactStrict && identityKeyRE.MatchString(key) {
		return r.placeholderFor(key, value)
	}
	// Both modes scrub embedded bearer literals.
	return bearerLiteralRE.ReplaceAllString(value, "Bearer [REDACTED]")
}

// placeholderFor mints (or reuses) a stable placeholder for the
// supplied identity value. Identical values always get the same
// placeholder so cross-slice references in the dump stay correlated.
func (r *Redactor) placeholderFor(key, original string) string {
	if existing, ok := r.reverseMap[original]; ok {
		return existing
	}
	h := sha256.Sum256([]byte(original))
	short := hex.EncodeToString(h[:4])
	bucket := bucketForKey(key)
	placeholder := fmt.Sprintf("%s-%s", bucket, short)
	r.reverseMap[original] = placeholder
	r.strictMap[placeholder] = original
	return placeholder
}

func bucketForKey(key string) string {
	lk := strings.ToLower(key)
	switch {
	case strings.Contains(lk, "cluster_id"):
		return "cluster"
	case strings.Contains(lk, "node") || strings.Contains(lk, "primary") || strings.Contains(lk, "leader") || strings.Contains(lk, "standby"):
		return "node"
	case strings.Contains(lk, "host") || strings.Contains(lk, "addr") || strings.Contains(lk, "url"):
		return "host"
	case strings.Contains(lk, "slot"):
		return "slot"
	default:
		return "id"
	}
}

// SortedCorrelation returns the strict-mode correlation table as a
// stable-sorted slice. Used by the tar writer to emit a deterministic
// correlation.json so the same input produces the same output bytes
// across runs (helps golden-file tests).
func (r *Redactor) SortedCorrelation() []CorrelationEntry {
	keys := make([]string, 0, len(r.strictMap))
	for k := range r.strictMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]CorrelationEntry, 0, len(keys))
	for _, k := range keys {
		out = append(out, CorrelationEntry{Placeholder: k, Original: r.strictMap[k]})
	}
	return out
}

// CorrelationEntry is one row of the strict-mode correlation table.
type CorrelationEntry struct {
	Placeholder string `json:"placeholder"`
	Original    string `json:"original"`
}
