package sessionlog

import (
	"encoding/json"
	"strings"
)

// claudeSystemEvent translates native error metadata without altering the
// conversation DAG or raw records. A retryable terminal failure is still an
// error: active retries use separate system/api_error records in Claude.
func claudeSystemEvent(entry *Entry) *SystemEvent {
	if entry.Type == "system" && entry.Subtype == "api_error" {
		return &SystemEvent{Kind: "retry", Category: "provider_retry", Message: "Provider retry in progress"}
	}
	if entry.Type != "assistant" || !entry.IsAPIErrorMessage {
		return nil
	}
	var metadata struct {
		Error json.RawMessage `json:"error"`
	}
	var code string
	if json.Unmarshal(entry.Raw, &metadata) == nil {
		_ = json.Unmarshal(metadata.Error, &code)
	}
	switch code {
	case "authentication_failed", "invalid_request", "rate_limit", "server_error", "overloaded", "billing_error", "model_not_found", "max_output_tokens", "account_on_hold", "oauth_org_not_allowed", "unknown":
	default:
		code = ""
	}
	text := entry.TextContent()
	if text == "" {
		var parts []string
		for _, block := range entry.ContentBlocks() {
			if block.Type == "text" {
				parts = append(parts, block.Text)
			}
		}
		text = strings.Join(parts, "\n")
	}
	if strings.TrimSpace(text) == "" {
		text = "Claude API request failed"
	}
	return &SystemEvent{Kind: "error", Category: "provider_error", Code: code, Message: text}
}
