package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestReplaceSchemaFlags_ClaudePermissionOverride(t *testing.T) {
	providers := config.BuiltinProviders()
	claude := providers["claude"]

	got := replaceSchemaFlags(
		"claude --dangerously-skip-permissions",
		claude.OptionsSchema,
		[]string{"--permission-mode", "full-auto"},
	)
	want := "claude --permission-mode full-auto"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestReplaceSchemaFlags_ClaudeModelOverride(t *testing.T) {
	providers := config.BuiltinProviders()
	claude := providers["claude"]

	got := replaceSchemaFlags(
		"claude --dangerously-skip-permissions",
		claude.OptionsSchema,
		[]string{"--dangerously-skip-permissions", "--model", "claude-opus-4-6"},
	)
	want := "claude --dangerously-skip-permissions --model claude-opus-4-6"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestReplaceSchemaFlags_CodexPermissionOverride(t *testing.T) {
	providers := config.BuiltinProviders()
	codex := providers["codex"]

	got := replaceSchemaFlags(
		"codex --dangerously-bypass-approvals-and-sandbox",
		codex.OptionsSchema,
		[]string{"--full-auto"},
	)
	want := "codex --full-auto"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestReplaceSchemaFlags_ClaudeResumePreserved(t *testing.T) {
	providers := config.BuiltinProviders()
	claude := providers["claude"]

	got := replaceSchemaFlags(
		"claude --dangerously-skip-permissions --resume abc",
		claude.OptionsSchema,
		[]string{"--permission-mode", "full-auto"},
	)
	want := "claude --resume abc --permission-mode full-auto"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestReplaceSchemaFlags_NoOverrides(t *testing.T) {
	providers := config.BuiltinProviders()
	claude := providers["claude"]

	got := replaceSchemaFlags(
		"claude --dangerously-skip-permissions",
		claude.OptionsSchema,
		nil,
	)
	want := "claude"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripFlags_EmptyFlags(t *testing.T) {
	got := stripFlags("claude --foo bar", nil)
	want := "claude --foo bar"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripFlags_MultiTokenFlag(t *testing.T) {
	flags := [][]string{
		{"--ask-for-approval", "untrusted", "--sandbox", "read-only"},
	}
	got := stripFlags("codex --ask-for-approval untrusted --sandbox read-only", flags)
	want := "codex"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
