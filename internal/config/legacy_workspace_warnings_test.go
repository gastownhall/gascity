package config

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestDetectLegacyWorkspaceFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		workspace Workspace
		wantField string // substring "workspace.<field>" expected in the single emitted warning
		wantHint  string // substring expected in suggested replacement
	}{
		{
			name:      "provider populated",
			workspace: Workspace{Provider: "claude"},
			wantField: "workspace.provider",
			wantHint:  "provider per agent in agents/<name>/agent.toml",
		},
		{
			name:      "start_command populated",
			workspace: Workspace{StartCommand: "claude --resume"},
			wantField: "workspace.start_command",
			wantHint:  "per-agent `start_command`",
		},
		{
			name:      "suspended true",
			workspace: Workspace{Suspended: true},
			wantField: "workspace.suspended",
			wantHint:  "No action is required",
		},
		{
			name:      "install_agent_hooks populated",
			workspace: Workspace{InstallAgentHooks: []string{"claude"}},
			wantField: "workspace.install_agent_hooks",
			wantHint:  "install_agent_hooks per agent in agents/<name>/agent.toml",
		},
		{
			name:      "global_fragments populated",
			workspace: Workspace{GlobalFragments: []string{"shared"}},
			wantField: "workspace.global_fragments",
			wantHint:  "append_fragments",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := &City{Workspace: tt.workspace}
			warnings := DetectLegacyWorkspaceFields(cfg, "test.toml")
			if len(warnings) != 1 {
				t.Fatalf("warnings = %d, want 1: %v", len(warnings), warnings)
			}
			w := warnings[0]
			if !strings.HasPrefix(w, "test.toml: ") {
				t.Errorf("warning missing source prefix: %q", w)
			}
			if !strings.Contains(w, tt.wantField+" is deprecated:") {
				t.Errorf("warning missing %q is-deprecated phrase: %q", tt.wantField, w)
			}
			if !strings.Contains(w, tt.wantHint) {
				t.Errorf("warning missing replacement hint %q: %q", tt.wantHint, w)
			}
		})
	}
}

func TestIsLegacyWorkspaceFieldWarning(t *testing.T) {
	t.Parallel()
	warnings := DetectLegacyWorkspaceFields(&City{Workspace: Workspace{
		Provider:          "claude",
		StartCommand:      "claude",
		Suspended:         true,
		InstallAgentHooks: []string{"claude"},
		GlobalFragments:   []string{"shared"},
	}}, "city.toml")
	if len(warnings) != 5 {
		t.Fatalf("len(warnings) = %d, want 5", len(warnings))
	}
	for _, warning := range warnings {
		if !IsLegacyWorkspaceFieldWarning(warning) {
			t.Errorf("IsLegacyWorkspaceFieldWarning(%q) = false, want true", warning)
		}
	}
	if IsLegacyWorkspaceFieldWarning("city.toml: workspace.name redefined by fragment.toml") {
		t.Error("IsLegacyWorkspaceFieldWarning matched unrelated workspace warning")
	}
}

func TestDetectLegacyWorkspaceFields_EmptyConfigNoWarnings(t *testing.T) {
	t.Parallel()
	cfg := &City{Workspace: Workspace{Name: "demo", Prefix: "demo"}}
	warnings := DetectLegacyWorkspaceFields(cfg, "test.toml")
	if len(warnings) != 0 {
		t.Errorf("empty workspace should produce no warnings, got %v", warnings)
	}
}

func TestDetectLegacyWorkspaceFields_NilConfig(t *testing.T) {
	t.Parallel()
	if warnings := DetectLegacyWorkspaceFields(nil, "test.toml"); warnings != nil {
		t.Errorf("nil cfg should produce nil warnings, got %v", warnings)
	}
}

func TestDetectLegacyWorkspaceFields_SuspendedFalseDoesNotFire(t *testing.T) {
	t.Parallel()
	cfg := &City{Workspace: Workspace{Suspended: false}}
	warnings := DetectLegacyWorkspaceFields(cfg, "test.toml")
	if len(warnings) != 0 {
		t.Errorf("suspended=false should not warn, got %v", warnings)
	}
}

func TestDetectLegacyWorkspaceFields_AllFieldsStableOrder(t *testing.T) {
	t.Parallel()
	cfg := &City{Workspace: Workspace{
		Provider:          "claude",
		StartCommand:      "claude",
		Suspended:         true,
		InstallAgentHooks: []string{"claude"},
		GlobalFragments:   []string{"shared"},
	}}
	warnings := DetectLegacyWorkspaceFields(cfg, "city.toml")
	if len(warnings) != 5 {
		t.Fatalf("want 5 warnings, got %d: %v", len(warnings), warnings)
	}
	expected := []string{
		"workspace.provider",
		"workspace.start_command",
		"workspace.suspended",
		"workspace.install_agent_hooks",
		"workspace.global_fragments",
	}
	for i, want := range expected {
		if !strings.Contains(warnings[i], want+" is deprecated:") {
			t.Errorf("warning[%d] = %q, want to reference %s", i, warnings[i], want)
		}
	}
}

func TestLoadWithIncludesEmitsNonFatalLegacyWorkspaceWarning(t *testing.T) {
	t.Parallel()
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "legacy-workspace"
provider = "claude"
`)

	_, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	var found bool
	for _, warning := range prov.Warnings {
		if strings.Contains(warning, "workspace.provider is deprecated:") {
			found = true
			if !IsLegacyWorkspaceFieldWarning(warning) {
				t.Fatalf("legacy workspace warning is not classified as non-fatal: %q", warning)
			}
		}
	}
	if !found {
		t.Fatalf("missing workspace.provider deprecation warning in %v", prov.Warnings)
	}
}

func TestLoadWithIncludesLegacyWorkspaceWarningsUseFragmentSource(t *testing.T) {
	t.Parallel()
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["fragment.toml"]

[workspace]
name = "legacy-workspace"
`)
	fs.Files["/city/fragment.toml"] = []byte(`
[workspace]
provider = "claude"
global_fragments = ["shared"]
`)

	_, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	for _, field := range []string{"provider", "global_fragments"} {
		want := "/city/fragment.toml: workspace." + field + " is deprecated:"
		if !containsWarningPrefix(prov.Warnings, want) {
			t.Fatalf("missing fragment-sourced %s warning with prefix %q in %v", field, want, prov.Warnings)
		}
		wrong := "/city/city.toml: workspace." + field + " is deprecated:"
		if containsWarningPrefix(prov.Warnings, wrong) {
			t.Fatalf("warning for fragment-authored %s was attributed to root: %v", field, prov.Warnings)
		}
	}
}

func containsWarningPrefix(warnings []string, prefix string) bool {
	for _, warning := range warnings {
		if strings.HasPrefix(warning, prefix) {
			return true
		}
	}
	return false
}
