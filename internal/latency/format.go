package latency

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// FormatTable writes the report as three aligned plain-text sections to w:
// claim-wait per pool, gate queue-wait per (formula, step), and gate bounce
// rate per formula. A section with no groups still prints its header and a
// "(no data)" line, so an operator sees which metric came back empty rather
// than a missing section.
func FormatTable(w io.Writer, r Report) error {
	if err := writeClaimWaitSection(w, r.ClaimWait); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	if err := writeGateQueueWaitSection(w, r.GateQueueWait); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	if err := writeGateBounceSection(w, r.GateBounce); err != nil {
		return err
	}
	if r.Skipped > 0 {
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "%d bead event(s) skipped: payload did not decode to a bead with an id.\n", r.Skipped); err != nil {
			return err
		}
	}
	return nil
}

func writeClaimWaitSection(w io.Writer, groups []ClaimWaitGroup) error {
	if _, err := fmt.Fprintln(w, "Claim wait per pool"); err != nil {
		return err
	}
	headers := []string{"Pool", "Count", "Min", "P50", "Avg", "P95", "Max"}
	rows := make([][]string, 0, len(groups))
	for _, g := range groups {
		rows = append(rows, statsRow(g.Pool, g.Stats))
	}
	return writeTableOrEmpty(w, headers, rows)
}

func writeGateQueueWaitSection(w io.Writer, groups []GateQueueWaitGroup) error {
	if _, err := fmt.Fprintln(w, "Gate queue wait (execution.step_defined -> execution.step_started) per formula/step"); err != nil {
		return err
	}
	headers := []string{"Formula", "Step", "Count", "Min", "P50", "Avg", "P95", "Max"}
	rows := make([][]string, 0, len(groups))
	for _, g := range groups {
		row := []string{or(g.Formula), or(g.StepID)}
		row = append(row, statsRow("", g.Stats)[1:]...)
		rows = append(rows, row)
	}
	return writeTableOrEmpty(w, headers, rows)
}

func writeGateBounceSection(w io.Writer, groups []GateBounceGroup) error {
	if _, err := fmt.Fprintln(w, "Gate bounce rate per formula"); err != nil {
		return err
	}
	headers := []string{"Formula", "Definitions", "Bounces", "BounceRate"}
	rows := make([][]string, 0, len(groups))
	for _, g := range groups {
		rows = append(rows, []string{
			or(g.Formula),
			itoa(g.Definitions),
			itoa(g.Bounces),
			fmt.Sprintf("%.1f%%", g.BounceRate*100),
		})
	}
	return writeTableOrEmpty(w, headers, rows)
}

func statsRow(label string, s DurationStats) []string {
	return []string{
		or(label),
		itoa(s.Count),
		durMs(s.MinMs),
		durMs(s.P50Ms),
		durMs(s.AvgMs),
		durMs(s.P95Ms),
		durMs(s.MaxMs),
	}
}

func writeTableOrEmpty(w io.Writer, headers []string, rows [][]string) error {
	if len(rows) == 0 {
		_, err := fmt.Fprintln(w, "(no data)")
		return err
	}
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
// matches the typed Report fields.
func FormatJSON(w io.Writer, r Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func durMs(ms int64) string { return fmt.Sprintf("%dms", ms) }

func or(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

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
