package acceptancehelpers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureClaudeStateFileCreatesOnboardingState(t *testing.T) {
	home := t.TempDir()

	if err := EnsureClaudeStateFile(home); err != nil {
		t.Fatalf("EnsureClaudeStateFile: %v", err)
	}

	state := readClaudeStateForTest(t, filepath.Join(home, ".claude.json"))
	if got := state["hasCompletedOnboarding"]; got != true {
		t.Fatalf("hasCompletedOnboarding = %#v, want true", got)
	}
}

func TestEnsureClaudeProjectStateMergesExistingState(t *testing.T) {
	home := t.TempDir()
	projectPath := filepath.Join(t.TempDir(), "city")

	initial := map[string]any{
		"otherSetting":           "keep-me",
		"hasCompletedOnboarding": false,
		"projects": map[string]any{
			projectPath: map[string]any{
				"customFlag": "keep-project-state",
			},
		},
	}
	writeClaudeStateForTest(t, filepath.Join(home, ".claude.json"), initial)

	env := &Env{vars: map[string]string{"HOME": home}}
	if err := EnsureClaudeProjectState(env, projectPath); err != nil {
		t.Fatalf("EnsureClaudeProjectState: %v", err)
	}

	state := readClaudeStateForTest(t, filepath.Join(home, ".claude.json"))
	if got := state["hasCompletedOnboarding"]; got != true {
		t.Fatalf("hasCompletedOnboarding = %#v, want true", got)
	}
	if got := state["otherSetting"]; got != "keep-me" {
		t.Fatalf("otherSetting = %#v, want keep-me", got)
	}

	projects, ok := state["projects"].(map[string]any)
	if !ok {
		t.Fatalf("projects missing or wrong type: %#v", state["projects"])
	}
	entry, ok := projects[projectPath].(map[string]any)
	if !ok {
		t.Fatalf("project entry missing or wrong type: %#v", projects[projectPath])
	}
	if got := entry["hasCompletedProjectOnboarding"]; got != true {
		t.Fatalf("hasCompletedProjectOnboarding = %#v, want true", got)
	}
	if got := entry["hasTrustDialogAccepted"]; got != true {
		t.Fatalf("hasTrustDialogAccepted = %#v, want true", got)
	}
	if got := entry["projectOnboardingSeenCount"]; got != float64(1) {
		t.Fatalf("projectOnboardingSeenCount = %#v, want 1", got)
	}
	if got := entry["customFlag"]; got != "keep-project-state" {
		t.Fatalf("customFlag = %#v, want keep-project-state", got)
	}
}

func readClaudeStateForTest(t *testing.T, path string) map[string]any {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return state
}

func writeClaudeStateForTest(t *testing.T, path string, state map[string]any) {
	t.Helper()

	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
