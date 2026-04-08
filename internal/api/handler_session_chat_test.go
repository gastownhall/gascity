package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/shellquote"
)

func TestShellJoinArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"empty slice", nil, ""},
		{"single arg no metachar", []string{"--model"}, "--model"},
		{"two clean args", []string{"--model", "opus"}, "--model opus"},
		{"arg with space", []string{"hello world"}, "'hello world'"},
		{"arg with single quote", []string{"it's"}, "'it'\\''s'"},
		{"empty string arg", []string{""}, "''"},
		{"mixed clean and dirty", []string{"--flag", "value with space", "--other"}, "--flag 'value with space' --other"},
		{"arg with special chars", []string{"$(whoami)"}, "'$(whoami)'"},
		{"arg with semicolon", []string{"foo;bar"}, "'foo;bar'"},
		{"multiple special", []string{"a b", "c'd", "e|f"}, "'a b' 'c'\\''d' 'e|f'"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shellquote.Join(tt.args)
			if got != tt.want {
				t.Errorf("shellquote.Join(%q) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestBuildSessionResumeUsesResolvedProviderCommand(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "mayor", Provider: "wrapped"},
		},
		Providers: map[string]config.ProviderSpec{
			"wrapped": {
				DisplayName:       "Wrapped Gemini",
				Command:           "aimux",
				Args:              []string{"run", "gemini", "--", "--approval-mode", "yolo"},
				ReadyPromptPrefix: "> ",
				Env: map[string]string{
					"GC_HOME": "/tmp/gc-accept-home",
				},
			},
		},
	}

	srv := New(fs)
	info := session.Info{
		ID:       "gc-1",
		Template: "mayor",
		Command:  "gemini --approval-mode yolo",
		Provider: "wrapped",
		WorkDir:  "/tmp/workdir",
	}

	cmd, hints := srv.buildSessionResume(info)
	if got, want := cmd, "aimux run gemini -- --approval-mode yolo"; got != want {
		t.Fatalf("resume command = %q, want %q", got, want)
	}
	if got, want := hints.WorkDir, "/tmp/workdir"; got != want {
		t.Fatalf("hints.WorkDir = %q, want %q", got, want)
	}
	if got, want := hints.ReadyPromptPrefix, "> "; got != want {
		t.Fatalf("hints.ReadyPromptPrefix = %q, want %q", got, want)
	}
	if got, want := hints.Env["GC_HOME"], "/tmp/gc-accept-home"; got != want {
		t.Fatalf("hints.Env[GC_HOME] = %q, want %q", got, want)
	}
}
