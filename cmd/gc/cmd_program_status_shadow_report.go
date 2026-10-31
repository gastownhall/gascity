package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/spf13/cobra"
)

// newInternalProgramStatusShadowReportCmd builds `gc internal
// program-status-shadow-report`: the shadow summarizer (design §9.5). It
// reads the city's session.program_status_shadow events, archives included,
// and prints one row per decision point and provider family.
func newInternalProgramStatusShadowReportCmd(stdout, stderr io.Writer) *cobra.Command {
	var since string
	var asJSON bool
	cmd := &cobra.Command{
		Use:    "program-status-shadow-report",
		Short:  "Summarize OSC 7501 shadow comparisons per decision point and family",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var window time.Duration
			if since != "" {
				d, err := time.ParseDuration(since)
				if err != nil || d <= 0 {
					fmt.Fprintf(stderr, "gc internal program-status-shadow-report: --since %q is not a positive duration\n", since) //nolint:errcheck // best-effort stderr
					return errExit
				}
				window = d
			}
			cityPath, err := resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc internal program-status-shadow-report: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			path, err := cityEventsLogPath(cityPath)
			if err != nil {
				fmt.Fprintf(stderr, "gc internal program-status-shadow-report: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			now := time.Now()
			filter := events.Filter{Type: events.SessionProgramStatusShadow}
			if window > 0 {
				filter.Since = now.Add(-window)
			}
			var records []programShadowRecord
			var undecodable int
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			err = events.ReadFilteredEach(ctx, path, filter, func(e events.Event) error {
				var p events.SessionProgramStatusShadowPayload
				if err := json.Unmarshal(e.Payload, &p); err != nil {
					undecodable++
					return nil
				}
				records = append(records, programShadowRecord{At: e.Ts, Session: e.Subject, Payload: p})
				return nil
			})
			if err != nil {
				fmt.Fprintf(stderr, "gc internal program-status-shadow-report: reading %s: %v\n", path, err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			if undecodable > 0 {
				fmt.Fprintf(stderr, "gc internal program-status-shadow-report: skipped %d undecodable %s events\n", undecodable, events.SessionProgramStatusShadow) //nolint:errcheck // best-effort stderr
			}
			// The leaf names a session's family from its provider name; the
			// city's providers resolve a wrapped alias to its built-in
			// ancestor, so every point of one family lands in one row.
			if cfg, err := loadCityConfig(cityPath, io.Discard); err == nil {
				for i, r := range records {
					if family := config.BuiltinFamily(r.Payload.Family, cfg.Providers); family != "" {
						records[i].Payload.Family = family
					}
				}
			} else {
				fmt.Fprintf(stderr, "gc internal program-status-shadow-report: city config unavailable, families are reported as recorded: %v\n", err) //nolint:errcheck // best-effort stderr
			}
			report := summarizeProgramShadow(records)
			if asJSON {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				if err := enc.Encode(report); err != nil {
					fmt.Fprintf(stderr, "gc internal program-status-shadow-report: %v\n", err) //nolint:errcheck // best-effort stderr
					return errExit
				}
				return nil
			}
			writeProgramShadowReport(stdout, report)
			return nil
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "Only comparisons newer than this duration (e.g. 24h); default all retained")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print the report as JSON")
	return cmd
}

// programShadowRecord is one shadow comparison as the events log holds it.
type programShadowRecord struct {
	At      time.Time
	Session string
	Payload events.SessionProgramStatusShadowPayload
}

// programShadowReport is the summarizer's output.
type programShadowReport struct {
	Total   int                 `json:"total"`
	Dropped int                 `json:"dropped"`
	Rows    []programShadowCell `json:"rows"`
}

// programShadowCell is one decision point by family. Agreement counts only
// valid comparisons; the other validities are capture health, never
// disagreement.
type programShadowCell struct {
	Point  string `json:"point"`
	Family string `json:"family"`
	Count  int    `json:"count"`
	Valid  int    `json:"valid"`
	Agree  int    `json:"agree"`
	// AgreePct is Agree over Valid, -1 with no valid comparison.
	AgreePct float64 `json:"agree_pct"`
	// TimeWeightedAgreePct weighs each valid comparison of a change point
	// by how long it stood (until the next comparison of its session and
	// point, or the session's last comparison at any point); -1 when not a
	// change point or with no valid time.
	TimeWeightedAgreePct float64 `json:"time_weighted_agree_pct"`
	// DisagreeByValidity counts the comparisons whose readings disagree, by
	// the record reading's validity.
	DisagreeByValidity map[string]int `json:"disagree_by_validity"`
	// LatencyP50Ms and LatencyP95Ms are over valid timed comparisons; Timed
	// is how many there were.
	Timed        int   `json:"timed"`
	LatencyP50Ms int64 `json:"latency_p50_ms"`
	LatencyP95Ms int64 `json:"latency_p95_ms"`
	Dropped      int   `json:"dropped"`
}

// programShadowCellKey names a cell.
type programShadowCellKey struct{ point, family string }

// summarizeProgramShadow builds the per-point, per-family report from
// records in log order.
func summarizeProgramShadow(records []programShadowRecord) programShadowReport {
	sorted := make([]programShadowRecord, len(records))
	copy(sorted, records)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].At.Before(sorted[j].At) })

	// Each change-point comparison stands until the next one of its session
	// and point. A session's last one stands until the session's last
	// comparison at any point, the last time it was seen: never to the end
	// of the report, which may be long after the session ended.
	lastSeen := map[string]time.Time{}
	for _, r := range sorted {
		if r.At.After(lastSeen[r.Session]) {
			lastSeen[r.Session] = r.At
		}
	}
	standing := make([]time.Duration, len(sorted))
	next := map[[2]string]int{}
	for i := len(sorted) - 1; i >= 0; i-- {
		key := [2]string{sorted[i].Session, sorted[i].Payload.Point}
		if j, ok := next[key]; ok {
			standing[i] = sorted[j].At.Sub(sorted[i].At)
		} else {
			standing[i] = lastSeen[sorted[i].Session].Sub(sorted[i].At)
		}
		next[key] = i
	}

	type acc struct {
		cell                 programShadowCell
		agreeTime, validTime time.Duration
		latencies            []int64
	}
	cells := map[programShadowCellKey]*acc{}
	report := programShadowReport{}
	for i, r := range sorted {
		p := r.Payload
		family := p.Family
		if family == "" {
			family = "unknown"
		}
		key := programShadowCellKey{point: p.Point, family: family}
		a := cells[key]
		if a == nil {
			a = &acc{cell: programShadowCell{Point: p.Point, Family: family, DisagreeByValidity: map[string]int{}}}
			cells[key] = a
		}
		report.Total++
		report.Dropped += p.Dropped
		a.cell.Count++
		a.cell.Dropped += p.Dropped
		if !p.Agree {
			a.cell.DisagreeByValidity[p.Validity]++
		}
		if p.Validity != string(runtime.ProgramStatusValid) {
			continue
		}
		a.cell.Valid++
		a.validTime += standing[i]
		if p.Agree {
			a.cell.Agree++
			a.agreeTime += standing[i]
		}
		if p.Timed {
			a.latencies = append(a.latencies, p.LatencyMs)
		}
	}
	for _, a := range cells {
		a.cell.AgreePct = percent(a.cell.Agree, a.cell.Valid)
		a.cell.TimeWeightedAgreePct = -1
		if runtime.ProgramStatusPoint(a.cell.Point).RecordsOnChange() && a.validTime > 0 {
			a.cell.TimeWeightedAgreePct = math.Round(1000*float64(a.agreeTime)/float64(a.validTime)) / 10
		}
		a.cell.Timed = len(a.latencies)
		a.cell.LatencyP50Ms = nearestRank(a.latencies, 50)
		a.cell.LatencyP95Ms = nearestRank(a.latencies, 95)
		report.Rows = append(report.Rows, a.cell)
	}
	order := map[string]int{}
	for i, p := range runtime.ProgramStatusPoints() {
		order[string(p)] = i
	}
	sort.Slice(report.Rows, func(i, j int) bool {
		a, b := report.Rows[i], report.Rows[j]
		if order[a.Point] != order[b.Point] {
			return order[a.Point] < order[b.Point]
		}
		return a.Family < b.Family
	})
	return report
}

// percent is n over of as a percentage to one decimal, -1 when of is 0.
func percent(n, of int) float64 {
	if of == 0 {
		return -1
	}
	return math.Round(1000*float64(n)/float64(of)) / 10
}

// nearestRank is the nearest-rank q-th percentile of values, 0 when empty.
func nearestRank(values []int64, q int) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := int(math.Ceil(float64(q) / 100 * float64(len(sorted))))
	return sorted[max(rank, 1)-1]
}

// writeProgramShadowReport prints report as a table.
func writeProgramShadowReport(w io.Writer, report programShadowReport) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "POINT\tFAMILY\tCOUNT\tVALID\tAGREE%\tTIME-AGREE%\tDISAGREE BY VALIDITY\tTIMED\tLAT P50\tLAT P95\tDROPPED") //nolint:errcheck // best-effort stdout
	for _, c := range report.Rows {
		timed, p50, p95 := "-", "-", "-"
		if c.Timed > 0 {
			timed = fmt.Sprint(c.Timed)
			p50 = fmt.Sprintf("%dms", c.LatencyP50Ms)
			p95 = fmt.Sprintf("%dms", c.LatencyP95Ms)
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%d\n", //nolint:errcheck // best-effort stdout
			c.Point, c.Family, c.Count, c.Valid, pctCell(c.AgreePct), pctCell(c.TimeWeightedAgreePct),
			disagreeCell(c.DisagreeByValidity), timed, p50, p95, c.Dropped)
	}
	tw.Flush()                                                                         //nolint:errcheck // best-effort stdout
	fmt.Fprintf(w, "total %d comparisons, %d dropped\n", report.Total, report.Dropped) //nolint:errcheck // best-effort stdout
}

// pctCell renders a percentage, "-" for none.
func pctCell(p float64) string {
	if p < 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f", p)
}

// disagreeCell renders disagreement counts by validity, sorted, "-" for none.
func disagreeCell(by map[string]int) string {
	if len(by) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s:%d", k, by[k])
	}
	return strings.Join(parts, " ")
}
