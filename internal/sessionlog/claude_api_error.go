package sessionlog

import "strings"

// attachClaudeAPIErrorEvent marks a Claude-generated provider failure with a
// provider_error system event. Only the CLI's explicit isApiErrorMessage flag
// counts: assistant refusals and text quoting an API error stay ordinary
// replies. The entry keeps its assistant type, so the conversation DAG and
// activity inference read the transcript as Claude wrote it; consumers that
// project history (the worker) present the event instead of a reply.
func attachClaudeAPIErrorEvent(entry *Entry) {
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
	entry.SystemEvent = &SystemEvent{Kind: "error", Category: "provider_error", Code: jsonStringValue(entry.APIError), Message: text}
}
