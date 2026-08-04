package main

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
)

// TestInitPromptTemplatePath verifies that embedded prompt-template paths
// resolve to their scaffolded agent destination on every OS.
//
// Regression test for a Windows-only bug: the prefix check compared the
// always-slash embedded path (e.g. "prompts/mayor.md") against
// citylayout.PromptsRoot+filepath.Separator, which is "prompts\\" on Windows.
// The prefix never matched, so initPromptTemplatePath returned ("", false) and
// every default-agent scaffold was silently skipped — the mayor named-session
// ended up with no backing prompt template. The "os-native separator path"
// case exercises exactly that path shape and fails on the pre-fix code on
// Windows while passing on all platforms after the fix.
func TestInitPromptTemplatePath(t *testing.T) {
	wantMayor := filepath.Join("agents", "mayor", "prompt.template.md")

	tests := []struct {
		name     string
		input    string
		wantPath string
		wantOK   bool
	}{
		{
			name:     "canonical slash path (embedded-config shape)",
			input:    citylayout.PromptsRoot + "/mayor.md",
			wantPath: wantMayor,
			wantOK:   true,
		},
		{
			name:     "os-native separator path",
			input:    filepath.Join(citylayout.PromptsRoot, "mayor.md"),
			wantPath: wantMayor,
			wantOK:   true,
		},
		{
			name:     "canonical template suffix",
			input:    citylayout.PromptsRoot + "/mayor.template.md",
			wantPath: wantMayor,
			wantOK:   true,
		},
		{
			name:   "outside prompts root",
			input:  "other/mayor.md",
			wantOK: false,
		},
		{
			name:   "prompts root but unsupported suffix",
			input:  citylayout.PromptsRoot + "/notes.txt",
			wantOK: false,
		},
		{
			name:   "empty base after stripping suffix",
			input:  citylayout.PromptsRoot + "/.md",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := initPromptTemplatePath(tt.input)
			if ok != tt.wantOK {
				t.Fatalf("initPromptTemplatePath(%q) ok = %v, want %v", tt.input, ok, tt.wantOK)
			}
			if ok && got != tt.wantPath {
				t.Fatalf("initPromptTemplatePath(%q) = %q, want %q", tt.input, got, tt.wantPath)
			}
		})
	}
}
