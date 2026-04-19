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

	workerfake "github.com/gastownhall/gascity/internal/worker/fake"
)

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
