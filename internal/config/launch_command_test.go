package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestBuildProviderLaunchCommandAddsDefaultsAndSettings(t *testing.T) {
	dir := t.TempDir()
	runtimeDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "settings.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := BuiltinProviders()["claude"]
	rp := specToResolved("claude", &spec)

	got, err := BuildProviderLaunchCommand(dir, rp, nil)
	if err != nil {
		t.Fatalf("BuildProviderLaunchCommand: %v", err)
	}

	wantCommand := fmt.Sprintf("claude --dangerously-skip-permissions --effort max --settings %q", filepath.Join(dir, ".gc", "settings.json"))
	if got.Command != wantCommand {
		t.Fatalf("Command = %q, want %q", got.Command, wantCommand)
	}
	if got.SettingsPath != filepath.Join(dir, ".gc", "settings.json") {
		t.Fatalf("SettingsPath = %q, want %q", got.SettingsPath, filepath.Join(dir, ".gc", "settings.json"))
	}
	if got.SettingsRel != filepath.Join(".gc", "settings.json") {
		t.Fatalf("SettingsRel = %q, want %q", got.SettingsRel, filepath.Join(".gc", "settings.json"))
	}
}

func TestBuildProviderLaunchCommandAppliesOptionOverrides(t *testing.T) {
	spec := BuiltinProviders()["claude"]
	rp := specToResolved("claude", &spec)

	got, err := BuildProviderLaunchCommand("", rp, map[string]string{
		"permission_mode": "plan",
		"effort":          "low",
	})
	if err != nil {
		t.Fatalf("BuildProviderLaunchCommand: %v", err)
	}

	want := "claude --permission-mode plan --effort low"
	if got.Command != want {
		t.Fatalf("Command = %q, want %q", got.Command, want)
	}
	if got.SettingsPath != "" || got.SettingsRel != "" {
		t.Fatalf("unexpected settings source: %#v", got)
	}
}

func TestBuildProviderLaunchCommandIgnoresInitialMessageOverride(t *testing.T) {
	spec := BuiltinProviders()["claude"]
	rp := specToResolved("claude", &spec)

	got, err := BuildProviderLaunchCommand("", rp, map[string]string{
		"initial_message": "hello",
		"effort":          "low",
	})
	if err != nil {
		t.Fatalf("BuildProviderLaunchCommand: %v", err)
	}

	want := "claude --dangerously-skip-permissions --effort low"
	if got.Command != want {
		t.Fatalf("Command = %q, want %q", got.Command, want)
	}
}
