package worker

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// InFlightSubagent identifies a background subagent that a session kill would destroy.
type InFlightSubagent struct {
	AgentID     string
	Description string
	StartedAt   time.Time
}

type subagentGuardTranscript interface {
	AgentMappings(context.Context) ([]AgentMapping, error)
	Transcript(context.Context, TranscriptRequest) (*TranscriptResult, error)
}

// InFlightBackgroundSubagents returns background subagents without a terminal
// task notification in the parent transcript. Transcript parse errors are
// returned so callers can deliberately fail open before a destructive action.
func InFlightBackgroundSubagents(ctx context.Context, transcript subagentGuardTranscript) ([]InFlightSubagent, error) {
	mappings, err := transcript.AgentMappings(ctx)
	if err != nil {
		return nil, err
	}
	if len(mappings) == 0 {
		return nil, nil
	}
	result, err := transcript.Transcript(ctx, TranscriptRequest{Raw: true})
	if err != nil {
		return nil, err
	}
	spawns, terminal := parseSubagentGuardTranscript(result.RawMessages)
	live := make([]InFlightSubagent, 0, len(mappings))
	for _, mapping := range mappings {
		spawn, ok := spawns[strings.TrimSpace(mapping.ParentToolUseID)]
		if !ok || terminal[strings.TrimSpace(mapping.AgentID)] {
			continue
		}
		spawn.AgentID = strings.TrimSpace(mapping.AgentID)
		live = append(live, spawn)
	}
	return live, nil
}

func parseSubagentGuardTranscript(messages []json.RawMessage) (map[string]InFlightSubagent, map[string]bool) {
	spawns := make(map[string]InFlightSubagent)
	terminal := make(map[string]bool)
	for _, raw := range messages {
		var entry struct {
			Type      string    `json:"type"`
			Operation string    `json:"operation"`
			Timestamp time.Time `json:"timestamp"`
			Message   struct {
				Content []rawSubagentToolUse `json:"content"`
			} `json:"message"`
			Content    json.RawMessage `json:"content"`
			Attachment struct {
				Prompt string `json:"prompt"`
			} `json:"attachment"`
		}
		if json.Unmarshal(raw, &entry) != nil {
			continue
		}
		for _, block := range entry.Message.Content {
			if block.Type != "tool_use" || block.Name != "Agent" {
				continue
			}
			background, description := backgroundTaskInput(block.Input)
			if background && strings.TrimSpace(block.ID) != "" {
				spawns[block.ID] = InFlightSubagent{Description: description, StartedAt: entry.Timestamp}
			}
		}
		if entry.Type == "queue-operation" && entry.Operation == "enqueue" {
			if taskID, status := taskNotification(entry.Content); terminalTaskStatus(status) {
				terminal[taskID] = true
			}
		}
		if entry.Type == "attachment" {
			if taskID, status := taskNotificationText(entry.Attachment.Prompt); terminalTaskStatus(status) {
				terminal[taskID] = true
			}
		}
	}
	return spawns, terminal
}

type rawSubagentToolUse struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

func backgroundTaskInput(raw json.RawMessage) (bool, string) {
	var input struct {
		RunInBackground *bool  `json:"run_in_background"`
		Description     string `json:"description"`
		Prompt          string `json:"prompt"`
	}
	if json.Unmarshal(raw, &input) != nil || (input.RunInBackground != nil && !*input.RunInBackground) {
		return false, ""
	}
	return true, firstNonEmptyString(strings.TrimSpace(input.Description), strings.TrimSpace(input.Prompt))
}

func taskNotification(raw json.RawMessage) (string, string) {
	var content string
	if json.Unmarshal(raw, &content) != nil {
		return "", ""
	}
	return taskNotificationText(content)
}

func taskNotificationText(content string) (string, string) {
	return xmlTagValue(content, "task-id"), xmlTagValue(content, "status")
}

func terminalTaskStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "failed", "killed", "stopped":
		return true
	default:
		return false
	}
}
