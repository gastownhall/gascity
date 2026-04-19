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
