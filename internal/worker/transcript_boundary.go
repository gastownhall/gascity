package worker

import "github.com/gastownhall/gascity/internal/sessionlog"

type (
	// TranscriptSession aliases the sessionlog transcript session payload.
	TranscriptSession = sessionlog.Session
	// TranscriptEntry aliases a single transcript entry.
	TranscriptEntry = sessionlog.Entry
	// TranscriptContentBlock aliases a single structured content block.
	TranscriptContentBlock = sessionlog.ContentBlock
	// TranscriptMessageContent aliases normalized message content.
	TranscriptMessageContent = sessionlog.MessageContent
	// TranscriptPagination aliases transcript pagination metadata.
	TranscriptPagination = sessionlog.PaginationInfo
	// TranscriptTailMeta aliases transcript tail metadata.
	TranscriptTailMeta = sessionlog.TailMeta
	// TranscriptContextUsage aliases transcript context-usage accounting.
	TranscriptContextUsage = sessionlog.ContextUsage
	// AgentMapping aliases transcript agent-mapping metadata.
	AgentMapping = sessionlog.AgentMapping
)

// ErrAgentNotFound reports that the requested transcript agent was not found.
var ErrAgentNotFound = sessionlog.ErrAgentNotFound

// DefaultSearchPaths returns the default transcript search roots.
func DefaultSearchPaths() []string {
	return sessionlog.DefaultSearchPaths()
}

// MergeSearchPaths normalizes and deduplicates transcript search roots.
func MergeSearchPaths(paths []string) []string {
	return sessionlog.MergeSearchPaths(paths)
}

// ValidateAgentID verifies that the supplied transcript agent identifier is valid.
func ValidateAgentID(agentID string) error {
	return sessionlog.ValidateAgentID(agentID)
}

// InferTranscriptActivity summarizes transcript activity from the supplied entries.
func InferTranscriptActivity(entries []*TranscriptEntry) string {
	return sessionlog.InferActivityFromEntries(entries)
}
