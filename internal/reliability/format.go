package reliability

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// FormatTable writes the report as an aligned plain-text table to w.
// Columns: Model | Version | Rig | Sessions | Crashed | Quarantined |
//
//	IdleKilled | Drained | Crash% | Unhealthy%
//
// Empty group fields render as "—" so missing instrumentation is
// visually distinct from an explicit empty value.
func FormatTable(w io.Writer, r Report) error {
	headers := []string{
		"Model", "Version", "Rig",
		"Sessions", "Crashed", "Quarantined", "IdleKilled", "Drained",
		"Crash%", "Unhealthy%",
	}
	rows := make([][]string, 0, len(r.Groups)+2)
	for _, g := range r.Groups {
		rows = append(rows, []string{
			or(g.Key.Model),
			or(g.Key.PromptVersion),
			or(g.Key.Rig),
			itoa(g.Sessions),
			itoa(g.Crashed),
			itoa(g.Quarantined),
			itoa(g.IdleKilled),
			itoa(g.Drained),
			pctStr(g.CrashRate()),
			pctStr(g.UnhealthyRate()),
		})
	}
	rows = append(rows, []string{
		"TOTAL", "", "",
		itoa(r.Total.Sessions),
		itoa(r.Total.Crashed),
		itoa(r.Total.Quarantined),
		itoa(r.Total.IdleKilled),
		itoa(r.Total.Drained),
		pctStr(r.Total.CrashRate()),
		pctStr(r.Total.UnhealthyRate()),
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
	notes := make([]string, 0, 4)
	if dropped := r.Skipped + r.AmbiguousAliases; dropped > 0 {
		notes = append(notes, fmt.Sprintf("%d lifecycle event(s) dropped before grouping (skipped + ambiguous_aliases).", dropped))
	}
	if r.Skipped > 0 {
		notes = append(notes, fmt.Sprintf("%d lifecycle event(s) skipped: no worker.operation observed for the session at or before the event (instrumentation gap).", r.Skipped))
	}
	if r.AmbiguousAliases > 0 {
		notes = append(notes, fmt.Sprintf("%d lifecycle event(s) counted as ambiguous_aliases: ambiguous worker.operation alias matched multiple sessions.", r.AmbiguousAliases))
	}
	inst := r.Instrumentation
	if inst.WorkerOperations > 0 && (inst.MissingModel > 0 || inst.MissingPromptVersion > 0) {
		notes = append(notes, fmt.Sprintf(
			"warning: model/prompt_version instrumentation incomplete: model missing on %d/%d worker.operation event(s), prompt_version missing on %d/%d (event counts, not session counts).",
			inst.MissingModel, inst.WorkerOperations, inst.MissingPromptVersion, inst.WorkerOperations,
		))
	}
	if inst.QuarantineSignalStatus == quarantineSignalStatusNotEmitted {
		notes = append(notes, "note: session.quarantined is not emitted by current production paths; the Quarantined column is reserved pending instrumentation.")
	}
	return notes
}

// FormatJSON writes the report as JSON. Indent is two spaces; the
// shape matches the typed Group/Report fields.
func FormatJSON(w io.Writer, r Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// or returns "—" when s is empty, otherwise s. Used so empty key
// dimensions are visually distinct from real empty-string values.
func or(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

// pctStr formats a 0–1 fraction as a percentage with one decimal place.
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
			if l := runeLen(cell); l > widths[i] {
				widths[i] = l
			}
		}
	}
	return widths
}

// runeLen counts visible-character width approximating most Unicode
// glyphs as one cell. Sufficient for our table dashes and ASCII.
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
