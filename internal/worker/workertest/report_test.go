package workertest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewRunReportSummarizesResults(t *testing.T) {
	start := time.Date(2026, 4, 5, 10, 0, 0, 0, time.UTC)
	end := start.Add(3 * time.Second)

	report := NewRunReport(ReportInput{
		RunID:       "phase1-local",
		Suite:       "phase1",
		StartedAt:   start,
		CompletedAt: end,
		Metadata: map[string]string{
			"transport": "tmux",
			"tier":      "worker-core",
		},
		Results: []Result{
			Pass(ProfileGeminiTmuxCLI, RequirementTranscriptNormalization, "normalized transcript"),
			Fail(ProfileClaudeTmuxCLI, RequirementContinuationContinuity, "missing recall"),
			Unsupported(ProfileCodexTmuxCLI, RequirementToolEventNormalization, "phase2 only"),
		},
	})

	if report.SchemaVersion != ReportSchemaVersion {
		t.Fatalf("SchemaVersion = %q, want %q", report.SchemaVersion, ReportSchemaVersion)
	}
	if report.Elapsed != "3s" {
		t.Fatalf("Elapsed = %q, want 3s", report.Elapsed)
	}
	if report.Summary.Status != ResultFail {
		t.Fatalf("Summary.Status = %q, want %q", report.Summary.Status, ResultFail)
	}
	if report.Summary.Total != 3 || report.Summary.Passed != 1 || report.Summary.Failed != 1 || report.Summary.Unsupported != 1 {
		t.Fatalf("unexpected summary counts: %+v", report.Summary)
	}
	if report.Summary.Profiles != 3 {
		t.Fatalf("Profiles = %d, want 3", report.Summary.Profiles)
	}
	if report.Summary.Requirements != 3 {
		t.Fatalf("Requirements = %d, want 3", report.Summary.Requirements)
	}
	if len(report.Summary.FailingProfiles) != 1 || report.Summary.FailingProfiles[0] != ProfileClaudeTmuxCLI {
		t.Fatalf("FailingProfiles = %+v, want [%s]", report.Summary.FailingProfiles, ProfileClaudeTmuxCLI)
	}
	if len(report.Results) != 3 {
		t.Fatalf("len(Results) = %d, want 3", len(report.Results))
	}
	if report.Results[0].Profile != ProfileClaudeTmuxCLI {
		t.Fatalf("Results sorted incorrectly: first profile = %q", report.Results[0].Profile)
	}
	if report.Metadata["transport"] != "tmux" {
		t.Fatalf("Metadata transport = %q, want tmux", report.Metadata["transport"])
	}
}

func TestMarshalReportProducesMachineReadableJSON(t *testing.T) {
	report := NewRunReport(ReportInput{
		RunID: "phase1-ci",
		Suite: "phase1",
		Results: []Result{
			Pass(ProfileClaudeTmuxCLI, RequirementTranscriptDiscovery, "discovered transcript"),
		},
	})

	data, err := MarshalReport(report)
	if err != nil {
		t.Fatalf("MarshalReport: %v", err)
	}
	if !strings.Contains(string(data), "\"schema_version\": \"gc.worker.conformance.v1\"") {
		t.Fatalf("report JSON missing schema version: %s", data)
	}

	var decoded RunReport
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Summary.Status != ResultPass {
		t.Fatalf("decoded summary status = %q, want %q", decoded.Summary.Status, ResultPass)
	}
	if decoded.Results[0].Requirement != RequirementTranscriptDiscovery {
		t.Fatalf("decoded requirement = %q, want %q", decoded.Results[0].Requirement, RequirementTranscriptDiscovery)
	}
}

func TestMarshalReportIncludesEvidence(t *testing.T) {
	report := NewRunReport(ReportInput{
		RunID: "phase2-ci",
		Suite: "phase2",
		Results: []Result{
			Pass(ProfileClaudeTmuxCLI, RequirementInteractionSignal, "interaction surfaced").WithEvidence(map[string]string{
				"event_log_path": "/tmp/events.jsonl",
				"state_path":     "/tmp/state.txt",
			}),
		},
	})

	data, err := MarshalReport(report)
	if err != nil {
		t.Fatalf("MarshalReport: %v", err)
	}

	var decoded RunReport
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got := decoded.Results[0].Evidence["event_log_path"]; got != "/tmp/events.jsonl" {
		t.Fatalf("evidence event_log_path = %q, want /tmp/events.jsonl", got)
	}
}

func TestNewRunReportWithoutResultsDefaultsToUnsupported(t *testing.T) {
	report := NewRunReport(ReportInput{Suite: "phase1"})
	if report.Summary.Status != ResultUnsupported {
		t.Fatalf("Summary.Status = %q, want %q", report.Summary.Status, ResultUnsupported)
	}
	if report.Summary.Total != 0 {
		t.Fatalf("Summary.Total = %d, want 0", report.Summary.Total)
	}
}

func TestSuiteReporterWritesJSONArtifact(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(reportDirEnv, dir)
	t.Setenv("PROFILE", string(ProfileCodexTmuxCLI))
	t.Setenv("GITHUB_JOB", "worker-core-codex")

	reporter := NewSuiteReporter(t, "phase2", map[string]string{
		"tier": "worker-core",
	})
	reporter.Require(t, Pass(ProfileCodexTmuxCLI, RequirementToolEventNormalization, "normalized").WithEvidence(map[string]string{
		"transcript_path": "/tmp/transcript.jsonl",
	}))
	reporter.flush(t)

	path := filepath.Join(dir, "phase2-codex-tmux-cli.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}

	var decoded RunReport
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.RunID != "phase2-codex-tmux-cli" {
		t.Fatalf("RunID = %q, want phase2-codex-tmux-cli", decoded.RunID)
	}
	if decoded.Metadata["github_job"] != "worker-core-codex" {
		t.Fatalf("github_job = %q, want worker-core-codex", decoded.Metadata["github_job"])
	}
	if decoded.Metadata["profile_filter"] != string(ProfileCodexTmuxCLI) {
		t.Fatalf("profile_filter = %q, want %q", decoded.Metadata["profile_filter"], ProfileCodexTmuxCLI)
	}
	if decoded.Results[0].Evidence["transcript_path"] != "/tmp/transcript.jsonl" {
		t.Fatalf("transcript_path evidence = %q, want /tmp/transcript.jsonl", decoded.Results[0].Evidence["transcript_path"])
	}
}

func TestSuiteReporterWritesEmptyArtifactWithoutResults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(reportDirEnv, dir)
	t.Setenv("PROFILE", string(ProfileGeminiTmuxCLI))

	reporter := NewSuiteReporter(t, "phase1", map[string]string{
		"tier": "worker-core",
	})
	reporter.flush(t)

	path := filepath.Join(dir, "phase1-gemini-tmux-cli.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}

	var decoded RunReport
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Summary.Total != 0 {
		t.Fatalf("Summary.Total = %d, want 0", decoded.Summary.Total)
	}
	if decoded.Summary.Status != ResultUnsupported {
		t.Fatalf("Summary.Status = %q, want %q", decoded.Summary.Status, ResultUnsupported)
	}
}

func TestNewRunReportMarksSuiteFailureAsFailed(t *testing.T) {
	report := NewRunReport(ReportInput{
		Suite:         "phase1",
		SuiteFailed:   true,
		FailureDetail: "suite failed outside recorded conformance results",
	})

	if report.Summary.Status != ResultFail {
		t.Fatalf("Summary.Status = %q, want %q", report.Summary.Status, ResultFail)
	}
	if !report.Summary.SuiteFailed {
		t.Fatal("Summary.SuiteFailed = false, want true")
	}
	if report.Summary.FailureDetail == "" {
		t.Fatal("Summary.FailureDetail is empty")
	}
}

func TestSuiteFailureDetailIgnoresRecordedRequirementFailures(t *testing.T) {
	detail := suiteFailureDetail(true, []Result{
		Fail(ProfileClaudeTmuxCLI, RequirementTranscriptDiscovery, "boom"),
	})
	if detail != "" {
		t.Fatalf("suiteFailureDetail = %q, want empty string", detail)
	}
}
