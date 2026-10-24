package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/promptmeta"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

func launchModelTestProvider(opt config.ProviderOption, defaults map[string]string) *config.ResolvedProvider {
	return &config.ResolvedProvider{
		Name:              "claude",
		OptionsSchema:     []config.ProviderOption{{Key: "effort", Type: "select", Choices: []config.OptionChoice{{Value: "high", FlagArgs: []string{"--effort", "high"}}}}, opt},
		EffectiveDefaults: defaults,
	}
}

// TestResolvedLaunchModelUsesFlagValue pins that the launch_model stamp is the
// provider-facing id rendered from the selected choice's flag args — never the
// choice key a deployed city pins in option_defaults / template_overrides.
func TestResolvedLaunchModelUsesFlagValue(t *testing.T) {
	closedModel := config.ProviderOption{
		Key:     "model",
		Type:    "select",
		Default: "sonnet-5",
		Choices: []config.OptionChoice{
			{Value: "", Label: "Default"},
			{Value: "opus-5-5", FlagArgs: []string{"--model", "claude-opus-5-5"}, FlagAliases: [][]string{{"-m", "claude-opus-5-5"}}},
			{Value: "sonnet-5", FlagArgs: []string{"--model", "claude-sonnet-5"}},
			{Value: "short-m", FlagArgs: []string{"-m", "claude-haiku-5"}},
			{Value: "assign", FlagArgs: []string{"--model=claude-opus-5-5-assign"}},
			{Value: "codex-style", FlagArgs: []string{"-c", "model=gpt-5.5"}},
		},
	}
	openModel := closedModel
	openModel.FlagTemplate = []string{"--model", config.OptionValuePlaceholder}

	tests := []struct {
		name      string
		rp        *config.ResolvedProvider
		overrides map[string]string
		want      string
	}{
		{
			name:      "override choice key renders its flag value",
			rp:        launchModelTestProvider(closedModel, map[string]string{"model": "sonnet-5"}),
			overrides: map[string]string{"model": "opus-5-5"},
			want:      "claude-opus-5-5",
		},
		{
			name: "effective default when no override",
			rp:   launchModelTestProvider(closedModel, map[string]string{"model": "opus-5-5"}),
			want: "claude-opus-5-5",
		},
		{
			name: "option default when no effective default",
			rp:   launchModelTestProvider(closedModel, nil),
			want: "claude-sonnet-5",
		},
		{
			name:      "override for another option does not select a model",
			rp:        launchModelTestProvider(closedModel, map[string]string{"model": "opus-5-5"}),
			overrides: map[string]string{"effort": "high"},
			want:      "claude-opus-5-5",
		},
		{
			name:      "-m alias flag shape",
			rp:        launchModelTestProvider(closedModel, nil),
			overrides: map[string]string{"model": "short-m"},
			want:      "claude-haiku-5",
		},
		{
			name:      "--model= assignment shape",
			rp:        launchModelTestProvider(closedModel, nil),
			overrides: map[string]string{"model": "assign"},
			want:      "claude-opus-5-5-assign",
		},
		{
			name:      "codex -c assignment falls back to the last token",
			rp:        launchModelTestProvider(closedModel, nil),
			overrides: map[string]string{"model": "codex-style"},
			want:      "model=gpt-5.5",
		},
		{
			name:      "open option renders an undeclared value through its template",
			rp:        launchModelTestProvider(openModel, nil),
			overrides: map[string]string{"model": "claude-opus-6"},
			want:      "claude-opus-6",
		},
		{
			name:      "closed option refuses an undeclared value",
			rp:        launchModelTestProvider(closedModel, nil),
			overrides: map[string]string{"model": "claude-opus-6"},
			want:      "",
		},
		{
			name:      "explicit leave-unset choice pins nothing",
			rp:        launchModelTestProvider(closedModel, nil),
			overrides: map[string]string{"model": ""},
			want:      "claude-sonnet-5",
		},
		{
			name: "provider without a model option",
			rp: &config.ResolvedProvider{Name: "bare", OptionsSchema: []config.ProviderOption{
				{Key: "effort", Type: "select", Choices: []config.OptionChoice{{Value: "high", FlagArgs: []string{"--effort", "high"}}}},
			}, EffectiveDefaults: map[string]string{"model": "opus-5-5"}},
			want: "",
		},
		{
			name: "nil provider",
			rp:   nil,
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolvedLaunchModel(tc.rp, tc.overrides); got != tc.want {
				t.Fatalf("resolvedLaunchModel = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestModelFlagValue(t *testing.T) {
	tests := []struct {
		args     []string
		fallback string
		want     string
	}{
		{[]string{"--model", "claude-opus-5-5"}, "x", "claude-opus-5-5"},
		{[]string{"-m", "grok-4.7"}, "x", "grok-4.7"},
		{[]string{"--model=gpt-5.5"}, "x", "gpt-5.5"},
		{[]string{"--effort", "high", "--model", " codex-mini "}, "x", "codex-mini"},
		{[]string{"-c", "model=gpt-5.5"}, "x", "model=gpt-5.5"},
		{[]string{"--model"}, "fallback", "--model"},
		{nil, "fallback", "fallback"},
		{nil, "", ""},
	}
	for _, tc := range tests {
		if got := modelFlagValue(tc.args, tc.fallback); got != tc.want {
			t.Errorf("modelFlagValue(%q, %q) = %q, want %q", tc.args, tc.fallback, got, tc.want)
		}
	}
}

// TestLaunchProvenanceReachesStartCommit drives the controller's own start
// path for an agent whose prompt template declares a frontmatter version and
// whose session pins a model through template_overrides: resolveTemplate
// renders the template and records its version and SHA, buildPreparedStart
// resolves the launch model from the builtin claude provider's schema, and
// startCommitPatch (shared by the legacy and v2 reconciler commits) carries
// all three into the session metadata that worker.operation events read.
func TestLaunchProvenanceReachesStartCommit(t *testing.T) {
	cityPath := t.TempDir()
	const templatePath = "prompts/worker.template.md"
	const body = "You are {{ .AgentName }}.\n"
	if err := os.MkdirAll(filepath.Join(cityPath, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, templatePath), []byte("---\nversion: v7\n---\n"+body), 0o644); err != nil {
		t.Fatal(err)
	}
	params := &agentBuildParams{
		fs:         fsys.OSFS{},
		cityName:   "bright-lights",
		cityPath:   cityPath,
		workspace:  &config.Workspace{Name: "bright-lights", Provider: "claude"},
		providers:  config.BuiltinProviders(),
		lookPath:   func(string) (string, error) { return "/usr/bin/claude", nil },
		beaconTime: testBeaconTime,
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}
	agent := &config.Agent{Name: "worker", PromptTemplate: templatePath, Provider: "claude"}
	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	if tp.PromptVersion != "v7" {
		t.Errorf("TemplateParams.PromptVersion = %q, want v7", tp.PromptVersion)
	}
	wantSHA := promptmeta.SHA("You are worker.\n")
	if tp.PromptSHA != wantSHA {
		t.Errorf("TemplateParams.PromptSHA = %q, want promptmeta.SHA of the rendered template %q", tp.PromptSHA, wantSHA)
	}
	if tp.PromptSHA == sessionpkg.PromptHash(tp.Prompt) {
		t.Errorf("prompt_sha must hash the rendered template, not the delivered prompt with its beacon")
	}

	for _, tc := range []struct {
		name      string
		overrides string
		wantModel string
	}{
		// "sonnet" is a choice key; the stamp is the id its flag passes.
		{name: "template_overrides model choice", overrides: `{"model":"sonnet"}`, wantModel: "claude-sonnet-5"},
		// The open model option honors an id outside the curated list.
		{name: "template_overrides id outside the curated list", overrides: `{"model":"claude-opus-9"}`, wantModel: "claude-opus-9"},
		// No pin and no provider default: the CLI picks its own model, so no
		// launch_model is stamped.
		{name: "no model pinned", wantModel: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			meta := map[string]string{"session_name": "worker", "template": "worker", "state": "asleep"}
			if tc.overrides != "" {
				meta["template_overrides"] = tc.overrides
			}
			bead, err := store.Create(beads.Bead{Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: meta})
			if err != nil {
				t.Fatalf("Create(session): %v", err)
			}
			prepared, info, err := buildPreparedStart(startCandidate{info: sessiontest.SeedBead(t, bead), tp: tp}, &config.City{}, store)
			if err != nil {
				t.Fatalf("buildPreparedStart: %v", err)
			}
			patch := startCommitPatch(prepared, info, time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC))
			t.Logf("start commit provenance: launch_model=%q prompt_version=%q prompt_sha=%q",
				patch[sessionpkg.LaunchModelMetadataKey], patch[sessionpkg.PromptVersionMetadataKey], patch[sessionpkg.PromptSHAMetadataKey])
			got, ok := patch[sessionpkg.LaunchModelMetadataKey]
			if tc.wantModel == "" {
				if ok {
					t.Errorf("launch_model = %q stamped with no model pinned", got)
				}
			} else if got != tc.wantModel {
				t.Errorf("launch_model = %q, want %q", got, tc.wantModel)
			}
			if got := patch[sessionpkg.PromptVersionMetadataKey]; got != "v7" {
				t.Errorf("prompt_version = %q, want v7", got)
			}
			if got := patch[sessionpkg.PromptSHAMetadataKey]; got != wantSHA {
				t.Errorf("prompt_sha = %q, want %q", got, wantSHA)
			}
		})
	}
}
