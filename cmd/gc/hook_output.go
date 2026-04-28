package main

import (
	"encoding/json"
	"io"
	"strings"
)

const (
	hookOutputFormatCodex  = "codex"
	hookOutputFormatGemini = "gemini"
)

func writeProviderHookContext(stdout io.Writer, format, content string) error {
	return writeProviderHookContextForEvent(stdout, format, "", content)
}

func writeProviderHookContextForEvent(stdout io.Writer, format, eventName, content string) error {
	if content == "" {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case hookOutputFormatCodex:
		return json.NewEncoder(stdout).Encode(codexHookOutput(eventName, content))
	case hookOutputFormatGemini:
		return json.NewEncoder(stdout).Encode(geminiHookAdditionalContext(content))
	}
	_, err := io.WriteString(stdout, content)
	return err
}

func codexHookOutput(eventName, content string) map[string]any {
	if strings.EqualFold(strings.TrimSpace(eventName), "Stop") {
		return map[string]any{
			"decision": "block",
			"reason":   strings.TrimRight(content, "\n"),
		}
	}
	return codexHookAdditionalContext(eventName, content)
}

func codexHookAdditionalContext(eventName, content string) map[string]any {
	if eventName == "" {
		eventName = "SessionStart"
	}
	return map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     eventName,
			"additionalContext": strings.TrimRight(content, "\n"),
		},
	}
}

func geminiHookAdditionalContext(content string) map[string]any {
	return map[string]any{
		"hookSpecificOutput": map[string]any{
			"additionalContext": strings.TrimRight(content, "\n"),
		},
	}
}
