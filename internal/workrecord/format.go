package workrecord

import (
	"fmt"
	"io"
	"strings"
)

// FormatTable writes r as an aligned plain-text summary table to w, followed
// by a "Missing bead IDs:" section listing every gated bead lacking a valid
// gc.work_outcome — omitted entirely when there is nothing missing.
func FormatTable(w io.Writer, r CoverageReport) error {
	headers := []string{"Total Gated", "Covered", "Missing", "Coverage"}
	row := []string{
		itoa(r.TotalGated),
		itoa(r.Covered),
		itoa(r.Missing),
		pctStr(r.Coverage),
	}
	widths := columnWidths(headers, [][]string{row})
	if err := writeRow(w, headers, widths); err != nil {
		return err
	}
	if err := writeSeparator(w, widths); err != nil {
		return err
	}
	if err := writeRow(w, row, widths); err != nil {
		return err
	}
	if len(r.MissingIDs) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "Missing bead IDs:"); err != nil {
		return err
	}
	for _, id := range r.MissingIDs {
		if _, err := fmt.Fprintf(w, "  %s\n", id); err != nil {
			return err
		}
	}
	return nil
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

// pctStr formats a 0-1 fraction as a percentage with one decimal place.
func pctStr(frac float64) string {
	return fmt.Sprintf("%.1f%%", frac*100)
}

// columnWidths returns the max-width-per-column from headers and rows.
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
			if l := len(cell); l > widths[i] {
				widths[i] = l
			}
		}
	}
	return widths
}

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
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}
