package main

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
)

const reconcilerPerfSchemaV1 = "gascity.reconciler-comparison.v1"

type reconcilerPerfAction string

const (
	reconcilerPerfActionStart reconcilerPerfAction = "start"
	reconcilerPerfActionStop  reconcilerPerfAction = "stop"
	reconcilerPerfActionNudge reconcilerPerfAction = "nudge"
)

var reconcilerPerfRequiredActions = []reconcilerPerfAction{
	reconcilerPerfActionStart,
	reconcilerPerfActionStop,
	reconcilerPerfActionNudge,
}

type reconcilerPerfProvenance struct {
	Commit      string `json:"commit"`
	Dirty       bool   `json:"dirty"`
	GOOS        string `json:"goos"`
	GOARCH      string `json:"goarch"`
	CPUs        int    `json:"cpus"`
	Store       string `json:"store"`
	StoreSchema string `json:"store_schema"`
	Runtime     string `json:"runtime"`
	Workload    string `json:"workload"`
}

type reconcilerPerfWarmupPolicy struct {
	PairsPerAction int    `json:"pairs_per_action"`
	Excluded       bool   `json:"excluded_from_statistics"`
	ExecutionOrder string `json:"execution_order"`
}

type reconcilerPerfArmSample struct {
	LatencyNS *int64 `json:"latency_ns"`
	Outcome   string `json:"outcome"`
	Error     string `json:"error,omitempty"`
}

type reconcilerPerfPairSample struct {
	PairID string                  `json:"pair_id"`
	Legacy reconcilerPerfArmSample `json:"legacy"`
	Keyed  reconcilerPerfArmSample `json:"keyed"`
}

type reconcilerPerfActionCohort struct {
	Action         reconcilerPerfAction
	LegacyWindowNS int64
	KeyedWindowNS  int64
	Pairs          []reconcilerPerfPairSample
}

type reconcilerPerfReportInput struct {
	Provenance reconcilerPerfProvenance
	Warmup     reconcilerPerfWarmupPolicy
	Cohorts    []reconcilerPerfActionCohort
}

type reconcilerPerfLatencyStats struct {
	P50NS int64 `json:"p50_ns"`
	P95NS int64 `json:"p95_ns"`
	P99NS int64 `json:"p99_ns"`
	MaxNS int64 `json:"max_ns"`
}

type reconcilerPerfArmSummary struct {
	AttemptedCount      int                         `json:"attempted_count"`
	SampleCount         int                         `json:"sample_count"`
	ErrorCount          int                         `json:"error_count"`
	MeasurementWindowNS int64                       `json:"measurement_window_ns"`
	ThroughputPerSecond float64                     `json:"throughput_per_second"`
	Latency             *reconcilerPerfLatencyStats `json:"latency"`
}

type reconcilerPerfActionSummary struct {
	Action          reconcilerPerfAction     `json:"action"`
	PairCount       int                      `json:"pair_count"`
	MismatchCount   int                      `json:"mismatch_count"`
	MismatchPairIDs []string                 `json:"mismatch_pair_ids"`
	Legacy          reconcilerPerfArmSummary `json:"legacy"`
	Keyed           reconcilerPerfArmSummary `json:"keyed"`
}

type reconcilerPerfCoverage struct {
	RequiredActions int      `json:"required_actions"`
	MeasuredActions int      `json:"measured_actions"`
	MissingActions  []string `json:"missing_actions"`
}

type reconcilerPerfTotals struct {
	PairCount        int `json:"pair_count"`
	MismatchCount    int `json:"mismatch_count"`
	LegacyErrorCount int `json:"legacy_error_count"`
	KeyedErrorCount  int `json:"keyed_error_count"`
}

type reconcilerPerfReport struct {
	SchemaVersion string                        `json:"schema_version"`
	OK            bool                          `json:"ok"`
	Provenance    reconcilerPerfProvenance      `json:"provenance"`
	Warmup        reconcilerPerfWarmupPolicy    `json:"warmup"`
	Coverage      reconcilerPerfCoverage        `json:"coverage"`
	Actions       []reconcilerPerfActionSummary `json:"actions"`
	Totals        reconcilerPerfTotals          `json:"totals"`
}

func computeReconcilerPerfLatencyStats(samples []int64) reconcilerPerfLatencyStats {
	if len(samples) == 0 {
		return reconcilerPerfLatencyStats{}
	}
	ordered := append([]int64(nil), samples...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	nearestRank := func(percentile float64) int64 {
		index := int(math.Ceil(percentile*float64(len(ordered)))) - 1
		return ordered[index]
	}
	return reconcilerPerfLatencyStats{
		P50NS: nearestRank(0.50),
		P95NS: nearestRank(0.95),
		P99NS: nearestRank(0.99),
		MaxNS: ordered[len(ordered)-1],
	}
}

func buildReconcilerPerfReport(input reconcilerPerfReportInput) (reconcilerPerfReport, error) {
	if err := validateReconcilerPerfProvenance(input.Provenance); err != nil {
		return reconcilerPerfReport{}, err
	}
	if input.Warmup.PairsPerAction < 0 {
		return reconcilerPerfReport{}, fmt.Errorf("warmup pairs per action must be non-negative")
	}
	if strings.TrimSpace(input.Warmup.ExecutionOrder) == "" {
		return reconcilerPerfReport{}, fmt.Errorf("warmup execution order is required")
	}
	if len(input.Cohorts) == 0 {
		return reconcilerPerfReport{}, fmt.Errorf("at least one action cohort is required")
	}

	cohortsByAction := make(map[reconcilerPerfAction]reconcilerPerfActionCohort, len(input.Cohorts))
	for _, cohort := range input.Cohorts {
		if !isReconcilerPerfAction(cohort.Action) {
			return reconcilerPerfReport{}, fmt.Errorf("unsupported action %q", cohort.Action)
		}
		if _, duplicate := cohortsByAction[cohort.Action]; duplicate {
			return reconcilerPerfReport{}, fmt.Errorf("duplicate action cohort %q", cohort.Action)
		}
		cohortsByAction[cohort.Action] = cohort
	}

	report := reconcilerPerfReport{
		SchemaVersion: reconcilerPerfSchemaV1,
		OK:            true,
		Provenance:    input.Provenance,
		Warmup:        input.Warmup,
		Coverage: reconcilerPerfCoverage{
			RequiredActions: len(reconcilerPerfRequiredActions),
			MeasuredActions: len(cohortsByAction),
		},
		Actions: make([]reconcilerPerfActionSummary, 0, len(cohortsByAction)),
	}
	for _, action := range reconcilerPerfRequiredActions {
		cohort, measured := cohortsByAction[action]
		if !measured {
			report.Coverage.MissingActions = append(report.Coverage.MissingActions, string(action))
			continue
		}
		summary, err := summarizeReconcilerPerfCohort(cohort)
		if err != nil {
			return reconcilerPerfReport{}, fmt.Errorf("%s cohort: %w", action, err)
		}
		report.Actions = append(report.Actions, summary)
		report.Totals.PairCount += summary.PairCount
		report.Totals.MismatchCount += summary.MismatchCount
		report.Totals.LegacyErrorCount += summary.Legacy.ErrorCount
		report.Totals.KeyedErrorCount += summary.Keyed.ErrorCount
	}
	return report, nil
}

func validateReconcilerPerfProvenance(provenance reconcilerPerfProvenance) error {
	required := []struct {
		name  string
		value string
	}{
		{"commit", provenance.Commit},
		{"goos", provenance.GOOS},
		{"goarch", provenance.GOARCH},
		{"store", provenance.Store},
		{"store schema", provenance.StoreSchema},
		{"runtime", provenance.Runtime},
		{"workload", provenance.Workload},
	}
	for _, field := range required {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("provenance %s is required", field.name)
		}
	}
	if provenance.CPUs <= 0 {
		return fmt.Errorf("provenance CPUs must be positive")
	}
	return nil
}

func isReconcilerPerfAction(action reconcilerPerfAction) bool {
	switch action {
	case reconcilerPerfActionStart, reconcilerPerfActionStop, reconcilerPerfActionNudge:
		return true
	default:
		return false
	}
}

func summarizeReconcilerPerfCohort(
	cohort reconcilerPerfActionCohort,
) (reconcilerPerfActionSummary, error) {
	if cohort.LegacyWindowNS <= 0 {
		return reconcilerPerfActionSummary{}, fmt.Errorf("legacy measurement window must be positive")
	}
	if cohort.KeyedWindowNS <= 0 {
		return reconcilerPerfActionSummary{}, fmt.Errorf("keyed measurement window must be positive")
	}
	if len(cohort.Pairs) == 0 {
		return reconcilerPerfActionSummary{}, fmt.Errorf("at least one measured pair is required")
	}

	pairs := append([]reconcilerPerfPairSample(nil), cohort.Pairs...)
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].PairID < pairs[j].PairID })
	seen := make(map[string]struct{}, len(pairs))
	legacyLatencies := make([]int64, 0, len(pairs))
	keyedLatencies := make([]int64, 0, len(pairs))
	summary := reconcilerPerfActionSummary{
		Action:          cohort.Action,
		PairCount:       len(pairs),
		MismatchPairIDs: []string{},
	}
	for _, pair := range pairs {
		if strings.TrimSpace(pair.PairID) == "" {
			return reconcilerPerfActionSummary{}, fmt.Errorf("pair ID is required")
		}
		if _, duplicate := seen[pair.PairID]; duplicate {
			return reconcilerPerfActionSummary{}, fmt.Errorf("duplicate pair %q", pair.PairID)
		}
		seen[pair.PairID] = struct{}{}
		if err := validateReconcilerPerfArm("legacy", pair.PairID, pair.Legacy); err != nil {
			return reconcilerPerfActionSummary{}, err
		}
		if err := validateReconcilerPerfArm("keyed", pair.PairID, pair.Keyed); err != nil {
			return reconcilerPerfActionSummary{}, err
		}
		if pair.Legacy.LatencyNS != nil {
			legacyLatencies = append(legacyLatencies, *pair.Legacy.LatencyNS)
		}
		if pair.Keyed.LatencyNS != nil {
			keyedLatencies = append(keyedLatencies, *pair.Keyed.LatencyNS)
		}
		if pair.Legacy.Error != "" {
			summary.Legacy.ErrorCount++
		}
		if pair.Keyed.Error != "" {
			summary.Keyed.ErrorCount++
		}
		if pair.Legacy.Outcome != pair.Keyed.Outcome ||
			(pair.Legacy.Error == "") != (pair.Keyed.Error == "") {
			summary.MismatchPairIDs = append(summary.MismatchPairIDs, pair.PairID)
		}
	}

	summary.MismatchCount = len(summary.MismatchPairIDs)
	summary.Legacy = finishReconcilerPerfArmSummary(
		summary.Legacy,
		len(pairs),
		cohort.LegacyWindowNS,
		legacyLatencies,
	)
	summary.Keyed = finishReconcilerPerfArmSummary(
		summary.Keyed,
		len(pairs),
		cohort.KeyedWindowNS,
		keyedLatencies,
	)
	return summary, nil
}

func validateReconcilerPerfArm(name, pairID string, sample reconcilerPerfArmSample) error {
	if strings.TrimSpace(sample.Outcome) == "" {
		return fmt.Errorf("pair %q %s outcome is required", pairID, name)
	}
	if sample.LatencyNS == nil && strings.TrimSpace(sample.Error) == "" {
		return fmt.Errorf("pair %q %s latency or error is required", pairID, name)
	}
	if sample.LatencyNS != nil && *sample.LatencyNS < 0 {
		return fmt.Errorf("pair %q %s latency must be non-negative", pairID, name)
	}
	return nil
}

func finishReconcilerPerfArmSummary(
	summary reconcilerPerfArmSummary,
	attempted int,
	windowNS int64,
	latencies []int64,
) reconcilerPerfArmSummary {
	summary.AttemptedCount = attempted
	summary.SampleCount = len(latencies)
	summary.MeasurementWindowNS = windowNS
	summary.ThroughputPerSecond = float64(len(latencies)) * 1e9 / float64(windowNS)
	if len(latencies) != 0 {
		stats := computeReconcilerPerfLatencyStats(latencies)
		summary.Latency = &stats
	}
	return summary
}

func writeReconcilerPerfReport(w io.Writer, report reconcilerPerfReport) error {
	if _, err := fmt.Fprintf(w, "Reconciler comparison %s\n", report.SchemaVersion); err != nil {
		return fmt.Errorf("writing reconciler comparison heading: %w", err)
	}
	if _, err := fmt.Fprintf(
		w,
		"provenance: commit=%s dirty=%t %s/%s cpus=%d store=%s schema=%s runtime=%s workload=%s\n",
		report.Provenance.Commit,
		report.Provenance.Dirty,
		report.Provenance.GOOS,
		report.Provenance.GOARCH,
		report.Provenance.CPUs,
		report.Provenance.Store,
		report.Provenance.StoreSchema,
		report.Provenance.Runtime,
		report.Provenance.Workload,
	); err != nil {
		return fmt.Errorf("writing reconciler comparison provenance: %w", err)
	}
	missing := "none"
	if len(report.Coverage.MissingActions) != 0 {
		missing = strings.Join(report.Coverage.MissingActions, ", ")
	}
	if _, err := fmt.Fprintf(
		w,
		"coverage: %d/%d actions (missing: %s)\n",
		report.Coverage.MeasuredActions,
		report.Coverage.RequiredActions,
		missing,
	); err != nil {
		return fmt.Errorf("writing reconciler comparison coverage: %w", err)
	}
	warmupDisposition := "included"
	if report.Warmup.Excluded {
		warmupDisposition = "excluded"
	}
	pairWord := "pairs"
	if report.Warmup.PairsPerAction == 1 {
		pairWord = "pair"
	}
	if _, err := fmt.Fprintf(
		w,
		"warmup: %d %s/action, %s; order=%s\n",
		report.Warmup.PairsPerAction,
		pairWord,
		warmupDisposition,
		report.Warmup.ExecutionOrder,
	); err != nil {
		return fmt.Errorf("writing reconciler comparison warmup: %w", err)
	}
	for _, action := range report.Actions {
		if _, err := fmt.Fprintf(
			w,
			"%s: pairs=%d mismatches=%d\n",
			action.Action,
			action.PairCount,
			action.MismatchCount,
		); err != nil {
			return fmt.Errorf("writing reconciler comparison action %q: %w", action.Action, err)
		}
		if err := writeReconcilerPerfArmSummary(w, "legacy", action.Legacy); err != nil {
			return err
		}
		if err := writeReconcilerPerfArmSummary(w, "keyed", action.Keyed); err != nil {
			return err
		}
	}
	return nil
}

func writeReconcilerPerfArmSummary(
	w io.Writer,
	name string,
	summary reconcilerPerfArmSummary,
) error {
	latency := "p50=n/a p95=n/a p99=n/a max=n/a"
	if summary.Latency != nil {
		latency = fmt.Sprintf(
			"p50=%.3fms p95=%.3fms p99=%.3fms max=%.3fms",
			float64(summary.Latency.P50NS)/1e6,
			float64(summary.Latency.P95NS)/1e6,
			float64(summary.Latency.P99NS)/1e6,
			float64(summary.Latency.MaxNS)/1e6,
		)
	}
	if _, err := fmt.Fprintf(
		w,
		"  %s: samples=%d/%d errors=%d throughput=%.3f/s %s\n",
		name,
		summary.SampleCount,
		summary.AttemptedCount,
		summary.ErrorCount,
		summary.ThroughputPerSecond,
		latency,
	); err != nil {
		return fmt.Errorf("writing reconciler comparison %s summary: %w", name, err)
	}
	return nil
}
