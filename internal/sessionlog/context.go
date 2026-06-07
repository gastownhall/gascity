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
	"gpt-5":  258_000,
	"codex":  258_000,
	"gpt-4":  128_000,
	"gpt-4o": 128_000,
}

// millionTokenWindow is the context window for 1M-token model variants.
const millionTokenWindow = 1_000_000

// claudeFamilies are the Claude model families whose context window scales to
// 1M when the model ID carries the "[1m]" suffix (e.g. "claude-opus-4-8[1m]").
// Without the suffix they use the 200K default in modelFamilyWindows.
var claudeFamilies = map[string]bool{"opus": true, "sonnet": true, "haiku": true}

// ModelContextWindow returns the context window size for a model ID.
// It parses the model ID to extract the family name and looks it up.
// Claude families carrying the "[1m]" suffix resolve to the 1M window so
// context utilization does not saturate against the 200K default.
// Returns 0 if the model family is unknown.
func ModelContextWindow(model string) int {
	lower := strings.ToLower(model)
	// Try longer matches first to avoid "gpt-4" matching before "gpt-4o".
	for _, family := range []string{"gpt-4o", "gpt-5", "gpt-4", "opus", "sonnet", "haiku", "gemini", "codex"} {
		if strings.Contains(lower, family) {
			if claudeFamilies[family] && strings.Contains(lower, "[1m]") {
				return millionTokenWindow
			}
			return modelFamilyWindows[family]
		}
	}
	return 0
}
