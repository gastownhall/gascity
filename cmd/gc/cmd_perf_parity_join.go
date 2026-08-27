package main

// gc perf parity-join is the WD parity campaign's join tool (DETECTOR.md
// section 3b). It joins legacy reconciler trace records against detector-shadow
// records and reports, per detector family, how far the shadow sweep tracks the
// god-function it is replacing. It is deleted with the rest of the D4-retained
// perf CLI at WE (DETECTOR.md section 5).
//
// Join contract (section 3b): the shared trace-cycle handle (trace_id, tick_id)
// plus the normalized session name, cross-checked on the session bead identity.
// The handle is an equality join, not a window, because section 2 runs the sweep
// inside beadReconcileTick beside the legacy call — no new loop, timer, or
// goroutine, therefore no cadence skew to reconcile. The one family that is
// genuinely time-skewed, D-DRAIN, surfaces as a legacy-only record in one cycle
// and a detector-only record in the next, which section 3b already names
// (ack-timing skew) and this tool triages as such.
//
// The left-hand side is legacy in EITHER of the two ways an auto-mode city
// records it (owner ruling, 2026-08-12):
//
//   - an ACT — a decision record the god function wrote unstamped, attributed by
//     elimination at a section 1 legacy site; and
//   - a YIELD — a traced stand-down at a coexistence seam, which is legacy
//     saying "I identified this row and stepped aside for the keyed owner".
//
// The yield is the evidence auto mode actually produces in volume, because auto
// mode is precisely the mode in which both writers step aside for each other:
// over the campaign's first hours the yield-join carries three orders of
// magnitude more pairs than the act-join. Both are joined and both are counted,
// separately — a both-act pair remains the strongest single piece of evidence.

import (
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

const (
	parityJoinSchemaV1      = "gascity.reconciler-parity-join.v2"
	parityJoinDefaultBar    = 0.995
	parityJoinDefaultSample = 10
	// parityJoinDefaultCountBar is section 3b's window size: at least 10,000
	// joined trace cycles. The owner's yield-join ruling keeps the figure and
	// re-expresses what counts toward it — act-pairs plus yield-pairs.
	parityJoinDefaultCountBar = 10000

	parityJoinOwnerLegacy         = "legacy"
	parityJoinOwnerDetectorShadow = "detector-shadow"
	parityJoinOwnerKeyed          = "keyed"
)

type parityJoinOptions struct {
	Bar      float64
	CountBar int
	Samples  int
	Template string
	// ExcludedWindows drop their records before anything is joined. See
	// parityJoinWindow.
	ExcludedWindows []parityJoinWindow
}

// parityJoinWindow is a half-open [Start, End) span of trace time whose records
// are dropped from the corpus before the join runs.
//
// The rule it serves is the WD.15 runbook's: cycles between a controller exit
// and the next good arming boundary are excluded from a section 3b readout. A
// restart cuts cycles in half — legacy writes its decision, the process stops,
// and the sweep's twin is never written — and the incoming instance's first
// ticks run before its detail arms are re-verified. Neither half is a
// divergence, but a same-cycle-handle join can only report them as singletons,
// and the section 3b table has nothing true to say about a record whose twin
// does not exist. Dropping the records BEFORE the join, rather than filtering
// the classified output, is what keeps a half-pair from leaking out.
//
// Boundaries are supplied, never inferred. The tool cannot know when an operator
// considered arming re-verified, and guessing it from the corpus would let the
// window grow to fit whatever is red.
type parityJoinWindow struct {
	Start time.Time
	End   time.Time
}

func (w parityJoinWindow) contains(ts time.Time) bool {
	return !ts.Before(w.Start) && ts.Before(w.End)
}

// parityJoinExcludedWindow is one exclusion window as the report declares it. A
// readout that dropped part of its own corpus is only citable if the artifact
// carries the windows and what they cost, so this is emitted whenever a window
// was passed — and omitted entirely when none was, keeping an unfiltered
// readout byte-identical to one produced before the flag existed.
//
// A record covered by more than one window counts against the first that covers
// it, so the RecordsExcluded values sum to the number of records dropped.
type parityJoinExcludedWindow struct {
	Start           time.Time `json:"start"`
	End             time.Time `json:"end"`
	RecordsExcluded int       `json:"records_excluded"`
}

// parseParityJoinExcludeWindow reads one --exclude-window value. RFC3339 has no
// unescaped '/', so the separator is unambiguous.
func parseParityJoinExcludeWindow(spec string) (parityJoinWindow, error) {
	rawStart, rawEnd, ok := strings.Cut(strings.TrimSpace(spec), "/")
	if !ok {
		return parityJoinWindow{}, fmt.Errorf("exclusion window %q: want <RFC3339-start>/<RFC3339-end>", spec)
	}
	start, err := time.Parse(time.RFC3339, strings.TrimSpace(rawStart))
	if err != nil {
		return parityJoinWindow{}, fmt.Errorf("exclusion window %q: parsing window start: %w", spec, err)
	}
	end, err := time.Parse(time.RFC3339, strings.TrimSpace(rawEnd))
	if err != nil {
		return parityJoinWindow{}, fmt.Errorf("exclusion window %q: parsing window end: %w", spec, err)
	}
	if !end.After(start) {
		return parityJoinWindow{}, fmt.Errorf("exclusion window %q: end must be after start", spec)
	}
	return parityJoinWindow{Start: start.UTC(), End: end.UTC()}, nil
}

// parityJoinExcludeWindows drops the windowed records and tallies what each
// window cost. It runs over the raw corpus, before cycles are bucketed, so an
// excluded cycle never reaches the classifier at all.
func parityJoinExcludeWindows(
	records []SessionReconcilerTraceRecord,
	windows []parityJoinWindow,
) ([]SessionReconcilerTraceRecord, []parityJoinExcludedWindow) {
	if len(windows) == 0 {
		return records, nil
	}
	excluded := make([]parityJoinExcludedWindow, len(windows))
	for i, window := range windows {
		excluded[i] = parityJoinExcludedWindow{Start: window.Start, End: window.End}
	}
	kept := make([]SessionReconcilerTraceRecord, 0, len(records))
	for _, rec := range records {
		dropped := false
		for i, window := range windows {
			if window.contains(rec.Ts) {
				excluded[i].RecordsExcluded++
				dropped = true
				break
			}
		}
		if !dropped {
			kept = append(kept, rec)
		}
	}
	return kept, excluded
}

type parityJoinCycleStats struct {
	Scanned              int `json:"scanned"`
	Considered           int `json:"considered"`
	ExcludedRecordBudget int `json:"excluded_record_budget_exceeded"`
	ExcludedNoRollup     int `json:"excluded_no_cycle_rollup"`
	WithoutDetailArms    int `json:"without_detail_arms"`
	UnownedRecords       int `json:"unowned_records"`
	LegacyByElimination  int `json:"legacy_by_elimination"`
	YieldRecords         int `json:"yield_records"`
	// UnpairedOwnershipYields are stand-downs that assert only "the keyed
	// controller holds this key" and had no actor beside them. They are not
	// evidence either way, so they are counted here and detailed in the YIELDS
	// log rather than scored as a divergence.
	UnpairedOwnershipYields int `json:"unpaired_ownership_yields"`
}

// Dispositions for an effect_owner-absent record. Exactly one is an
// attribution; the rest are refusals, each carrying why.
const (
	parityJoinDispositionLegacy         = "legacy"
	parityJoinDispositionPhaseMarker    = "phase_marker"
	parityJoinDispositionUnattributable = "unattributable"
	parityJoinDispositionNoSessionKey   = "no_session_key"
)

// The four populations a record at a section 3b site can belong to.
const (
	parityJoinRoleLegacyAct = "legacy_act"
	parityJoinRoleYield     = "yield"
	parityJoinRoleDetector  = "detector"
	parityJoinRoleKeyed     = "keyed"
	parityJoinRoleRefused   = ""
)

// parityJoinDispositionEntry is one (site, reason, disposition) group of records
// that carried no effect_owner stamp. Every owner-absent record lands in
// exactly one group, so the readout accounts for all of them.
type parityJoinDispositionEntry struct {
	Site        TraceSiteCode   `json:"site_code"`
	Reason      TraceReasonCode `json:"reason_code,omitempty"`
	Disposition string          `json:"disposition"`
	Note        string          `json:"note,omitempty"`
	Count       int             `json:"count"`
}

type parityJoinFamilyReport struct {
	Family string          `json:"family"`
	Level  parityJoinLevel `json:"level"`
	// Joined counts act-vs-act pairs: a legacy decision record beside the
	// sweep's record for the same row in the same cycle.
	Joined int `json:"joined"`
	// YieldJoined counts yield-pairs: legacy's traced stand-down beside the
	// actor's record for the same row in the same cycle.
	YieldJoined  int     `json:"yield_joined"`
	YieldOnly    int     `json:"yield_only"`
	LegacyOnly   int     `json:"legacy_only"`
	DetectorOnly int     `json:"detector_only"`
	Keyed        int     `json:"keyed"`
	Matched      int     `json:"matched"`
	Mismatched   int     `json:"mismatched"`
	Incomparable int     `json:"incomparable"`
	Unclassified int     `json:"unclassified"`
	MatchRate    float64 `json:"match_rate"`
	BarMet       bool    `json:"bar_met"`
	// BarStatus is the bar verdict as a word, and it is the only field that
	// separates "every comparable pair disagreed" from "there were no
	// comparable pairs". Both leave match_rate at 0 and bar_met at false,
	// because a family with nothing to compare cannot clear a bar and does not
	// count against one (it is skipped by the report-level blocker below). The
	// human table has always drawn that distinction; a JSON reader could not,
	// and read a no-data family as a total divergence.
	BarStatus string `json:"bar_status"`
}

// parityJoinYieldEntry is one (site, reason) group of legacy stand-downs: the
// yield-side vocabulary as the corpus actually exercised it.
type parityJoinYieldEntry struct {
	Site     TraceSiteCode      `json:"site_code"`
	Reason   TraceReasonCode    `json:"reason_code"`
	Family   string             `json:"family"`
	Arm      parityJoinYieldArm `json:"arm"`
	Joined   int                `json:"joined"`
	Unpaired int                `json:"unpaired"`
	Note     string             `json:"note,omitempty"`
}

type parityJoinTriageEntry struct {
	Family         string `json:"family"`
	Class          string `json:"class"`
	Classification string `json:"classification"`
	Count          int    `json:"count"`
}

type parityJoinSample struct {
	Family          string         `json:"family"`
	Site            TraceSiteCode  `json:"site_code"`
	Side            parityJoinSide `json:"side"`
	TraceID         string         `json:"trace_id"`
	TickID          string         `json:"tick_id"`
	SessionName     string         `json:"session_name"`
	SessionBeadID   string         `json:"session_bead_id,omitempty"`
	LegacyReason    string         `json:"legacy_reason,omitempty"`
	LegacyOutcome   string         `json:"legacy_outcome,omitempty"`
	DetectorReason  string         `json:"detector_reason,omitempty"`
	DetectorOutcome string         `json:"detector_outcome,omitempty"`
}

type parityJoinReport struct {
	SchemaVersion string  `json:"schema_version"`
	Bar           float64 `json:"bar"`
	CountBar      int     `json:"count_bar"`
	// ExcludedWindows is empty — and the key absent — unless --exclude-window
	// was passed, so an unfiltered readout is byte-identical to one produced
	// before the flag existed.
	ExcludedWindows []parityJoinExcludedWindow `json:"excluded_windows,omitempty"`
	Cycles          parityJoinCycleStats       `json:"cycles"`
	// JoinedActs and JoinedYields are the two evidence populations, kept apart
	// because they prove different things: an act-pair is two writers deciding
	// the same row, a yield-pair is one writer deciding it and the other
	// recording that it saw the row and stood down.
	JoinedActs             int                          `json:"joined_acts"`
	JoinedYields           int                          `json:"joined_yields"`
	JoinedTotal            int                          `json:"joined_total"`
	CountBarMet            bool                         `json:"count_bar_met"`
	Families               []parityJoinFamilyReport     `json:"families"`
	Triage                 []parityJoinTriageEntry      `json:"triage"`
	Yields                 []parityJoinYieldEntry       `json:"yields,omitempty"`
	Dispositions           []parityJoinDispositionEntry `json:"dispositions,omitempty"`
	Unclassified           []parityJoinSample           `json:"unclassified,omitempty"`
	ShadowEffectViolations int                          `json:"shadow_effect_violations"`
	NoEvidence             bool                         `json:"no_evidence"`
	BarMet                 bool                         `json:"bar_met"`
	WEBlocker              bool                         `json:"we_blocker"`
}

// newPerfParityJoinCmd builds the hidden `gc perf parity-join` subcommand.
func newPerfParityJoinCmd(stdout io.Writer) *cobra.Command {
	var traceDir, since, windowStart, template string
	var excludeWindows []string
	var bar float64
	var samples, countBar int
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "parity-join",
		Short: "Join legacy and detector-shadow reconciler traces (WD campaign; removed at WE)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			filter := TraceFilter{}
			if strings.TrimSpace(since) != "" && strings.TrimSpace(windowStart) != "" {
				return fmt.Errorf("gc perf parity-join: --since and --window-start are alternatives; pass one")
			}
			if strings.TrimSpace(since) != "" {
				window, err := time.ParseDuration(since)
				if err != nil {
					return fmt.Errorf("gc perf parity-join: parsing --since: %w", err)
				}
				filter.Since = time.Now().UTC().Add(-window)
			}
			if strings.TrimSpace(windowStart) != "" {
				start, err := time.Parse(time.RFC3339, strings.TrimSpace(windowStart))
				if err != nil {
					return fmt.Errorf("gc perf parity-join: parsing --window-start: %w", err)
				}
				filter.Since = start.UTC()
			}
			excluded := make([]parityJoinWindow, 0, len(excludeWindows))
			for _, spec := range excludeWindows {
				window, err := parseParityJoinExcludeWindow(spec)
				if err != nil {
					return fmt.Errorf("gc perf parity-join: parsing --exclude-window: %w", err)
				}
				excluded = append(excluded, window)
			}
			records, err := ReadTraceRecords(traceDir, filter)
			if err != nil {
				return fmt.Errorf("gc perf parity-join: reading trace store %q: %w", traceDir, err)
			}
			report := buildParityJoinReport(records, parityJoinOptions{
				Bar:             bar,
				CountBar:        countBar,
				Samples:         samples,
				Template:        template,
				ExcludedWindows: excluded,
			})
			if jsonOut {
				err = writeCLIJSONLine(stdout, report)
			} else {
				err = writeParityJoinReport(stdout, report)
			}
			if err != nil {
				return fmt.Errorf("gc perf parity-join: writing readout: %w", err)
			}
			return parityJoinVerdictError(report)
		},
	}
	cmd.Flags().StringVar(&traceDir, "trace-dir", "", "session-reconciler-trace store directory (the one holding segments/)")
	cmd.Flags().StringVar(&since, "since", "", "only join records newer than this duration ago (e.g. 168h)")
	cmd.Flags().StringVar(&windowStart, "window-start", "", "only join records at or after this RFC3339 instant; the reproducible form of --since for a fixed campaign window")
	cmd.Flags().StringArrayVar(&excludeWindows, "exclude-window", nil,
		"drop every record in this half-open <RFC3339-start>/<RFC3339-end> span before joining; repeatable. "+
			"Use it for the WD.15 restart rule (cycles between a controller exit and the next good arming "+
			"boundary are excluded), and pad the start back one reconcile tick (~10s) so a pair split across "+
			"the stop is dropped whole. Boundaries are never inferred — pass the window you recorded.")
	cmd.Flags().StringVar(&template, "template", "", "only join records for this normalized template selector")
	cmd.Flags().Float64Var(&bar, "bar", parityJoinDefaultBar, "section 3b must-match bar per family")
	cmd.Flags().IntVar(&countBar, "count-bar", parityJoinDefaultCountBar, "section 3b joined-row count bar for the window")
	cmd.Flags().IntVar(&samples, "samples", parityJoinDefaultSample, "unclassified mismatch samples to report")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit versioned JSON instead of the table")
	_ = cmd.MarkFlagRequired("trace-dir")
	return cmd
}

func parityJoinVerdictError(report parityJoinReport) error {
	switch {
	case report.NoEvidence:
		return fmt.Errorf("gc perf parity-join: no evidence — %d cycles scanned, zero joined; an unarmed window records nothing durable", report.Cycles.Scanned)
	case report.WEBlocker:
		return fmt.Errorf("gc perf parity-join: WE blocker — %d unclassified mismatches, %d shadow effect violations", len(report.Unclassified), report.ShadowEffectViolations)
	default:
		return nil
	}
}

type parityJoinCycleKey struct{ TraceID, TickID string }

type parityJoinCycleBucket struct {
	rollup  *SessionReconcilerTraceRecord
	records []SessionReconcilerTraceRecord
}

type parityJoinRowKey struct {
	Site    TraceSiteCode
	Session string
}

type parityJoinAccumulator struct {
	rows         map[string]*parityJoinFamilyReport
	triage       map[parityJoinTriageEntry]int
	dispositions map[parityJoinDispositionEntry]int
	yields       map[parityJoinYieldKey]*parityJoinYieldEntry
	samples      []parityJoinSample
	opts         parityJoinOptions
	report       *parityJoinReport
	// keyedAcks is the corpus-wide keyed-stamped record index, keyed by session.
	// The join is per-cycle; this is the only thing in the accumulator that
	// crosses a cycle boundary, and only rules that name it can see it.
	keyedAcks map[string][]parityJoinKeyedAck
}

// parityJoinKeyedAck is one keyed-stamped write: the cycle it landed in, the
// site it was written at, and when. It is the evidence an adjacent-cycle class
// rests on.
type parityJoinKeyedAck struct {
	cycle parityJoinCycleKey
	site  TraceSiteCode
	at    time.Time
}

// parityJoinRowContext is the evidence OUTSIDE the two records under comparison
// that a section 3b rule may consult for one row.
type parityJoinRowContext struct {
	// cycle is the cycle this row is being joined in, so a rule can tell an
	// adjacent-cycle twin from one the join already had in hand.
	cycle parityJoinCycleKey
	// coTwins is every reason written for this session anywhere in THIS cycle.
	coTwins map[TraceReasonCode]bool
	// keyedAcks is every keyed-stamped record written for this session anywhere
	// in the corpus, this cycle included.
	keyedAcks []parityJoinKeyedAck
}

type parityJoinYieldKey struct {
	Site   TraceSiteCode
	Reason TraceReasonCode
}

// parityJoinYieldRecord is one stand-down plus the vocabulary entry that says
// what it proves.
type parityJoinYieldRecord struct {
	rec  SessionReconcilerTraceRecord
	spec parityJoinYieldSpec
}

// buildParityJoinReport joins one trace corpus per the section 3b contract.
func buildParityJoinReport(records []SessionReconcilerTraceRecord, opts parityJoinOptions) parityJoinReport {
	if opts.Bar <= 0 {
		opts.Bar = parityJoinDefaultBar
	}
	if opts.Samples <= 0 {
		opts.Samples = parityJoinDefaultSample
	}
	if opts.CountBar <= 0 {
		opts.CountBar = parityJoinDefaultCountBar
	}
	records, excluded := parityJoinExcludeWindows(records, opts.ExcludedWindows)
	report := parityJoinReport{
		SchemaVersion:   parityJoinSchemaV1,
		Bar:             opts.Bar,
		CountBar:        opts.CountBar,
		ExcludedWindows: excluded,
	}
	acc := &parityJoinAccumulator{
		rows:         make(map[string]*parityJoinFamilyReport, len(parityJoinFamilySpecs)),
		triage:       make(map[parityJoinTriageEntry]int),
		dispositions: make(map[parityJoinDispositionEntry]int),
		yields:       make(map[parityJoinYieldKey]*parityJoinYieldEntry),
		opts:         opts,
		report:       &report,
	}
	for i := range parityJoinFamilySpecs {
		spec := &parityJoinFamilySpecs[i]
		acc.rows[spec.Family] = &parityJoinFamilyReport{Family: spec.Family, Level: spec.Level}
	}

	cycles := parityJoinCycles(records, &report.Cycles)
	// Built over every kept cycle BEFORE the join runs. An ack-timing skew's
	// twin is by definition not in the cycle being joined, so it cannot be
	// indexed from inside joinCycle the way the same-cycle co-twins are. Cycles
	// the exclusion windows or the rollup filters dropped are already gone here,
	// so a dropped cycle vouches for nothing.
	acc.keyedAcks = parityJoinKeyedAckIndexOf(cycles, opts)
	for _, cycle := range cycles {
		acc.joinCycle(cycle.key, cycle.bucket)
	}

	report.Families = make([]parityJoinFamilyReport, 0, len(parityJoinFamilySpecs))
	for i := range parityJoinFamilySpecs {
		row := *acc.rows[parityJoinFamilySpecs[i].Family]
		comparable := row.Matched + row.Mismatched
		if comparable > 0 {
			row.MatchRate = float64(row.Matched) / float64(comparable)
			row.BarMet = row.MatchRate >= opts.Bar
		}
		row.BarStatus = parityJoinBarCell(row)
		report.JoinedActs += row.Joined
		report.JoinedYields += row.YieldJoined
		report.Families = append(report.Families, row)
	}
	report.JoinedTotal = report.JoinedActs + report.JoinedYields
	report.CountBarMet = report.JoinedTotal >= opts.CountBar
	report.Triage = parityJoinTriageLog(acc.triage)
	report.Yields = parityJoinYieldLog(acc.yields)
	report.Dispositions = parityJoinDispositionLog(acc.dispositions)
	report.Unclassified = acc.samples
	// The evidence a section 3b readout rests on is JOINED rows. A corpus can be
	// full of owned records and still join nothing — that was exactly the day-0
	// campaign corpus, which reported no_evidence=false beside joined=0 in every
	// family. Counting owned records instead of joined ones made the flag read
	// backwards on the one case it exists to catch.
	report.NoEvidence = report.JoinedTotal == 0
	report.WEBlocker = len(acc.samples) > 0 || report.ShadowEffectViolations > 0
	report.BarMet = !report.NoEvidence && !report.WEBlocker
	for _, row := range report.Families {
		if row.Matched+row.Mismatched > 0 && !row.BarMet {
			report.BarMet = false
		}
	}
	return report
}

type parityJoinOrderedCycle struct {
	key    parityJoinCycleKey
	bucket *parityJoinCycleBucket
}

// parityJoinCycles buckets records by cycle handle and applies the section 3b
// exclusion rules: a cycle whose rollup reports record_budget_exceeded drops is
// dropped from the readout, and so is a cycle whose rollup never landed (its
// evidence is truncated, and counting it would understate divergence).
func parityJoinCycles(records []SessionReconcilerTraceRecord, stats *parityJoinCycleStats) []parityJoinOrderedCycle {
	buckets := make(map[parityJoinCycleKey]*parityJoinCycleBucket)
	order := make([]parityJoinCycleKey, 0, len(records))
	for i := range records {
		rec := records[i]
		key := parityJoinCycleKey{TraceID: rec.TraceID, TickID: rec.TickID}
		bucket, ok := buckets[key]
		if !ok {
			bucket = &parityJoinCycleBucket{}
			buckets[key] = bucket
			order = append(order, key)
		}
		if rec.RecordType == TraceRecordCycleResult {
			bucket.rollup = &rec
			continue
		}
		bucket.records = append(bucket.records, rec)
	}

	kept := make([]parityJoinOrderedCycle, 0, len(order))
	for _, key := range order {
		bucket := buckets[key]
		stats.Scanned++
		switch {
		case bucket.rollup == nil:
			stats.ExcludedNoRollup++
		case parityJoinDropCount(bucket.rollup, "record_budget_exceeded") > 0:
			stats.ExcludedRecordBudget++
		default:
			stats.Considered++
			if parityJoinRollupCount(bucket.rollup, "detailed_template_count") == 0 {
				stats.WithoutDetailArms++
			}
			kept = append(kept, parityJoinOrderedCycle{key: key, bucket: bucket})
		}
	}
	return kept
}

// parityJoinRollupCount reads a cycle-rollup counter from rec.Fields, which is
// where the collector writes every rollup counter
// (session_reconciler_trace_collector.go:970-983, serialized as the nested
// "fields" object). The typed mirrors on the record struct are NOT all
// maintained — nothing in the tree assigns DetailedTemplateCount — so a reader
// of the typed field sees zero on every real rollup. Values arrive as float64
// once the store has round-tripped them through JSON.
func parityJoinRollupCount(rec *SessionReconcilerTraceRecord, key string) int {
	if rec == nil {
		return 0
	}
	return parityJoinInt(rec.Fields[key])
}

// parityJoinDropCount reads one drop-reason counter. This is the one rollup
// counter the collector also mirrors onto a typed field, so either copy is
// authoritative; the Fields copy is a map[string]int in memory and a
// map[string]any once decoded.
func parityJoinDropCount(rec *SessionReconcilerTraceRecord, reason string) int {
	if rec == nil {
		return 0
	}
	if count, ok := rec.DropReasonCounts[reason]; ok {
		return count
	}
	switch counts := rec.Fields["drop_reason_counts"].(type) {
	case map[string]int:
		return counts[reason]
	case map[string]any:
		return parityJoinInt(counts[reason])
	default:
		return 0
	}
}

// parityJoinInt reads a rollup counter in either shape it occurs in: the int the
// collector wrote, or the float64 it becomes once the store has decoded it.
func parityJoinInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	default:
		return 0
	}
}

func (a *parityJoinAccumulator) joinCycle(key parityJoinCycleKey, bucket *parityJoinCycleBucket) {
	legacy := make(map[parityJoinRowKey][]SessionReconcilerTraceRecord)
	detector := make(map[parityJoinRowKey][]SessionReconcilerTraceRecord)
	yields := make(map[parityJoinRowKey][]parityJoinYieldRecord)
	rowOrder := make([]parityJoinRowKey, 0, len(bucket.records))
	seen := make(map[parityJoinRowKey]bool, len(bucket.records))
	// Every reason written for a session anywhere in the cycle, so a cross-family
	// split can be triaged against the twin it names instead of a guess.
	coTwins := parityJoinCoTwinIndex(bucket.records)

	for _, rec := range bucket.records {
		spec, ok := parityJoinSiteFamily[rec.SiteCode]
		if !ok {
			continue
		}
		if !traceTemplateMatches(rec.Template, a.opts.Template) {
			continue
		}
		role, yield, disposition := parityJoinRecordRole(rec)
		if disposition != nil {
			a.dispositions[*disposition]++
			if disposition.Disposition != parityJoinDispositionLegacy {
				a.report.Cycles.UnownedRecords++
				continue
			}
			a.report.Cycles.LegacyByElimination++
		}
		row := parityJoinRowKey{Site: rec.SiteCode, Session: parityJoinSessionKey(rec)}
		if !seen[row] {
			seen[row] = true
			rowOrder = append(rowOrder, row)
		}
		switch role {
		case parityJoinRoleLegacyAct:
			legacy[row] = append(legacy[row], rec)
		case parityJoinRoleYield:
			a.report.Cycles.YieldRecords++
			a.yieldEntry(rec, yield)
			yields[row] = append(yields[row], parityJoinYieldRecord{rec: rec, spec: yield})
		case parityJoinRoleDetector:
			// The sweep is zero-write by design (section 2), whether it routed
			// the condition to a keyed handler or left it in shadow. A sweep
			// record claiming an applied effect breaks the invariant the whole
			// campaign rests on.
			if parityJoinEffectApplied(rec) {
				a.report.ShadowEffectViolations++
			}
			detector[row] = append(detector[row], rec)
		case parityJoinRoleKeyed:
			a.rows[spec.Family].Keyed++
		default:
			a.report.Cycles.UnownedRecords++
		}
	}

	for _, row := range rowOrder {
		left, right, stood := legacy[row], detector[row], yields[row]
		spec := parityJoinSiteFamily[row.Site]

		// Act-vs-act first: two writers deciding the same row is the strongest
		// evidence in the corpus and it gets first claim on the sweep's record.
		acts := min(len(left), len(right))
		for i := range acts {
			a.classify(key, spec, row, coTwins, &left[i], &right[i])
		}
		for i := acts; i < len(left); i++ {
			a.classify(key, spec, row, coTwins, &left[i], nil)
		}

		// Then the yield-join over whatever the act-join did not consume.
		rest := right[acts:]
		pairs := min(len(stood), len(rest))
		for i := range pairs {
			a.classifyYield(key, spec, row, coTwins, stood[i], &rest[i])
		}
		for i := pairs; i < len(stood); i++ {
			a.classifyYield(key, spec, row, coTwins, stood[i], nil)
		}
		for i := pairs; i < len(rest); i++ {
			a.classify(key, spec, row, coTwins, nil, &rest[i])
		}
	}
}

// parityJoinCoTwinIndex maps a session key to every reason code written for it
// anywhere in the cycle, at any site.
func parityJoinCoTwinIndex(records []SessionReconcilerTraceRecord) map[string]map[TraceReasonCode]bool {
	index := make(map[string]map[TraceReasonCode]bool)
	for _, rec := range records {
		session := parityJoinSessionKey(rec)
		if session == "" || rec.ReasonCode == "" {
			continue
		}
		reasons, ok := index[session]
		if !ok {
			reasons = make(map[TraceReasonCode]bool)
			index[session] = reasons
		}
		reasons[rec.ReasonCode] = true
	}
	return index
}

// parityJoinKeyedAckIndexOf maps a session key to every keyed-stamped record
// written for it anywhere in the corpus, at any site the section 3b table owns.
//
// The keyed side is identified by the OWNERSHIP STAMP alone. That is deliberate:
// parityJoinRecordRole reaches parityJoinRoleKeyed only for a record carrying an
// effect_owner that is neither legacy nor the detector shadow, and the keyed
// handler's own ack operations carry no detector_family label — so a
// detector_family predicate, which is what identifies the SWEEP, never fires on
// them and would index nothing.
func parityJoinKeyedAckIndexOf(cycles []parityJoinOrderedCycle, opts parityJoinOptions) map[string][]parityJoinKeyedAck {
	index := make(map[string][]parityJoinKeyedAck)
	for _, cycle := range cycles {
		for _, rec := range cycle.bucket.records {
			if _, ok := parityJoinSiteFamily[rec.SiteCode]; !ok {
				continue
			}
			if !traceTemplateMatches(rec.Template, opts.Template) {
				continue
			}
			session := parityJoinSessionKey(rec)
			if session == "" || rec.Ts.IsZero() {
				continue
			}
			if role, _, _ := parityJoinRecordRole(rec); role != parityJoinRoleKeyed {
				continue
			}
			index[session] = append(index[session], parityJoinKeyedAck{
				cycle: cycle.key,
				site:  rec.SiteCode,
				at:    rec.Ts,
			})
		}
	}
	return index
}

// parityJoinHasAdjacentCycleKeyedTwin answers whether a row carries the twin an
// adjacent-cycle class requires: a keyed-stamped record for the same session, at
// one of the named sites, written in a DIFFERENT cycle within one tick of the
// record under comparison.
//
// Same-cycle keyed records are skipped on purpose. The class exists to explain a
// SKEW, and a keyed record the join already held in the same cycle is evidence
// of the opposite — that the two writers were in step. Absence of the twin is
// the whole answer: the caller keeps looking down the table and the row ends up
// unclassified.
func parityJoinHasAdjacentCycleKeyedTwin(
	sites []TraceSiteCode,
	ctx parityJoinRowContext,
	rec *SessionReconcilerTraceRecord,
) bool {
	if rec == nil || rec.Ts.IsZero() {
		return false
	}
	for _, ack := range ctx.keyedAcks {
		if ack.cycle == ctx.cycle || !slices.Contains(sites, ack.site) {
			continue
		}
		if ack.at.Sub(rec.Ts).Abs() <= parityJoinAdjacentCycleWindow {
			return true
		}
	}
	return false
}

// yieldEntry records one stand-down against the yield-side vocabulary log.
func (a *parityJoinAccumulator) yieldEntry(rec SessionReconcilerTraceRecord, spec parityJoinYieldSpec) *parityJoinYieldEntry {
	key := parityJoinYieldKey{Site: rec.SiteCode, Reason: rec.ReasonCode}
	entry, ok := a.yields[key]
	if !ok {
		entry = &parityJoinYieldEntry{
			Site:   key.Site,
			Reason: key.Reason,
			Family: spec.Family,
			Arm:    spec.Arm,
			Note:   spec.Note,
		}
		a.yields[key] = entry
	}
	return entry
}

// classifyYield scores one stand-down. A yield beside an actor is the pair the
// owner ruling calls D4's side-by-side evidence: legacy identified the row and
// stepped aside, the actor acted, and the two must be talking about the same
// family. A stand-down with nothing beside it proves something only when the
// seam that wrote it sits inside the family's arm — an ownership-arm yield says
// "the keyed controller holds this key" and nothing about the row.
func (a *parityJoinAccumulator) classifyYield(
	cycle parityJoinCycleKey,
	spec *parityJoinFamilySpec,
	row parityJoinRowKey,
	coTwins map[string]map[TraceReasonCode]bool,
	yield parityJoinYieldRecord,
	actor *SessionReconcilerTraceRecord,
) {
	stats := a.rows[spec.Family]
	entry := a.yieldEntry(yield.rec, yield.spec)
	if actor == nil {
		if yield.spec.Arm == parityJoinYieldOwnership {
			entry.Unpaired++
			a.report.Cycles.UnpairedOwnershipYields++
			return
		}
		entry.Unpaired++
		stats.YieldOnly++
		// The unpaired candidacy yield classifies against the SAME co-twin
		// index the act path uses. The live case is the pre-wake supersede's
		// keyed_start_owner spelling (WD.15 day 6, cycle-88f708218a46489c):
		// its detector_wake_target co-twin sits in the same cycle but the
		// act-join consumes that record against legacy's own wake decision, so
		// the yield is what is left — and a nil index here made the
		// twin-REQUIRING class unreachable for a spelling that had its twin.
		// A twinless yield still finds nothing and stays unclassified.
		ctx := a.rowContext(cycle, coTwins, row)
		classification, class := parityJoinClassify(spec, parityJoinSideLegacyOnly, ctx, &yield.rec, nil)
		a.record(cycle, spec, row, parityJoinSideLegacyOnly, classification, class, &yield.rec, nil)
		return
	}

	entry.Joined++
	stats.YieldJoined++
	switch {
	case !parityJoinIdentitiesAgree(yield.rec, *actor):
		a.record(cycle, spec, row, parityJoinSideBoth, parityJoinIncomparable, parityJoinClassBeadIDCrossCheck, &yield.rec, actor)
	case parityJoinActorFamily(*actor, spec) != yield.spec.Family:
		a.record(cycle, spec, row, parityJoinSideBoth, parityJoinMismatched, parityJoinClassYieldFamilyMismatch, &yield.rec, actor)
	default:
		stats.Matched++
	}
}

// parityJoinActorFamily is the family the acting record claims: the sweep's own
// label where it carries one, else the family that owns the site.
func parityJoinActorFamily(rec SessionReconcilerTraceRecord, spec *parityJoinFamilySpec) string {
	label, _ := rec.Fields["detector_family"].(string)
	if family, ok := parityJoinDetectorFamilies[strings.TrimSpace(label)]; ok {
		return family
	}
	return spec.Family
}

func (a *parityJoinAccumulator) classify(
	cycle parityJoinCycleKey,
	spec *parityJoinFamilySpec,
	row parityJoinRowKey,
	coTwins map[string]map[TraceReasonCode]bool,
	legacyRec, shadowRec *SessionReconcilerTraceRecord,
) {
	stats := a.rows[spec.Family]
	side := parityJoinSideBoth
	switch {
	case shadowRec == nil:
		side = parityJoinSideLegacyOnly
		stats.LegacyOnly++
	case legacyRec == nil:
		side = parityJoinSideDetectorOnly
		stats.DetectorOnly++
	default:
		stats.Joined++
	}

	ctx := a.rowContext(cycle, coTwins, row)
	classification, class := parityJoinClassify(spec, side, ctx, legacyRec, shadowRec)
	a.record(cycle, spec, row, side, classification, class, legacyRec, shadowRec)
}

// rowContext resolves the twin evidence for one row: the same-cycle reason index
// the cross-family splits read, and the corpus-wide keyed-stamp index the
// adjacent-cycle classes read.
func (a *parityJoinAccumulator) rowContext(
	cycle parityJoinCycleKey,
	coTwins map[string]map[TraceReasonCode]bool,
	row parityJoinRowKey,
) parityJoinRowContext {
	return parityJoinRowContext{
		cycle:     cycle,
		coTwins:   coTwins[row.Session],
		keyedAcks: a.keyedAcks[row.Session],
	}
}

// record books one classified row into the family counters, the triage log and
// — for an unclassified mismatch — the sample list the WE blocker rests on.
func (a *parityJoinAccumulator) record(
	cycle parityJoinCycleKey,
	spec *parityJoinFamilySpec,
	row parityJoinRowKey,
	side parityJoinSide,
	classification, class string,
	legacyRec, shadowRec *SessionReconcilerTraceRecord,
) {
	stats := a.rows[spec.Family]
	switch classification {
	case parityJoinMatched:
		stats.Matched++
		return
	case parityJoinIncomparable:
		stats.Incomparable++
	default:
		stats.Mismatched++
	}
	a.triage[parityJoinTriageEntry{Family: spec.Family, Class: class, Classification: classification}]++
	if class != parityJoinClassUnclassified {
		return
	}
	stats.Unclassified++
	if len(a.samples) < a.opts.Samples {
		a.samples = append(a.samples, parityJoinSampleOf(cycle, spec.Family, row, side, legacyRec, shadowRec))
	}
}

// parityJoinClassify applies the section 3b classification table to one joined
// row: matched, mismatched, or incomparable, plus the triage class.
func parityJoinClassify(
	spec *parityJoinFamilySpec,
	side parityJoinSide,
	ctx parityJoinRowContext,
	legacyRec, shadowRec *SessionReconcilerTraceRecord,
) (string, string) {
	if side == parityJoinSideBoth {
		if !parityJoinIdentitiesAgree(*legacyRec, *shadowRec) {
			return parityJoinIncomparable, parityJoinClassBeadIDCrossCheck
		}
		// Detection-level families predict only (key, condition); the decision
		// arms are handler-side and deliberately not compared.
		if spec.Level == parityJoinLevelDetection {
			return parityJoinMatched, ""
		}
		// Decision level compares the OUTCOME only. The two writers' reason
		// vocabularies are disjoint by construction — every sweep condition
		// stamps a detector_-prefixed reason (session_reconciler_trace_types.go
		// :171-206) while legacy stamps its own strings — so a reason-equality
		// clause can never hold on a real pair: the campaign corpus pairs
		// idle_timeout/stop with detector_idle_timeout/stop, orphaned/drain with
		// detector_orphan_live/drain, wake/start_candidate with
		// detector_wake_target/start_candidate. The outcome IS the decision
		// section 3b's must-match cells name; the reason is the why-label. The
		// clause read green only against seeded pairs that put one reason on
		// both sides — a shape production cannot write.
		if legacyRec.OutcomeCode == shadowRec.OutcomeCode {
			return parityJoinMatched, ""
		}
	}
	for _, rule := range slices.Concat(spec.Divergences, parityJoinGlobalDivergences) {
		if !parityJoinRuleMatches(rule, side, ctx, legacyRec, shadowRec) {
			continue
		}
		if rule.Classification != "" {
			return rule.Classification, rule.Class
		}
		return parityJoinMismatched, rule.Class
	}
	return parityJoinMismatched, parityJoinClassUnclassified
}

func parityJoinRuleMatches(
	rule parityJoinDivergenceRule,
	side parityJoinSide,
	ctx parityJoinRowContext,
	legacyRec, shadowRec *SessionReconcilerTraceRecord,
) bool {
	if rule.Side != "" && rule.Side != side {
		return false
	}
	if len(rule.CoTwinReasons) > 0 && !parityJoinAnyCoTwin(rule.CoTwinReasons, ctx.coTwins) {
		return false
	}
	if len(rule.AdjacentCycleKeyedTwinSites) > 0 {
		rec := legacyRec
		if rec == nil {
			rec = shadowRec
		}
		if !parityJoinHasAdjacentCycleKeyedTwin(rule.AdjacentCycleKeyedTwinSites, ctx, rec) {
			return false
		}
	}
	if len(rule.DetectorAdmissionOutcomes) > 0 {
		if shadowRec == nil {
			return false
		}
		outcome, _ := shadowRec.Fields["admission_outcome"].(string)
		if !slices.Contains(rule.DetectorAdmissionOutcomes, strings.TrimSpace(outcome)) {
			return false
		}
	}
	site := TraceSiteUnknown
	if legacyRec != nil {
		site = legacyRec.SiteCode
	} else if shadowRec != nil {
		site = shadowRec.SiteCode
	}
	if len(rule.Sites) > 0 && !slices.Contains(rule.Sites, site) {
		return false
	}
	if !parityJoinCodesMatch(legacyRec, rule.LegacyReasons, rule.LegacyOutcomes) {
		return false
	}
	if !parityJoinCodesMatch(shadowRec, rule.DetectorReasons, rule.DetectorOutcomes) {
		return false
	}
	if len(rule.AnyReasons) > 0 && !parityJoinAnyReason(rule.AnyReasons, legacyRec, shadowRec) {
		return false
	}
	if len(rule.AnyOutcomes) > 0 && !parityJoinAnyOutcome(rule.AnyOutcomes, legacyRec, shadowRec) {
		return false
	}
	return true
}

func parityJoinCodesMatch(rec *SessionReconcilerTraceRecord, reasons []TraceReasonCode, outcomes []TraceOutcomeCode) bool {
	if len(reasons) == 0 && len(outcomes) == 0 {
		return true
	}
	if rec == nil {
		return false
	}
	if len(reasons) > 0 && !slices.Contains(reasons, rec.ReasonCode) {
		return false
	}
	if len(outcomes) > 0 && !slices.Contains(outcomes, rec.OutcomeCode) {
		return false
	}
	return true
}

func parityJoinAnyReason(reasons []TraceReasonCode, recs ...*SessionReconcilerTraceRecord) bool {
	for _, rec := range recs {
		if rec != nil && slices.Contains(reasons, rec.ReasonCode) {
			return true
		}
	}
	return false
}

func parityJoinAnyOutcome(outcomes []TraceOutcomeCode, recs ...*SessionReconcilerTraceRecord) bool {
	for _, rec := range recs {
		if rec != nil && slices.Contains(outcomes, rec.OutcomeCode) {
			return true
		}
	}
	return false
}

func parityJoinAnyCoTwin(reasons []TraceReasonCode, coTwins map[TraceReasonCode]bool) bool {
	for _, reason := range reasons {
		if coTwins[reason] {
			return true
		}
	}
	return false
}

// parityJoinIdentitiesAgree is the section 3b cross-check on the normalized
// session-name join key. Two rows that share a name but not an identity are a
// D-DUP shape, not a comparable pair.
func parityJoinIdentitiesAgree(left, right SessionReconcilerTraceRecord) bool {
	leftID, rightID := parityJoinSessionIdentity(left), parityJoinSessionIdentity(right)
	if leftID == "" || rightID == "" {
		return true
	}
	return leftID == rightID
}

// parityJoinSessionIdentity is the bead identity behind a record's session
// name. The typed field carries it on the records that have one; the reconciler
// decision sites put it in the payload instead, under either spelling, and on
// the campaign corpus that payload copy is the ONLY copy — a cross-check that
// read the typed field alone would silently pass on every row.
func parityJoinSessionIdentity(rec SessionReconcilerTraceRecord) string {
	if id := strings.TrimSpace(rec.SessionBeadID); id != "" {
		return id
	}
	for _, key := range []string{"session_bead_id", "session_id"} {
		if id, ok := rec.Fields[key].(string); ok && strings.TrimSpace(id) != "" {
			return strings.TrimSpace(id)
		}
	}
	return ""
}

func parityJoinSessionKey(rec SessionReconcilerTraceRecord) string {
	if name := strings.TrimSpace(rec.SessionName); name != "" {
		return name
	}
	if name, ok := rec.Fields["session_name"].(string); ok && strings.TrimSpace(name) != "" {
		return strings.TrimSpace(name)
	}
	return strings.TrimSpace(rec.SessionBeadID)
}

// parityJoinRecordRole resolves which population wrote a record, and for a
// legacy stand-down also which seam wrote it.
//
// The order matters. A YIELD is identified by its reason before anything else,
// because most seams stamp effect_owner=keyed (the effect really is keyed's) and
// two of them stamp nothing — so neither the stamp nor its absence can tell a
// stand-down from an act. A SWEEP record is identified by carrying the sweep's
// own family label under a detector_-prefixed reason, which is what separates
// the detector's judgment from a keyed handler's own operation record, and what
// lets a ROUTED condition (stamped keyed, not detector-shadow) join at all —
// without that, every routed family's evidence fell on the floor.
//
// What is left is the pre-ruling classification: a stamped record is taken at
// its word, and an unstamped one is classified by ELIMINATION —
//
// Elimination beats teaching the god function to stamp for two reasons. The
// stamp would be scaffolding threaded through ~19 legacy decision sites in a
// function scheduled for deletion at WE, and it would need to survive every
// remaining WD slice; the classification rule is one function that dies with
// the tool. And it is sound where a log-line census would not be: a log line
// carries no engine attribution — the correction recorded on ga-ij8mh turned on
// exactly that, a legacy-looking line that no engine could be pinned to — while
// a trace record carries the site code and the stamp its keyed and detector
// writers already emit, which is enough to eliminate them.
//
// The guard is that absence only attributes inside the section 1 legacy
// vocabulary. A phase site's per-cycle marker, a keyed-owned or shared-writer
// site, and a record with no session identity to join on are each refused with
// the reason, counted, and surfaced — never silently binned as legacy.
func parityJoinRecordRole(rec SessionReconcilerTraceRecord) (string, parityJoinYieldSpec, *parityJoinDispositionEntry) {
	site := parityJoinSiteDispositions[rec.SiteCode]
	entry := &parityJoinDispositionEntry{Site: rec.SiteCode, Reason: rec.ReasonCode, Note: site.Note}
	// A phase site's per-cycle marker is refused before anything else: binning it
	// would manufacture one phantom row per cycle in D-DUP and D-STRANDED, whose
	// only sites are phase sites. What makes it a marker rather than a decision
	// is that it carries no session identity, so a per-session record at a phase
	// site still falls through to the normal resolution.
	if site.Attribution == parityJoinSitePhase && parityJoinSessionKey(rec) == "" {
		entry.Disposition = parityJoinDispositionPhaseMarker
		return parityJoinRoleRefused, parityJoinYieldSpec{}, entry
	}
	// A record with no session identity is not a row in a per-session join, no
	// matter who wrote it or whether it carries a stamp. The live case is the
	// sweep's pool-under-min FILL condition
	// (detectorAdmissionQueuedPoolAllocation): a wake for a session that does
	// not exist yet, so there is no row to join it against, and scoring it as a
	// detector-only singleton invents a divergence out of an empty key.
	if parityJoinSessionKey(rec) == "" {
		entry.Disposition = parityJoinDispositionNoSessionKey
		entry.Note = "no session identity: not a row in a per-session join"
		return parityJoinRoleRefused, parityJoinYieldSpec{}, entry
	}
	if yield, ok := parityJoinYieldOf(rec); ok {
		return parityJoinRoleYield, yield, nil
	}
	if parityJoinIsSweepRecord(rec) {
		return parityJoinRoleDetector, parityJoinYieldSpec{}, nil
	}
	stamped, _ := rec.Fields["effect_owner"].(string)
	switch strings.TrimSpace(stamped) {
	case "":
	case parityJoinOwnerDetectorShadow:
		return parityJoinRoleDetector, parityJoinYieldSpec{}, nil
	case parityJoinOwnerLegacy:
		return parityJoinRoleLegacyAct, parityJoinYieldSpec{}, nil
	default:
		return parityJoinRoleKeyed, parityJoinYieldSpec{}, nil
	}
	if site.Attribution != parityJoinSiteLegacy {
		entry.Disposition = parityJoinDispositionUnattributable
		return parityJoinRoleRefused, parityJoinYieldSpec{}, entry
	}
	entry.Disposition = parityJoinDispositionLegacy
	return parityJoinRoleLegacyAct, parityJoinYieldSpec{}, entry
}

// parityJoinIsSweepRecord identifies the detector sweep's own record: it is the
// only writer that stamps its family label beside a detector_-prefixed reason.
// The stamp alone cannot do it — the sweep writes effect_owner=keyed for a
// condition it ROUTED (session_detector_sweep.go:2103-2130) and a keyed
// handler's own operation records carry the same stamp.
func parityJoinIsSweepRecord(rec SessionReconcilerTraceRecord) bool {
	label, _ := rec.Fields["detector_family"].(string)
	if strings.TrimSpace(label) == "" {
		return false
	}
	return strings.HasPrefix(string(rec.ReasonCode), "detector_")
}

func parityJoinDispositionLog(counts map[parityJoinDispositionEntry]int) []parityJoinDispositionEntry {
	log := make([]parityJoinDispositionEntry, 0, len(counts))
	for entry, count := range counts {
		entry.Count = count
		log = append(log, entry)
	}
	sort.Slice(log, func(i, j int) bool {
		if log[i].Site != log[j].Site {
			return log[i].Site < log[j].Site
		}
		return log[i].Reason < log[j].Reason
	})
	return log
}

func parityJoinEffectApplied(rec SessionReconcilerTraceRecord) bool {
	applied, _ := rec.Fields["effect_applied"].(bool)
	return applied
}

func parityJoinSampleOf(
	cycle parityJoinCycleKey,
	family string,
	row parityJoinRowKey,
	side parityJoinSide,
	legacyRec, shadowRec *SessionReconcilerTraceRecord,
) parityJoinSample {
	sample := parityJoinSample{
		Family:      family,
		Site:        row.Site,
		Side:        side,
		TraceID:     cycle.TraceID,
		TickID:      cycle.TickID,
		SessionName: row.Session,
	}
	if legacyRec != nil {
		sample.SessionBeadID = legacyRec.SessionBeadID
		sample.LegacyReason = string(legacyRec.ReasonCode)
		sample.LegacyOutcome = string(legacyRec.OutcomeCode)
	}
	if shadowRec != nil {
		if sample.SessionBeadID == "" {
			sample.SessionBeadID = shadowRec.SessionBeadID
		}
		sample.DetectorReason = string(shadowRec.ReasonCode)
		sample.DetectorOutcome = string(shadowRec.OutcomeCode)
	}
	return sample
}

func parityJoinYieldLog(entries map[parityJoinYieldKey]*parityJoinYieldEntry) []parityJoinYieldEntry {
	log := make([]parityJoinYieldEntry, 0, len(entries))
	for _, entry := range entries {
		log = append(log, *entry)
	}
	sort.Slice(log, func(i, j int) bool {
		if log[i].Site != log[j].Site {
			return log[i].Site < log[j].Site
		}
		return log[i].Reason < log[j].Reason
	})
	return log
}

func parityJoinTriageLog(counts map[parityJoinTriageEntry]int) []parityJoinTriageEntry {
	log := make([]parityJoinTriageEntry, 0, len(counts))
	for entry, count := range counts {
		entry.Count = count
		log = append(log, entry)
	}
	sort.Slice(log, func(i, j int) bool {
		if log[i].Family != log[j].Family {
			return log[i].Family < log[j].Family
		}
		return log[i].Class < log[j].Class
	})
	return log
}

// writeParityJoinReport renders the human readout.
func writeParityJoinReport(w io.Writer, report parityJoinReport) error {
	var b strings.Builder
	fmt.Fprintf(&b, "\ngc perf parity-join (DETECTOR.md 3b, bar %.3f)\n\n", report.Bar)
	for _, window := range report.ExcludedWindows {
		fmt.Fprintf(&b, "window: %s/%s excluded before the join — %d records dropped\n",
			window.Start.Format(time.RFC3339), window.End.Format(time.RFC3339), window.RecordsExcluded)
	}
	fmt.Fprintf(&b, "cycles: %d scanned, %d considered, %d excluded (record_budget_exceeded=%d, no_rollup=%d)\n",
		report.Cycles.Scanned, report.Cycles.Considered,
		report.Cycles.ExcludedRecordBudget+report.Cycles.ExcludedNoRollup,
		report.Cycles.ExcludedRecordBudget, report.Cycles.ExcludedNoRollup)
	fmt.Fprintf(&b, "arms:   %d considered cycles carried no detail arm\n", report.Cycles.WithoutDetailArms)
	fmt.Fprintf(&b, "owner:  %d unstamped records classified legacy by elimination; %d refused (see OWNER-ABSENT)\n",
		report.Cycles.LegacyByElimination, report.Cycles.UnownedRecords)
	fmt.Fprintf(&b, "joined: %d total (%d yield-pairs, %d act-pairs) against a count bar of %d — %s\n",
		report.JoinedTotal, report.JoinedYields, report.JoinedActs, report.CountBar,
		parityJoinCountBarCell(report))
	fmt.Fprintf(&b, "yields: %d stand-downs; %d unpaired ownership-arm yields carry no candidacy (see YIELDS)\n\n",
		report.Cycles.YieldRecords, report.Cycles.UnpairedOwnershipYields)

	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FAMILY\tLEVEL\tY-JOIN\tY-ONLY\tJOINED\tLEG-ONLY\tDET-ONLY\tKEYED\tMATCH\tMISMATCH\tINCOMP\tUNCLASS\tRATE\tBAR") //nolint:errcheck
	for _, row := range report.Families {
		rate := "-"
		if row.Matched+row.Mismatched > 0 {
			rate = fmt.Sprintf("%.2f%%", row.MatchRate*100)
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%s\n", //nolint:errcheck
			row.Family, row.Level, row.YieldJoined, row.YieldOnly, row.Joined, row.LegacyOnly, row.DetectorOnly, row.Keyed,
			row.Matched, row.Mismatched, row.Incomparable, row.Unclassified, rate,
			parityJoinBarCell(row))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if len(report.Yields) > 0 {
		b.WriteString("\nYIELDS (legacy's traced stand-downs — the yield-join's left-hand side)\n")
		for _, entry := range report.Yields {
			fmt.Fprintf(&b, "  %-40s %-32s %-14s %-10s joined %6d  unpaired %6d  %s\n",
				entry.Site, entry.Reason, entry.Family, entry.Arm, entry.Joined, entry.Unpaired, entry.Note)
		}
	}
	if len(report.Triage) > 0 {
		b.WriteString("\nTRIAGE\n")
		for _, entry := range report.Triage {
			fmt.Fprintf(&b, "  %-14s %-40s %-12s %d\n", entry.Family, entry.Class, entry.Classification, entry.Count)
		}
	}
	if len(report.Dispositions) > 0 {
		b.WriteString("\nOWNER-ABSENT (records carrying no effect_owner stamp)\n")
		for _, entry := range report.Dispositions {
			fmt.Fprintf(&b, "  %-44s %-26s %-18s %6d  %s\n",
				entry.Site, entry.Reason, entry.Disposition, entry.Count, entry.Note)
		}
	}
	for _, sample := range report.Unclassified {
		fmt.Fprintf(&b, "  unclassified: %s %s %s session=%s bead=%s legacy=(%s,%s) detector=(%s,%s) cycle=%s/%s\n",
			sample.Family, sample.Site, sample.Side, sample.SessionName, sample.SessionBeadID,
			sample.LegacyReason, sample.LegacyOutcome, sample.DetectorReason, sample.DetectorOutcome,
			sample.TraceID, sample.TickID)
	}
	fmt.Fprintf(&b, "\nRESULT: %s\n", parityJoinVerdict(report))
	_, err := io.WriteString(w, b.String())
	return err
}

// The three bar verdicts. "no-data" is not a failing grade: it says the family
// produced no comparable pair, so neither the rate nor bar_met carries meaning.
const (
	parityJoinBarNoData = "no-data"
	parityJoinBarOK     = "ok"
	parityJoinBarBelow  = "BELOW"
)

func parityJoinBarCell(row parityJoinFamilyReport) string {
	switch {
	case row.Matched+row.Mismatched == 0:
		return parityJoinBarNoData
	case row.BarMet:
		return parityJoinBarOK
	default:
		return parityJoinBarBelow
	}
}

func parityJoinCountBarCell(report parityJoinReport) string {
	if report.CountBarMet {
		return "reached"
	}
	return "WINDOW SHORT"
}

func parityJoinVerdict(report parityJoinReport) string {
	switch {
	case report.NoEvidence:
		return "NO EVIDENCE — zero joined records; an unarmed window records nothing durable"
	case report.ShadowEffectViolations > 0:
		return fmt.Sprintf("WE BLOCKER — %d detector-shadow records claimed an applied effect", report.ShadowEffectViolations)
	case len(report.Unclassified) > 0:
		return fmt.Sprintf("WE BLOCKER — unclassified mismatches: %d; triage them into the 3b table", len(report.Unclassified))
	case !report.BarMet:
		return fmt.Sprintf("BELOW BAR — a must-match family is under %.3f", report.Bar)
	default:
		return "PASS"
	}
}
