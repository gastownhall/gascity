package workertest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	worker "github.com/gastownhall/gascity/internal/worker"
)

// NormalizedMessage is the reduced transcript shape asserted by phase-1 tests.
type NormalizedMessage struct {
	Role string
	Text string
}

// Snapshot is the phase-1 normalized transcript view.
type Snapshot struct {
	SessionID          string
	TranscriptPath     string
	TranscriptPathHint string
	History            *worker.HistorySnapshot
	Messages           []NormalizedMessage
}

// DiscoverTranscript resolves the provider-native transcript path for a profile fixture root.
func DiscoverTranscript(profile Profile, fixtureRoot string) (string, error) {
	adapter := worker.SessionLogAdapter{SearchPaths: []string{fixtureRoot}}
	path := adapter.DiscoverTranscript(profile.Provider, profile.WorkDir, "")
	if path == "" {
		return "", fmt.Errorf("no transcript discovered for %s in %s", profile.ID, fixtureRoot)
	}
	return path, nil
}

// LoadSnapshot reads and normalizes a profile transcript fixture.
func LoadSnapshot(profile Profile, fixtureRoot string) (*Snapshot, error) {
	path, err := DiscoverTranscript(profile, fixtureRoot)
	if err != nil {
		return nil, err
	}

	adapter := worker.SessionLogAdapter{SearchPaths: []string{fixtureRoot}}
	history, err := adapter.LoadHistory(worker.LoadRequest{
		Provider:        profile.Provider,
		TranscriptPath:  path,
		TailCompactions: 0,
	})
	if err != nil {
		return nil, fmt.Errorf("load transcript history: %w", err)
	}

	rel, err := filepath.Rel(fixtureRoot, path)
	if err != nil {
		return nil, fmt.Errorf("relative transcript path: %w", err)
	}

	return &Snapshot{
		SessionID:          strings.TrimSpace(history.ProviderSessionID),
		TranscriptPath:     path,
		TranscriptPathHint: rel,
		History:            history,
		Messages:           normalizeMessages(history.Entries),
	}, nil
}

func normalizeMessages(entries []worker.HistoryEntry) []NormalizedMessage {
	out := make([]NormalizedMessage, 0, len(entries))
	for _, entry := range entries {
		role := string(entry.Actor)
		text := strings.TrimSpace(entry.Text)
		if text == "" {
			var blocks []string
			for _, block := range entry.Blocks {
				switch block.Kind {
				case worker.BlockKindThinking, worker.BlockKindText:
					if strings.TrimSpace(block.Text) != "" {
						blocks = append(blocks, strings.TrimSpace(block.Text))
					}
				case worker.BlockKindToolUse:
					name := strings.TrimSpace(block.Name)
					if name == "" {
						name = "tool"
					}
					blocks = append(blocks, "tool_use:"+name)
				case worker.BlockKindToolResult:
					blocks = append(blocks, "tool_result")
				}
			}
			text = strings.Join(blocks, "\n")
		}

		out = append(out, NormalizedMessage{
			Role: role,
			Text: text,
		})
	}
	return out
}

// ContinuationResult validates that a continued transcript stays on the same logical conversation.
func ContinuationResult(profile Profile, before, after *Snapshot) Result {
	if before.TranscriptPathHint != after.TranscriptPathHint {
		return Fail(profile.ID, RequirementContinuationContinuity,
			fmt.Sprintf("transcript path changed from %q to %q", before.TranscriptPathHint, after.TranscriptPathHint))
	}
	if before.History == nil || after.History == nil {
		return Fail(profile.ID, RequirementContinuationContinuity, "missing normalized history snapshot")
	}
	if before.SessionID == "" || after.SessionID == "" {
		return Fail(profile.ID, RequirementContinuationContinuity, "session identity is empty")
	}
	if before.SessionID != after.SessionID {
		return Fail(profile.ID, RequirementContinuationContinuity,
			fmt.Sprintf("session changed from %q to %q", before.SessionID, after.SessionID))
	}
	if before.History.LogicalConversationID == "" || after.History.LogicalConversationID == "" {
		return Fail(profile.ID, RequirementContinuationContinuity, "logical conversation identity is empty")
	}
	if before.History.LogicalConversationID != after.History.LogicalConversationID {
		return Fail(profile.ID, RequirementContinuationContinuity,
			fmt.Sprintf("logical conversation changed from %q to %q", before.History.LogicalConversationID, after.History.LogicalConversationID))
	}
	if len(after.Messages) <= len(before.Messages) {
		return Fail(profile.ID, RequirementContinuationContinuity,
			fmt.Sprintf("continued transcript length %d did not grow beyond %d", len(after.Messages), len(before.Messages)))
	}
	if !hasPrefixMessages(after.Messages, before.Messages) {
		return Fail(profile.ID, RequirementContinuationContinuity, "continued transcript does not preserve prior normalized history")
	}
	if before.History.Cursor.AfterEntryID == "" || after.History.Cursor.AfterEntryID == "" {
		return Fail(profile.ID, RequirementContinuationContinuity, "continuation cursor is empty")
	}
	if before.History.Cursor.AfterEntryID == after.History.Cursor.AfterEntryID {
		return Fail(profile.ID, RequirementContinuationContinuity, "continuation cursor did not advance")
	}
	if !containsMessageText(before.Messages, "", profile.Continuation.AnchorText) {
		return Fail(profile.ID, RequirementContinuationContinuity,
			fmt.Sprintf("fresh transcript does not contain continuation anchor %q", profile.Continuation.AnchorText))
	}
	suffix := after.Messages[len(before.Messages):]
	promptIndex := findMessageIndex(suffix, "user", profile.Continuation.RecallPromptContains)
	if promptIndex < 0 {
		return Fail(profile.ID, RequirementContinuationContinuity,
			fmt.Sprintf("continued transcript missing recall prompt %q", profile.Continuation.RecallPromptContains))
	}
	responseIndex := findMessageIndex(suffix[promptIndex+1:], "assistant", profile.Continuation.RecallResponseContains)
	if responseIndex < 0 {
		return Fail(profile.ID, RequirementContinuationContinuity,
			fmt.Sprintf("continued transcript missing recall response %q after restart prompt", profile.Continuation.RecallResponseContains))
	}
	return Pass(profile.ID, RequirementContinuationContinuity, "continued transcript preserved identity, history, and restart recall")
}

// FreshSessionResult validates that a reset fixture does not look like a continuation.
func FreshSessionResult(profile Profile, before, reset *Snapshot) Result {
	if before.History == nil || reset.History == nil {
		return Fail(profile.ID, RequirementFreshSessionIsolation, "missing normalized history snapshot")
	}
	if before.SessionID == "" || reset.SessionID == "" {
		return Fail(profile.ID, RequirementFreshSessionIsolation, "session identity is empty")
	}
	if strings.TrimSpace(reset.History.LogicalConversationID) == "" {
		return Fail(profile.ID, RequirementFreshSessionIsolation, "reset fixture logical conversation identity is empty")
	}
	if strings.TrimSpace(reset.History.Cursor.AfterEntryID) == "" {
		return Fail(profile.ID, RequirementFreshSessionIsolation, "reset fixture cursor is empty")
	}
	if before.SessionID == reset.SessionID && hasPrefixMessages(reset.Messages, before.Messages) {
		return Fail(profile.ID, RequirementFreshSessionIsolation, "reset fixture still aliases the prior logical conversation")
	}
	if before.History.LogicalConversationID != "" && before.History.LogicalConversationID == reset.History.LogicalConversationID {
		return Fail(profile.ID, RequirementFreshSessionIsolation, "reset fixture reused the prior logical conversation id")
	}
	promptIndex := findMessageIndex(reset.Messages, "user", profile.Continuation.RecallPromptContains)
	if promptIndex < 0 {
		return Fail(profile.ID, RequirementFreshSessionIsolation,
			fmt.Sprintf("reset transcript missing negative-control recall prompt %q", profile.Continuation.RecallPromptContains))
	}
	if containsMessageText(reset.Messages, "assistant", profile.Continuation.AnchorText) {
		return Fail(profile.ID, RequirementFreshSessionIsolation,
			fmt.Sprintf("reset transcript unexpectedly recalled prior anchor %q", profile.Continuation.AnchorText))
	}
	if findMessageIndex(reset.Messages[promptIndex+1:], "assistant", profile.Continuation.ResetResponseContains) < 0 {
		return Fail(profile.ID, RequirementFreshSessionIsolation,
			fmt.Sprintf("reset transcript missing fresh-session response %q", profile.Continuation.ResetResponseContains))
	}
	return Pass(profile.ID, RequirementFreshSessionIsolation, "reset fixture preserves workspace but does not recall prior conversation content")
}

func hasPrefixMessages(messages, prefix []NormalizedMessage) bool {
	if len(prefix) > len(messages) {
		return false
	}
	for i := range prefix {
		if messages[i] != prefix[i] {
			return false
		}
	}
	return true
}

func findMessageIndex(messages []NormalizedMessage, role, contains string) int {
	for i, message := range messages {
		if role != "" && message.Role != role {
			continue
		}
		if contains != "" && !strings.Contains(message.Text, contains) {
			continue
		}
		return i
	}
	return -1
}

func containsMessageText(messages []NormalizedMessage, role, contains string) bool {
	return findMessageIndex(messages, role, contains) >= 0
}

func selectedProfiles() ([]Profile, error) {
	filter := strings.TrimSpace(os.Getenv("PROFILE"))
	if filter == "" {
		return Phase1Profiles(), nil
	}

	var selected []Profile
	for _, profile := range Phase1Profiles() {
		if string(profile.ID) == filter || profile.Provider == filter {
			selected = append(selected, profile)
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("unknown PROFILE %q", filter)
	}
	return selected, nil
}
