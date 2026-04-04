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
		RequirementInteractionSignal,
		RequirementInteractionPending,
		RequirementInteractionRespond,
		RequirementInteractionReject,
		RequirementToolEventNormalization,
		RequirementToolEventOpenTail,
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

func TestPhase2StartupOutcomeBounds(t *testing.T) {
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

					if got := strings.TrimSpace(readFile(t, run.StatePath)); got != tt.outcome {
						t.Fatalf("startup state = %q, want %q", got, tt.outcome)
					}
					if run.LaunchToWait > fakeStartupLaunchBound {
						t.Fatalf("%s exceeded launch-to-wait bound: %s > %s", RequirementStartupOutcomeBound, run.LaunchToWait, fakeStartupLaunchBound)
					}
					if len(run.Events) < 3 {
						t.Fatalf("expected control wait, control observed, and startup events for %s", RequirementStartupOutcomeBound)
					}
					if run.Events[0].Kind != "control_waiting" {
						t.Fatalf("first event kind = %q, want control_waiting", run.Events[0].Kind)
					}
					if run.Events[1].Kind != "control_observed" {
						t.Fatalf("second event kind = %q, want control_observed", run.Events[1].Kind)
					}
					event := run.Events[2]
					if event.Kind != "state_transition" {
						t.Fatalf("event kind = %q, want state_transition", event.Kind)
					}
					if event.State != tt.outcome {
						t.Fatalf("event state = %q, want %q", event.State, tt.outcome)
					}
					if event.Provider != string(profile.ID) {
						t.Fatalf("event provider = %q, want %q", event.Provider, profile.ID)
					}
					postControl := event.Time.Sub(run.Events[1].Time)
					maxPostControl := tt.delay + fakeStartupPostControlOverhead
					if postControl > maxPostControl {
						t.Fatalf("%s exceeded post-control bound: %s > %s", RequirementStartupOutcomeBound, postControl, maxPostControl)
					}
				})
			}
		})
	}
}

func TestPhase2RequiredInteractions(t *testing.T) {
	profiles, err := selectedProfiles()
	if err != nil {
		t.Fatal(err)
	}

	for _, profile := range profiles {
		profile := profile
		t.Run(string(profile.ID), func(t *testing.T) {
			t.Run(string(RequirementInteractionSignal), func(t *testing.T) {
				run := runFakeInteraction(t, profile.ID)
				if got := strings.TrimSpace(readFile(t, run.StatePath)); got != "blocked" {
					t.Fatalf("interaction state = %q, want blocked", got)
				}
				if len(run.Events) != 2 {
					t.Fatalf("event count = %d, want 2", len(run.Events))
				}
				event := run.Events[1]
				if event.Kind != "interaction" {
					t.Fatalf("event kind = %q, want interaction", event.Kind)
				}
				if event.Provider != string(profile.ID) {
					t.Fatalf("event provider = %q, want %q", event.Provider, profile.ID)
				}
				if event.State != "blocked" {
					t.Fatalf("event state = %q, want blocked", event.State)
				}
				if event.Interaction == nil {
					t.Fatal("interaction event missing payload")
				}
				if event.Interaction.Kind != "approval" {
					t.Fatalf("interaction kind = %q, want approval", event.Interaction.Kind)
				}
				if event.Interaction.RequestID != "req-1" {
					t.Fatalf("interaction request ID = %q, want req-1", event.Interaction.RequestID)
				}
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
				if err != nil {
					t.Fatalf("Pending: %v", err)
				}
				if got == nil {
					t.Fatal("expected pending interaction")
				}
				if got.RequestID != pending.RequestID {
					t.Fatalf("RequestID = %q, want %q", got.RequestID, pending.RequestID)
				}
				if got.Kind != pending.Kind {
					t.Fatalf("Kind = %q, want %q", got.Kind, pending.Kind)
				}
				if got.Metadata["profile"] != string(profile.ID) {
					t.Fatalf("profile metadata = %q, want %q", got.Metadata["profile"], profile.ID)
				}
			})

			t.Run(string(RequirementInteractionReject), func(t *testing.T) {
				err := sp.Respond(sessionName, runtime.InteractionResponse{
					RequestID: "wrong-req",
					Action:    "approve",
				})
				if err == nil {
					t.Fatal("Respond should fail for mismatched request id")
				}

				stillPending, pErr := sp.Pending(sessionName)
				if pErr != nil {
					t.Fatalf("Pending after reject: %v", pErr)
				}
				if stillPending == nil {
					t.Fatal("pending interaction cleared after mismatched response")
				}
				if len(sp.Responses[sessionName]) != 0 {
					t.Fatalf("recorded responses = %d, want 0", len(sp.Responses[sessionName]))
				}
			})

			t.Run(string(RequirementInteractionRespond), func(t *testing.T) {
				err := sp.Respond(sessionName, runtime.InteractionResponse{
					RequestID: pending.RequestID,
					Action:    "approve",
					Text:      "continue",
				})
				if err != nil {
					t.Fatalf("Respond: %v", err)
				}

				got, pErr := sp.Pending(sessionName)
				if pErr != nil {
					t.Fatalf("Pending after respond: %v", pErr)
				}
				if got != nil {
					t.Fatal("pending interaction not cleared after response")
				}
				if len(sp.Responses[sessionName]) != 1 {
					t.Fatalf("recorded responses = %d, want 1", len(sp.Responses[sessionName]))
				}
				if sp.Responses[sessionName][0].Action != "approve" {
					t.Fatalf("recorded action = %q, want approve", sp.Responses[sessionName][0].Action)
				}
			})
		})
	}
}

func TestPhase2ToolEventSubstrate(t *testing.T) {
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
				if got := history.TailState.OpenToolUseIDs; len(got) != 0 {
					t.Fatalf("open tool uses = %v, want none", got)
				}
				if len(history.Entries) < 2 {
					t.Fatalf("entries = %d, want at least 2", len(history.Entries))
				}
				if !historyHasBlockKind(history, worker.BlockKindToolUse) {
					t.Fatalf("normalized history missing %q block", worker.BlockKindToolUse)
				}
				if !historyHasBlockKind(history, worker.BlockKindToolResult) {
					t.Fatalf("normalized history missing %q block", worker.BlockKindToolResult)
				}
			})

			t.Run(string(RequirementToolEventOpenTail), func(t *testing.T) {
				path := writeToolTranscript(t, profile, true)
				history := loadHistory(t, profile.Provider, path)
				if !historyHasOpenToolUseEvidence(history) {
					t.Fatalf("normalized history does not preserve unresolved tool-use evidence: %+v", history.TailState.OpenToolUseIDs)
				}
			})
		})
	}
}

type fakeStartupRun struct {
	StatePath    string
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
		Events:    readWorkerFakeEvents(t, eventPath),
		Elapsed:   time.Since(start),
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

func readFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
