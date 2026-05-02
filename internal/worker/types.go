package worker

import (
	"encoding/json"
	"time"
)

// Profile identifies a canonical worker profile.
type Profile string

// revive:disable:exported
const ( //nolint:revive // exported enum values are documented by the enclosing type.
	// Profile* identify the supported canonical worker profiles.
	ProfileClaudeTmuxCLI   Profile = "claude/tmux-cli"
	ProfileCodexTmuxCLI    Profile = "codex/tmux-cli"
	ProfileGeminiTmuxCLI   Profile = "gemini/tmux-cli"
	ProfileOpenCodeTmuxCLI Profile = "opencode/tmux-cli"
)

// CapabilityStatus expresses whether a Phase 1 capability is available.
type CapabilityStatus string

const ( //nolint:revive // exported enum values are documented by the enclosing type.
	// CapabilityStatus* describe whether a capability is available.
	CapabilityStatusUnknown     CapabilityStatus = "unknown"
	CapabilityStatusSupported   CapabilityStatus = "supported"
	CapabilityStatusUnsupported CapabilityStatus = "unsupported"
)

// ResultStatus tracks normalized entry lifecycle state.
type ResultStatus string

const ( //nolint:revive // exported enum values are documented by the enclosing type.
	// ResultStatus* track normalized entry lifecycle state.
	ResultStatusUnknown    ResultStatus = "unknown"
	ResultStatusFinal      ResultStatus = "final"
	ResultStatusPartial    ResultStatus = "partial"
	ResultStatusSuperseded ResultStatus = "superseded"
)

// Actor identifies the normalized entry author.
type Actor string

const ( //nolint:revive // exported enum values are documented by the enclosing type.
	// Actor* identify normalized transcript authors.
	ActorUnknown   Actor = "unknown"
	ActorUser      Actor = "user"
	ActorAssistant Actor = "assistant"
	ActorSystem    Actor = "system"
	ActorTool      Actor = "tool"
)

// BlockKind classifies normalized message/tool blocks.
type BlockKind string

const ( //nolint:revive // exported enum values are documented by the enclosing type.
	// BlockKind* classify normalized transcript blocks.
	BlockKindText        BlockKind = "text"
	BlockKindThinking    BlockKind = "thinking"
	BlockKindToolUse     BlockKind = "tool_use"
	BlockKindToolResult  BlockKind = "tool_result"
	BlockKindInteraction BlockKind = "interaction"
	BlockKindImage       BlockKind = "image"
	BlockKindUnknown     BlockKind = "unknown"
)

// InteractionState captures the durable lifecycle state for a required
// structured interaction recorded in normalized history.
type InteractionState string

const ( //nolint:revive // exported enum values are documented by the enclosing type.
	// InteractionState* capture the durable lifecycle of a required interaction.
	InteractionStateUnknown             InteractionState = "unknown"
	InteractionStateOpened              InteractionState = "opened"
	InteractionStatePending             InteractionState = "pending"
	InteractionStateResolved            InteractionState = "resolved"
	InteractionStateDismissed           InteractionState = "dismissed"
	InteractionStateResumedAfterRestart InteractionState = "resumed_after_restart"
)

// ContinuityStatus captures the adapter's continuity proof level.
type ContinuityStatus string

const ( //nolint:revive // exported enum values are documented by the enclosing type.
	// ContinuityStatus* capture the adapter's continuity proof level.
	ContinuityStatusUnknown    ContinuityStatus = "unknown"
	ContinuityStatusContinuous ContinuityStatus = "continuous"
	ContinuityStatusCompacted  ContinuityStatus = "compacted"
	ContinuityStatusDegraded   ContinuityStatus = "degraded"
)

// TailActivity summarizes the observed state of the transcript tail.
type TailActivity string

const ( //nolint:revive // exported enum values are documented by the enclosing type.
	// TailActivity* summarize normalized tail activity.
	TailActivityUnknown TailActivity = "unknown"
	TailActivityIdle    TailActivity = "idle"
	TailActivityInTurn  TailActivity = "in_turn"
)

// revive:enable:exported

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
	// Status is the highest-severity continuity state. CompactionCount and
	// HasBranches remain populated even when Status is degraded.
	Status          ContinuityStatus `json:"status"`
	CompactionCount int              `json:"compaction_count,omitempty"`
	HasBranches     bool             `json:"has_branches,omitempty"`
	Note            string           `json:"note,omitempty"`
}

// TailState captures the current transcript tail state.
type TailState struct {
	Activity              TailActivity `json:"activity"`
	LastEntryID           string       `json:"last_entry_id,omitempty"`
	OpenToolUseIDs        []string     `json:"open_tool_use_ids,omitempty"`
	PendingInteractionIDs []string     `json:"pending_interaction_ids,omitempty"`
	// Degraded is limited to tail-local transcript damage. Whole-transcript
	// diagnostics are reported on HistorySnapshot.Diagnostics.
	Degraded       bool   `json:"degraded,omitempty"`
	DegradedReason string `json:"degraded_reason,omitempty"`
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

// HistoryDiagnostic records normalized-history evidence that could affect
// conformance assertions without discarding the readable transcript prefix.
type HistoryDiagnostic struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
	Count   int    `json:"count,omitempty"`
}

// HistoryInteraction records a provider-neutral required interaction event
// durably embedded in normalized history.
type HistoryInteraction struct {
	RequestID string            `json:"request_id,omitempty"`
	Kind      string            `json:"kind,omitempty"`
	State     InteractionState  `json:"state"`
	Prompt    string            `json:"prompt,omitempty"`
	Options   []string          `json:"options,omitempty"`
	Action    string            `json:"action,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// HistorySnapshot is the Phase 1 normalized transcript/history view.
type HistorySnapshot struct {
	GCSessionID           string              `json:"gc_session_id,omitempty"`
	LogicalConversationID string              `json:"logical_conversation_id,omitempty"`
	ProviderSessionID     string              `json:"provider_session_id,omitempty"`
	TranscriptStreamID    string              `json:"transcript_stream_id"`
	Generation            Generation          `json:"generation"`
	Cursor                Cursor              `json:"cursor"`
	Continuity            Continuity          `json:"continuity"`
	TailState             TailState           `json:"tail_state"`
	Diagnostics           []HistoryDiagnostic `json:"diagnostics,omitempty"`
	Entries               []HistoryEntry      `json:"entries"`
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
	Kind        BlockKind           `json:"kind"`
	Text        string              `json:"text,omitempty"`
	ToolUseID   string              `json:"tool_use_id,omitempty"`
	Name        string              `json:"name,omitempty"`
	Input       json.RawMessage     `json:"input,omitempty"`
	Content     json.RawMessage     `json:"content,omitempty"`
	IsError     bool                `json:"is_error,omitempty"`
	Interaction *HistoryInteraction `json:"interaction,omitempty"`
	Derived     bool                `json:"derived,omitempty"`
}
