package workertest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/sessionlog"
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
	Messages           []NormalizedMessage
}

// DiscoverTranscript resolves the provider-native transcript path for a profile fixture root.
func DiscoverTranscript(profile Profile, fixtureRoot string) (string, error) {
	path := sessionlog.FindSessionFileForProvider([]string{fixtureRoot}, profile.Provider, profile.WorkDir)
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

	session, err := sessionlog.ReadProviderFile(profile.Provider, path, 0)
	if err != nil {
		return nil, fmt.Errorf("read transcript: %w", err)
	}

	rel, err := filepath.Rel(fixtureRoot, path)
	if err != nil {
		return nil, fmt.Errorf("relative transcript path: %w", err)
	}

	return &Snapshot{
		SessionID:          strings.TrimSpace(session.ID),
		TranscriptPath:     path,
		TranscriptPathHint: rel,
		Messages:           normalizeMessages(session.Messages),
	}, nil
}

func normalizeMessages(entries []*sessionlog.Entry) []NormalizedMessage {
	out := make([]NormalizedMessage, 0, len(entries))
	for _, entry := range entries {
		role := strings.TrimSpace(entry.Type)
		text := strings.TrimSpace(entry.TextContent())
		if text == "" {
			var blocks []string
			for _, block := range entry.ContentBlocks() {
				switch block.Type {
				case "thinking", "text":
					if strings.TrimSpace(block.Text) != "" {
						blocks = append(blocks, strings.TrimSpace(block.Text))
					}
				case "tool_use":
					name := strings.TrimSpace(block.Name)
					if name == "" {
						name = "tool"
					}
					blocks = append(blocks, "tool_use:"+name)
				case "tool_result":
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
func ContinuationResult(profile ProfileID, before, after *Snapshot) Result {
	if before.TranscriptPathHint != after.TranscriptPathHint {
		return Fail(profile, RequirementContinuationContinuity,
			fmt.Sprintf("transcript path changed from %q to %q", before.TranscriptPathHint, after.TranscriptPathHint))
	}
	if before.SessionID == "" || after.SessionID == "" {
		return Fail(profile, RequirementContinuationContinuity, "session identity is empty")
	}
	if before.SessionID != after.SessionID {
		return Fail(profile, RequirementContinuationContinuity,
			fmt.Sprintf("session changed from %q to %q", before.SessionID, after.SessionID))
	}
	if len(after.Messages) <= len(before.Messages) {
		return Fail(profile, RequirementContinuationContinuity,
			fmt.Sprintf("continued transcript length %d did not grow beyond %d", len(after.Messages), len(before.Messages)))
	}
	if !hasPrefixMessages(after.Messages, before.Messages) {
		return Fail(profile, RequirementContinuationContinuity, "continued transcript does not preserve prior normalized history")
	}
	return Pass(profile, RequirementContinuationContinuity, "continued transcript preserved identity and history")
}

// FreshSessionResult validates that a reset fixture does not look like a continuation.
func FreshSessionResult(profile ProfileID, before, reset *Snapshot) Result {
	if before.SessionID == "" || reset.SessionID == "" {
		return Fail(profile, RequirementFreshSessionIsolation, "session identity is empty")
	}
	if before.SessionID == reset.SessionID && hasPrefixMessages(reset.Messages, before.Messages) {
		return Fail(profile, RequirementFreshSessionIsolation, "reset fixture still aliases the prior logical conversation")
	}
	return Pass(profile, RequirementFreshSessionIsolation, "reset fixture starts a distinct logical conversation")
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
