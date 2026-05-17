package watch

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// AppendRenderer is the renderer for append-only streams (transitions
// / events / node). One line per SSE frame; gap markers print a
// highlighted divider; reconnect attempts print a status-bar line.
type AppendRenderer struct {
	W        io.Writer
	YellowFn func(string) string // optional ANSI yellow for divider
	RedFn    func(string) string // optional ANSI red for terminal errors
}

// Render writes one line for the given frame. Returns the error from
// the underlying writer (callers typically ignore).
func (r *AppendRenderer) Render(f Frame) error {
	switch f.Event {
	case "gap_marker":
		divider := fmt.Sprintf("--- gap_marker (%s) ---", parseReason(f.Data))
		if r.YellowFn != nil {
			divider = r.YellowFn(divider)
		}
		_, err := fmt.Fprintln(r.W, divider)
		return err
	case "error":
		text := fmt.Sprintf("[error] %s", oneLine(f.Data))
		if r.RedFn != nil {
			text = r.RedFn(text)
		}
		_, err := fmt.Fprintln(r.W, text)
		return err
	case "":
		return nil
	}

	t, evType, node, details := summarizeHistoryFrame(f.Data)
	if t.IsZero() {
		// Frame without a parseable HistoryEvent — print whatever we can.
		_, err := fmt.Fprintf(r.W, "%-20s  %-20s  %s\n",
			time.Now().UTC().Format(time.RFC3339), f.Event, oneLine(f.Data))
		return err
	}
	_, err := fmt.Fprintf(r.W, "%s  %-24s  %-12s  %s\n",
		t.UTC().Format(time.RFC3339), evType, node, details)
	return err
}

// RenderReconnectAttempt writes a single-line status bar surfacing a
// reconnect attempt. Renderers call this from TailOptions.OnReconnect.
func (r *AppendRenderer) RenderReconnectAttempt(attempt int, delay time.Duration) {
	line := fmt.Sprintf("[reconnect attempt %d — sleeping %s]", attempt, delay)
	if r.YellowFn != nil {
		line = r.YellowFn(line)
	}
	_, _ = fmt.Fprintln(r.W, line)
}

func parseReason(raw string) string {
	idx := strings.Index(raw, `"reason":"`)
	if idx < 0 {
		return "unknown"
	}
	s := raw[idx+len(`"reason":"`):]
	if end := strings.IndexByte(s, '"'); end >= 0 {
		return s[:end]
	}
	return "unknown"
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
}

// summarizeHistoryFrame parses an SSE data line as a HistoryEvent
// shape (cluster history wire form) and returns the fields used by
// the append-only renderer. Zero time means parse failed.
func summarizeHistoryFrame(raw string) (time.Time, string, string, string) {
	var ev struct {
		Time    time.Time              `json:"time"`
		Type    string                 `json:"type"`
		NodeID  string                 `json:"node_id"`
		Details map[string]interface{} `json:"details"`
	}
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		return time.Time{}, "", "", ""
	}
	return ev.Time, ev.Type, ev.NodeID, summarizeDetails(ev.Details)
}

// summarizeDetails compresses an event's details map into a single
// "k=v k=v" string. Keys are sorted so identical events render
// identically (matches pgmctl events behaviour).
func summarizeDetails(d map[string]interface{}) string {
	if len(d) == 0 {
		return ""
	}
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := d[k]
		if m, ok := v.(map[string]interface{}); ok {
			parts = append(parts, fmt.Sprintf("%s=<%d fields>", k, len(m)))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	return strings.Join(parts, " ")
}
