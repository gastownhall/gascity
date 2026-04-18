package fake

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadProfileJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profile.json")
	data := `{
  "name": "claude-phase1",
  "provider": "claude",
  "claims": {
    "profile_flavor": "tmux-cli",
    "supports_continuation": true,
    "supports_transcript": true
  },
  "launch": {
    "args": ["fake-worker"],
    "env": {"CLAUDE_CODE_ENTRYPOINT": "fake"},
    "startup": {"outcome": "ready", "ready_after": "250ms"}
  },
  "transcript": {"format": "jsonl", "path": ".runtime/transcript.jsonl"},
  "continuation": {"mode": "handle", "handle_env": "FAKE_CONT_HANDLE", "same_conversation": true},
  "interactions": [{"kind": "tool_call", "required": true}]
}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	profile, err := LoadProfile(path)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if profile.Provider != "claude" {
		t.Fatalf("Provider = %q, want claude", profile.Provider)
	}
	if !profile.Claims.SupportsContinuation {
		t.Fatal("SupportsContinuation = false, want true")
	}
	if got := profile.Launch.Startup.ReadyAfter; got != "250ms" {
		t.Fatalf("ReadyAfter = %q, want 250ms", got)
	}
}

func TestLoadScenarioYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scenario.yaml")
	data := `
name: transcript-growth
provider: codex
steps:
  - id: boot
    action: startup
    state: ready
  - id: first-turn
    action: append_transcript
    transcript:
      role: assistant
      type: tool_use
      tool_name: Read
      tool_use_id: tool-1
      text: ready for task
  - id: approval
    action: emit_interaction
    interaction:
      kind: approval
      request_id: req-1
      prompt: continue?
      options: [yes, no]
  - id: block-for-control
    action: wait_for_control
    path: control/start.txt
    expect_control: proceed
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	scenario, err := LoadScenario(path)
	if err != nil {
		t.Fatalf("LoadScenario: %v", err)
	}
	if len(scenario.Steps) != 4 {
		t.Fatalf("len(Steps) = %d, want 4", len(scenario.Steps))
	}
	if got := scenario.Steps[1].Transcript.Text; got != "ready for task" {
		t.Fatalf("Transcript.Text = %q, want ready for task", got)
	}
	if got := scenario.Steps[1].Transcript.ToolName; got != "Read" {
		t.Fatalf("Transcript.ToolName = %q, want Read", got)
	}
	if got := scenario.Steps[2].Interaction.Kind; got != "approval" {
		t.Fatalf("Interaction.Kind = %q, want approval", got)
	}
}

func TestLoadScenarioRejectsUnknownAction(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scenario.json")
	data := `{
  "name": "bad",
  "steps": [{"action": "teleport"}]
}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := LoadScenario(path)
	if err == nil {
		t.Fatal("LoadScenario should fail")
	}
	if !strings.Contains(err.Error(), "unsupported action") {
		t.Fatalf("LoadScenario error = %v, want unsupported action", err)
	}
}

func TestLoadHelperConfigRequiresProfile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `
scenario:
  name: no-profile
  steps:
    - action: startup
      state: ready
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := LoadHelperConfig(path)
	if err == nil {
		t.Fatal("LoadHelperConfig should fail")
	}
	if !strings.Contains(err.Error(), "profile or scenario.profile is required") {
		t.Fatalf("LoadHelperConfig error = %v", err)
	}
}

func TestLoadScenarioRejectsInteractionWithoutKind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scenario.yaml")
	data := `
name: missing-kind
steps:
  - action: emit_interaction
    interaction:
      prompt: approve?
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := LoadScenario(path)
	if err == nil {
		t.Fatal("LoadScenario should fail")
	}
	if !strings.Contains(err.Error(), "interaction.kind") {
		t.Fatalf("LoadScenario error = %v, want interaction.kind", err)
	}
}

func TestLoadScenarioRejectsTranscriptWithoutContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scenario.yaml")
	data := `
name: missing-transcript-content
steps:
  - action: append_transcript
    transcript:
      role: assistant
      type: tool_use
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := LoadScenario(path)
	if err == nil {
		t.Fatal("LoadScenario should fail")
	}
	if !strings.Contains(err.Error(), "transcript text, content, or tool_name") {
		t.Fatalf("LoadScenario error = %v, want transcript content validation", err)
	}
}

func TestLoadScenarioAcceptsInputStep(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scenario.json")
	data := `{
  "name": "input-receipt",
  "steps": [
    {"action": "input", "input": {"path": "input.txt", "expect": "hello", "receipt_path": "receipt.txt", "echo_path": "echo.txt"}}
  ]
}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	scenario, err := LoadScenario(path)
	if err != nil {
		t.Fatalf("LoadScenario: %v", err)
	}
	if got := scenario.Steps[0].Input.Path; got != "input.txt" {
		t.Fatalf("Input.Path = %q, want input.txt", got)
	}
	if got := scenario.Steps[0].Input.Expect; got != "hello" {
		t.Fatalf("Input.Expect = %q, want hello", got)
	}
	if got := scenario.Steps[0].Input.ReceiptPath; got != "receipt.txt" {
		t.Fatalf("Input.ReceiptPath = %q, want receipt.txt", got)
	}
}

func TestLoadScenarioRejectsInputWithoutPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scenario.yaml")
	data := `
name: missing-input-path
steps:
  - action: input
    input:
      expect: hello
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := LoadScenario(path)
	if err == nil {
		t.Fatal("LoadScenario should fail")
	}
	if !strings.Contains(err.Error(), "input requires input.path") {
		t.Fatalf("LoadScenario error = %v, want input.path validation", err)
	}
}

func TestLoadScenarioRejectsInputWithoutExpect(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scenario.yaml")
	data := `
name: missing-input-expect
steps:
  - action: input
    input:
      path: input.txt
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := LoadScenario(path)
	if err == nil {
		t.Fatal("LoadScenario should fail")
	}
	if !strings.Contains(err.Error(), "input requires input.expect") {
		t.Fatalf("LoadScenario error = %v, want input.expect validation", err)
	}
}

func TestRunnerRunEmitsInteractionAndToolTranscriptEvents(t *testing.T) {
	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "transcript.jsonl")
	statePath := filepath.Join(dir, "state.txt")

	cfg := HelperConfig{
		Profile: &Profile{
			Name:     "phase2",
			Provider: "claude",
		},
		Scenario: Scenario{
			Name: "scripted-interaction",
			Steps: []Step{
				{ID: "boot", Action: "startup", State: "ready"},
				{
					ID:     "tool-call",
					Action: "append_transcript",
					Transcript: TranscriptEvent{
						Role:      "assistant",
						Type:      "tool_use",
						ToolName:  "Read",
						ToolUseID: "tool-1",
						Text:      "reading config",
					},
				},
				{
					ID:     "approval",
					Action: "emit_interaction",
					Interaction: InteractionEvent{
						Kind:      "approval",
						RequestID: "req-1",
						Prompt:    "Continue startup?",
						Options:   []string{"yes", "no"},
						State:     "blocked",
						Metadata:  map[string]string{"source": "startup"},
					},
					Metadata: map[string]string{"step": "approval"},
				},
				{
					ID:     "tool-result",
					Action: "append_transcript",
					Transcript: TranscriptEvent{
						Role:      "tool",
						Type:      "tool_result",
						ToolUseID: "tool-1",
						Content:   "ok",
					},
				},
			},
		},
		Output: OutputSpec{
			TranscriptPath: transcriptPath,
			StatePath:      statePath,
		},
	}

	var stdout bytes.Buffer
	now := time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC)
	runner := Runner{Now: func() time.Time { return now }}
	if err := runner.Run(context.Background(), cfg, &stdout); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if data, err := os.ReadFile(statePath); err != nil {
		t.Fatalf("ReadFile(state): %v", err)
	} else if string(data) != "blocked\n" {
		t.Fatalf("state file = %q, want blocked", string(data))
	}

	eventLines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if got := len(eventLines); got != 4 {
		t.Fatalf("event count = %d, want 4", got)
	}
	var interactionEvent Event
	if err := json.Unmarshal([]byte(eventLines[2]), &interactionEvent); err != nil {
		t.Fatalf("Unmarshal interaction event: %v", err)
	}
	if interactionEvent.Kind != "interaction" {
		t.Fatalf("interaction event kind = %q, want interaction", interactionEvent.Kind)
	}
	if interactionEvent.State != "blocked" {
		t.Fatalf("interaction event state = %q, want blocked", interactionEvent.State)
	}
	if interactionEvent.Interaction == nil || interactionEvent.Interaction.RequestID != "req-1" {
		t.Fatalf("interaction request ID = %+v, want req-1", interactionEvent.Interaction)
	}
	if interactionEvent.Metadata["source"] != "startup" || interactionEvent.Metadata["step"] != "approval" {
		t.Fatalf("interaction metadata = %+v, want merged metadata", interactionEvent.Metadata)
	}

	transcriptData, err := os.ReadFile(transcriptPath)
	if err != nil {
		t.Fatalf("ReadFile(transcript): %v", err)
	}
	transcriptLines := strings.Split(strings.TrimSpace(string(transcriptData)), "\n")
	if got := len(transcriptLines); got != 2 {
		t.Fatalf("transcript line count = %d, want 2", got)
	}
	var toolUse map[string]any
	if err := json.Unmarshal([]byte(transcriptLines[0]), &toolUse); err != nil {
		t.Fatalf("Unmarshal tool_use transcript: %v", err)
	}
	if toolUse["tool_name"] != "Read" {
		t.Fatalf("tool_name = %#v, want Read", toolUse["tool_name"])
	}
	if toolUse["tool_use_id"] != "tool-1" {
		t.Fatalf("tool_use_id = %#v, want tool-1", toolUse["tool_use_id"])
	}
	var toolResult map[string]any
	if err := json.Unmarshal([]byte(transcriptLines[1]), &toolResult); err != nil {
		t.Fatalf("Unmarshal tool_result transcript: %v", err)
	}
	if toolResult["content"] != "ok" {
		t.Fatalf("content = %#v, want ok", toolResult["content"])
	}
}

func TestRunnerRunEmitsInputReceiptEvidence(t *testing.T) {
	dir := t.TempDir()
	inputPath := filepath.Join(dir, "input.txt")
	receiptPath := filepath.Join(dir, "receipt.txt")
	echoPath := filepath.Join(dir, "echo.txt")
	eventPath := filepath.Join(dir, "events.jsonl")
	statePath := filepath.Join(dir, "state.txt")

	cfg := HelperConfig{
		Profile: &Profile{
			Name:     "phase2-input",
			Provider: "codex",
		},
		Scenario: Scenario{
			Name: "scripted-input",
			Steps: []Step{
				{ID: "boot", Action: "startup", State: "ready"},
				{
					ID:     "input",
					Action: "input",
					Input: InputEvent{
						Path:        inputPath,
						Expect:      "deliver this",
						ReceiptPath: receiptPath,
						EchoPath:    echoPath,
						Metadata:    map[string]string{"source": "phase2"},
					},
					State: "received",
				},
			},
		},
		Output: OutputSpec{
			EventLogPath: eventPath,
			StatePath:    statePath,
		},
	}

	var stdout bytes.Buffer
	runner := Runner{Now: func() time.Time {
		return time.Date(2026, 4, 4, 12, 30, 0, 0, time.UTC)
	}}
	errCh := make(chan error, 1)
	go func() {
		errCh <- runner.Run(context.Background(), cfg, &stdout)
	}()

	inputEvent := waitForEventKind(t, eventPath, "input_waiting", 2*time.Second)
	if inputEvent.Input == nil {
		t.Fatal("input_waiting event missing input payload")
	}
	if inputEvent.Input.Path != inputPath {
		t.Fatalf("input path = %q, want %q", inputEvent.Input.Path, inputPath)
	}

	if err := os.WriteFile(inputPath, []byte("deliver this\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(input): %v", err)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run: %v\nstdout:\n%s", err, stdout.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for fake worker to finish")
	}

	if data, err := os.ReadFile(receiptPath); err != nil {
		t.Fatalf("ReadFile(receipt): %v", err)
	} else if got := strings.TrimSpace(string(data)); got != "deliver this" {
		t.Fatalf("receipt = %q, want deliver this", got)
	}
	if data, err := os.ReadFile(echoPath); err != nil {
		t.Fatalf("ReadFile(echo): %v", err)
	} else if got := strings.TrimSpace(string(data)); got != "deliver this" {
		t.Fatalf("echo = %q, want deliver this", got)
	}
	if data, err := os.ReadFile(statePath); err != nil {
		t.Fatalf("ReadFile(state): %v", err)
	} else if got := strings.TrimSpace(string(data)); got != "received" {
		t.Fatalf("state = %q, want received", got)
	}

	events := readEvents(t, eventPath)
	if len(events) != 3 {
		t.Fatalf("event count = %d, want 3", len(events))
	}
	if events[2].Kind != "input_received" {
		t.Fatalf("third event kind = %q, want input_received", events[2].Kind)
	}
	if events[2].Input == nil {
		t.Fatal("input_received event missing input payload")
	}
	if events[2].Input.Observed != "deliver this\n" {
		t.Fatalf("observed input = %q, want raw input contents", events[2].Input.Observed)
	}
	if events[2].Input.Metadata["source"] != "phase2" {
		t.Fatalf("input metadata = %+v, want source=phase2", events[2].Input.Metadata)
	}
}

func waitForEventKind(t *testing.T, path, kind string, timeout time.Duration) Event {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		events, err := readEventsMaybe(path)
		if err != nil {
			if os.IsNotExist(err) {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			t.Fatalf("read events: %v", err)
		}
		for _, event := range events {
			if event.Kind == kind {
				return event
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in %s", kind, path)
	return Event{}
}

func readEventsMaybe(path string) ([]Event, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	events := make([]Event, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, nil
}

func readEvents(t *testing.T, path string) []Event {
	t.Helper()

	events, err := readEventsMaybe(path)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	return events
}
