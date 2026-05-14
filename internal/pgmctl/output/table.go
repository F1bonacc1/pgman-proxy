package output

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// Table is a thin tabwriter wrapper that retains the input order of
// rows and column widths suitable for a kubectl-style narrow layout.
//
// The renderer is deliberately small — pgmctl's text view fits in
// 80×24 for the common 3-peer case (FR-011); anything wider gates on
// --output wide and gets an additional set of columns appended on the
// same writer.
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

// Render writes the table to w. Two spaces between columns; no
// ASCII-art borders. Matches the kubectl convention.
func (t *Table) Render(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, strings.Join(t.headers, "\t")); err != nil {
		return err
	}
	for _, row := range t.rows {
		if _, err := fmt.Fprintln(tw, strings.Join(row, "\t")); err != nil {
			return err
		}
	}
	return tw.Flush()
}
