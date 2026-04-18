package main

import (
	"encoding/json"
	"io"
	"strings"
)

const hookOutputFormatGemini = "gemini"

func writeProviderHookContext(stdout io.Writer, format, content string) error {
	if content == "" {
		return nil
	}
	if strings.EqualFold(strings.TrimSpace(format), hookOutputFormatGemini) {
		return json.NewEncoder(stdout).Encode(geminiHookAdditionalContext(content))
	}
	_, err := io.WriteString(stdout, content)
	return err
}

func geminiHookAdditionalContext(content string) map[string]any {
	return map[string]any{
		"hookSpecificOutput": map[string]any{
			"additionalContext": strings.TrimRight(content, "\n"),
		},
	}
}
