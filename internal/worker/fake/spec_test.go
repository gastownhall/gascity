package fake

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
      type: message
      text: ready for task
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
	if len(scenario.Steps) != 3 {
		t.Fatalf("len(Steps) = %d, want 3", len(scenario.Steps))
	}
	if got := scenario.Steps[1].Transcript.Text; got != "ready for task" {
		t.Fatalf("Transcript.Text = %q, want ready for task", got)
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
