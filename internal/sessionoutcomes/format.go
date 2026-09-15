package sessionoutcomes

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// FormatTable writes the report as an aligned plain-text table to w.
// Columns: Bucket | Template | Provider | Started | Succeeded | Failed |
// Other | Fail% | AvgMs | MinMs | MaxMs | Outage?
//
// Bucket renders as RFC3339 (UTC) so it sorts and diffs cleanly; groups
// are already chronological (see sortedGroups). A row with Outage=yes
// marks the "possible provider outage" signature from issue #5852.
func FormatTable(w io.Writer, r Report) error {
	headers := []string{
		"Bucket", "Template", "Provider",
		"Started", "Succeeded", "Failed", "Other",
		"Fail%", "AvgMs", "MinMs", "MaxMs", "Outage?",
	}
	rows := make([][]string, 0, len(r.Groups)+2)
	for _, g := range r.Groups {
		rows = append(rows, []string{
			g.Key.BucketStart.Format("2006-01-02T15:04:05Z"),
			g.Key.Template,
			g.Key.Provider,
			itoa(g.Started),
			itoa(g.Succeeded),
			itoa(g.Failed),
			itoa(g.Other),
			pctStr(g.FailureRate()),
			fmt.Sprintf("%.0f", g.AvgDurationMs()),
			itoa64(g.MinDurationMs),
			itoa64(g.MaxDurationMs),
			yesNo(g.PossibleOutage()),
		})
	}
	rows = append(rows, []string{
		"TOTAL", "", "",
		itoa(r.Total.Started),
		itoa(r.Total.Succeeded),
		itoa(r.Total.Failed),
		itoa(r.Total.Other),
		pctStr(r.Total.FailureRate()),
		fmt.Sprintf("%.0f", r.Total.AvgDurationMs()),
		itoa64(r.Total.MinDurationMs),
		itoa64(r.Total.MaxDurationMs),
		"",
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
	notes := reportNotes(r)
	if len(notes) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	for _, note := range notes {
		if _, err := fmt.Fprintln(w, note); err != nil {
			return err
		}
	}
	return nil
}

func reportNotes(r Report) []string {
	notes := make([]string, 0, 3)
	if r.Skipped > 0 {
		notes = append(notes, fmt.Sprintf("%d worker.operation event(s) skipped: payload did not decode.", r.Skipped))
	}
	inst := r.Instrumentation
	if inst.StartAttempts > 0 && (inst.MissingTemplate > 0 || inst.MissingProvider > 0) {
		notes = append(notes, fmt.Sprintf(
			"warning: template/provider instrumentation incomplete: template missing on %d/%d session-start event(s), provider missing on %d/%d (grouped under %q).",
			inst.MissingTemplate, inst.StartAttempts, inst.MissingProvider, inst.StartAttempts, unknownDim,
		))
	}
	for _, g := range r.Groups {
		if g.PossibleOutage() {
			notes = append(notes, fmt.Sprintf(
				"possible outage: %s/%s at %s — %d/%d attempts failed within a %dms spread.",
				g.Key.Template, g.Key.Provider, g.Key.BucketStart.Format("2006-01-02T15:04:05Z"),
				g.Failed, g.Started, g.MaxDurationMs-g.MinDurationMs,
			))
		}
	}
	return notes
}

// FormatJSON writes the report as JSON. Indent is two spaces; the shape
// matches the typed Group/Report fields.
func FormatJSON(w io.Writer, r Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func itoa(n int) string     { return fmt.Sprintf("%d", n) }
func itoa64(n int64) string { return fmt.Sprintf("%d", n) }

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return ""
}

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
			if l := len([]rune(cell)); l > widths[i] {
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
	if len([]rune(s)) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len([]rune(s)))
}
