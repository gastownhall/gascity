package worker

import "github.com/gastownhall/gascity/internal/sessionlog"

type (
	TranscriptSession        = sessionlog.Session
	TranscriptEntry          = sessionlog.Entry
	TranscriptContentBlock   = sessionlog.ContentBlock
	TranscriptMessageContent = sessionlog.MessageContent
	TranscriptPagination     = sessionlog.PaginationInfo
	TranscriptTailMeta       = sessionlog.TailMeta
	TranscriptContextUsage   = sessionlog.ContextUsage
	AgentMapping             = sessionlog.AgentMapping
)

var ErrAgentNotFound = sessionlog.ErrAgentNotFound

func DefaultSearchPaths() []string {
	return sessionlog.DefaultSearchPaths()
}

func MergeSearchPaths(paths []string) []string {
	return sessionlog.MergeSearchPaths(paths)
}

func ValidateAgentID(agentID string) error {
	return sessionlog.ValidateAgentID(agentID)
}

func InferTranscriptActivity(entries []*TranscriptEntry) string {
	return sessionlog.InferActivityFromEntries(entries)
}
