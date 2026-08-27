package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestReconcilerPerfLatencyStatsUsesNearestRank(t *testing.T) {
	t.Parallel()

	samples := make([]int64, 100)
	for i := range samples {
		samples[i] = int64(100 - i)
	}

	got := computeReconcilerPerfLatencyStats(samples)
	want := reconcilerPerfLatencyStats{
		P50NS: 50,
		P95NS: 95,
		P99NS: 99,
		MaxNS: 100,
	}
	if got != want {
		t.Fatalf("latency stats = %+v, want %+v", got, want)
	}
}

func TestBuildReconcilerPerfReportAggregatesPairedStartDeterministically(t *testing.T) {
	t.Parallel()

	input := reconcilerPerfReportInput{
		Provenance: validReconcilerPerfProvenance(),
		Warmup: reconcilerPerfWarmupPolicy{
			PairsPerAction: 1,
			Excluded:       true,
			ExecutionOrder: "alternating_legacy_first",
		},
		Cohorts: []reconcilerPerfActionCohort{{
			Action:         reconcilerPerfActionStart,
			LegacyWindowNS: (2 * time.Second).Nanoseconds(),
			KeyedWindowNS:  time.Second.Nanoseconds(),
			Pairs: []reconcilerPerfPairSample{
				{
					PairID: "pair-b",
					Legacy: reconcilerPerfArmSample{LatencyNS: latencyNS(95), Outcome: "started"},
					Keyed:  reconcilerPerfArmSample{LatencyNS: latencyNS(20), Outcome: "started"},
				},
				{
					PairID: "pair-a",
					Legacy: reconcilerPerfArmSample{LatencyNS: latencyNS(50), Outcome: "started"},
					Keyed:  reconcilerPerfArmSample{LatencyNS: latencyNS(10), Outcome: "started"},
				},
				{
					PairID: "pair-c",
					Legacy: reconcilerPerfArmSample{Outcome: "provider_error", Error: "legacy unavailable"},
					Keyed:  reconcilerPerfArmSample{LatencyNS: latencyNS(30), Outcome: "started"},
				},
			},
		}},
	}

	got, err := buildReconcilerPerfReport(input)
	if err != nil {
		t.Fatalf("build report: %v", err)
	}
	if got.SchemaVersion != reconcilerPerfSchemaV1 || !got.OK {
		t.Fatalf("schema/ok = %q/%t, want %q/true", got.SchemaVersion, got.OK, reconcilerPerfSchemaV1)
	}
	if got.Coverage.MeasuredActions != 1 ||
		got.Coverage.RequiredActions != 3 ||
		strings.Join(got.Coverage.MissingActions, ",") != "stop,nudge" {
		t.Fatalf("coverage = %+v, want start measured with stop/nudge missing", got.Coverage)
	}
	if len(got.Actions) != 1 {
		t.Fatalf("actions = %d, want 1", len(got.Actions))
	}
	action := got.Actions[0]
	if action.Action != reconcilerPerfActionStart ||
		action.PairCount != 3 ||
		action.MismatchCount != 1 ||
		strings.Join(action.MismatchPairIDs, ",") != "pair-c" {
		t.Fatalf("start aggregate = %+v", action)
	}
	if action.Legacy.AttemptedCount != 3 ||
		action.Legacy.SampleCount != 2 ||
		action.Legacy.ErrorCount != 1 ||
		action.Legacy.ThroughputPerSecond != 1 {
		t.Fatalf("legacy aggregate = %+v", action.Legacy)
	}
	if action.Legacy.Latency == nil || *action.Legacy.Latency != (reconcilerPerfLatencyStats{
		P50NS: 50,
		P95NS: 95,
		P99NS: 95,
		MaxNS: 95,
	}) {
		t.Fatalf("legacy latency = %+v", action.Legacy.Latency)
	}
	if action.Keyed.AttemptedCount != 3 ||
		action.Keyed.SampleCount != 3 ||
		action.Keyed.ErrorCount != 0 ||
		action.Keyed.ThroughputPerSecond != 3 {
		t.Fatalf("keyed aggregate = %+v", action.Keyed)
	}

	input.Cohorts[0].Pairs[0], input.Cohorts[0].Pairs[2] = input.Cohorts[0].Pairs[2], input.Cohorts[0].Pairs[0]
	reordered, err := buildReconcilerPerfReport(input)
	if err != nil {
		t.Fatalf("build reordered report: %v", err)
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	reorderedJSON, err := json.Marshal(reordered)
	if err != nil {
		t.Fatalf("marshal reordered report: %v", err)
	}
	if !bytes.Equal(gotJSON, reorderedJSON) {
		t.Fatalf("report depends on pair input order:\n%s\n%s", gotJSON, reorderedJSON)
	}
}

func TestBuildReconcilerPerfReportRejectsIncompleteEvidence(t *testing.T) {
	t.Parallel()

	valid := func() reconcilerPerfReportInput {
		return reconcilerPerfReportInput{
			Provenance: validReconcilerPerfProvenance(),
			Warmup: reconcilerPerfWarmupPolicy{
				Excluded:       true,
				ExecutionOrder: "alternating_legacy_first",
			},
			Cohorts: []reconcilerPerfActionCohort{{
				Action:         reconcilerPerfActionStart,
				LegacyWindowNS: 1,
				KeyedWindowNS:  1,
				Pairs: []reconcilerPerfPairSample{{
					PairID: "pair-1",
					Legacy: reconcilerPerfArmSample{LatencyNS: latencyNS(1), Outcome: "started"},
					Keyed:  reconcilerPerfArmSample{LatencyNS: latencyNS(1), Outcome: "started"},
				}},
			}},
		}
	}

	tests := []struct {
		name    string
		mutate  func(*reconcilerPerfReportInput)
		wantErr string
	}{
		{
			name:    "no cohorts",
			mutate:  func(in *reconcilerPerfReportInput) { in.Cohorts = nil },
			wantErr: "at least one action cohort",
		},
		{
			name: "missing provenance",
			mutate: func(in *reconcilerPerfReportInput) {
				in.Provenance.Commit = ""
			},
			wantErr: "provenance commit",
		},
		{
			name: "unknown action",
			mutate: func(in *reconcilerPerfReportInput) {
				in.Cohorts[0].Action = "restart"
			},
			wantErr: "unsupported action",
		},
		{
			name: "zero window",
			mutate: func(in *reconcilerPerfReportInput) {
				in.Cohorts[0].KeyedWindowNS = 0
			},
			wantErr: "keyed measurement window",
		},
		{
			name: "duplicate pair",
			mutate: func(in *reconcilerPerfReportInput) {
				in.Cohorts[0].Pairs = append(in.Cohorts[0].Pairs, in.Cohorts[0].Pairs[0])
			},
			wantErr: "duplicate pair",
		},
		{
			name: "missing outcome",
			mutate: func(in *reconcilerPerfReportInput) {
				in.Cohorts[0].Pairs[0].Legacy.Outcome = ""
			},
			wantErr: "legacy outcome",
		},
		{
			name: "no latency or error",
			mutate: func(in *reconcilerPerfReportInput) {
				in.Cohorts[0].Pairs[0].Legacy.LatencyNS = nil
			},
			wantErr: "legacy latency or error",
		},
		{
			name: "negative latency",
			mutate: func(in *reconcilerPerfReportInput) {
				in.Cohorts[0].Pairs[0].Keyed.LatencyNS = latencyNS(-1)
			},
			wantErr: "keyed latency",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			input := valid()
			tt.mutate(&input)
			_, err := buildReconcilerPerfReport(input)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestWriteReconcilerPerfReportUsesAggregateData(t *testing.T) {
	t.Parallel()

	report, err := buildReconcilerPerfReport(reconcilerPerfReportInput{
		Provenance: validReconcilerPerfProvenance(),
		Warmup: reconcilerPerfWarmupPolicy{
			PairsPerAction: 1,
			Excluded:       true,
			ExecutionOrder: "alternating_legacy_first",
		},
		Cohorts: []reconcilerPerfActionCohort{{
			Action:         reconcilerPerfActionStart,
			LegacyWindowNS: time.Second.Nanoseconds(),
			KeyedWindowNS:  time.Second.Nanoseconds(),
			Pairs: []reconcilerPerfPairSample{{
				PairID: "pair-1",
				Legacy: reconcilerPerfArmSample{LatencyNS: latencyNS(time.Millisecond.Nanoseconds()), Outcome: "started"},
				Keyed:  reconcilerPerfArmSample{LatencyNS: latencyNS((500 * time.Microsecond).Nanoseconds()), Outcome: "started"},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("build report: %v", err)
	}

	var out bytes.Buffer
	if err := writeReconcilerPerfReport(&out, report); err != nil {
		t.Fatalf("write report: %v", err)
	}
	text := out.String()
	for _, want := range []string{
		reconcilerPerfSchemaV1,
		"coverage: 1/3 actions (missing: stop, nudge)",
		"start",
		"legacy",
		"keyed",
		"p99=1.000ms",
		"p99=0.500ms",
		"warmup: 1 pair/action, excluded",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("summary missing %q:\n%s", want, text)
		}
	}
}

func validReconcilerPerfProvenance() reconcilerPerfProvenance {
	return reconcilerPerfProvenance{
		Commit:      "0123456789abcdef",
		GOOS:        "linux",
		GOARCH:      "amd64",
		CPUs:        8,
		Store:       "memory",
		StoreSchema: "synthetic-v1",
		Runtime:     "fake",
		Workload:    "one-ready-session-per-pair",
	}
}

func latencyNS(value int64) *int64 {
	return &value
}
