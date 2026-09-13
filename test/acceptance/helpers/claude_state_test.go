package acceptancehelpers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureClaudeStateFileCreatesOnboardingState(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(home, "custom-claude")

	if err := EnsureClaudeStateFile(home, configDir); err != nil {
		t.Fatalf("EnsureClaudeStateFile: %v", err)
	}

	for _, statePath := range []string{
		filepath.Join(home, ".claude.json"),
		filepath.Join(configDir, ".claude.json"),
	} {
		state := readClaudeStateForTest(t, statePath)
		if got := state["hasCompletedOnboarding"]; got != true {
			t.Fatalf("%s hasCompletedOnboarding = %#v, want true", statePath, got)
		}
		if got := state["theme"]; got != "light" {
			t.Fatalf("%s theme = %#v, want light", statePath, got)
		}
	}
}

func TestEnsureClaudeProjectStateMergesExistingState(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(home, "isolated-claude")
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
	writeClaudeStateForTest(t, filepath.Join(configDir, ".claude.json"), map[string]any{
		"nestedSetting": "keep-nested-state",
		"theme":         "dark",
	})

	env := &Env{vars: map[string]string{"HOME": home, "CLAUDE_CONFIG_DIR": configDir}}
	if err := EnsureClaudeProjectState(env, projectPath); err != nil {
		t.Fatalf("EnsureClaudeProjectState: %v", err)
	}

	assertClaudeProjectTrustedForTest(t, filepath.Join(home, ".claude.json"), projectPath, map[string]any{
		"otherSetting": "keep-me",
	}, map[string]any{
		"customFlag": "keep-project-state",
	})
	assertClaudeProjectTrustedForTest(t, filepath.Join(configDir, ".claude.json"), projectPath, map[string]any{
		"nestedSetting": "keep-nested-state",
		"theme":         "dark",
	}, nil)
}

func assertClaudeProjectTrustedForTest(t *testing.T, statePath, projectPath string, preservedState, preservedProject map[string]any) {
	t.Helper()

	state := readClaudeStateForTest(t, statePath)
	if got := state["hasCompletedOnboarding"]; got != true {
		t.Fatalf("%s hasCompletedOnboarding = %#v, want true", statePath, got)
	}
	for key, want := range preservedState {
		if got := state[key]; got != want {
			t.Fatalf("%s %s = %#v, want %#v", statePath, key, got, want)
		}
	}

	projects, ok := state["projects"].(map[string]any)
	if !ok {
		t.Fatalf("%s projects missing or wrong type: %#v", statePath, state["projects"])
	}
	entry, ok := projects[projectPath].(map[string]any)
	if !ok {
		t.Fatalf("%s project entry missing or wrong type: %#v", statePath, projects[projectPath])
	}
	if got := entry["hasCompletedProjectOnboarding"]; got != true {
		t.Fatalf("%s hasCompletedProjectOnboarding = %#v, want true", statePath, got)
	}
	if got := entry["hasTrustDialogAccepted"]; got != true {
		t.Fatalf("%s hasTrustDialogAccepted = %#v, want true", statePath, got)
	}
	if got := entry["projectOnboardingSeenCount"]; got != float64(1) {
		t.Fatalf("%s projectOnboardingSeenCount = %#v, want 1", statePath, got)
	}
	for key, want := range preservedProject {
		if got := entry[key]; got != want {
			t.Fatalf("%s project %s = %#v, want %#v", statePath, key, got, want)
		}
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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
