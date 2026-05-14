package output

import (
	"fmt"
	"io"
	"strings"
)

// Table renders kubectl-style columns: two spaces between fields, left-
// aligned, no ASCII-art borders. The renderer is intentionally small —
// pgmctl's text view fits in 80×24 for the common 3-peer case (FR-011).
//
// Column widths are measured against *visible* width, not byte length:
// every cell may contain ANSI color escapes from severity coloring, and
// text/tabwriter — which the table previously used — pads on byte
// length, which makes a colored row look "narrower" than its uncolored
// header and shears columns.
type Table struct {
	headers []string
	rows    [][]string
}

// NewTable starts a table with the given header row.
func NewTable(headers ...string) *Table {
	return &Table{headers: headers}
}

// AddRow appends a row. Number of fields MUST equal len(headers).
func (t *Table) AddRow(fields ...string) {
	if len(fields) != len(t.headers) {
		panic(fmt.Sprintf("table row has %d fields, want %d (headers: %v)", len(fields), len(t.headers), t.headers))
	}
	t.rows = append(t.rows, fields)
}

// Render writes the table to w. Two spaces between columns.
func (t *Table) Render(w io.Writer) error {
	widths := make([]int, len(t.headers))
	for i, h := range t.headers {
		if n := visibleWidth(h); n > widths[i] {
			widths[i] = n
		}
	}
	for _, row := range t.rows {
		for i, cell := range row {
			if n := visibleWidth(cell); n > widths[i] {
				widths[i] = n
			}
		}
	}
	if err := writeRow(w, t.headers, widths); err != nil {
		return err
	}
	for _, row := range t.rows {
		if err := writeRow(w, row, widths); err != nil {
			return err
		}
	}
	return nil
}

func writeRow(w io.Writer, cells []string, widths []int) error {
	for i, cell := range cells {
		if i > 0 {
			if _, err := w.Write([]byte("  ")); err != nil {
				return err
			}
		}
		if _, err := w.Write([]byte(cell)); err != nil {
			return err
		}
		// Don't pad the final column — trailing spaces are noise.
		if i == len(cells)-1 {
			continue
		}
		pad := widths[i] - visibleWidth(cell)
		if pad > 0 {
			if _, err := w.Write([]byte(strings.Repeat(" ", pad))); err != nil {
				return err
			}
		}
	}
	_, err := w.Write([]byte{'\n'})
	return err
}

// visibleWidth returns the on-screen column width of s, stripping ANSI
// CSI sequences ("\x1b[" + parameter bytes + final byte in 0x40-0x7E).
// Tabs and newlines are not expected in cell values; they pass through
// counted as one column each. Multibyte runes count as one column each
// (a reasonable approximation for the ASCII data pgmctl actually puts
// in tables — node IDs, role/state words, byte sizes, ISO timestamps).
func visibleWidth(s string) int {
	width := 0
	for i := 0; i < len(s); {
		if i+1 < len(s) && s[i] == 0x1b && s[i+1] == '[' {
			// CSI sequence — skip up to and including the final byte.
			j := i + 2
			for j < len(s) {
				c := s[j]
				j++
				if c >= 0x40 && c <= 0x7e {
					break
				}
			}
			i = j
			continue
		}
		// Advance one rune; we count it as one column.
		_, size := decodeRune(s[i:])
		width++
		i += size
	}
	return width
}

// decodeRune is the tiny subset of utf8.DecodeRuneInString we need —
// pulled inline to keep this file free of imports beyond fmt / io /
// strings.
func decodeRune(s string) (r rune, size int) {
	if s == "" {
		return 0, 0
	}
	b := s[0]
	switch {
	case b < 0x80:
		return rune(b), 1
	case b < 0xc0:
		return 0xfffd, 1
	case b < 0xe0:
		return 0xfffd, 2
	case b < 0xf0:
		return 0xfffd, 3
	default:
		return 0xfffd, 4
	}
}
