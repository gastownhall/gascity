package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
)

const (
	hookOutputFormatAntigravity = "antigravity"
	hookOutputFormatCodex       = "codex"
	hookOutputFormatGemini      = "gemini"
)

func writeProviderHookContextForEvent(stdout io.Writer, format, eventName, content string) error {
	if content == "" {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case hookOutputFormatAntigravity:
		return json.NewEncoder(stdout).Encode(antigravityHookAdditionalContext(content))
	case hookOutputFormatCodex:
		return json.NewEncoder(stdout).Encode(codexHookOutput(eventName, content))
	case hookOutputFormatGemini:
		return json.NewEncoder(stdout).Encode(geminiHookAdditionalContext(content))
	}
	_, err := io.WriteString(stdout, content)
	return err
}

func antigravityHookAdditionalContext(content string) map[string]any {
	return map[string]any{
		"injectSteps": []map[string]any{
			{"ephemeralMessage": strings.TrimRight(content, "\n")},
		},
	}
}

func codexHookOutput(eventName, content string) map[string]any {
	switch strings.ToLower(strings.TrimSpace(eventName)) {
	case "stop":
		return map[string]any{
			"decision": "block",
			"reason":   strings.TrimRight(content, "\n"),
		}
	case "precompact":
		// Codex has no PreCompactHookSpecificOutputWire: unlike every other
		// hook event, PreCompact deserializes into PreCompactCommandOutputWire,
		// which sets deny_unknown_fields and accepts only continue/stopReason/
		// suppressOutput/systemMessage. The hookSpecificOutput envelope every
		// other codex event accepts is rejected outright here, so PreCompact
		// gets its own flat shape instead (gastownhall/gascity#6349).
		return map[string]any{
			"systemMessage": strings.TrimRight(content, "\n"),
		}
	}
	return codexHookAdditionalContext(eventName, content)
}

func codexHookAdditionalContext(eventName, content string) map[string]any {
	if strings.TrimSpace(eventName) == "" {
		eventName = strings.TrimSpace(os.Getenv("GC_HOOK_EVENT_NAME"))
	}
	if strings.TrimSpace(eventName) == "" {
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
