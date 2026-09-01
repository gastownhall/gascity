package beadthroughput

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// FormatTable writes the report as an aligned plain-text table to w.
// Columns: Store | Type | Label | Opened | Closed | Net.
// Empty group fields render as "—" so an unlabeled bead is visually
// distinct from a bead genuinely labeled with an empty string (which the
// beads store does not produce, but the table treats identically either
// way).
func FormatTable(w io.Writer, r Report) error {
	headers := []string{"Store", "Type", "Label", "Opened", "Closed", "Net"}
	rows := make([][]string, 0, len(r.Groups)+2)
	for _, g := range r.Groups {
		rows = append(rows, []string{
			or(g.Key.Store),
			or(g.Key.Type),
			or(g.Key.Label),
			itoa(g.Opened),
			itoa(g.Closed),
			signedItoa(g.Net()),
		})
	}
	rows = append(rows, []string{
		"TOTAL", "", "",
		itoa(r.Total.Opened),
		itoa(r.Total.Closed),
		signedItoa(r.Total.Net()),
	})
	widths := columnWidths(headers, rows)
	if err := writeRow(w, headers, widths); err != nil {
		return err
	}
	if err := writeSeparator(w, widths); err != nil {
		return err
	}
	for _, row := range rows {
		if err := writeRow(w, row, widths); err != nil {
			return err
		}
	}
	if r.Skipped > 0 {
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "%d bead.created/bead.closed event(s) skipped: payload did not decode to a bead with an id.\n", r.Skipped); err != nil {
			return err
		}
	}
	return nil
}

// FormatJSON writes the report as JSON. Indent is two spaces; the shape
// matches the typed Group/Report fields.
func FormatJSON(w io.Writer, r Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func or(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

func signedItoa(n int) string {
	if n > 0 {
		return fmt.Sprintf("+%d", n)
	}
	return fmt.Sprintf("%d", n)
}

func columnWidths(headers []string, rows [][]string) []int {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i >= len(widths) {
				break
			}
			if l := runeLen(cell); l > widths[i] {
				widths[i] = l
			}
		}
	}
	return widths
}

func runeLen(s string) int { return len([]rune(s)) }

func writeRow(w io.Writer, cells []string, widths []int) error {
	parts := make([]string, len(cells))
	for i, cell := range cells {
		parts[i] = padRight(cell, widths[i])
	}
	_, err := fmt.Fprintln(w, strings.Join(parts, "  "))
	return err
}

func writeSeparator(w io.Writer, widths []int) error {
	parts := make([]string, len(widths))
	for i, n := range widths {
		parts[i] = strings.Repeat("-", n)
	}
	_, err := fmt.Fprintln(w, strings.Join(parts, "  "))
	return err
}

func padRight(s string, n int) string {
	if runeLen(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-runeLen(s))
}
