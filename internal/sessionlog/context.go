// Package sessionlog reads Claude Code JSONL session files for
// lightweight metadata extraction (model, context usage).
package sessionlog

import "strings"

// modelFamilyWindows maps model family keywords to their context window sizes.
var modelFamilyWindows = map[string]int{
	"opus":   200_000,
	"sonnet": 200_000,
	"haiku":  200_000,
	"gemini": 1_000_000,
	"gpt-5":  400_000,
	"codex":  400_000,
	"gpt-4":  128_000,
	"gpt-4o": 128_000,
}

// ModelContextWindow returns the context window size for a model ID.
// It parses the model ID to extract the family name and looks it up.
// Returns 0 if the model family is unknown.
func ModelContextWindow(model string) int {
	lower := strings.ToLower(model)
	// Try longer matches first to avoid "gpt-4" matching before "gpt-4o".
	for _, family := range []string{"gpt-4o", "gpt-5", "gpt-4", "opus", "sonnet", "haiku", "gemini", "codex"} {
		if strings.Contains(lower, family) {
			return modelFamilyWindows[family]
		}
	}
	return 0
}
