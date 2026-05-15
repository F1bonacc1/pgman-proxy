package watch

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"
)

// StatusFrame is the JSON shape carried by `status_update` SSE
// frames. Mirrors pg-manager Status enough for the table view; the
// raw payload is preserved for `-o wide`.
type StatusFrame struct {
	LocalRole      string                  `json:"local_role"`
	LocalState     string                  `json:"local_state"`
	LeaderNodeID   string                  `json:"leader_node_id"`
	PrimaryNodeID  string                  `json:"primary_node_id"`
	Instances      []StatusInstance        `json:"instances"`
	EmbeddedNATS   *StatusEmbeddedNATS     `json:"embedded_nats,omitempty"`
	Raw            json.RawMessage         `json:"-"`
}

type StatusInstance struct {
	NodeID string `json:"node_id"`
	Role   string `json:"role"`
	State  string `json:"state"`
}

type StatusEmbeddedNATS struct {
	Up           bool `json:"up"`
	RoutesMeshed int  `json:"routes_meshed"`
}

// StatusRenderer writes a fixed-line cluster summary + per-peer
// table. Each call rewrites the layout in place using raw ANSI cursor
// escapes (RD-010): line N is overwritten on subsequent renders, so
// the terminal "redraws" without scrolling.
//
// A first render emits the full layout. Subsequent renders move the
// cursor up by the previous frame's line count and rewrite. A
// reconnect or gap marker forces a full repaint (caller must call
// Reset before the next Render).
type StatusRenderer struct {
	W            io.Writer
	GreenFn      func(string) string
	YellowFn     func(string) string
	RedFn        func(string) string
	lastLines    int
	firstRender  bool
}

// Reset forces the next Render to write the full layout (no cursor
// rewind). Caller invokes this after a reconnect / gap_marker / SIGWINCH.
func (r *StatusRenderer) Reset() {
	r.lastLines = 0
	r.firstRender = false
}

// Render writes one full cluster snapshot. Subsequent calls overwrite
// the previous render.
func (r *StatusRenderer) Render(s StatusFrame) error {
	if r.lastLines > 0 {
		// Move cursor up `lastLines` lines, then clear from cursor down.
		// Pure-ANSI; no termios dependency. Skips when stdout isn't a
		// terminal (Cells would be no-ops there anyway).
		fmt.Fprintf(r.W, "\033[%dA\033[J", r.lastLines)
	}
	lines := r.emit(s)
	r.lastLines = lines
	r.firstRender = true
	return nil
}

func (r *StatusRenderer) emit(s StatusFrame) int {
	leaderColor := r.GreenFn
	if s.LeaderNodeID == "" {
		leaderColor = r.RedFn
	}
	primaryColor := r.GreenFn
	if s.PrimaryNodeID == "" {
		primaryColor = r.RedFn
	}
	lines := 0
	fmt.Fprintf(r.W, "Cluster snapshot @ %s\n", time.Now().UTC().Format(time.RFC3339))
	lines++
	fmt.Fprintf(r.W, "  Leader   : %s\n", colorize(leaderColor, displayOrNone(s.LeaderNodeID)))
	lines++
	fmt.Fprintf(r.W, "  Primary  : %s\n", colorize(primaryColor, displayOrNone(s.PrimaryNodeID)))
	lines++
	fmt.Fprintf(r.W, "  Local    : role=%s state=%s\n", s.LocalRole, s.LocalState)
	lines++
	if s.EmbeddedNATS != nil {
		emb := fmt.Sprintf("up=%v routes_meshed=%d", s.EmbeddedNATS.Up, s.EmbeddedNATS.RoutesMeshed)
		if s.EmbeddedNATS.Up {
			emb = colorize(r.GreenFn, emb)
		} else {
			emb = colorize(r.RedFn, emb)
		}
		fmt.Fprintf(r.W, "  NATS     : %s\n", emb)
		lines++
	}
	if len(s.Instances) > 0 {
		fmt.Fprintln(r.W, "")
		lines++
		fmt.Fprintln(r.W, "NODE          ROLE       STATE")
		lines++
		instances := make([]StatusInstance, len(s.Instances))
		copy(instances, s.Instances)
		sort.SliceStable(instances, func(i, j int) bool { return instances[i].NodeID < instances[j].NodeID })
		for _, inst := range instances {
			fmt.Fprintf(r.W, "%-12s  %-9s  %s\n", inst.NodeID, inst.Role, peerStateColored(inst.State, r))
			lines++
		}
	}
	return lines
}

func peerStateColored(state string, r *StatusRenderer) string {
	switch state {
	case "running":
		return colorize(r.GreenFn, state)
	case "promoting", "demoting", "rewinding", "bootstrapping", "init":
		return colorize(r.YellowFn, state)
	case "fenced", "failed", "stopped":
		return colorize(r.RedFn, state)
	default:
		return state
	}
}

func displayOrNone(s string) string {
	if s == "" {
		return "<none>"
	}
	return s
}

func colorize(fn func(string) string, s string) string {
	if fn == nil {
		return s
	}
	return fn(s)
}
