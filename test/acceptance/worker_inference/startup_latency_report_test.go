//go:build acceptance_c

package workerinference_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestAgentStartLatencyReportUsesNearestRankAndSeparatesInference(t *testing.T) {
	samples := make([]agentStartLatencySample, 20)
	for i := range samples {
		samples[i] = completedAgentStartLatencySample(i+1, time.Duration(i+10)*time.Second)
	}

	report, err := buildAgentStartLatencyReport(
		agentStartLatencyProvenanceForTest(),
		agentStartLatencyRunState{
			Requested: 20,
			Warmup:    completedAgentStartLatencySample(0, 20*time.Second),
			Samples:   samples,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	total := requireAgentStartLatencyMetric(t, report, agentStartMetricTotal)
	if got, want := []time.Duration{total.Latency.P50, total.Latency.P95, total.Latency.P99, total.Latency.Max}, []time.Duration{19 * time.Second, 28 * time.Second, 29 * time.Second, 29 * time.Second}; !reflect.DeepEqual(got, want) {
		t.Fatalf("total latency = %v, want %v", got, want)
	}
	platform := requireAgentStartLatencyMetric(t, report, agentStartMetricNonInference)
	modelTTFT := requireAgentStartLatencyMetric(t, report, agentStartMetricPromptToFirstOutput)
	activeTurn := requireAgentStartLatencyMetric(t, report, agentStartMetricFirstOutputToCompletion)
	if platform.ExcludedFromOptimization || !modelTTFT.ExcludedFromOptimization || !activeTurn.ExcludedFromOptimization {
		t.Fatalf("optimization classification: platform=%t model_ttft=%t active_turn=%t", platform.ExcludedFromOptimization, modelTTFT.ExcludedFromOptimization, activeTurn.ExcludedFromOptimization)
	}
	if !report.OK || report.BaselineEligible {
		t.Fatalf("report OK=%t baseline_eligible=%t, want true/false for a correctness-clean 20-sample diagnostic", report.OK, report.BaselineEligible)
	}
}

func TestAgentStartLatencyReportIncludesObservedUserPromptSubmitHook(t *testing.T) {
	samples := make([]agentStartLatencySample, 20)
	for i := range samples {
		samples[i] = completedAgentStartLatencySample(i+1, 20*time.Second)
		duration := time.Duration(i+1) * 100 * time.Millisecond
		samples[i].UserPromptSubmitHook = &duration
	}
	samples[0].UserPromptSubmitHook = nil

	report, err := buildAgentStartLatencyReport(
		agentStartLatencyProvenanceForTest(),
		agentStartLatencyRunState{
			Requested: 20,
			Warmup:    completedAgentStartLatencySample(0, 20*time.Second),
			Samples:   samples,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	metric := requireAgentStartLatencyMetric(t, report, agentStartMetricUserPromptSubmitHook)
	if metric.ObservedCount != 19 || metric.MissingCount != 1 {
		t.Fatalf("UserPromptSubmit hook counts = %d observed, %d missing; want 19 observed, 1 missing", metric.ObservedCount, metric.MissingCount)
	}
	if got, want := []time.Duration{metric.Latency.P50, metric.Latency.P95, metric.Latency.P99, metric.Latency.Max}, []time.Duration{1100 * time.Millisecond, 2 * time.Second, 2 * time.Second, 2 * time.Second}; !reflect.DeepEqual(got, want) {
		t.Fatalf("UserPromptSubmit hook latency = %v, want %v", got, want)
	}
	if metric.ExcludedFromOptimization {
		t.Fatal("UserPromptSubmit hook is Gas City-controlled latency and must remain an optimization metric")
	}
	if !report.OK || report.BaselineEligible {
		t.Fatalf("missing optional hook evidence changed report eligibility: OK=%t baseline_eligible=%t", report.OK, report.BaselineEligible)
	}
}

func TestAgentStartLatencyReportRequiresThirtyCleanSamplesForBaseline(t *testing.T) {
	samples := make([]agentStartLatencySample, agentStartLatencyDefaultSamples)
	for i := range samples {
		samples[i] = completedAgentStartLatencySample(i+1, 20*time.Second)
	}
	report, err := buildAgentStartLatencyReport(
		agentStartLatencyProvenanceForTest(),
		agentStartLatencyRunState{
			Requested: agentStartLatencyDefaultSamples,
			Warmup:    completedAgentStartLatencySample(0, 20*time.Second),
			Samples:   samples,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK || !report.BaselineEligible {
		t.Fatalf("report OK=%t baseline_eligible=%t, want true/true", report.OK, report.BaselineEligible)
	}
	if report.LatencyOutcomeCounts.Completed != agentStartLatencyDefaultSamples {
		t.Fatalf("completed count = %d, want %d", report.LatencyOutcomeCounts.Completed, agentStartLatencyDefaultSamples)
	}
}

func TestAgentStartLatencyReportRetainsIncompleteErrorCanceledAndNotAttempted(t *testing.T) {
	run := agentStartLatencyRunState{
		Requested: 5,
		Warmup:    completedAgentStartLatencySample(0, 20*time.Second),
		Samples: []agentStartLatencySample{
			completedAgentStartLatencySample(1, 20*time.Second),
			{Index: 2, Outcome: agentStartOutcomeIncomplete, Error: "deadline"},
			{Index: 3, Outcome: agentStartOutcomeError, Error: "provider exited"},
			{Index: 4, Outcome: agentStartOutcomeCanceled, Error: "context canceled"},
			{Index: 5, Outcome: agentStartOutcomeNotAttempted, Error: "unsafe predecessor"},
		},
	}
	report, err := buildAgentStartLatencyReport(agentStartLatencyProvenanceForTest(), run)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Samples) != 5 {
		t.Fatalf("retained samples = %d, want 5", len(report.Samples))
	}
	want := agentStartLatencyOutcomeCounts{Completed: 1, Incomplete: 1, Error: 1, Canceled: 1, NotAttempted: 1}
	if report.LatencyOutcomeCounts != want {
		t.Fatalf("outcome counts = %+v, want %+v", report.LatencyOutcomeCounts, want)
	}
	if got := requireAgentStartLatencyMetric(t, report, agentStartMetricTotal).Latency.Count; got != 1 {
		t.Fatalf("total percentile count = %d, want completed-only count 1", got)
	}
	if report.OK || report.BaselineEligible {
		t.Fatalf("mixed-outcome report OK=%t baseline_eligible=%t, want false/false", report.OK, report.BaselineEligible)
	}
}

func TestAgentStartLatencyReportRejectsInvalidCompletedSamples(t *testing.T) {
	valid := completedAgentStartLatencySample(1, 20*time.Second)
	for name, mutate := range map[string]func(*agentStartLatencySample){
		"missing opaque run identity": func(sample *agentStartLatencySample) { sample.RunIdentity = "" },
		"missing opaque session id":   func(sample *agentStartLatencySample) { sample.SessionID = "" },
		"missing runtime timestamp":   func(sample *agentStartLatencySample) { sample.Timestamps.RuntimeAvailableAt = nil },
		"missing prompt timestamp":    func(sample *agentStartLatencySample) { sample.Timestamps.PromptDeliveredAt = nil },
		"missing first output":        func(sample *agentStartLatencySample) { sample.Timestamps.FirstAssistantOutputAt = nil },
		"missing completion":          func(sample *agentStartLatencySample) { sample.Timestamps.FirstTurnCompletedAt = nil },
		"missing cleanup":             func(sample *agentStartLatencySample) { sample.Timestamps.CleanupCompletedAt = nil },
		"trace identity mismatch":     func(sample *agentStartLatencySample) { sample.Controller.SessionID = "other" },
		"non-monotonic phases": func(sample *agentStartLatencySample) {
			t := sample.Timestamps.StartInitiatedAt.Add(-time.Second)
			sample.Timestamps.FirstAssistantOutputAt = &t
		},
	} {
		t.Run(name, func(t *testing.T) {
			sample := valid
			mutate(&sample)
			_, err := buildAgentStartLatencyReport(agentStartLatencyProvenanceForTest(), agentStartLatencyRunState{
				Requested: 1,
				Warmup:    completedAgentStartLatencySample(0, 20*time.Second),
				Samples:   []agentStartLatencySample{sample},
			})
			if err == nil {
				t.Fatal("invalid completed sample was accepted")
			}
		})
	}
}

func TestAgentStartLatencyReportRequiresEveryTerminalProof(t *testing.T) {
	valid := completedAgentStartLatencySample(1, 20*time.Second)
	proof := reflect.ValueOf(&valid.Terminal).Elem()
	for i := 0; i < proof.NumField(); i++ {
		fieldName := proof.Type().Field(i).Name
		t.Run(fieldName, func(t *testing.T) {
			sample := valid
			reflect.ValueOf(&sample.Terminal).Elem().Field(i).SetBool(false)
			_, err := buildAgentStartLatencyReport(agentStartLatencyProvenanceForTest(), agentStartLatencyRunState{
				Requested: 1,
				Warmup:    completedAgentStartLatencySample(0, 20*time.Second),
				Samples:   []agentStartLatencySample{sample},
			})
			if err == nil {
				t.Fatalf("completed sample without %s proof was accepted", fieldName)
			}
		})
	}
}

func TestAgentStartLatencyReportAllowsPartialEvidenceOnlyForNonCompletedOutcomes(t *testing.T) {
	start := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	runtimeAt := start.Add(time.Second)
	report, err := buildAgentStartLatencyReport(agentStartLatencyProvenanceForTest(), agentStartLatencyRunState{
		Requested: 2,
		Warmup:    agentStartLatencySample{Outcome: agentStartOutcomeNotAttempted, Error: "setup failed"},
		Samples: []agentStartLatencySample{
			{Index: 1, Outcome: agentStartOutcomeIncomplete, SessionID: "gc-session-opaque", Timestamps: agentStartLatencyTimestamps{StartInitiatedAt: start, RuntimeAvailableAt: &runtimeAt}, Error: "deadline"},
			{Index: 2, Outcome: agentStartOutcomeCanceled, Error: "canceled before invocation"},
		},
		FatalError: "run canceled",
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.OK || len(report.Samples) != 2 || report.Samples[0].Timestamps.RuntimeAvailableAt == nil {
		t.Fatalf("partial report = %+v", report)
	}
}

func TestAgentStartLatencyReportPublicationPreservesPriorFileOnValidationFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.json")
	const prior = "prior complete report\n"
	if err := os.WriteFile(path, []byte(prior), 0o644); err != nil {
		t.Fatal(err)
	}
	invalid := completedAgentStartLatencySample(1, 20*time.Second)
	invalid.SessionID = ""
	err := writeAgentStartLatencyReport(path, agentStartLatencyProvenanceForTest(), agentStartLatencyRunState{
		Requested: 1,
		Warmup:    completedAgentStartLatencySample(0, 20*time.Second),
		Samples:   []agentStartLatencySample{invalid},
	})
	if err == nil {
		t.Fatal("invalid report publication succeeded")
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != prior {
		t.Fatalf("prior report changed on failed publication: %q", got)
	}
}

func completedAgentStartLatencySample(index int, total time.Duration) agentStartLatencySample {
	start := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC).Add(time.Duration(index) * time.Minute)
	intent := start.Add(100 * time.Millisecond)
	runtimeAt := start.Add(time.Second)
	execAt := start.Add(2 * time.Second)
	readyAt := start.Add(3 * time.Second)
	promptAt := start.Add(5 * time.Second)
	firstOutputAt := start.Add(8 * time.Second)
	completedAt := start.Add(total)
	cleanupAt := completedAt.Add(time.Second)
	return agentStartLatencySample{
		Index:       index,
		Outcome:     agentStartOutcomeCompleted,
		RunIdentity: "latency-run-opaque",
		SessionID:   "gc-session-opaque",
		SessionName: "gc-test-probe",
		Timestamps: agentStartLatencyTimestamps{
			StartInitiatedAt:       start,
			IntentReturnedAt:       &intent,
			RuntimeAvailableAt:     &runtimeAt,
			CLIProcessExecAt:       &execAt,
			CLIReadyAt:             &readyAt,
			PromptDeliveredAt:      &promptAt,
			FirstAssistantOutputAt: &firstOutputAt,
			FirstTurnCompletedAt:   &completedAt,
			CleanupCompletedAt:     &cleanupAt,
		},
		Terminal: agentStartLatencyTerminalProof{
			ExpectedOutputMatched: true,
			AssistantAfterPrompt:  true,
			TranscriptIdle:        true,
			NoOpenToolUse:         true,
			NoPendingInteraction:  true,
			DurableSessionRetired: true,
			TmuxSessionAbsent:     true,
		},
		Controller: agentStartLatencyControllerTiming{
			SessionID:        "gc-session-opaque",
			Total:            4 * time.Second,
			StartCall:        3 * time.Second,
			PostStartObserve: time.Second,
		},
	}
}

func agentStartLatencyProvenanceForTest() agentStartLatencyProvenance {
	binaries := make([]agentStartLatencyBinary, len(agentStartLatencyRequiredBinaries))
	for i, name := range agentStartLatencyRequiredBinaries {
		binaries[i] = agentStartLatencyBinary{Name: name, Path: "/bin/" + name, SHA256: "hash", Version: "version"}
	}
	return agentStartLatencyProvenance{
		Profile:           "claude/tmux-cli",
		Provider:          "claude",
		RuntimeProvider:   "tmux",
		ReconcilerMode:    "require",
		GCCommit:          "commit",
		HostOS:            "linux",
		HostArch:          "amd64",
		CPUCount:          8,
		CityConfigSHA256:  "city-config-hash",
		AgentConfigSHA256: "agent-config-hash",
		Readiness: agentStartLatencyReadiness{
			Strategy:     agentStartReadinessPromptPrefix,
			PromptPrefix: "ready> ",
			Delay:        5 * time.Second,
		},
		Binaries: binaries,
	}
}

func requireAgentStartLatencyMetric(t *testing.T, report agentStartLatencyReport, name string) agentStartLatencyMetricStats {
	t.Helper()
	for _, metric := range report.Metrics {
		if metric.Name == name {
			if metric.Latency == nil {
				t.Fatalf("metric %q has no latency", name)
			}
			return metric
		}
	}
	t.Fatalf("metric %q not found", name)
	return agentStartLatencyMetricStats{}
}
