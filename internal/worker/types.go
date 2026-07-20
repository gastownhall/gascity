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
	ProfileClaudeTmuxCLI      Profile = "claude/tmux-cli"
	ProfileCodexTmuxCLI       Profile = "codex/tmux-cli"
	ProfileGeminiTmuxCLI      Profile = "gemini/tmux-cli"
	ProfileKimiTmuxCLI        Profile = "kimi/tmux-cli"
	ProfileOpenCodeTmuxCLI    Profile = "opencode/tmux-cli"
	ProfileMimoCodeTmuxCLI    Profile = "mimocode/tmux-cli"
	ProfilePiTmuxCLI          Profile = "pi/tmux-cli"
	ProfileAntigravityTmuxCLI Profile = "antigravity/tmux-cli"
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
	RawRecordID       string          `json:"-"`
}

// HistoryDiagnostic records normalized-history evidence that could affect
// conformance assertions without discarding the readable transcript prefix.
type HistoryDiagnostic struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
	Count   int    `json:"count,omitempty"`
}

// HistoryUsage records provider-neutral token usage for one normalized entry.
type HistoryUsage struct {
	InputTokens         int `json:"input_tokens,omitempty"`
	OutputTokens        int `json:"output_tokens,omitempty"`
	ReasoningTokens     int `json:"reasoning_tokens,omitempty"`
	CacheReadTokens     int `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int `json:"cache_creation_tokens,omitempty"`
	ContextWindowTokens int `json:"context_window_tokens,omitempty"`
	ContextUsedTokens   int `json:"context_used_tokens,omitempty"`
	ContextPercent      int `json:"context_percent,omitempty"`
}

// HistoryUserPrompt is provider-neutral metadata extracted from a user prompt.
type HistoryUserPrompt struct {
	Text          string                 `json:"text,omitempty"`
	OpenedFiles   []string               `json:"opened_files,omitempty"`
	Selections    []HistoryUserSelection `json:"selections,omitempty"`
	UploadedFiles []HistoryUploadedFile  `json:"uploaded_files,omitempty"`
}

// HistorySystemEvent is provider-neutral metadata extracted from a system
// transcript event such as a provider error or turn-aborted notice.
type HistorySystemEvent struct {
	Kind     string `json:"kind,omitempty"`
	Category string `json:"category,omitempty"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message,omitempty"`
}

// HistoryUserSelection is one IDE selection carried in user prompt metadata.
type HistoryUserSelection struct {
	Text string `json:"text,omitempty"`
}

// HistoryUploadedFile is one uploaded file attachment mentioned by a user
// prompt.
type HistoryUploadedFile struct {
	OriginalName string `json:"original_name,omitempty"`
	Size         string `json:"size,omitempty"`
	MIMEType     string `json:"mime_type,omitempty"`
	FilePath     string `json:"file_path,omitempty"`
	PreviewURL   string `json:"preview_url,omitempty"`
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

// StructuredArgument is one provider-neutral string argument parsed from a
// tool input.
type StructuredArgument struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// StructuredToolInput is a provider-neutral tool input carried in normalized
// history.
type StructuredToolInput struct {
	Kind          string               `json:"kind,omitempty"`
	Text          string               `json:"text,omitempty"`
	Command       string               `json:"command,omitempty"`
	LinkedCommand string               `json:"linked_command,omitempty"`
	Code          string               `json:"code,omitempty"`
	Patch         string               `json:"patch,omitempty"`
	FilePath      string               `json:"file_path,omitempty"`
	Language      string               `json:"language,omitempty"`
	URL           string               `json:"url,omitempty"`
	Prompt        string               `json:"prompt,omitempty"`
	TaskID        string               `json:"task_id,omitempty"`
	TaskType      string               `json:"task_type,omitempty"`
	TaskStatus    string               `json:"task_status,omitempty"`
	Description   string               `json:"description,omitempty"`
	Question      string               `json:"question,omitempty"`
	Options       []string             `json:"options,omitempty"`
	Query         string               `json:"query,omitempty"`
	Pattern       string               `json:"pattern,omitempty"`
	Plan          string               `json:"plan,omitempty"`
	Explanation   string               `json:"explanation,omitempty"`
	Steps         []StructuredPlanStep `json:"steps,omitempty"`
	Todos         []StructuredTodoItem `json:"todos,omitempty"`
	Arguments     []StructuredArgument `json:"arguments,omitempty"`
}

// StructuredPlanStep is one provider-neutral plan step carried in normalized
// tool input or result data.
type StructuredPlanStep struct {
	Step   string `json:"step,omitempty"`
	Status string `json:"status,omitempty"`
}

// StructuredTodoItem is one provider-neutral todo item carried in normalized
// tool input or result data.
type StructuredTodoItem struct {
	ID         string `json:"id,omitempty"`
	Content    string `json:"content,omitempty"`
	Status     string `json:"status,omitempty"`
	ActiveForm string `json:"active_form,omitempty"`
	Priority   string `json:"priority,omitempty"`
}

// StructuredPatchHunk is one provider-neutral unified diff hunk carried in a
// structured edit result.
type StructuredPatchHunk struct {
	FilePath string   `json:"file_path,omitempty"`
	OldStart int      `json:"old_start,omitempty"`
	OldLines int      `json:"old_lines,omitempty"`
	NewStart int      `json:"new_start,omitempty"`
	NewLines int      `json:"new_lines,omitempty"`
	Lines    []string `json:"lines,omitempty"`
}

// StructuredSearchResultItem is one provider-neutral web/search result item
// carried in normalized tool-result data.
type StructuredSearchResultItem struct {
	Title   string `json:"title,omitempty"`
	URL     string `json:"url,omitempty"`
	Snippet string `json:"snippet,omitempty"`
}

// StructuredQuestionOption is one provider-neutral selectable answer option.
type StructuredQuestionOption struct {
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
}

// StructuredQuestion is one provider-neutral user question carried in a
// structured question result.
type StructuredQuestion struct {
	Question    string                     `json:"question,omitempty"`
	Header      string                     `json:"header,omitempty"`
	Options     []StructuredQuestionOption `json:"options,omitempty"`
	MultiSelect bool                       `json:"multi_select,omitempty"`
}

// StructuredToolError is provider-neutral typed error data for a failed tool
// result.
type StructuredToolError struct {
	Category   string `json:"category,omitempty"`
	Message    string `json:"message,omitempty"`
	UserReason string `json:"user_reason,omitempty"`
}

// StructuredToolResult is provider-neutral typed tool-result data carried in
// normalized history before API projection.
type StructuredToolResult struct {
	Kind              string                       `json:"kind"`
	Text              string                       `json:"text,omitempty"`
	Command           string                       `json:"command,omitempty"`
	Stdout            string                       `json:"stdout,omitempty"`
	Stderr            string                       `json:"stderr,omitempty"`
	ExitCode          *int                         `json:"exit_code,omitempty"`
	Interrupted       bool                         `json:"interrupted,omitempty"`
	Truncated         bool                         `json:"truncated,omitempty"`
	IsImage           bool                         `json:"is_image,omitempty"`
	Mode              string                       `json:"mode,omitempty"`
	Query             string                       `json:"query,omitempty"`
	URL               string                       `json:"url,omitempty"`
	TaskID            string                       `json:"task_id,omitempty"`
	TaskType          string                       `json:"task_type,omitempty"`
	TaskStatus        string                       `json:"task_status,omitempty"`
	Description       string                       `json:"description,omitempty"`
	TotalDurationMs   int                          `json:"total_duration_ms,omitempty"`
	TotalTokens       int                          `json:"total_tokens,omitempty"`
	TotalToolUseCount int                          `json:"total_tool_use_count,omitempty"`
	Output            string                       `json:"output,omitempty"`
	Question          string                       `json:"question,omitempty"`
	Questions         []StructuredQuestion         `json:"questions,omitempty"`
	Answer            string                       `json:"answer,omitempty"`
	Options           []string                     `json:"options,omitempty"`
	Answers           []StructuredArgument         `json:"answers,omitempty"`
	Counts            []StructuredArgument         `json:"counts,omitempty"`
	StatusCode        int                          `json:"status_code,omitempty"`
	StatusText        string                       `json:"status_text,omitempty"`
	Bytes             int                          `json:"bytes,omitempty"`
	Filenames         []string                     `json:"filenames,omitempty"`
	NumFiles          int                          `json:"num_files,omitempty"`
	NumResults        int                          `json:"num_results,omitempty"`
	DurationMs        int                          `json:"duration_ms,omitempty"`
	AppliedLimit      int                          `json:"applied_limit,omitempty"`
	StdoutLines       int                          `json:"stdout_lines,omitempty"`
	StderrLines       int                          `json:"stderr_lines,omitempty"`
	Timestamp         string                       `json:"timestamp,omitempty"`
	ResultItems       []StructuredSearchResultItem `json:"result_items,omitempty"`
	Content           string                       `json:"content,omitempty"`
	NumLines          int                          `json:"num_lines,omitempty"`
	FilePath          string                       `json:"file_path,omitempty"`
	FilePaths         []string                     `json:"file_paths,omitempty"`
	Language          string                       `json:"language,omitempty"`
	Code              string                       `json:"code,omitempty"`
	Plan              string                       `json:"plan,omitempty"`
	Explanation       string                       `json:"explanation,omitempty"`
	Steps             []StructuredPlanStep         `json:"steps,omitempty"`
	Patch             string                       `json:"patch,omitempty"`
	PatchHunks        []StructuredPatchHunk        `json:"patch_hunks,omitempty"`
	OldString         string                       `json:"old_string,omitempty"`
	NewString         string                       `json:"new_string,omitempty"`
	OriginalFile      string                       `json:"original_file,omitempty"`
	ReplaceAll        *bool                        `json:"replace_all,omitempty"`
	UserModified      *bool                        `json:"user_modified,omitempty"`
	OldTodos          []StructuredTodoItem         `json:"old_todos,omitempty"`
	NewTodos          []StructuredTodoItem         `json:"new_todos,omitempty"`
	StartLine         int                          `json:"start_line,omitempty"`
	TotalLines        int                          `json:"total_lines,omitempty"`
	Error             *StructuredToolError         `json:"error,omitempty"`
}

// HistorySnapshot is the Phase 1 normalized transcript/history view.
type HistorySnapshot struct {
	GCSessionID           string                `json:"gc_session_id,omitempty"`
	LogicalConversationID string                `json:"logical_conversation_id,omitempty"`
	ProviderSessionID     string                `json:"provider_session_id,omitempty"`
	TranscriptStreamID    string                `json:"transcript_stream_id"`
	Generation            Generation            `json:"generation"`
	Cursor                Cursor                `json:"cursor"`
	Continuity            Continuity            `json:"continuity"`
	TailState             TailState             `json:"tail_state"`
	Diagnostics           []HistoryDiagnostic   `json:"diagnostics,omitempty"`
	Pagination            *TranscriptPagination `json:"pagination,omitempty"`
	Entries               []HistoryEntry        `json:"entries"`
}

// HistoryEntry is a normalized transcript entry.
type HistoryEntry struct {
	ID          string              `json:"id"`
	Kind        string              `json:"kind"`
	Actor       Actor               `json:"actor"`
	Order       int                 `json:"order"`
	Timestamp   *time.Time          `json:"timestamp,omitempty"`
	Status      ResultStatus        `json:"status"`
	Text        string              `json:"text,omitempty"`
	Model       string              `json:"model,omitempty"`
	StopReason  string              `json:"stop_reason,omitempty"`
	Usage       *HistoryUsage       `json:"usage,omitempty"`
	UserPrompt  *HistoryUserPrompt  `json:"user_prompt,omitempty"`
	SystemEvent *HistorySystemEvent `json:"system_event,omitempty"`
	Blocks      []HistoryBlock      `json:"blocks,omitempty"`
	Provenance  Provenance          `json:"provenance"`
}

// HistoryBlock carries normalized content/tool payload.
type HistoryBlock struct {
	Kind             BlockKind             `json:"kind"`
	Text             string                `json:"text,omitempty"`
	Signature        string                `json:"signature,omitempty"`
	ToolUseID        string                `json:"tool_use_id,omitempty"`
	Name             string                `json:"name,omitempty"`
	FilePath         string                `json:"file_path,omitempty"`
	ImageURL         string                `json:"image_url,omitempty"`
	MIMEType         string                `json:"mime_type,omitempty"`
	Input            json.RawMessage       `json:"input,omitempty"`
	StructuredInput  *StructuredToolInput  `json:"structured_input,omitempty"`
	Content          json.RawMessage       `json:"content,omitempty"`
	ContentText      string                `json:"content_text,omitempty"`
	StructuredResult *StructuredToolResult `json:"structured_result,omitempty"`
	IsError          bool                  `json:"is_error,omitempty"`
	Interaction      *HistoryInteraction   `json:"interaction,omitempty"`
	Derived          bool                  `json:"derived,omitempty"`
}
