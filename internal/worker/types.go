package worker

import (
	"encoding/json"
	"time"
)

// Profile identifies a canonical worker profile.
type Profile string

const (
	ProfileClaudeTmuxCLI Profile = "claude/tmux-cli"
	ProfileCodexTmuxCLI  Profile = "codex/tmux-cli"
	ProfileGeminiTmuxCLI Profile = "gemini/tmux-cli"
)

// CapabilityStatus expresses whether a Phase 1 capability is available.
type CapabilityStatus string

const (
	CapabilityStatusUnknown     CapabilityStatus = "unknown"
	CapabilityStatusSupported   CapabilityStatus = "supported"
	CapabilityStatusUnsupported CapabilityStatus = "unsupported"
)

// ResultStatus tracks normalized entry lifecycle state.
type ResultStatus string

const (
	ResultStatusUnknown    ResultStatus = "unknown"
	ResultStatusFinal      ResultStatus = "final"
	ResultStatusPartial    ResultStatus = "partial"
	ResultStatusSuperseded ResultStatus = "superseded"
)

// Actor identifies the normalized entry author.
type Actor string

const (
	ActorUnknown   Actor = "unknown"
	ActorUser      Actor = "user"
	ActorAssistant Actor = "assistant"
	ActorSystem    Actor = "system"
	ActorTool      Actor = "tool"
)

// BlockKind classifies normalized message/tool blocks.
type BlockKind string

const (
	BlockKindText       BlockKind = "text"
	BlockKindThinking   BlockKind = "thinking"
	BlockKindToolUse    BlockKind = "tool_use"
	BlockKindToolResult BlockKind = "tool_result"
	BlockKindImage      BlockKind = "image"
	BlockKindUnknown    BlockKind = "unknown"
)

// ContinuityStatus captures the adapter's continuity proof level.
type ContinuityStatus string

const (
	ContinuityStatusUnknown    ContinuityStatus = "unknown"
	ContinuityStatusContinuous ContinuityStatus = "continuous"
	ContinuityStatusCompacted  ContinuityStatus = "compacted"
	ContinuityStatusDegraded   ContinuityStatus = "degraded"
)

// TailActivity summarizes the observed state of the transcript tail.
type TailActivity string

const (
	TailActivityUnknown TailActivity = "unknown"
	TailActivityIdle    TailActivity = "idle"
	TailActivityInTurn  TailActivity = "in_turn"
)

// Generation identifies a raw transcript stream instance.
type Generation struct {
	ID         string    `json:"id"`
	ObservedAt time.Time `json:"observed_at,omitempty"`
}

// Cursor identifies the adapter's current normalized tip.
type Cursor struct {
	AfterEntryID string `json:"after_entry_id,omitempty"`
}

// Continuity describes compaction/branch evidence on a snapshot.
type Continuity struct {
	Status          ContinuityStatus `json:"status"`
	CompactionCount int              `json:"compaction_count,omitempty"`
	HasBranches     bool             `json:"has_branches,omitempty"`
	Note            string           `json:"note,omitempty"`
}

// TailState captures the current transcript tail state.
type TailState struct {
	Activity       TailActivity `json:"activity"`
	LastEntryID    string       `json:"last_entry_id,omitempty"`
	OpenToolUseIDs []string     `json:"open_tool_use_ids,omitempty"`
}

// Provenance points back to the provider-native transcript evidence.
type Provenance struct {
	Provider          string          `json:"provider"`
	TranscriptPath    string          `json:"transcript_path"`
	ProviderSessionID string          `json:"provider_session_id,omitempty"`
	RawEntryID        string          `json:"raw_entry_id,omitempty"`
	RawType           string          `json:"raw_type,omitempty"`
	Derived           bool            `json:"derived,omitempty"`
	Raw               json.RawMessage `json:"raw,omitempty"`
}

// HistorySnapshot is the Phase 1 normalized transcript/history view.
type HistorySnapshot struct {
	GCSessionID           string         `json:"gc_session_id,omitempty"`
	LogicalConversationID string         `json:"logical_conversation_id,omitempty"`
	ProviderSessionID     string         `json:"provider_session_id,omitempty"`
	TranscriptStreamID    string         `json:"transcript_stream_id"`
	Generation            Generation     `json:"generation"`
	Cursor                Cursor         `json:"cursor"`
	Continuity            Continuity     `json:"continuity"`
	TailState             TailState      `json:"tail_state"`
	Entries               []HistoryEntry `json:"entries"`
}

// HistoryEntry is a normalized transcript entry.
type HistoryEntry struct {
	ID         string         `json:"id"`
	Kind       string         `json:"kind"`
	Actor      Actor          `json:"actor"`
	Order      int            `json:"order"`
	Timestamp  *time.Time     `json:"timestamp,omitempty"`
	Status     ResultStatus   `json:"status"`
	Text       string         `json:"text,omitempty"`
	Blocks     []HistoryBlock `json:"blocks,omitempty"`
	Provenance Provenance     `json:"provenance"`
}

// HistoryBlock carries normalized content/tool payload.
type HistoryBlock struct {
	Kind      BlockKind       `json:"kind"`
	Text      string          `json:"text,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Derived   bool            `json:"derived,omitempty"`
}
