package workertest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	worker "github.com/gastownhall/gascity/internal/worker"
	workerfake "github.com/gastownhall/gascity/internal/worker/fake"
)

func TestPhase2Catalog(t *testing.T) {
	expected := []RequirementCode{
		RequirementStartupOutcomeBound,
		RequirementStartupCommandMaterialization,
		RequirementStartupRuntimeConfigMaterialization,
		RequirementInputInitialMessageFirstStart,
		RequirementInputInitialMessageResume,
		RequirementInputOverrideDefaults,
		RequirementTranscriptDiagnostics,
		RequirementInteractionSignal,
		RequirementInteractionPending,
		RequirementInteractionRespond,
		RequirementInteractionReject,
		RequirementInteractionInstanceLocalDedup,
		RequirementInteractionDurableHistory,
		RequirementInteractionLifecycleHistory,
		RequirementToolEventNormalization,
		RequirementToolEventOpenTail,
		RequirementRealTransportProof,
	}

	catalog := Phase2Catalog()
	if len(catalog) != len(expected) {
		t.Fatalf("catalog entries = %d, want %d", len(catalog), len(expected))
	}

	seen := make(map[RequirementCode]Requirement, len(catalog))
	for _, requirement := range catalog {
		if requirement.Group == "" {
			t.Fatalf("requirement %s has empty group", requirement.Code)
		}
		if requirement.Description == "" {
			t.Fatalf("requirement %s has empty description", requirement.Code)
		}
		seen[requirement.Code] = requirement
	}
	for _, code := range expected {
		if _, ok := seen[code]; !ok {
			t.Fatalf("catalog missing requirement %s", code)
		}
	}
}

func TestPhase2HistoryDiagnostics(t *testing.T) {
	reporter := NewSuiteReporter(t, "phase2-history", map[string]string{
		"tier":  "worker-core",
		"phase": "phase2",
	})

	profiles, err := selectedProfiles()
	if err != nil {
		t.Fatal(err)
	}

	for _, profile := range profiles {
		profile := profile
		t.Run(string(profile.ID), func(t *testing.T) {
			path := writeMalformedHistoryTranscript(t, profile)
			history, err := worker.SessionLogAdapter{}.LoadHistory(worker.LoadRequest{
				Provider:       profile.Provider,
				TranscriptPath: path,
			})
			reporter.Require(t, historyDiagnosticsResult(profile.ID, path, history, err))
		})
	}
}

func TestPhase2StartupOutcomeBounds(t *testing.T) {
	reporter := NewSuiteReporter(t, "phase2-startup", map[string]string{
		"tier":  "worker-core",
		"phase": "phase2",
	})

	profiles, err := selectedProfiles()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		outcome string
		delay   time.Duration
	}{
		{name: "Ready", outcome: "ready", delay: 10 * time.Millisecond},
		{name: "Blocked", outcome: "blocked", delay: 15 * time.Millisecond},
		{name: "Failed", outcome: "failed", delay: 20 * time.Millisecond},
	}

	for _, profile := range profiles {
		profile := profile
		t.Run(string(profile.ID), func(t *testing.T) {
			for _, tt := range tests {
				tt := tt
				t.Run(tt.name, func(t *testing.T) {
					run := runFakeStartup(t, profile.ID, tt.outcome, tt.delay)
					reporter.Require(t, startupOutcomeResult(profile.ID, tt.outcome, tt.delay, run))
				})
			}
		})
	}
}

func TestPhase2RequiredInteractions(t *testing.T) {
	reporter := NewSuiteReporter(t, "phase2-interaction", map[string]string{
		"tier":  "worker-core",
		"phase": "phase2",
	})

	profiles, err := selectedProfiles()
	if err != nil {
		t.Fatal(err)
	}

	for _, profile := range profiles {
		profile := profile
		t.Run(string(profile.ID), func(t *testing.T) {
			t.Run(string(RequirementInteractionSignal), func(t *testing.T) {
				run := runFakeInteraction(t, profile.ID)
				reporter.Require(t, interactionSignalResult(profile.ID, run))
			})

			sp := runtime.NewFake()
			sessionName := "worker-int-" + strings.ReplaceAll(string(profile.ID), "/", "-")
			if err := sp.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
				t.Fatalf("Start: %v", err)
			}

			pending := &runtime.PendingInteraction{
				RequestID: "req-1",
				Kind:      "approval",
				Prompt:    "Allow Read?",
				Options:   []string{"approve", "deny"},
				Metadata: map[string]string{
					"profile":   string(profile.ID),
					"tool_name": "Read",
				},
			}
			sp.SetPendingInteraction(sessionName, pending)

			t.Run(string(RequirementInteractionPending), func(t *testing.T) {
				got, err := sp.Pending(sessionName)
				reporter.Require(t, pendingInteractionResult(profile.ID, got, pending, err))
			})

			t.Run(string(RequirementInteractionReject), func(t *testing.T) {
				err := sp.Respond(sessionName, runtime.InteractionResponse{
					RequestID: "wrong-req",
					Action:    "approve",
				})
				stillPending, pErr := sp.Pending(sessionName)
				reporter.Require(t, rejectInteractionResult(profile.ID, err, stillPending, pErr, len(sp.Responses[sessionName])))
			})

			t.Run(string(RequirementInteractionRespond), func(t *testing.T) {
				err := sp.Respond(sessionName, runtime.InteractionResponse{
					RequestID: pending.RequestID,
					Action:    "approve",
					Text:      "continue",
				})
				got, pErr := sp.Pending(sessionName)
				reporter.Require(t, respondInteractionResult(profile.ID, err, got, pErr, sp.Responses[sessionName]))
			})
		})
	}
}

func TestPhase2DurableInteractionHistory(t *testing.T) {
	reporter := NewSuiteReporter(t, "phase2-interaction-history", map[string]string{
		"tier":  "worker-core",
		"phase": "phase2",
	})

	profiles, err := selectedProfiles()
	if err != nil {
		t.Fatal(err)
	}

	for _, profile := range profiles {
		profile := profile
		t.Run(string(profile.ID), func(t *testing.T) {
			path := writeInteractionHistoryTranscript(t, profile)
			history := loadHistory(t, profile.Provider, path)
			reporter.Require(t, interactionDurableHistoryResult(profile.ID, path, history))
		})
	}
}

func TestPhase2InteractionLifecycleHistory(t *testing.T) {
	reporter := NewSuiteReporter(t, "phase2-interaction-lifecycle", map[string]string{
		"tier":  "worker-core",
		"phase": "phase2",
	})

	profiles, err := selectedProfiles()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		finalState  worker.InteractionState
		wantPending bool
	}{
		{name: "dismissed", finalState: worker.InteractionStateDismissed, wantPending: false},
		{name: "resumed_after_restart", finalState: worker.InteractionStateResumedAfterRestart, wantPending: true},
	}

	for _, profile := range profiles {
		profile := profile
		t.Run(string(profile.ID), func(t *testing.T) {
			for _, tt := range tests {
				tt := tt
				t.Run(tt.name, func(t *testing.T) {
					path := writeInteractionLifecycleTranscript(t, profile, tt.finalState)
					history := loadHistory(t, profile.Provider, path)
					reporter.Require(t, interactionLifecycleHistoryResult(profile.ID, path, history, tt.finalState, tt.wantPending))
				})
			}
		})
	}
}

func TestPhase2ToolEventSubstrate(t *testing.T) {
	reporter := NewSuiteReporter(t, "phase2-tool", map[string]string{
		"tier":  "worker-core",
		"phase": "phase2",
	})

	profiles, err := selectedProfiles()
	if err != nil {
		t.Fatal(err)
	}

	for _, profile := range profiles {
		profile := profile
		t.Run(string(profile.ID), func(t *testing.T) {
			t.Run(string(RequirementToolEventNormalization), func(t *testing.T) {
				path := writeToolTranscript(t, profile, false)
				history := loadHistory(t, profile.Provider, path)
				reporter.Require(t, toolNormalizationResult(profile.ID, path, history))
			})

			t.Run(string(RequirementToolEventOpenTail), func(t *testing.T) {
				path := writeToolTranscript(t, profile, true)
				history := loadHistory(t, profile.Provider, path)
				reporter.Require(t, toolOpenTailResult(profile.ID, path, history))
			})
		})
	}
}

type fakeStartupRun struct {
	StatePath    string
	EventPath    string
	Events       []workerfake.Event
	Elapsed      time.Duration
	LaunchToWait time.Duration
}

var (
	fakeWorkerBinaryOnce sync.Once
	fakeWorkerBinaryPath string
	fakeWorkerBinaryErr  error
)

const (
	fakeStartupGateTimeout         = 2 * time.Second
	fakeStartupLaunchBound         = 500 * time.Millisecond
	fakeStartupPostControlOverhead = 250 * time.Millisecond
	fakeInteractionSignalBound     = 2 * time.Second
)

func runFakeStartup(t *testing.T, profile ProfileID, outcome string, delay time.Duration) fakeStartupRun {
	t.Helper()

	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.txt")
	eventPath := filepath.Join(dir, "events.jsonl")
	startFile := filepath.Join(dir, "start.txt")
	configPath := filepath.Join(dir, "config.json")
	cfg := workerfake.HelperConfig{
		Profile: &workerfake.Profile{
			Name:     string(profile),
			Provider: string(profile),
			Launch: workerfake.LaunchSpec{
				Startup: workerfake.StartupSpec{
					Outcome:            outcome,
					ReadyAfter:         delay.String(),
					RequireControlFile: true,
				},
			},
		},
		Scenario: workerfake.Scenario{
			Name: "startup-bound",
			Steps: []workerfake.Step{
				{
					ID:      "startup",
					Action:  "startup",
					Delay:   delay.String(),
					State:   outcome,
					Message: "bounded startup outcome",
				},
			},
		},
		Output: workerfake.OutputSpec{
			EventLogPath: eventPath,
			StatePath:    statePath,
		},
		Control: workerfake.ControlSpec{
			StartFile: startFile,
		},
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal fake config: %v", err)
	}
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		t.Fatalf("write fake config: %v", err)
	}

	cmd := exec.CommandContext(context.Background(), fakeWorkerBinary(t))
	cmd.Env = append(os.Environ(),
		"GC_FAKE_WORKER_CONFIG="+configPath,
		"GC_FAKE_WORKER_START_FILE="+startFile,
	)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	launchStart := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake worker CLI: %v", err)
	}
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	waitEvent := waitForWorkerFakeEvent(t, eventPath, "control_waiting", fakeStartupGateTimeout)
	launchToWait := time.Since(launchStart)
	select {
	case err := <-waitCh:
		t.Fatalf("fake worker CLI exited before start gate opened: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	default:
	}
	if _, err := os.Stat(statePath); err == nil {
		t.Fatalf("state file %q should not exist before start gate opens", statePath)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat state file before gate: %v", err)
	}
	if waitEvent.Provider != string(profile) {
		t.Fatalf("pre-release event provider = %q, want %q", waitEvent.Provider, profile)
	}
	if waitEvent.Path != startFile {
		t.Fatalf("pre-release event path = %q, want %q", waitEvent.Path, startFile)
	}

	if err := os.WriteFile(startFile, []byte("go\n"), 0o644); err != nil {
		t.Fatalf("write start file: %v", err)
	}
	start := time.Now()
	if err := <-waitCh; err != nil {
		t.Fatalf("fake worker CLI: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	return fakeStartupRun{
		StatePath:    statePath,
		EventPath:    eventPath,
		Events:       readWorkerFakeEvents(t, eventPath),
		Elapsed:      time.Since(start),
		LaunchToWait: launchToWait,
	}
}

func runFakeInteraction(t *testing.T, profile ProfileID) fakeStartupRun {
	t.Helper()

	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.txt")
	eventPath := filepath.Join(dir, "events.jsonl")
	configPath := filepath.Join(dir, "config.json")
	cfg := workerfake.HelperConfig{
		Profile: &workerfake.Profile{
			Name:     string(profile),
			Provider: string(profile),
		},
		Scenario: workerfake.Scenario{
			Name: "interaction-bound",
			Steps: []workerfake.Step{
				{
					ID:      "startup",
					Action:  "startup",
					State:   "ready",
					Message: "worker ready",
				},
				{
					ID:     "approval",
					Action: "emit_interaction",
					Interaction: workerfake.InteractionEvent{
						Kind:      "approval",
						RequestID: "req-1",
						Prompt:    "Allow Read?",
						Options:   []string{"approve", "deny"},
						State:     "blocked",
						Metadata: map[string]string{
							"profile":   string(profile),
							"tool_name": "Read",
						},
					},
					Message: "interaction pending",
				},
			},
		},
		Output: workerfake.OutputSpec{
			EventLogPath: eventPath,
			StatePath:    statePath,
		},
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal fake config: %v", err)
	}
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		t.Fatalf("write fake config: %v", err)
	}

	cmd := exec.CommandContext(context.Background(), fakeWorkerBinary(t))
	cmd.Env = append(os.Environ(), "GC_FAKE_WORKER_CONFIG="+configPath)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatalf("fake worker CLI: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	return fakeStartupRun{
		StatePath: statePath,
		EventPath: eventPath,
		Events:    readWorkerFakeEvents(t, eventPath),
		Elapsed:   time.Since(start),
	}
}

func startupOutcomeResult(profile ProfileID, outcome string, delay time.Duration, run fakeStartupRun) Result {
	evidence := map[string]string{
		"state_path":     run.StatePath,
		"event_log_path": run.EventPath,
		"expected_state": outcome,
		"launch_to_wait": run.LaunchToWait.String(),
		"expected_delay": delay.String(),
		"event_count":    fmt.Sprintf("%d", len(run.Events)),
		"elapsed":        run.Elapsed.String(),
	}
	stateData, err := os.ReadFile(run.StatePath)
	if err != nil {
		evidence["read_error"] = err.Error()
		return Fail(profile, RequirementStartupOutcomeBound,
			fmt.Sprintf("read %s: %v", run.StatePath, err)).WithEvidence(evidence)
	}
	got := strings.TrimSpace(string(stateData))
	evidence["observed_state"] = got
	if got != outcome {
		return Fail(profile, RequirementStartupOutcomeBound,
			fmt.Sprintf("startup state = %q, want %q", got, outcome)).WithEvidence(evidence)
	}
	if run.LaunchToWait > fakeStartupLaunchBound {
		return Fail(profile, RequirementStartupOutcomeBound,
			fmt.Sprintf("%s exceeded launch-to-wait bound: %s > %s", RequirementStartupOutcomeBound, run.LaunchToWait, fakeStartupLaunchBound)).WithEvidence(evidence)
	}
	if len(run.Events) < 3 {
		return Fail(profile, RequirementStartupOutcomeBound,
			fmt.Sprintf("expected control wait, control observed, and startup events for %s", RequirementStartupOutcomeBound)).WithEvidence(evidence)
	}
	if run.Events[0].Kind != "control_waiting" {
		return Fail(profile, RequirementStartupOutcomeBound,
			fmt.Sprintf("first event kind = %q, want control_waiting", run.Events[0].Kind)).WithEvidence(evidence)
	}
	if run.Events[1].Kind != "control_observed" {
		return Fail(profile, RequirementStartupOutcomeBound,
			fmt.Sprintf("second event kind = %q, want control_observed", run.Events[1].Kind)).WithEvidence(evidence)
	}
	event := run.Events[2]
	evidence["observed_transition_kind"] = event.Kind
	evidence["observed_transition_state"] = event.State
	if event.Kind != "state_transition" {
		return Fail(profile, RequirementStartupOutcomeBound,
			fmt.Sprintf("event kind = %q, want state_transition", event.Kind)).WithEvidence(evidence)
	}
	if event.State != outcome {
		return Fail(profile, RequirementStartupOutcomeBound,
			fmt.Sprintf("event state = %q, want %q", event.State, outcome)).WithEvidence(evidence)
	}
	if event.Provider != string(profile) {
		return Fail(profile, RequirementStartupOutcomeBound,
			fmt.Sprintf("event provider = %q, want %q", event.Provider, profile)).WithEvidence(evidence)
	}
	postControl := event.Time.Sub(run.Events[1].Time)
	evidence["post_control_delay"] = postControl.String()
	maxPostControl := delay + fakeStartupPostControlOverhead
	if postControl > maxPostControl {
		return Fail(profile, RequirementStartupOutcomeBound,
			fmt.Sprintf("%s exceeded post-control bound: %s > %s", RequirementStartupOutcomeBound, postControl, maxPostControl)).WithEvidence(evidence)
	}
	return Pass(profile, RequirementStartupOutcomeBound, "standalone fake worker surfaced the bounded startup outcome").WithEvidence(evidence)
}

func interactionSignalResult(profile ProfileID, run fakeStartupRun) Result {
	evidence := map[string]string{
		"state_path":     run.StatePath,
		"event_log_path": run.EventPath,
		"event_count":    fmt.Sprintf("%d", len(run.Events)),
		"elapsed":        run.Elapsed.String(),
	}
	stateData, err := os.ReadFile(run.StatePath)
	if err != nil {
		evidence["read_error"] = err.Error()
		return Fail(profile, RequirementInteractionSignal,
			fmt.Sprintf("read %s: %v", run.StatePath, err)).WithEvidence(evidence)
	}
	got := strings.TrimSpace(string(stateData))
	evidence["observed_state"] = got
	if got != "blocked" {
		return Fail(profile, RequirementInteractionSignal,
			fmt.Sprintf("interaction state = %q, want blocked", got)).WithEvidence(evidence)
	}
	if run.Elapsed > fakeInteractionSignalBound {
		return Fail(profile, RequirementInteractionSignal,
			fmt.Sprintf("blocked interaction signal exceeded bound: %s > %s", run.Elapsed, fakeInteractionSignalBound)).WithEvidence(evidence)
	}
	if len(run.Events) != 2 {
		return Fail(profile, RequirementInteractionSignal,
			fmt.Sprintf("event count = %d, want 2", len(run.Events))).WithEvidence(evidence)
	}
	event := run.Events[1]
	if event.Kind != "interaction" {
		return Fail(profile, RequirementInteractionSignal,
			fmt.Sprintf("event kind = %q, want interaction", event.Kind)).WithEvidence(evidence)
	}
	if event.Provider != string(profile) {
		return Fail(profile, RequirementInteractionSignal,
			fmt.Sprintf("event provider = %q, want %q", event.Provider, profile)).WithEvidence(evidence)
	}
	if event.State != "blocked" {
		return Fail(profile, RequirementInteractionSignal,
			fmt.Sprintf("event state = %q, want blocked", event.State)).WithEvidence(evidence)
	}
	if event.Interaction == nil {
		return Fail(profile, RequirementInteractionSignal, "interaction event missing payload").WithEvidence(evidence)
	}
	evidence["interaction_kind"] = event.Interaction.Kind
	evidence["interaction_request_id"] = event.Interaction.RequestID
	if event.Interaction.Kind != "approval" {
		return Fail(profile, RequirementInteractionSignal,
			fmt.Sprintf("interaction kind = %q, want approval", event.Interaction.Kind)).WithEvidence(evidence)
	}
	if event.Interaction.RequestID != "req-1" {
		return Fail(profile, RequirementInteractionSignal,
			fmt.Sprintf("interaction request ID = %q, want req-1", event.Interaction.RequestID)).WithEvidence(evidence)
	}
	return Pass(profile, RequirementInteractionSignal, "standalone fake worker surfaced the required interaction signal").WithEvidence(evidence)
}

func pendingInteractionResult(profile ProfileID, got, expected *runtime.PendingInteraction, err error) Result {
	evidence := map[string]string{
		"expected_request_id": expected.RequestID,
		"expected_kind":       expected.Kind,
	}
	if err != nil {
		evidence["error"] = err.Error()
		return Fail(profile, RequirementInteractionPending, fmt.Sprintf("Pending: %v", err)).WithEvidence(evidence)
	}
	if got == nil {
		return Fail(profile, RequirementInteractionPending, "expected pending interaction").WithEvidence(evidence)
	}
	evidence["observed_request_id"] = got.RequestID
	evidence["observed_kind"] = got.Kind
	evidence["observed_profile"] = got.Metadata["profile"]
	if got.RequestID != expected.RequestID {
		return Fail(profile, RequirementInteractionPending,
			fmt.Sprintf("RequestID = %q, want %q", got.RequestID, expected.RequestID)).WithEvidence(evidence)
	}
	if got.Kind != expected.Kind {
		return Fail(profile, RequirementInteractionPending,
			fmt.Sprintf("Kind = %q, want %q", got.Kind, expected.Kind)).WithEvidence(evidence)
	}
	if got.Metadata["profile"] != string(profile) {
		return Fail(profile, RequirementInteractionPending,
			fmt.Sprintf("profile metadata = %q, want %q", got.Metadata["profile"], profile)).WithEvidence(evidence)
	}
	return Pass(profile, RequirementInteractionPending, "runtime interaction seam exposed the pending approval request").WithEvidence(evidence)
}

func rejectInteractionResult(profile ProfileID, respondErr error, stillPending *runtime.PendingInteraction, pendingErr error, responseCount int) Result {
	evidence := map[string]string{
		"response_count": fmt.Sprintf("%d", responseCount),
	}
	if respondErr == nil {
		return Fail(profile, RequirementInteractionReject, "Respond should fail for mismatched request id").WithEvidence(evidence)
	}
	evidence["respond_error"] = respondErr.Error()
	if pendingErr != nil {
		evidence["pending_error"] = pendingErr.Error()
		return Fail(profile, RequirementInteractionReject, fmt.Sprintf("Pending after reject: %v", pendingErr)).WithEvidence(evidence)
	}
	if stillPending == nil {
		return Fail(profile, RequirementInteractionReject, "pending interaction cleared after mismatched response").WithEvidence(evidence)
	}
	evidence["remaining_request_id"] = stillPending.RequestID
	if responseCount != 0 {
		return Fail(profile, RequirementInteractionReject,
			fmt.Sprintf("recorded responses = %d, want 0", responseCount)).WithEvidence(evidence)
	}
	return Pass(profile, RequirementInteractionReject, "mismatched responses are rejected without clearing the pending interaction").WithEvidence(evidence)
}

func respondInteractionResult(profile ProfileID, respondErr error, got *runtime.PendingInteraction, pendingErr error, responses []runtime.InteractionResponse) Result {
	evidence := map[string]string{
		"response_count": fmt.Sprintf("%d", len(responses)),
	}
	if respondErr != nil {
		evidence["respond_error"] = respondErr.Error()
		return Fail(profile, RequirementInteractionRespond, fmt.Sprintf("Respond: %v", respondErr)).WithEvidence(evidence)
	}
	if pendingErr != nil {
		evidence["pending_error"] = pendingErr.Error()
		return Fail(profile, RequirementInteractionRespond, fmt.Sprintf("Pending after respond: %v", pendingErr)).WithEvidence(evidence)
	}
	if got != nil {
		evidence["remaining_request_id"] = got.RequestID
		return Fail(profile, RequirementInteractionRespond, "pending interaction not cleared after response").WithEvidence(evidence)
	}
	if len(responses) != 1 {
		return Fail(profile, RequirementInteractionRespond,
			fmt.Sprintf("recorded responses = %d, want 1", len(responses))).WithEvidence(evidence)
	}
	evidence["recorded_action"] = responses[0].Action
	if responses[0].Action != "approve" {
		return Fail(profile, RequirementInteractionRespond,
			fmt.Sprintf("recorded action = %q, want approve", responses[0].Action)).WithEvidence(evidence)
	}
	return Pass(profile, RequirementInteractionRespond, "responding to a pending interaction clears the request and records the response").WithEvidence(evidence)
}

func interactionDurableHistoryResult(profile ProfileID, transcriptPath string, history *worker.HistorySnapshot) Result {
	evidence := map[string]string{
		"transcript_path": transcriptPath,
	}
	if history == nil {
		return Fail(profile, RequirementInteractionDurableHistory, "expected history snapshot").WithEvidence(evidence)
	}
	evidence["entry_count"] = fmt.Sprintf("%d", len(history.Entries))
	evidence["pending_interaction_ids"] = strings.Join(history.TailState.PendingInteractionIDs, ",")

	interaction, ok := findHistoryInteraction(history, "approval-1")
	if !ok {
		return Fail(profile, RequirementInteractionDurableHistory, "normalized history missing durable interaction record").WithEvidence(evidence)
	}
	evidence["interaction_request_id"] = interaction.RequestID
	evidence["interaction_kind"] = interaction.Kind
	evidence["interaction_state"] = string(interaction.State)
	evidence["interaction_prompt"] = interaction.Prompt
	evidence["interaction_options"] = strings.Join(interaction.Options, ",")

	if interaction.Kind != "approval" {
		return Fail(profile, RequirementInteractionDurableHistory,
			fmt.Sprintf("interaction kind = %q, want approval", interaction.Kind)).WithEvidence(evidence)
	}
	if interaction.State != worker.InteractionStatePending {
		return Fail(profile, RequirementInteractionDurableHistory,
			fmt.Sprintf("interaction state = %q, want %q", interaction.State, worker.InteractionStatePending)).WithEvidence(evidence)
	}
	if interaction.Prompt != "Allow Read?" {
		return Fail(profile, RequirementInteractionDurableHistory,
			fmt.Sprintf("interaction prompt = %q, want Allow Read?", interaction.Prompt)).WithEvidence(evidence)
	}
	if !containsString(history.TailState.PendingInteractionIDs, "approval-1") {
		return Fail(profile, RequirementInteractionDurableHistory,
			"pending interaction not visible in transcript tail state").WithEvidence(evidence)
	}
	return Pass(profile, RequirementInteractionDurableHistory, "normalized history preserved durable pending interaction state").WithEvidence(evidence)
}

func interactionLifecycleHistoryResult(profile ProfileID, transcriptPath string, history *worker.HistorySnapshot, finalState worker.InteractionState, wantPending bool) Result {
	evidence := map[string]string{
		"transcript_path": transcriptPath,
		"expected_state":  string(finalState),
		"want_pending":    fmt.Sprintf("%t", wantPending),
	}
	if history == nil {
		return Fail(profile, RequirementInteractionLifecycleHistory, "expected history snapshot").WithEvidence(evidence)
	}
	evidence["entry_count"] = fmt.Sprintf("%d", len(history.Entries))
	evidence["pending_interaction_ids"] = strings.Join(history.TailState.PendingInteractionIDs, ",")

	interaction, ok := findLastHistoryInteraction(history, "approval-1")
	if !ok {
		return Fail(profile, RequirementInteractionLifecycleHistory, "normalized history missing final interaction lifecycle record").WithEvidence(evidence)
	}
	evidence["observed_state"] = string(interaction.State)
	evidence["interaction_action"] = interaction.Action
	if interaction.State != finalState {
		return Fail(profile, RequirementInteractionLifecycleHistory,
			fmt.Sprintf("final interaction state = %q, want %q", interaction.State, finalState)).WithEvidence(evidence)
	}
	isPending := containsString(history.TailState.PendingInteractionIDs, "approval-1")
	evidence["observed_pending"] = fmt.Sprintf("%t", isPending)
	if isPending != wantPending {
		return Fail(profile, RequirementInteractionLifecycleHistory,
			fmt.Sprintf("tail pending = %t, want %t", isPending, wantPending)).WithEvidence(evidence)
	}
	return Pass(profile, RequirementInteractionLifecycleHistory, "normalized history applied durable interaction lifecycle state to the transcript tail").WithEvidence(evidence)
}

func findHistoryInteraction(history *worker.HistorySnapshot, requestID string) (*worker.HistoryInteraction, bool) {
	for _, entry := range history.Entries {
		for _, block := range entry.Blocks {
			if block.Kind == worker.BlockKindInteraction && block.Interaction != nil && block.Interaction.RequestID == requestID {
				return block.Interaction, true
			}
		}
	}
	return nil, false
}

func findLastHistoryInteraction(history *worker.HistorySnapshot, requestID string) (*worker.HistoryInteraction, bool) {
	for i := len(history.Entries) - 1; i >= 0; i-- {
		entry := history.Entries[i]
		for j := len(entry.Blocks) - 1; j >= 0; j-- {
			block := entry.Blocks[j]
			if block.Kind == worker.BlockKindInteraction && block.Interaction != nil && block.Interaction.RequestID == requestID {
				return block.Interaction, true
			}
		}
	}
	return nil, false
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func toolNormalizationResult(profile ProfileID, transcriptPath string, history *worker.HistorySnapshot) Result {
	evidence := toolHistoryEvidence(transcriptPath, history)
	switch {
	case len(history.TailState.OpenToolUseIDs) != 0:
		return Fail(profile, RequirementToolEventNormalization,
			fmt.Sprintf("open tool uses = %v, want none", history.TailState.OpenToolUseIDs)).WithEvidence(evidence)
	case len(history.Entries) < 2:
		return Fail(profile, RequirementToolEventNormalization,
			fmt.Sprintf("entries = %d, want at least 2", len(history.Entries))).WithEvidence(evidence)
	case !historyHasBlockKind(history, worker.BlockKindToolUse):
		return Fail(profile, RequirementToolEventNormalization,
			fmt.Sprintf("normalized history missing %q block", worker.BlockKindToolUse)).WithEvidence(evidence)
	case !historyHasBlockKind(history, worker.BlockKindToolResult):
		return Fail(profile, RequirementToolEventNormalization,
			fmt.Sprintf("normalized history missing %q block", worker.BlockKindToolResult)).WithEvidence(evidence)
	default:
		return Pass(profile, RequirementToolEventNormalization, "normalized history preserved tool_use/tool_result substrate events").WithEvidence(evidence)
	}
}

func toolOpenTailResult(profile ProfileID, transcriptPath string, history *worker.HistorySnapshot) Result {
	evidence := toolHistoryEvidence(transcriptPath, history)
	if !historyHasOpenToolUseEvidence(history) {
		return Fail(profile, RequirementToolEventOpenTail,
			fmt.Sprintf("normalized history does not preserve unresolved tool-use evidence: %+v", history.TailState.OpenToolUseIDs)).WithEvidence(evidence)
	}
	return Pass(profile, RequirementToolEventOpenTail, "normalized history preserved unresolved tool-use evidence at the transcript tail").WithEvidence(evidence)
}

func toolHistoryEvidence(transcriptPath string, history *worker.HistorySnapshot) map[string]string {
	evidence := map[string]string{
		"transcript_path": transcriptPath,
	}
	if history == nil {
		return evidence
	}
	evidence["entry_count"] = fmt.Sprintf("%d", len(history.Entries))
	evidence["open_tool_use_count"] = fmt.Sprintf("%d", len(history.TailState.OpenToolUseIDs))
	if len(history.TailState.OpenToolUseIDs) > 0 {
		evidence["open_tool_use_ids"] = strings.Join(history.TailState.OpenToolUseIDs, ",")
	}
	return evidence
}

func historyDiagnosticsResult(profile ProfileID, transcriptPath string, history *worker.HistorySnapshot, loadErr error) Result {
	evidence := historyDiagnosticsEvidence(transcriptPath, history)
	if loadErr != nil {
		evidence["load_error"] = loadErr.Error()
		if profile == ProfileGeminiTmuxCLI {
			return Pass(profile, RequirementTranscriptDiagnostics, "malformed single-file transcript failed closed").WithEvidence(evidence)
		}
		return Fail(profile, RequirementTranscriptDiagnostics, fmt.Sprintf("LoadHistory: %v", loadErr)).WithEvidence(evidence)
	}
	if history == nil {
		return Fail(profile, RequirementTranscriptDiagnostics, "expected history snapshot").WithEvidence(evidence)
	}
	if len(history.Entries) == 0 {
		return Fail(profile, RequirementTranscriptDiagnostics, "degraded history has no readable prefix").WithEvidence(evidence)
	}
	if history.Continuity.Status != worker.ContinuityStatusDegraded {
		return Fail(profile, RequirementTranscriptDiagnostics,
			fmt.Sprintf("continuity status = %q, want %q", history.Continuity.Status, worker.ContinuityStatusDegraded)).WithEvidence(evidence)
	}
	expectedCode := expectedHistoryDiagnosticCode(profile)
	if expectedCode != "" && !historyHasDiagnosticCode(history, expectedCode) {
		return Fail(profile, RequirementTranscriptDiagnostics,
			fmt.Sprintf("diagnostics missing %q", expectedCode)).WithEvidence(evidence)
	}
	if len(history.Diagnostics) == 0 {
		return Fail(profile, RequirementTranscriptDiagnostics, "expected history diagnostics").WithEvidence(evidence)
	}
	return Pass(profile, RequirementTranscriptDiagnostics, "malformed transcript surfaced degraded history diagnostics").WithEvidence(evidence)
}

func historyDiagnosticsEvidence(transcriptPath string, history *worker.HistorySnapshot) map[string]string {
	evidence := map[string]string{
		"transcript_path": transcriptPath,
	}
	if history == nil {
		return evidence
	}
	evidence["entry_count"] = fmt.Sprintf("%d", len(history.Entries))
	evidence["continuity_status"] = string(history.Continuity.Status)
	if history.Continuity.Note != "" {
		evidence["continuity_note"] = history.Continuity.Note
	}
	if len(history.Diagnostics) > 0 {
		evidence["diagnostic_count"] = fmt.Sprintf("%d", len(history.Diagnostics))
		evidence["diagnostic_codes"] = diagnosticCodes(history.Diagnostics)
		for _, diagnostic := range history.Diagnostics {
			if diagnostic.Count > 0 {
				evidence["diagnostic_"+diagnostic.Code+"_count"] = fmt.Sprintf("%d", diagnostic.Count)
			}
		}
	}
	if history.TailState.Degraded {
		evidence["tail_degraded"] = "true"
		evidence["tail_degraded_reason"] = history.TailState.DegradedReason
	}
	return evidence
}

func historyHasDiagnosticCode(history *worker.HistorySnapshot, code string) bool {
	for _, diagnostic := range history.Diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}

func expectedHistoryDiagnosticCode(profile ProfileID) string {
	switch profile {
	case ProfileClaudeTmuxCLI:
		return "malformed_tail"
	case ProfileCodexTmuxCLI:
		return "malformed_jsonl"
	default:
		// Gemini stores one JSON document, so malformed/truncated transcript
		// input fails closed in encoding/json before a diagnostic code exists.
		return ""
	}
}

func fakeWorkerBinary(t *testing.T) string {
	t.Helper()

	fakeWorkerBinaryOnce.Do(func() {
		root, err := workerRepoRoot()
		if err != nil {
			fakeWorkerBinaryErr = err
			return
		}
		buildDir, err := os.MkdirTemp("", "gc-fake-worker-*")
		if err != nil {
			fakeWorkerBinaryErr = err
			return
		}
		fakeWorkerBinaryPath = filepath.Join(buildDir, "fake-worker")
		cmd := exec.Command("go", "build", "-o", fakeWorkerBinaryPath, "./internal/worker/fakecmd")
		cmd.Dir = root
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			fakeWorkerBinaryErr = fmt.Errorf("build fake worker: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
	})

	if fakeWorkerBinaryErr != nil {
		t.Fatal(fakeWorkerBinaryErr)
	}
	return fakeWorkerBinaryPath
}

func workerRepoRoot() (string, error) {
	_, file, _, ok := goruntime.Caller(0)
	if !ok {
		return "", fmt.Errorf("resolve caller path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..")), nil
}

func readWorkerFakeEvents(t *testing.T, path string) []workerfake.Event {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read event log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	events := make([]workerfake.Event, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event workerfake.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode event %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

func waitForWorkerFakeEvent(t *testing.T, path, kind string, timeout time.Duration) workerfake.Event {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			for _, line := range lines {
				if strings.TrimSpace(line) == "" {
					continue
				}
				var event workerfake.Event
				if err := json.Unmarshal([]byte(line), &event); err != nil {
					continue
				}
				if event.Kind == kind {
					return event
				}
			}
		} else if !os.IsNotExist(err) {
			t.Fatalf("read event log: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q event in %s", kind, path)
	return workerfake.Event{}
}

func writeMalformedHistoryTranscript(t *testing.T, profile Profile) string {
	t.Helper()

	switch profile.ID {
	case ProfileClaudeTmuxCLI:
		path := filepath.Join(t.TempDir(), "session.jsonl")
		body := strings.Join([]string{
			`{"uuid":"u1","type":"user","message":{"role":"user","content":"hello"},"timestamp":"2026-04-04T09:00:00Z","sessionId":"malformed-claude"}`,
			`{"uuid":"a1","parentUuid":"u1","type":"assistant","message":{"role":"assistant","content":"done"},"timestamp":"2026-04-04T09:00:01Z","sessionId":"malformed-claude"}`,
		}, "\n") + "\n" + `{"uuid":"torn","type":"assistant","message":`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write malformed claude transcript: %v", err)
		}
		return path
	case ProfileCodexTmuxCLI:
		return writeLinesFile(t, filepath.Join("2026", "04", "04", "session.jsonl"), []string{
			`{"timestamp":"2026-04-04T09:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"hello"}]}}`,
			`not json`,
			`{"timestamp":"2026-04-04T09:00:01Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"done"}]}}`,
		})
	case ProfileGeminiTmuxCLI:
		path := filepath.Join(t.TempDir(), "session.json")
		if err := os.WriteFile(path, []byte(`{"sessionId":"malformed-gemini","messages":[`), 0o644); err != nil {
			t.Fatalf("write malformed gemini transcript: %v", err)
		}
		return path
	default:
		t.Fatalf("unsupported profile %s", profile.ID)
		return ""
	}
}

func writeInteractionHistoryTranscript(t *testing.T, profile Profile) string {
	t.Helper()

	switch profile.ID {
	case ProfileClaudeTmuxCLI:
		return writeLinesFile(t, "session.jsonl", []string{
			`{"uuid":"u1","type":"user","message":{"role":"user","content":"run a tool"},"timestamp":"2026-04-04T09:00:00Z","sessionId":"interaction-phase2"}`,
			`{"uuid":"a1","parentUuid":"u1","type":"assistant","message":{"role":"assistant","content":[{"type":"interaction","request_id":"approval-1","kind":"approval","state":"pending","prompt":"Allow Read?","options":["approve","deny"],"metadata":{"tool_name":"Read"}}]},"timestamp":"2026-04-04T09:00:01Z","sessionId":"interaction-phase2"}`,
		})
	case ProfileCodexTmuxCLI:
		return writeLinesFile(t, filepath.Join("2026", "04", "04", "session.jsonl"), []string{
			`{"timestamp":"2026-04-04T09:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"run a tool"}]}}`,
			`{"timestamp":"2026-04-04T09:00:01Z","type":"response_item","payload":{"type":"interaction","request_id":"approval-1","kind":"approval","state":"pending","prompt":"Allow Read?","options":["approve","deny"],"metadata":{"tool_name":"Read"}}}`,
		})
	case ProfileGeminiTmuxCLI:
		dir := t.TempDir()
		path := filepath.Join(dir, "session.json")
		body := `{
  "sessionId": "gemini-interaction-phase2",
  "messages": [
    {"id":"m1","timestamp":"2026-04-04T09:00:00Z","type":"user","content":"run a tool"},
    {"id":"m2","timestamp":"2026-04-04T09:00:01Z","type":"gemini","content":"approval needed","interactions":[{"request_id":"approval-1","kind":"approval","state":"pending","prompt":"Allow Read?","options":["approve","deny"],"metadata":{"tool_name":"Read"}}]}
  ]
}`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write gemini interaction transcript: %v", err)
		}
		return path
	default:
		t.Fatalf("unsupported profile %s", profile.ID)
		return ""
	}
}

func writeInteractionLifecycleTranscript(t *testing.T, profile Profile, finalState worker.InteractionState) string {
	t.Helper()

	finalStateText := string(finalState)
	finalAction := "acknowledge"
	if finalState == worker.InteractionStateDismissed {
		finalAction = "dismiss"
	}

	switch profile.ID {
	case ProfileClaudeTmuxCLI:
		return writeLinesFile(t, "session.jsonl", []string{
			`{"uuid":"u1","type":"user","message":{"role":"user","content":"run a tool"},"timestamp":"2026-04-04T09:00:00Z","sessionId":"interaction-lifecycle-phase2"}`,
			`{"uuid":"a1","parentUuid":"u1","type":"assistant","message":{"role":"assistant","content":[{"type":"interaction","request_id":"approval-1","kind":"approval","state":"pending","prompt":"Allow Read?","options":["approve","deny"]}]},"timestamp":"2026-04-04T09:00:01Z","sessionId":"interaction-lifecycle-phase2"}`,
			fmt.Sprintf(`{"uuid":"u2","parentUuid":"a1","type":"user","message":{"role":"user","content":[{"type":"interaction","request_id":"approval-1","kind":"approval","state":%q,"action":%q}]},"timestamp":"2026-04-04T09:00:02Z","sessionId":"interaction-lifecycle-phase2"}`, finalStateText, finalAction),
		})
	case ProfileCodexTmuxCLI:
		return writeLinesFile(t, filepath.Join("2026", "04", "04", "session.jsonl"), []string{
			`{"timestamp":"2026-04-04T09:00:00Z","type":"response_item","payload":{"type":"interaction","request_id":"approval-1","kind":"approval","state":"pending","prompt":"Allow Read?","options":["approve","deny"]}}`,
			fmt.Sprintf(`{"timestamp":"2026-04-04T09:00:01Z","type":"response_item","payload":{"type":"interaction","request_id":"approval-1","kind":"approval","state":%q,"action":%q}}`, finalStateText, finalAction),
		})
	case ProfileGeminiTmuxCLI:
		dir := t.TempDir()
		path := filepath.Join(dir, "session.json")
		body := fmt.Sprintf(`{
  "sessionId": "gemini-interaction-lifecycle-phase2",
  "messages": [
    {"id":"m1","timestamp":"2026-04-04T09:00:00Z","type":"gemini","content":"approval needed","interactions":[{"request_id":"approval-1","kind":"approval","state":"pending","prompt":"Allow Read?","options":["approve","deny"]}]},
    {"id":"m2","timestamp":"2026-04-04T09:00:01Z","type":"user","content":"interaction updated","interactions":[{"request_id":"approval-1","kind":"approval","state":%q,"action":%q}]}
  ]
}`, finalStateText, finalAction)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write gemini interaction lifecycle transcript: %v", err)
		}
		return path
	default:
		t.Fatalf("unsupported profile %s", profile.ID)
		return ""
	}
}

func writeToolTranscript(t *testing.T, profile Profile, openTail bool) string {
	t.Helper()

	switch profile.ID {
	case ProfileClaudeTmuxCLI:
		lines := []string{
			`{"uuid":"u1","type":"user","message":{"role":"user","content":"read the file"},"timestamp":"2026-04-04T09:00:00Z","sessionId":"tool-phase2"}`,
			`{"uuid":"a1","parentUuid":"u1","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"Read","input":{"path":"README.md"}}]},"timestamp":"2026-04-04T09:00:01Z","sessionId":"tool-phase2"}`,
		}
		if !openTail {
			lines = append(lines,
				`{"uuid":"r1","parentUuid":"a1","type":"result","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"file data"}],"is_error":false},"timestamp":"2026-04-04T09:00:02Z","sessionId":"tool-phase2"}`,
				`{"uuid":"a2","parentUuid":"r1","type":"assistant","message":{"role":"assistant","content":"done"},"timestamp":"2026-04-04T09:00:03Z","sessionId":"tool-phase2"}`,
			)
		}
		return writeLinesFile(t, "session.jsonl", lines)
	case ProfileCodexTmuxCLI:
		lines := []string{
			`{"timestamp":"2026-04-04T09:00:00Z","type":"session_meta","payload":{"cwd":"/tmp/gascity/phase2/codex"}}`,
			`{"timestamp":"2026-04-04T09:00:01Z","type":"response_item","payload":{"type":"function_call","call_id":"call-1","name":"Read"}}`,
		}
		if !openTail {
			lines = append(lines,
				`{"timestamp":"2026-04-04T09:00:02Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":"file data"}}`,
				`{"timestamp":"2026-04-04T09:00:03Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"done"}]}}`,
			)
		}
		return writeLinesFile(t, filepath.Join("2026", "04", "04", "session.jsonl"), lines)
	case ProfileGeminiTmuxCLI:
		dir := t.TempDir()
		projectDir := filepath.Join(dir, "project-a", "chats")
		if err := os.MkdirAll(projectDir, 0o755); err != nil {
			t.Fatalf("mkdir gemini tree: %v", err)
		}
		path := filepath.Join(projectDir, "session.json")
		body := `{
  "sessionId": "gemini-phase2",
  "messages": [
    {"id":"m1","timestamp":"2026-04-04T09:00:00Z","type":"user","content":"hello"},
    {"id":"m2","timestamp":"2026-04-04T09:00:01Z","type":"gemini","content":"checking","toolCalls":[{"id":"tool-2","name":"Read","args":{"path":"README.md"}}]}` + trailingGeminiToolResult(openTail) + `
  ]
}`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write gemini transcript: %v", err)
		}
		return path
	default:
		t.Fatalf("unsupported profile %s", profile.ID)
		return ""
	}
}

func writeLinesFile(t *testing.T, rel string, lines []string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir transcript dir: %v", err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return path
}

func trailingGeminiToolResult(openTail bool) string {
	if openTail {
		return ""
	}
	return `,
    {"id":"m3","timestamp":"2026-04-04T09:00:02Z","type":"gemini","content":"done","toolCalls":[{"id":"tool-2","name":"Read","args":{"path":"README.md"},"result":[{"functionResponse":{"id":"tool-2","response":{"output":"file data"}}}]}]}`
}

func historyHasBlockKind(history *worker.HistorySnapshot, kind worker.BlockKind) bool {
	for _, entry := range history.Entries {
		for _, block := range entry.Blocks {
			if block.Kind == kind {
				return true
			}
		}
	}
	return false
}

func historyHasOpenToolUseEvidence(history *worker.HistorySnapshot) bool {
	if len(history.TailState.OpenToolUseIDs) > 0 {
		for _, toolUseID := range history.TailState.OpenToolUseIDs {
			if strings.TrimSpace(toolUseID) != "" {
				return true
			}
		}
	}
	if len(history.Entries) == 0 {
		return false
	}
	last := history.Entries[len(history.Entries)-1]
	for _, block := range last.Blocks {
		if block.Kind == worker.BlockKindToolUse {
			return true
		}
	}
	return false
}

func loadHistory(t *testing.T, provider, path string) *worker.HistorySnapshot {
	t.Helper()

	history, err := worker.SessionLogAdapter{}.LoadHistory(worker.LoadRequest{
		Provider:       provider,
		TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("LoadHistory(%s): %v", provider, err)
	}
	return history
}
