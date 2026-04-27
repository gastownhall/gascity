package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestWriteProviderHookContextGemini(t *testing.T) {
	var out bytes.Buffer
	err := writeProviderHookContext(&out, "gemini", "<system-reminder>\nhello\n</system-reminder>\n")
	if err != nil {
		t.Fatalf("writeProviderHookContext: %v", err)
	}

	var payload struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal output: %v\n%s", err, out.String())
	}
	if got, want := payload.HookSpecificOutput.AdditionalContext, "<system-reminder>\nhello\n</system-reminder>"; got != want {
		t.Fatalf("additionalContext = %q, want %q", got, want)
	}
}

func TestWriteProviderHookContextCodex(t *testing.T) {
	var out bytes.Buffer
	err := writeProviderHookContextForEvent(&out, "codex", "Stop", "<system-reminder>\nhello\n</system-reminder>\n")
	if err != nil {
		t.Fatalf("writeProviderHookContextForEvent: %v", err)
	}

	var payload struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal output: %v\n%s", err, out.String())
	}
	if got, want := payload.Decision, "block"; got != want {
		t.Fatalf("decision = %q, want %q", got, want)
	}
	if got, want := payload.Reason, "<system-reminder>\nhello\n</system-reminder>"; got != want {
		t.Fatalf("reason = %q, want %q", got, want)
	}
}

func TestWriteProviderHookContextCodexAdditionalContext(t *testing.T) {
	var out bytes.Buffer
	err := writeProviderHookContextForEvent(&out, "codex", "UserPromptSubmit", "<system-reminder>\nhello\n</system-reminder>\n")
	if err != nil {
		t.Fatalf("writeProviderHookContextForEvent: %v", err)
	}

	var payload struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal output: %v\n%s", err, out.String())
	}
	if got, want := payload.HookSpecificOutput.HookEventName, "UserPromptSubmit"; got != want {
		t.Fatalf("hookEventName = %q, want %q", got, want)
	}
	if got, want := payload.HookSpecificOutput.AdditionalContext, "<system-reminder>\nhello\n</system-reminder>"; got != want {
		t.Fatalf("additionalContext = %q, want %q", got, want)
	}
}

func TestWriteProviderHookContextPlain(t *testing.T) {
	var out bytes.Buffer
	err := writeProviderHookContext(&out, "", "<system-reminder>\nhello\n</system-reminder>\n")
	if err != nil {
		t.Fatalf("writeProviderHookContext: %v", err)
	}
	if got, want := out.String(), "<system-reminder>\nhello\n</system-reminder>\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}
