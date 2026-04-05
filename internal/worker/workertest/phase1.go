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

// TranscriptDiscoveryResult validates transcript discovery for a snapshot.
func TranscriptDiscoveryResult(profile Profile, snapshot *Snapshot) Result {
	evidence := phase1SnapshotEvidence(snapshot)
	switch {
	case snapshot == nil:
		return Fail(profile.ID, RequirementTranscriptDiscovery, "missing snapshot").WithEvidence(evidence)
	case snapshot.TranscriptPath == "":
		return Fail(profile.ID, RequirementTranscriptDiscovery, "expected discovered transcript path").WithEvidence(evidence)
	case snapshot.TranscriptPathHint == ".":
		return Fail(profile.ID, RequirementTranscriptDiscovery,
			fmt.Sprintf("relative transcript path = %q, want provider-native file path", snapshot.TranscriptPathHint)).WithEvidence(evidence)
	default:
		return Pass(profile.ID, RequirementTranscriptDiscovery, "discovered provider-native transcript fixture").WithEvidence(evidence)
	}
}

// TranscriptNormalizationResult validates canonical history normalization for a snapshot.
func TranscriptNormalizationResult(profile Profile, snapshot *Snapshot) Result {
	evidence := phase1SnapshotEvidence(snapshot)
	switch {
	case snapshot == nil:
		return Fail(profile.ID, RequirementTranscriptNormalization, "missing snapshot").WithEvidence(evidence)
	case len(snapshot.Messages) < 2:
		return Fail(profile.ID, RequirementTranscriptNormalization,
			fmt.Sprintf("messages = %d, want at least 2", len(snapshot.Messages))).WithEvidence(evidence)
	case snapshot.History == nil:
		return Fail(profile.ID, RequirementTranscriptNormalization, "expected history snapshot").WithEvidence(evidence)
	case snapshot.History.ProviderSessionID == "":
		return Fail(profile.ID, RequirementTranscriptNormalization, "provider session id is empty").WithEvidence(evidence)
	case snapshot.History.LogicalConversationID == "":
		return Fail(profile.ID, RequirementTranscriptNormalization, "logical conversation id is empty").WithEvidence(evidence)
	case snapshot.History.TranscriptStreamID == "":
		return Fail(profile.ID, RequirementTranscriptNormalization, "transcript stream id is empty").WithEvidence(evidence)
	case snapshot.History.Generation.ID == "":
		return Fail(profile.ID, RequirementTranscriptNormalization, "generation id is empty").WithEvidence(evidence)
	case snapshot.History.Cursor.AfterEntryID == "":
		return Fail(profile.ID, RequirementTranscriptNormalization, "cursor after-entry id is empty").WithEvidence(evidence)
	case snapshot.History.Continuity.Status == worker.ContinuityStatusUnknown:
		return Fail(profile.ID, RequirementTranscriptNormalization, "continuity status is unknown").WithEvidence(evidence)
	case len(snapshot.History.Entries) != len(snapshot.Messages):
		return Fail(profile.ID, RequirementTranscriptNormalization,
			fmt.Sprintf("history entries = %d, want %d", len(snapshot.History.Entries), len(snapshot.Messages))).WithEvidence(evidence)
	case snapshot.Messages[0].Role != "user":
		return Fail(profile.ID, RequirementTranscriptNormalization,
			fmt.Sprintf("first role = %q, want user", snapshot.Messages[0].Role)).WithEvidence(evidence)
	case snapshot.Messages[0].Text == "":
		return Fail(profile.ID, RequirementTranscriptNormalization, "first normalized message text is empty").WithEvidence(evidence)
	case snapshot.Messages[len(snapshot.Messages)-1].Text == "":
		return Fail(profile.ID, RequirementTranscriptNormalization, "last normalized message text is empty").WithEvidence(evidence)
	default:
		return Pass(profile.ID, RequirementTranscriptNormalization, "normalized provider transcript into canonical history").WithEvidence(evidence)
	}
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
	evidence := continuationEvidence(before, after)
	if before.TranscriptPathHint != after.TranscriptPathHint {
		return Fail(profile.ID, RequirementContinuationContinuity,
			fmt.Sprintf("transcript path changed from %q to %q", before.TranscriptPathHint, after.TranscriptPathHint)).WithEvidence(evidence)
	}
	if before.History == nil || after.History == nil {
		return Fail(profile.ID, RequirementContinuationContinuity, "missing normalized history snapshot").WithEvidence(evidence)
	}
	if before.SessionID == "" || after.SessionID == "" {
		return Fail(profile.ID, RequirementContinuationContinuity, "session identity is empty").WithEvidence(evidence)
	}
	if before.SessionID != after.SessionID {
		return Fail(profile.ID, RequirementContinuationContinuity,
			fmt.Sprintf("session changed from %q to %q", before.SessionID, after.SessionID)).WithEvidence(evidence)
	}
	if before.History.LogicalConversationID == "" || after.History.LogicalConversationID == "" {
		return Fail(profile.ID, RequirementContinuationContinuity, "logical conversation identity is empty").WithEvidence(evidence)
	}
	if before.History.LogicalConversationID != after.History.LogicalConversationID {
		return Fail(profile.ID, RequirementContinuationContinuity,
			fmt.Sprintf("logical conversation changed from %q to %q", before.History.LogicalConversationID, after.History.LogicalConversationID)).WithEvidence(evidence)
	}
	if len(after.Messages) <= len(before.Messages) {
		return Fail(profile.ID, RequirementContinuationContinuity,
			fmt.Sprintf("continued transcript length %d did not grow beyond %d", len(after.Messages), len(before.Messages))).WithEvidence(evidence)
	}
	if !hasPrefixMessages(after.Messages, before.Messages) {
		return Fail(profile.ID, RequirementContinuationContinuity, "continued transcript does not preserve prior normalized history").WithEvidence(evidence)
	}
	if before.History.Cursor.AfterEntryID == "" || after.History.Cursor.AfterEntryID == "" {
		return Fail(profile.ID, RequirementContinuationContinuity, "continuation cursor is empty").WithEvidence(evidence)
	}
	if before.History.Cursor.AfterEntryID == after.History.Cursor.AfterEntryID {
		return Fail(profile.ID, RequirementContinuationContinuity, "continuation cursor did not advance").WithEvidence(evidence)
	}
	if !containsMessageText(before.Messages, "", profile.Continuation.AnchorText) {
		return Fail(profile.ID, RequirementContinuationContinuity,
			fmt.Sprintf("fresh transcript does not contain continuation anchor %q", profile.Continuation.AnchorText)).WithEvidence(evidence)
	}
	suffix := after.Messages[len(before.Messages):]
	promptIndex := findMessageIndex(suffix, "user", profile.Continuation.RecallPromptContains)
	if promptIndex < 0 {
		return Fail(profile.ID, RequirementContinuationContinuity,
			fmt.Sprintf("continued transcript missing recall prompt %q", profile.Continuation.RecallPromptContains)).WithEvidence(evidence)
	}
	responseIndex := findMessageIndex(suffix[promptIndex+1:], "assistant", profile.Continuation.RecallResponseContains)
	if responseIndex < 0 {
		return Fail(profile.ID, RequirementContinuationContinuity,
			fmt.Sprintf("continued transcript missing recall response %q after restart prompt", profile.Continuation.RecallResponseContains)).WithEvidence(evidence)
	}
	return Pass(profile.ID, RequirementContinuationContinuity, "continued transcript preserved identity, history, and restart recall").WithEvidence(evidence)
}

// FreshSessionResult validates that a reset fixture does not look like a continuation.
func FreshSessionResult(profile Profile, before, reset *Snapshot) Result {
	evidence := continuationEvidence(before, reset)
	if before.History == nil || reset.History == nil {
		return Fail(profile.ID, RequirementFreshSessionIsolation, "missing normalized history snapshot").WithEvidence(evidence)
	}
	if before.SessionID == "" || reset.SessionID == "" {
		return Fail(profile.ID, RequirementFreshSessionIsolation, "session identity is empty").WithEvidence(evidence)
	}
	if strings.TrimSpace(reset.History.LogicalConversationID) == "" {
		return Fail(profile.ID, RequirementFreshSessionIsolation, "reset fixture logical conversation identity is empty").WithEvidence(evidence)
	}
	if strings.TrimSpace(reset.History.Cursor.AfterEntryID) == "" {
		return Fail(profile.ID, RequirementFreshSessionIsolation, "reset fixture cursor is empty").WithEvidence(evidence)
	}
	if before.SessionID == reset.SessionID && hasPrefixMessages(reset.Messages, before.Messages) {
		return Fail(profile.ID, RequirementFreshSessionIsolation, "reset fixture still aliases the prior logical conversation").WithEvidence(evidence)
	}
	if before.History.LogicalConversationID != "" && before.History.LogicalConversationID == reset.History.LogicalConversationID {
		return Fail(profile.ID, RequirementFreshSessionIsolation, "reset fixture reused the prior logical conversation id").WithEvidence(evidence)
	}
	promptIndex := findMessageIndex(reset.Messages, "user", profile.Continuation.RecallPromptContains)
	if promptIndex < 0 {
		return Fail(profile.ID, RequirementFreshSessionIsolation,
			fmt.Sprintf("reset transcript missing negative-control recall prompt %q", profile.Continuation.RecallPromptContains)).WithEvidence(evidence)
	}
	if containsMessageText(reset.Messages, "assistant", profile.Continuation.AnchorText) {
		return Fail(profile.ID, RequirementFreshSessionIsolation,
			fmt.Sprintf("reset transcript unexpectedly recalled prior anchor %q", profile.Continuation.AnchorText)).WithEvidence(evidence)
	}
	if findMessageIndex(reset.Messages[promptIndex+1:], "assistant", profile.Continuation.ResetResponseContains) < 0 {
		return Fail(profile.ID, RequirementFreshSessionIsolation,
			fmt.Sprintf("reset transcript missing fresh-session response %q", profile.Continuation.ResetResponseContains)).WithEvidence(evidence)
	}
	return Pass(profile.ID, RequirementFreshSessionIsolation, "reset fixture preserves workspace but does not recall prior conversation content").WithEvidence(evidence)
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

func phase1SnapshotEvidence(snapshot *Snapshot) map[string]string {
	if snapshot == nil {
		return nil
	}
	evidence := map[string]string{
		"transcript_path":      snapshot.TranscriptPath,
		"transcript_path_hint": snapshot.TranscriptPathHint,
		"session_id":           snapshot.SessionID,
	}
	if snapshot.History != nil {
		evidence["logical_conversation_id"] = snapshot.History.LogicalConversationID
		evidence["provider_session_id"] = snapshot.History.ProviderSessionID
		evidence["cursor_after_entry_id"] = snapshot.History.Cursor.AfterEntryID
		evidence["message_count"] = fmt.Sprintf("%d", len(snapshot.Messages))
	}
	return evidence
}

func continuationEvidence(before, after *Snapshot) map[string]string {
	evidence := map[string]string{}
	mergeEvidence(evidence, "before_", phase1SnapshotEvidence(before))
	mergeEvidence(evidence, "after_", phase1SnapshotEvidence(after))
	return evidence
}

func mergeEvidence(dst map[string]string, prefix string, values map[string]string) {
	for key, value := range values {
		dst[prefix+key] = value
	}
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
