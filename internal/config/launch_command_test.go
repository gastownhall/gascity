package config

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
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

	got, err := BuildProviderLaunchCommand(dir, rp, nil, "")
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
	}, "")
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
	}, "")
	if err != nil {
		t.Fatalf("BuildProviderLaunchCommand: %v", err)
	}

	want := "claude --dangerously-skip-permissions --effort low"
	if got.Command != want {
		t.Fatalf("Command = %q, want %q", got.Command, want)
	}
}

func TestProviderOptionMapCapacity(t *testing.T) {
	tests := []struct {
		name         string
		defaultsLen  int
		overridesLen int
		want         int
	}{
		{
			name:         "adds safe override capacity",
			defaultsLen:  2,
			overridesLen: 3,
			want:         5,
		},
		{
			name:         "keeps defaults when overrides are empty",
			defaultsLen:  4,
			overridesLen: 0,
			want:         4,
		},
		{
			name:         "uses exact boundary when addition is safe",
			defaultsLen:  math.MaxInt - 1,
			overridesLen: 1,
			want:         math.MaxInt,
		},
		{
			name:         "skips override capacity when addition would overflow",
			defaultsLen:  math.MaxInt,
			overridesLen: 1,
			want:         math.MaxInt,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := providerOptionMapCapacity(tt.defaultsLen, tt.overridesLen); got != tt.want {
				t.Fatalf("providerOptionMapCapacity(%d, %d) = %d, want %d", tt.defaultsLen, tt.overridesLen, got, tt.want)
			}
		})
	}
}

func TestBuildProviderLaunchCommandUsesACPCommand(t *testing.T) {
	rp := &ResolvedProvider{
		Command: "custom-opencode",
		ACPArgs: []string{"acp"},
	}

	t.Run("acp transport uses ACPCommandString", func(t *testing.T) {
		got, err := BuildProviderLaunchCommand("", rp, nil, "acp")
		if err != nil {
			t.Fatalf("BuildProviderLaunchCommand: %v", err)
		}
		want := "custom-opencode acp"
		if got.Command != want {
			t.Fatalf("Command = %q, want %q", got.Command, want)
		}
	})

	t.Run("default transport uses CommandString", func(t *testing.T) {
		got, err := BuildProviderLaunchCommand("", rp, nil, "")
		if err != nil {
			t.Fatalf("BuildProviderLaunchCommand: %v", err)
		}
		want := "custom-opencode"
		if got.Command != want {
			t.Fatalf("Command = %q, want %q", got.Command, want)
		}
	})

	t.Run("tmux transport uses CommandString", func(t *testing.T) {
		got, err := BuildProviderLaunchCommand("", rp, nil, "tmux")
		if err != nil {
			t.Fatalf("BuildProviderLaunchCommand: %v", err)
		}
		want := "custom-opencode"
		if got.Command != want {
			t.Fatalf("Command = %q, want %q", got.Command, want)
		}
	})

	t.Run("unknown transport errors", func(t *testing.T) {
		_, err := BuildProviderLaunchCommand("", rp, nil, "stdio")
		if err == nil {
			t.Fatal("BuildProviderLaunchCommand() error = nil, want unknown transport error")
		}
	})
}

func TestBuildProviderTemplateLaunchCommandAddsDefaultsAndSettings(t *testing.T) {
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

	got, err := BuildProviderTemplateLaunchCommand(dir, rp, "", nil, nil)
	if err != nil {
		t.Fatalf("BuildProviderTemplateLaunchCommand: %v", err)
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

func TestBuildProviderTemplateLaunchCommandRejectsInvalidInput(t *testing.T) {
	if _, err := BuildProviderTemplateLaunchCommand("", nil, "", nil, nil); err == nil {
		t.Fatal("BuildProviderTemplateLaunchCommand(nil provider) error = nil, want error")
	}
	rp := &ResolvedProvider{Command: "custom-agent"}
	if _, err := BuildProviderTemplateLaunchCommand("", rp, "stdio", nil, nil); err == nil {
		t.Fatal("BuildProviderTemplateLaunchCommand(unknown transport) error = nil, want error")
	}
}

// TestProviderTemplateCommandOmitsUnhonoredDefaults pins the template command
// to the controller's tolerance: option defaults that cannot render as flags
// are omitted, where BuildProviderLaunchCommand's strict merge rejects them.
// Config loading does not validate agent option_defaults, so a template
// command that errored on them would fail every create for that agent.
func TestProviderTemplateCommandOmitsUnhonoredDefaults(t *testing.T) {
	schema := []ProviderOption{
		{
			Key: "permission_mode", Type: "select", Default: "auto-edit",
			Choices: []OptionChoice{
				{Value: "auto-edit", FlagArgs: []string{"--permission-mode", "auto-edit"}},
				{Value: "unrestricted", FlagArgs: []string{"--skip-permissions"}},
			},
		},
		{
			Key: "effort", Type: "select",
			Choices: []OptionChoice{
				{Value: "", FlagArgs: nil},
				{Value: "max", FlagArgs: []string{"--effort", "max"}},
			},
		},
	}
	tests := []struct {
		name     string
		defaults map[string]string
		want     string
	}{
		{
			name:     "resolvable defaults",
			defaults: map[string]string{"permission_mode": "unrestricted", "effort": "max"},
			want:     "agent --skip-permissions --effort max",
		},
		{
			name:     "unknown option",
			defaults: map[string]string{"permission_mode": "unrestricted", "effort": "max", "bogus": "x"},
			want:     "agent --skip-permissions --effort max",
		},
		{
			name:     "invalid closed value",
			defaults: map[string]string{"permission_mode": "unrestricted", "effort": "ultra"},
			want:     "agent --skip-permissions",
		},
		{
			name:     "empty value",
			defaults: map[string]string{"permission_mode": "", "effort": "max"},
			want:     "agent --effort max",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rp := &ResolvedProvider{Name: "agent", Command: "agent", OptionsSchema: schema, EffectiveDefaults: tt.defaults}

			got, err := ProviderTemplateCommand(rp, "", nil, nil)
			if err != nil {
				t.Fatalf("ProviderTemplateCommand: %v", err)
			}
			if got != tt.want {
				t.Fatalf("ProviderTemplateCommand() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProviderTemplateCommandAddsKimiHookConfigWhenKimiHooksInstalled(t *testing.T) {
	kimi := &ResolvedProvider{
		Name:    "kimi",
		Command: "kimi",
		Args:    []string{"--yolo"},
		ACPArgs: []string{"--yolo", "acp"},
	}
	wrappedKimi := &ResolvedProvider{
		Name:            "fast-kimi",
		BuiltinAncestor: "kimi",
		Command:         "kimi",
		Args:            []string{"--yolo"},
	}
	kimiBase := "builtin:kimi"
	cityProviders := map[string]ProviderSpec{"fast-kimi": {Base: &kimiBase}}
	tests := []struct {
		name         string
		resolved     *ResolvedProvider
		transport    string
		installHooks []string
		want         string
	}{
		{
			name:         "tmux appends the config file",
			resolved:     kimi,
			installHooks: []string{"claude", "kimi"},
			want:         "kimi --yolo --config-file .kimi/config.toml",
		},
		{
			name:         "acp inserts the config file before the acp subcommand",
			resolved:     kimi,
			transport:    SessionTransportACP,
			installHooks: []string{"kimi"},
			want:         "kimi --yolo --config-file .kimi/config.toml acp",
		},
		{
			name:         "a wrapped kimi provider matches a kimi-family hook alias",
			resolved:     wrappedKimi,
			installHooks: []string{"fast-kimi"},
			want:         "kimi --yolo --config-file .kimi/config.toml",
		},
		{
			name:     "kimi without kimi hooks keeps the bare command",
			resolved: kimi,
			want:     "kimi --yolo",
		},
		{
			name:         "other providers ignore kimi hooks",
			resolved:     &ResolvedProvider{Name: "codex", Command: "codex"},
			installHooks: []string{"kimi"},
			want:         "codex",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ProviderTemplateCommand(tt.resolved, tt.transport, tt.installHooks, cityProviders)
			if err != nil {
				t.Fatalf("ProviderTemplateCommand: %v", err)
			}
			if got != tt.want {
				t.Fatalf("ProviderTemplateCommand() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildProviderTemplateLaunchCommandUsesBuiltinAncestorForSettings(t *testing.T) {
	dir := t.TempDir()
	runtimeDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(runtimeDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	rp := &ResolvedProvider{
		Name:            "claude-max",
		BuiltinAncestor: "claude",
		Command:         "aimux",
		Args:            []string{"run", "claude", "--"},
	}

	got, err := BuildProviderTemplateLaunchCommand(dir, rp, "", nil, nil)
	if err != nil {
		t.Fatalf("BuildProviderTemplateLaunchCommand: %v", err)
	}

	if want := fmt.Sprintf("--settings %q", settingsPath); !strings.Contains(got.Command, want) {
		t.Fatalf("Command = %q, want settings arg %q", got.Command, want)
	}
	if count := strings.Count(got.Command, "--settings"); count != 1 {
		t.Fatalf("Command has %d --settings flags, want 1: %q", count, got.Command)
	}
	if got.SettingsPath != settingsPath {
		t.Fatalf("SettingsPath = %q, want %q", got.SettingsPath, settingsPath)
	}
}

func TestBuildProviderTemplateLaunchCommandIgnoresDeprecatedKindForSettings(t *testing.T) {
	dir := t.TempDir()
	runtimeDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "settings.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	rp := &ResolvedProvider{
		Name:    "custom-provider",
		Kind:    "claude",
		Command: "custom-provider",
	}

	got, err := BuildProviderTemplateLaunchCommand(dir, rp, "", nil, nil)
	if err != nil {
		t.Fatalf("BuildProviderTemplateLaunchCommand: %v", err)
	}
	if strings.Contains(got.Command, "--settings") {
		t.Fatalf("Command = %q, want no settings from deprecated Kind fallback", got.Command)
	}
	if got.SettingsPath != "" || got.SettingsRel != "" {
		t.Fatalf("unexpected settings source from deprecated Kind fallback: %#v", got)
	}
}
