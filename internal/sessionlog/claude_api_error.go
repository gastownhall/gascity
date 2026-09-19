package sessionlog

import "strings"

// normalizeClaudeAPIError uses the CLI's explicit failure marker. Assistant
// refusals and text quoting an API error remain ordinary assistant messages.
func normalizeClaudeAPIError(entry *Entry) {
	if entry.Type != "assistant" || !entry.IsAPIErrorMessage {
		return
	}
	text := entry.TextContent()
	if text == "" {
		var parts []string
		for _, block := range entry.ContentBlocks() {
			if block.Type == "text" && block.Text != "" {
				parts = append(parts, block.Text)
			}
		}
		text = strings.Join(parts, "\n")
	}
	if strings.TrimSpace(text) == "" {
		text = "Claude provider failed"
	}
	entry.Type = "system"
	entry.SystemEvent = &SystemEvent{Kind: "error", Category: "provider_error", Code: jsonStringValue(entry.APIError), Message: text}
}
