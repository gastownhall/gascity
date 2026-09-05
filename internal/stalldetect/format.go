package stalldetect

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// FormatTable writes the report as an aligned plain-text table to w.
// Columns: Bead | Pool | Assignee | Last Event | Age | Stalled.
// Entries are already sorted oldest-first by Analyze, so the table reads
// as a wedged-first triage list without further sorting here.
func FormatTable(w io.Writer, r Report) error {
	headers := []string{"Bead", "Pool", "Assignee", "Last Event", "Age", "Stalled"}
	rows := make([][]string, 0, len(r.Entries))
	for _, e := range r.Entries {
		rows = append(rows, []string{
			e.BeadID,
			e.Pool,
			or(e.Assignee),
			or(e.LastEventType),
			formatAge(e.AgeSeconds),
			formatBool(e.Stalled),
		})
	}
	if err := renderTable(w, headers, rows); err != nil {
		return err
	}

	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "%d in-progress, %d stalled (threshold %s)\n",
		r.TotalInProgress, r.TotalStalled, formatAge(r.ThresholdSeconds)); err != nil {
		return err
	}
	if len(r.Pools) > 0 {
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		if err := formatPoolSummary(w, r.Pools); err != nil {
			return err
		}
	}
	if r.Skipped > 0 {
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "%d bead.created/bead.updated/bead.closed event(s) skipped: payload did not decode to a bead with an id.\n", r.Skipped); err != nil {
			return err
		}
	}
	return nil
}

func formatPoolSummary(w io.Writer, pools []PoolSummary) error {
	headers := []string{"Pool", "InProgress", "Stalled", "OldestAge"}
	rows := make([][]string, 0, len(pools))
	for _, p := range pools {
		rows = append(rows, []string{
			or(p.Pool),
			strconv.Itoa(p.InProgress),
			strconv.Itoa(p.Stalled),
			formatAge(p.OldestAgeSeconds),
		})
	}
	return renderTable(w, headers, rows)
}

// renderTable writes headers, a separator, and rows as an aligned
// plain-text table. Shared by FormatTable and formatPoolSummary.
func renderTable(w io.Writer, headers []string, rows [][]string) error {
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
	return nil
}

// FormatJSON writes the report as JSON. Indent is two spaces; the shape
// matches the typed Entry/PoolSummary/Report fields.
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

func formatBool(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// formatAge renders a duration given in seconds as a compact
// human-readable string (e.g. "3m12s", "2h0m"). Negative or zero
// durations render as "0s" — a stall report has no meaningful negative
// age, and treating it as zero avoids a confusing "-1m" from clock skew
// between event write time and the evaluation instant.
func formatAge(seconds float64) string {
	if seconds <= 0 {
		return "0s"
	}
	total := int64(seconds)
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
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
