package api

import (
	"os"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestTruncateTitle(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    string
		maxLen  int // 0 means just check it's non-empty and ≤80 runes
	}{
		{
			name:    "empty",
			message: "",
			want:    "",
		},
		{
			name:    "short message",
			message: "fix the login bug",
			want:    "fix the login bug",
		},
		{
			name:    "exactly 80 chars",
			message: strings.Repeat("a", 80),
			want:    strings.Repeat("a", 80),
		},
		{
			name:    "long message truncated at word boundary",
			message: "Please help me understand why the authentication middleware is rejecting valid tokens when the user has multiple active sessions",
			maxLen:  84, // 80 + "..."
		},
		{
			name:    "newlines collapsed",
			message: "first line\nsecond line\nthird line",
			want:    "first line second line third line",
		},
		{
			name:    "whitespace only",
			message: "   \n\t  ",
			want:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateTitle(tt.message)
			if tt.want != "" {
				if got != tt.want {
					t.Errorf("truncateTitle() = %q, want %q", got, tt.want)
				}
			}
			if tt.maxLen > 0 {
				if len(got) > tt.maxLen {
					t.Errorf("truncateTitle() len = %d, want ≤ %d: %q", len(got), tt.maxLen, got)
				}
				if !strings.HasSuffix(got, "...") {
					t.Errorf("truncateTitle() should end with '...' for truncated output: %q", got)
				}
			}
		})
	}
}

func TestGenerateTitle_NoProvider(t *testing.T) {
	got := generateTitle(nil, "hello world", "")
	if got != "hello world" {
		t.Errorf("generateTitle(nil) = %q, want truncated message", got)
	}
}

func TestGenerateTitle_NoPrintArgs(t *testing.T) {
	provider := &config.ResolvedProvider{
		Command: "claude",
		// PrintArgs intentionally empty
	}
	got := generateTitle(provider, "hello world", "")
	if got != "hello world" {
		t.Errorf("generateTitle(no PrintArgs) = %q, want truncated message", got)
	}
}

func TestGenerateTitle_SubprocessFailure(t *testing.T) {
	provider := &config.ResolvedProvider{
		Command:   "false", // always exits 1
		PrintArgs: []string{},
	}
	// PrintArgs is empty (len 0), so should fall back to truncation
	got := generateTitle(provider, "hello world", "")
	if got != "hello world" {
		t.Errorf("generateTitle(failing subprocess) = %q, want truncated message", got)
	}
}

func TestMaybeGenerateTitleAsync_ExplicitTitle(t *testing.T) {
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{Title: "template-name"})
	provider := &config.ResolvedProvider{
		Command:   "echo",
		PrintArgs: []string{},
	}

	// When userTitle is set, no generation should happen.
	done := MaybeGenerateTitleAsync(store, b.ID, "my explicit title", "hello world", provider, "", func(string, ...any) {})
	<-done

	got, _ := store.Get(b.ID)
	if got.Title != "template-name" {
		t.Errorf("title changed to %q, want unchanged %q", got.Title, "template-name")
	}
}

func TestMaybeGenerateTitleAsync_EmptyMessage(t *testing.T) {
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{Title: "template-name"})

	done := MaybeGenerateTitleAsync(store, b.ID, "", "", nil, "", func(string, ...any) {})
	<-done

	got, _ := store.Get(b.ID)
	if got.Title != "template-name" {
		t.Errorf("title changed to %q, want unchanged %q", got.Title, "template-name")
	}
}

func TestMaybeGenerateTitleAsync_MockProvider(t *testing.T) {
	// Create a mock provider script that outputs a title.
	dir := t.TempDir()
	script := dir + "/mock-title-gen"
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"Generated Title\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{Title: "template-name"})
	if err != nil {
		t.Fatal(err)
	}

	provider := &config.ResolvedProvider{
		Command:   script,
		PrintArgs: []string{"--print"},
	}

	done := MaybeGenerateTitleAsync(store, b.ID, "", "fix the login redirect loop", provider, "", func(string, ...any) {})
	<-done

	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Generated Title" {
		t.Errorf("title = %q, want %q (model-generated title from mock provider)", got.Title, "Generated Title")
	}
}

func TestTitleModelFlagArgs(t *testing.T) {
	rp := &config.ResolvedProvider{
		TitleModel: "haiku",
		OptionsSchema: []config.ProviderOption{
			{
				Key: "model",
				Choices: []config.OptionChoice{
					{Value: "opus", FlagArgs: []string{"--model", "claude-opus-4-7"}},
					{Value: "haiku", FlagArgs: []string{"--model", "claude-haiku-4-5-20251001"}},
				},
			},
		},
	}

	args := rp.TitleModelFlagArgs()
	if len(args) != 2 || args[0] != "--model" || args[1] != "claude-haiku-4-5-20251001" {
		t.Errorf("TitleModelFlagArgs() = %v, want [--model claude-haiku-4-5-20251001]", args)
	}
}

func TestTitleModelFlagArgs_NoMatch(t *testing.T) {
	rp := &config.ResolvedProvider{
		TitleModel: "nonexistent",
		OptionsSchema: []config.ProviderOption{
			{
				Key:     "model",
				Choices: []config.OptionChoice{{Value: "haiku", FlagArgs: []string{"--model", "haiku"}}},
			},
		},
	}

	if args := rp.TitleModelFlagArgs(); args != nil {
		t.Errorf("TitleModelFlagArgs() = %v, want nil", args)
	}
}

func TestTitleModelFlagArgs_Empty(t *testing.T) {
	rp := &config.ResolvedProvider{}
	if args := rp.TitleModelFlagArgs(); args != nil {
		t.Errorf("TitleModelFlagArgs() = %v, want nil", args)
	}
}
