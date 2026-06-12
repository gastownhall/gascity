package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// Characterization tests for the template_overrides parse seams folded onto
// session.ParseTemplateOverrides (ga-9s1a4a). Each case pins the observable
// result for the same raw metadata value, including error inputs, so the fold
// is provably behavior-preserving.

func TestApplyTemplateOverridesToConfig_ParseSeam(t *testing.T) {
	const baseCommand = "claude"
	tests := []struct {
		name        string
		metadata    map[string]string
		provider    *config.ResolvedProvider
		wantCommand string
	}{
		{name: "nil metadata", metadata: nil, provider: optionSchemaProvider(), wantCommand: baseCommand},
		{name: "empty value", metadata: map[string]string{"template_overrides": ""}, provider: optionSchemaProvider(), wantCommand: baseCommand},
		{name: "whitespace only", metadata: map[string]string{"template_overrides": "   "}, provider: optionSchemaProvider(), wantCommand: baseCommand},
		{name: "invalid json", metadata: map[string]string{"template_overrides": "{not json"}, provider: optionSchemaProvider(), wantCommand: baseCommand},
		{name: "empty object", metadata: map[string]string{"template_overrides": "{}"}, provider: optionSchemaProvider(), wantCommand: baseCommand},
		{name: "json null", metadata: map[string]string{"template_overrides": "null"}, provider: optionSchemaProvider(), wantCommand: baseCommand},
		{name: "nil provider", metadata: map[string]string{"template_overrides": `{"model":"sonnet"}`}, provider: nil, wantCommand: baseCommand},
		{
			name:     "initial_message only resolves no schema flags",
			metadata: map[string]string{"template_overrides": `{"initial_message":"hi"}`},
			provider: optionSchemaProvider(),
			// initial_message is excluded from option resolution and there are
			// no effective defaults, so the command stays untouched.
			wantCommand: baseCommand,
		},
		{
			name:        "unknown option key is rejected whole",
			metadata:    map[string]string{"template_overrides": `{"bogus":"x"}`},
			provider:    optionSchemaProvider(),
			wantCommand: baseCommand,
		},
		{
			name:        "valid override replaces schema flags",
			metadata:    map[string]string{"template_overrides": `{"model":"sonnet"}`},
			provider:    optionSchemaProvider(),
			wantCommand: "claude --model claude-sonnet-4-6",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agentCfg := runtime.Config{Command: baseCommand}
			session := beads.Bead{Metadata: tt.metadata}
			tp := TemplateParams{Command: baseCommand, ResolvedProvider: tt.provider}
			applyTemplateOverridesToConfig(&agentCfg, session, tp)
			if agentCfg.Command != tt.wantCommand {
				t.Fatalf("Command = %q, want %q", agentCfg.Command, tt.wantCommand)
			}
		})
	}
}

func TestApplyTemplateOverridesToConfig_DefaultsPreservedAlongsideOverride(t *testing.T) {
	provider := optionSchemaProvider()
	provider.EffectiveDefaults = map[string]string{"effort": "low"}
	agentCfg := runtime.Config{Command: "claude"}
	session := beads.Bead{Metadata: map[string]string{"template_overrides": `{"model":"sonnet"}`}}
	tp := TemplateParams{Command: "claude", ResolvedProvider: provider}
	applyTemplateOverridesToConfig(&agentCfg, session, tp)
	want := "claude --model claude-sonnet-4-6 --effort low"
	if agentCfg.Command != want {
		t.Fatalf("Command = %q, want %q", agentCfg.Command, want)
	}
}

func TestParseSessionTemplateOverridesForLaunch_ParseSeam(t *testing.T) {
	tests := []struct {
		name     string
		session  *beads.Bead
		want     map[string]string
		wantNone bool
	}{
		{name: "nil session", session: nil, wantNone: true},
		{name: "nil metadata", session: &beads.Bead{ID: "gc-1"}, wantNone: true},
		{name: "empty value", session: &beads.Bead{ID: "gc-1", Metadata: map[string]string{"template_overrides": ""}}, wantNone: true},
		{name: "whitespace only", session: &beads.Bead{ID: "gc-1", Metadata: map[string]string{"template_overrides": " \t"}}, wantNone: true},
		{name: "json null", session: &beads.Bead{ID: "gc-1", Metadata: map[string]string{"template_overrides": "null"}}, wantNone: true},
		// Empty objects and parse failures are equivalent to "no overrides"
		// for every caller (len checks, range loops, key lookups), so the
		// pin is len==0, not nil-ness.
		{name: "empty object", session: &beads.Bead{ID: "gc-1", Metadata: map[string]string{"template_overrides": "{}"}}, wantNone: true},
		{name: "invalid json", session: &beads.Bead{ID: "gc-1", Metadata: map[string]string{"template_overrides": "{not json"}}, wantNone: true},
		{name: "non-string value", session: &beads.Bead{ID: "gc-1", Metadata: map[string]string{"template_overrides": `{"model":1}`}}, wantNone: true},
		{
			name:    "valid object retains initial_message",
			session: &beads.Bead{ID: "gc-1", Metadata: map[string]string{"template_overrides": `{"model":"sonnet","initial_message":"hi"}`}},
			want:    map[string]string{"model": "sonnet", "initial_message": "hi"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseSessionTemplateOverridesForLaunch(tt.session)
			if tt.wantNone {
				if len(got) != 0 {
					t.Fatalf("parseSessionTemplateOverridesForLaunch() = %v, want no overrides", got)
				}
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseSessionTemplateOverridesForLaunch() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestDispatchOptionMetadataKeyPinned locks the per-dispatch option metadata
// key shape to the declared beadmeta prefix: work and session beads carry
// provider option choices as opt_<OptionsSchema key>.
func TestDispatchOptionMetadataKeyPinned(t *testing.T) {
	if got := dispatchOptionMetadataKey("model"); got != "opt_model" {
		t.Fatalf("dispatchOptionMetadataKey(model) = %q, want %q", got, "opt_model")
	}
	if got := dispatchOptionMetadataKey("effort"); got != "opt_effort" {
		t.Fatalf("dispatchOptionMetadataKey(effort) = %q, want %q", got, "opt_effort")
	}
}

// TestBuildPreparedStart_InitialMessageParseSeam pins the launch-path
// initial_message behavior across the parse fold: a malformed payload is
// ignored without failing the start, and a valid payload applies both the
// schema override and the first-start prompt-suffix message from one parse.
func TestBuildPreparedStart_InitialMessageParseSeam(t *testing.T) {
	newCandidate := func(t *testing.T, store beads.Store, rawOverrides string) startCandidate {
		t.Helper()
		session, err := store.Create(beads.Bead{
			Title:  "worker",
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel},
			Metadata: map[string]string{
				"session_name":       "worker",
				"template":           "worker",
				"state":              "creating",
				"generation":         "1",
				"instance_token":     "tok-worker",
				"template_overrides": rawOverrides,
			},
		})
		if err != nil {
			t.Fatalf("Create(session): %v", err)
		}
		return startCandidate{
			session: &session,
			tp: TemplateParams{
				TemplateName:     "worker",
				SessionName:      "worker",
				Command:          "claude",
				Prompt:           "startup prompt",
				ResolvedProvider: optionSchemaProvider(),
			},
		}
	}

	t.Run("invalid json ignored without failing start", func(t *testing.T) {
		store := beads.NewMemStore()
		prepared, err := buildPreparedStart(newCandidate(t, store, "{not json"), &config.City{}, store)
		if err != nil {
			t.Fatalf("buildPreparedStart: %v", err)
		}
		if prepared.cfg.PromptSuffix != shellquote.Quote("startup prompt") {
			t.Fatalf("PromptSuffix = %q, want base prompt only", prepared.cfg.PromptSuffix)
		}
		if strings.Contains(prepared.cfg.Command, "--model") {
			t.Fatalf("Command = %q, want no schema flags from malformed overrides", prepared.cfg.Command)
		}
	})

	t.Run("valid overrides apply schema flag and initial message", func(t *testing.T) {
		store := beads.NewMemStore()
		prepared, err := buildPreparedStart(newCandidate(t, store, `{"model":"sonnet","initial_message":"hello from the user"}`), &config.City{}, store)
		if err != nil {
			t.Fatalf("buildPreparedStart: %v", err)
		}
		if !strings.Contains(prepared.cfg.Command, "--model claude-sonnet-4-6") {
			t.Fatalf("Command = %q, want --model claude-sonnet-4-6", prepared.cfg.Command)
		}
		want := shellquote.Quote("startup prompt\n\n---\n\nUser message:\nhello from the user")
		if prepared.cfg.PromptSuffix != want {
			t.Fatalf("PromptSuffix = %q, want %q", prepared.cfg.PromptSuffix, want)
		}
	})
}
