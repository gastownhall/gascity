package api

import (
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/worker"
)

const sessionStructuredSchemaVersion = "session.structured.v1"

const (
	structuredTranscriptUnavailableCode    = "transcript_unavailable"
	structuredTranscriptUnavailableMessage = "provider transcript is unavailable; using provider-neutral text fallback"
)

// SessionStreamStructuredMessageEvent carries provider-normalized structured
// transcript messages on the session SSE stream.
type SessionStreamStructuredMessageEvent struct {
	ID                 string                     `json:"id"`
	Template           string                     `json:"template"`
	Provider           string                     `json:"provider" doc:"Producing provider identifier (claude, codex, gemini, opencode, etc.)."`
	Format             string                     `json:"format" enum:"structured" doc:"Always structured for this event."`
	SchemaVersion      string                     `json:"schema_version" enum:"session.structured.v1" doc:"Structured session transcript schema version."`
	Operation          string                     `json:"operation" enum:"snapshot,upsert,reset" doc:"How the client applies this structured frame: replace from a snapshot/reset or merge an upsert."`
	ResetReason        string                     `json:"reset_reason,omitempty" enum:"resume_invalid,stream_changed,cursor_invalidated,history_rewritten" doc:"Present if and only if operation is reset; absent for snapshot and upsert. Identifies why the reset replaced the client transcript."`
	History            *SessionStructuredHistory  `json:"history" doc:"Normalized worker-history envelope for this snapshot or stream batch."`
	StructuredMessages []SessionStructuredMessage `json:"structured_messages" doc:"Provider-normalized structured messages."`
	Pagination         *sessionlog.PaginationInfo `json:"pagination,omitempty"`
}

// SessionStructuredHistory is the normalized worker-history envelope projected
// onto the session transcript API.
type SessionStructuredHistory struct {
	GCSessionID           string                        `json:"gc_session_id,omitempty"`
	LogicalConversationID string                        `json:"logical_conversation_id,omitempty"`
	ProviderSessionID     string                        `json:"provider_session_id,omitempty"`
	TranscriptStreamID    string                        `json:"transcript_stream_id"`
	Generation            SessionStructuredGeneration   `json:"generation"`
	Cursor                SessionStructuredCursor       `json:"cursor"`
	Continuity            SessionStructuredContinuity   `json:"continuity"`
	TailState             SessionStructuredTailState    `json:"tail_state"`
	Diagnostics           []SessionStructuredDiagnostic `json:"diagnostics,omitempty"`
}

// SessionStructuredGeneration identifies a raw transcript stream instance.
type SessionStructuredGeneration struct {
	ID         string `json:"id"`
	ObservedAt string `json:"observed_at,omitempty"`
}

// SessionStructuredCursor identifies the normalized transcript tip.
type SessionStructuredCursor struct {
	AfterEntryID string `json:"after_entry_id,omitempty"`
	ResumeToken  string `json:"resume_token" doc:"Opaque cursor for an exact structured REST-to-SSE handoff or SSE reconnect."`
}

// SessionStructuredContinuity describes compaction/branch evidence.
type SessionStructuredContinuity struct {
	Status          string `json:"status"`
	CompactionCount int    `json:"compaction_count,omitempty"`
	HasBranches     bool   `json:"has_branches,omitempty"`
	Note            string `json:"note,omitempty"`
}

// SessionStructuredTailState captures the current transcript tail state.
type SessionStructuredTailState struct {
	Activity              string   `json:"activity"`
	LastEntryID           string   `json:"last_entry_id,omitempty"`
	OpenToolCallIDs       []string `json:"open_tool_call_ids,omitempty"`
	PendingInteractionIDs []string `json:"pending_interaction_ids,omitempty"`
	Degraded              bool     `json:"degraded,omitempty"`
	DegradedReason        string   `json:"degraded_reason,omitempty"`
}

// SessionStructuredDiagnostic records normalized-history diagnostics.
type SessionStructuredDiagnostic struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
	Count   int    `json:"count,omitempty"`
}

// SessionStructuredMessage is one provider-normalized transcript message.
type SessionStructuredMessage struct {
	ID          string                        `json:"id"`
	Role        string                        `json:"role"`
	Provider    string                        `json:"provider,omitempty"`
	Timestamp   string                        `json:"timestamp,omitempty"`
	Model       string                        `json:"model,omitempty"`
	StopReason  string                        `json:"stop_reason,omitempty"`
	Usage       *SessionStructuredUsage       `json:"usage,omitempty"`
	UserPrompt  *SessionStructuredUserPrompt  `json:"user_prompt,omitempty"`
	SystemEvent *SessionStructuredSystemEvent `json:"system_event,omitempty"`
	Status      string                        `json:"status" enum:"unknown,final,partial,superseded"`
	Blocks      []SessionStructuredBlock      `json:"blocks"`
}

// SessionStructuredSystemEvent carries provider-neutral system-event metadata
// extracted from a provider transcript.
type SessionStructuredSystemEvent struct {
	Kind     string `json:"kind,omitempty"`
	Category string `json:"category,omitempty"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message,omitempty"`
}

// SessionStructuredUserPrompt carries provider-neutral prompt text and metadata
// extracted from a user message.
type SessionStructuredUserPrompt struct {
	Text          string                          `json:"text,omitempty"`
	OpenedFiles   []string                        `json:"opened_files,omitempty"`
	UploadedFiles []SessionStructuredUploadedFile `json:"uploaded_files,omitempty"`
	Selections    []SessionStructuredIDESelection `json:"selections,omitempty"`
}

// SessionStructuredUploadedFile is one uploaded-file attachment referenced by
// a user prompt.
type SessionStructuredUploadedFile struct {
	OriginalName string `json:"original_name,omitempty"`
	Size         string `json:"size,omitempty"`
	MIMEType     string `json:"mime_type,omitempty"`
	FilePath     string `json:"file_path,omitempty"`
	PreviewURL   string `json:"preview_url,omitempty"`
}

// SessionStructuredIDESelection is one IDE selection metadata item referenced
// by a user prompt.
type SessionStructuredIDESelection struct {
	Text string `json:"text,omitempty"`
}

// SessionStructuredUsage is provider-neutral token usage for one structured
// transcript message.
type SessionStructuredUsage struct {
	InputTokens         int `json:"input_tokens,omitempty"`
	OutputTokens        int `json:"output_tokens,omitempty"`
	ReasoningTokens     int `json:"reasoning_tokens,omitempty"`
	CacheReadTokens     int `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int `json:"cache_creation_tokens,omitempty"`
	ContextWindowTokens int `json:"context_window_tokens,omitempty"`
	ContextUsedTokens   int `json:"context_used_tokens,omitempty"`
	ContextPercent      int `json:"context_percent,omitempty"`
}

// SessionStructuredBlock is one structured content/tool/interaction block.
type SessionStructuredBlock struct {
	Type        string                        `json:"type"`
	Text        string                        `json:"text,omitempty"`
	Thinking    string                        `json:"thinking,omitempty"`
	Signature   string                        `json:"signature,omitempty"`
	ID          string                        `json:"id,omitempty"`
	ToolCallID  string                        `json:"tool_call_id,omitempty"`
	Name        string                        `json:"name,omitempty"`
	FilePath    string                        `json:"file_path,omitempty"`
	ImageURL    string                        `json:"image_url,omitempty"`
	MIMEType    string                        `json:"mime_type,omitempty"`
	Input       *SessionStructuredToolInput   `json:"input,omitempty"`
	Content     string                        `json:"content,omitempty"`
	IsError     bool                          `json:"is_error,omitempty"`
	Structured  *SessionStructuredToolResult  `json:"structured,omitempty"`
	Interaction *SessionStructuredInteraction `json:"interaction,omitempty"`
}

// SessionStructuredToolInput is a provider-neutral projection of a tool call's
// input. Provider-native input JSON is available only through format=raw.
type SessionStructuredToolInput struct {
	Kind          string                      `json:"kind,omitempty" doc:"Provider-neutral input kind such as command, code, patch, glob, fetch, search, file, arguments, or text."`
	Text          string                      `json:"text,omitempty"`
	Command       string                      `json:"command,omitempty"`
	LinkedCommand string                      `json:"linked_command,omitempty"`
	Code          string                      `json:"code,omitempty"`
	Patch         string                      `json:"patch,omitempty"`
	FilePath      string                      `json:"file_path,omitempty"`
	Language      string                      `json:"language,omitempty"`
	URL           string                      `json:"url,omitempty"`
	Prompt        string                      `json:"prompt,omitempty"`
	TaskID        string                      `json:"task_id,omitempty"`
	TaskType      string                      `json:"task_type,omitempty"`
	TaskStatus    string                      `json:"task_status,omitempty"`
	Description   string                      `json:"description,omitempty"`
	Question      string                      `json:"question,omitempty"`
	Options       []string                    `json:"options,omitempty"`
	Query         string                      `json:"query,omitempty"`
	Pattern       string                      `json:"pattern,omitempty"`
	Plan          string                      `json:"plan,omitempty"`
	Explanation   string                      `json:"explanation,omitempty"`
	Steps         []SessionStructuredPlanStep `json:"steps,omitempty"`
	Todos         []SessionStructuredTodoItem `json:"todos,omitempty"`
	Arguments     []SessionStructuredArgument `json:"arguments,omitempty"`
}

// SessionStructuredArgument is one provider-neutral string argument.
type SessionStructuredArgument struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// SessionStructuredPlanStep is one provider-neutral plan step.
type SessionStructuredPlanStep struct {
	Step   string `json:"step,omitempty"`
	Status string `json:"status,omitempty"`
}

// SessionStructuredToolResult is a typed structured tool-result projection.
// The Kind field discriminates which fields are populated.
type SessionStructuredToolResult struct {
	Kind              string                              `json:"kind"`
	Text              string                              `json:"text,omitempty"`
	Command           string                              `json:"command,omitempty"`
	Stdout            string                              `json:"stdout,omitempty"`
	Stderr            string                              `json:"stderr,omitempty"`
	ExitCode          *int                                `json:"exit_code,omitempty"`
	Interrupted       bool                                `json:"interrupted,omitempty"`
	Truncated         bool                                `json:"truncated,omitempty"`
	IsImage           bool                                `json:"is_image,omitempty"`
	Mode              string                              `json:"mode,omitempty"`
	Query             string                              `json:"query,omitempty"`
	URL               string                              `json:"url,omitempty"`
	TaskID            string                              `json:"task_id,omitempty"`
	TaskType          string                              `json:"task_type,omitempty"`
	TaskStatus        string                              `json:"task_status,omitempty"`
	Description       string                              `json:"description,omitempty"`
	TotalDurationMs   int                                 `json:"total_duration_ms,omitempty"`
	TotalTokens       int                                 `json:"total_tokens,omitempty"`
	TotalToolUseCount int                                 `json:"total_tool_use_count,omitempty"`
	Output            string                              `json:"output,omitempty"`
	Question          string                              `json:"question,omitempty"`
	Questions         []SessionStructuredQuestion         `json:"questions,omitempty"`
	Answer            string                              `json:"answer,omitempty"`
	Options           []string                            `json:"options,omitempty"`
	Answers           []SessionStructuredArgument         `json:"answers,omitempty"`
	Counts            []SessionStructuredArgument         `json:"counts,omitempty"`
	StatusCode        int                                 `json:"status_code,omitempty"`
	StatusText        string                              `json:"status_text,omitempty"`
	Bytes             int                                 `json:"bytes,omitempty"`
	Filenames         []string                            `json:"filenames,omitempty"`
	NumFiles          int                                 `json:"num_files,omitempty"`
	NumResults        int                                 `json:"num_results,omitempty"`
	DurationMs        int                                 `json:"duration_ms,omitempty"`
	AppliedLimit      int                                 `json:"applied_limit,omitempty"`
	StdoutLines       int                                 `json:"stdout_lines,omitempty"`
	StderrLines       int                                 `json:"stderr_lines,omitempty"`
	Timestamp         string                              `json:"timestamp,omitempty"`
	ResultItems       []SessionStructuredSearchResultItem `json:"result_items,omitempty"`
	Content           string                              `json:"content,omitempty"`
	NumLines          int                                 `json:"num_lines,omitempty"`
	FilePath          string                              `json:"file_path,omitempty"`
	FilePaths         []string                            `json:"file_paths,omitempty"`
	Language          string                              `json:"language,omitempty"`
	Code              string                              `json:"code,omitempty"`
	Plan              string                              `json:"plan,omitempty"`
	Explanation       string                              `json:"explanation,omitempty"`
	Steps             []SessionStructuredPlanStep         `json:"steps,omitempty"`
	Patch             string                              `json:"patch,omitempty"`
	PatchHunks        []SessionStructuredPatchHunk        `json:"patch_hunks,omitempty"`
	OldString         string                              `json:"old_string,omitempty"`
	NewString         string                              `json:"new_string,omitempty"`
	OriginalFile      string                              `json:"original_file,omitempty"`
	ReplaceAll        *bool                               `json:"replace_all,omitempty"`
	UserModified      *bool                               `json:"user_modified,omitempty"`
	OldTodos          []SessionStructuredTodoItem         `json:"old_todos,omitempty"`
	NewTodos          []SessionStructuredTodoItem         `json:"new_todos,omitempty"`
	StartLine         int                                 `json:"start_line,omitempty"`
	TotalLines        int                                 `json:"total_lines,omitempty"`
	Error             *SessionStructuredToolError         `json:"error,omitempty"`
}

// SessionStructuredToolError is provider-neutral typed error data for a failed
// tool result.
type SessionStructuredToolError struct {
	Category   string `json:"category" enum:"user_rejection,user_rejection_with_reason,command_failure,file_error,validation_error,timeout,network_error,unknown" doc:"Provider-neutral category: user_rejection, user_rejection_with_reason, command_failure, file_error, validation_error, timeout, network_error, or unknown."`
	Message    string `json:"message,omitempty"`
	UserReason string `json:"user_reason,omitempty"`
}

// SessionStructuredPatchHunk is one provider-neutral unified diff hunk.
type SessionStructuredPatchHunk struct {
	FilePath string   `json:"file_path,omitempty"`
	OldStart int      `json:"old_start,omitempty"`
	OldLines int      `json:"old_lines,omitempty"`
	NewStart int      `json:"new_start,omitempty"`
	NewLines int      `json:"new_lines,omitempty"`
	Lines    []string `json:"lines,omitempty"`
}

// SessionStructuredSearchResultItem is one provider-neutral web/search result
// item.
type SessionStructuredSearchResultItem struct {
	Title   string `json:"title,omitempty"`
	URL     string `json:"url,omitempty"`
	Snippet string `json:"snippet,omitempty"`
}

// SessionStructuredQuestionOption is one provider-neutral selectable answer
// option.
type SessionStructuredQuestionOption struct {
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
}

// SessionStructuredQuestion is one provider-neutral user question.
type SessionStructuredQuestion struct {
	Question    string                            `json:"question,omitempty"`
	Header      string                            `json:"header,omitempty"`
	Options     []SessionStructuredQuestionOption `json:"options,omitempty"`
	MultiSelect bool                              `json:"multi_select,omitempty"`
}

// SessionStructuredTodoItem is one provider-neutral todo item.
type SessionStructuredTodoItem struct {
	ID         string `json:"id,omitempty"`
	Content    string `json:"content,omitempty"`
	Status     string `json:"status,omitempty"`
	ActiveForm string `json:"active_form,omitempty"`
	Priority   string `json:"priority,omitempty"`
}

// SessionStructuredInteraction is a provider-neutral required interaction
// embedded in normalized history.
type SessionStructuredInteraction struct {
	RequestID string   `json:"request_id,omitempty"`
	Kind      string   `json:"kind,omitempty"`
	State     string   `json:"state"`
	Prompt    string   `json:"prompt,omitempty"`
	Options   []string `json:"options,omitempty"`
	Action    string   `json:"action,omitempty"`
}

func structuredHistoryFromSnapshot(snapshot *worker.HistorySnapshot) *SessionStructuredHistory {
	if snapshot == nil {
		return nil
	}
	diagnostics := make([]SessionStructuredDiagnostic, 0, len(snapshot.Diagnostics))
	for _, d := range snapshot.Diagnostics {
		diagnostics = append(diagnostics, SessionStructuredDiagnostic{
			Code:    d.Code,
			Message: d.Message,
			Count:   d.Count,
		})
	}
	return &SessionStructuredHistory{
		GCSessionID:           snapshot.GCSessionID,
		LogicalConversationID: snapshot.LogicalConversationID,
		ProviderSessionID:     snapshot.ProviderSessionID,
		TranscriptStreamID:    opaqueTranscriptStreamID(snapshot),
		Generation: SessionStructuredGeneration{
			ID: opaqueGenerationID(snapshot.Generation.ID),
		},
		Cursor: SessionStructuredCursor{
			AfterEntryID: snapshot.Cursor.AfterEntryID,
		},
		Continuity: SessionStructuredContinuity{
			Status:          string(snapshot.Continuity.Status),
			CompactionCount: snapshot.Continuity.CompactionCount,
			HasBranches:     snapshot.Continuity.HasBranches,
			Note:            snapshot.Continuity.Note,
		},
		TailState: SessionStructuredTailState{
			Activity:              string(snapshot.TailState.Activity),
			LastEntryID:           snapshot.TailState.LastEntryID,
			OpenToolCallIDs:       append([]string(nil), snapshot.TailState.OpenToolUseIDs...),
			PendingInteractionIDs: append([]string(nil), snapshot.TailState.PendingInteractionIDs...),
			Degraded:              snapshot.TailState.Degraded,
			DegradedReason:        snapshot.TailState.DegradedReason,
		},
		Diagnostics: diagnostics,
	}
}

// opaqueTranscriptStreamID derives a stable, path-free wire identity for a
// transcript stream. The worker's HistorySnapshot.TranscriptStreamID is the
// absolute server-side transcript file path, which must never reach the
// structured wire: it discloses the OS username, the on-disk directory layout,
// the project working directory, and the provider session UUID. Hashing the
// path together with the provider and logical conversation IDs yields an
// identifier that is stable for a given stream and changes when the transcript
// rotates to a new path — all a client needs for stream identity — while
// revealing none of the underlying filesystem detail.
func opaqueTranscriptStreamID(snapshot *worker.HistorySnapshot) string {
	if snapshot == nil {
		return ""
	}
	identity := snapshot.TranscriptStreamID + "\x00" + snapshot.ProviderSessionID + "\x00" + snapshot.LogicalConversationID
	return sha256Hex([]byte(identity))
}

// opaqueGenerationID hashes the raw generation token (the worker records it as
// "<mtime>:<size>" file-observation evidence) so the wire keeps a per-generation
// change discriminator without disclosing the transcript file's modification
// time or size. Generation is deliberately excluded from the projection hash
// (it is not transcript identity), so the wire has no need for the raw values.
// An empty token stays empty.
func opaqueGenerationID(raw string) string {
	if raw == "" {
		return ""
	}
	return sha256Hex([]byte("generation\x00" + raw))
}

func structuredFallbackHistory(sessionID, providerSessionID, activity string) *SessionStructuredHistory {
	if sessionID == "" {
		sessionID = "unknown"
	}
	if providerSessionID == "" {
		providerSessionID = sessionID
	}
	if activity == "" {
		activity = string(worker.TailActivityUnknown)
	}
	streamID := "fallback:" + sessionID
	return &SessionStructuredHistory{
		GCSessionID:           sessionID,
		LogicalConversationID: sessionID,
		ProviderSessionID:     providerSessionID,
		TranscriptStreamID:    streamID,
		Generation: SessionStructuredGeneration{
			ID: streamID,
		},
		Continuity: SessionStructuredContinuity{
			Status: string(worker.ContinuityStatusDegraded),
			Note:   structuredTranscriptUnavailableMessage,
		},
		TailState: SessionStructuredTailState{
			Activity:       activity,
			Degraded:       true,
			DegradedReason: structuredTranscriptUnavailableMessage,
		},
		Diagnostics: []SessionStructuredDiagnostic{{
			Code:    structuredTranscriptUnavailableCode,
			Message: structuredTranscriptUnavailableMessage,
			Count:   1,
		}},
	}
}

func structuredFallbackMessages(sessionID, provider, text string) []SessionStructuredMessage {
	if strings.TrimSpace(text) == "" {
		return []SessionStructuredMessage{}
	}
	if sessionID == "" {
		sessionID = "unknown"
	}
	return []SessionStructuredMessage{{
		ID:       "fallback:" + sessionID + ":1",
		Role:     "assistant",
		Provider: provider,
		Status:   string(worker.ResultStatusPartial),
		Blocks: []SessionStructuredBlock{{
			Type: string(worker.BlockKindText),
			Text: text,
		}},
	}}
}

func historySnapshotStructuredMessages(snapshot *worker.HistorySnapshot, includeThinking bool) ([]SessionStructuredMessage, []string) {
	if snapshot == nil {
		return []SessionStructuredMessage{}, []string{}
	}
	messages := make([]SessionStructuredMessage, 0, len(snapshot.Entries))
	ids := make([]string, 0, len(snapshot.Entries))
	for _, entry := range snapshot.Entries {
		msg := historyEntryToStructuredMessage(entry, includeThinking)
		if len(msg.Blocks) == 0 && msg.Role == "" {
			continue
		}
		messages = append(messages, msg)
		ids = append(ids, entry.ID)
	}
	return messages, ids
}

func historyEntryToStructuredMessage(entry worker.HistoryEntry, includeThinking bool) SessionStructuredMessage {
	role := sessionStructuredMessageRole(entry.Actor)
	msg := SessionStructuredMessage{
		ID:       entry.ID,
		Role:     role,
		Provider: entry.Provenance.Provider,
		Status:   sessionStructuredMessageStatus(entry.Status),
		Blocks:   make([]SessionStructuredBlock, 0, len(entry.Blocks)),
	}
	switch role {
	case string(worker.ActorAssistant):
		msg.Model = entry.Model
		msg.StopReason = entry.StopReason
		msg.Usage = sessionStructuredUsageFromWorker(entry.Usage)
	case string(worker.ActorUser):
		msg.UserPrompt = sessionStructuredUserPromptFromWorker(entry.UserPrompt)
	case string(worker.ActorSystem):
		msg.SystemEvent = sessionStructuredSystemEventFromWorker(entry.SystemEvent)
	case string(worker.ActorUnknown):
		msg.Model = entry.Model
		msg.StopReason = entry.StopReason
		msg.Usage = sessionStructuredUsageFromWorker(entry.Usage)
		msg.UserPrompt = sessionStructuredUserPromptFromWorker(entry.UserPrompt)
		msg.SystemEvent = sessionStructuredSystemEventFromWorker(entry.SystemEvent)
	}
	if entry.Timestamp != nil {
		msg.Timestamp = entry.Timestamp.Format(time.RFC3339Nano)
	}
	for _, block := range entry.Blocks {
		if structured := historyBlockToStructuredBlock(block, includeThinking); structured != nil {
			msg.Blocks = append(msg.Blocks, *structured)
		}
	}
	return msg
}

func sessionStructuredSystemEventFromWorker(event *worker.HistorySystemEvent) *SessionStructuredSystemEvent {
	if event == nil {
		return nil
	}
	return &SessionStructuredSystemEvent{
		Kind:     event.Kind,
		Category: event.Category,
		Code:     event.Code,
		Message:  event.Message,
	}
}

func sessionStructuredUserPromptFromWorker(prompt *worker.HistoryUserPrompt) *SessionStructuredUserPrompt {
	if prompt == nil {
		return nil
	}
	return &SessionStructuredUserPrompt{
		Text:          prompt.Text,
		OpenedFiles:   append([]string(nil), prompt.OpenedFiles...),
		UploadedFiles: sessionStructuredUploadedFilesFromWorker(prompt.UploadedFiles),
		Selections:    sessionStructuredIDESelectionsFromWorker(prompt.Selections),
	}
}

func sessionStructuredUploadedFilesFromWorker(files []worker.HistoryUploadedFile) []SessionStructuredUploadedFile {
	if len(files) == 0 {
		return nil
	}
	out := make([]SessionStructuredUploadedFile, 0, len(files))
	for _, file := range files {
		out = append(out, SessionStructuredUploadedFile{
			OriginalName: file.OriginalName,
			Size:         file.Size,
			MIMEType:     file.MIMEType,
			FilePath:     file.FilePath,
			PreviewURL:   file.PreviewURL,
		})
	}
	return out
}

func sessionStructuredIDESelectionsFromWorker(selections []worker.HistoryUserSelection) []SessionStructuredIDESelection {
	if len(selections) == 0 {
		return nil
	}
	out := make([]SessionStructuredIDESelection, 0, len(selections))
	for _, selection := range selections {
		out = append(out, SessionStructuredIDESelection{Text: selection.Text})
	}
	return out
}

func sessionStructuredUsageFromWorker(usage *worker.HistoryUsage) *SessionStructuredUsage {
	if usage == nil {
		return nil
	}
	return &SessionStructuredUsage{
		InputTokens:         usage.InputTokens,
		OutputTokens:        usage.OutputTokens,
		ReasoningTokens:     usage.ReasoningTokens,
		CacheReadTokens:     usage.CacheReadTokens,
		CacheCreationTokens: usage.CacheCreationTokens,
		ContextWindowTokens: usage.ContextWindowTokens,
		ContextUsedTokens:   usage.ContextUsedTokens,
		ContextPercent:      usage.ContextPercent,
	}
}

func historyBlockToStructuredBlock(block worker.HistoryBlock, includeThinking bool) *SessionStructuredBlock {
	out := &SessionStructuredBlock{Type: sessionStructuredBlockType(block.Kind)}
	switch block.Kind {
	case worker.BlockKindText:
		out.Text = block.Text
	case worker.BlockKindThinking:
		if includeThinking {
			out.Thinking = block.Text
			out.Signature = block.Signature
		}
	case worker.BlockKindToolUse:
		out.ID = block.ToolUseID
		out.Name = block.Name
		out.FilePath = block.FilePath
		out.Input = sessionStructuredToolInputFromWorker(block.StructuredInput)
	case worker.BlockKindToolResult:
		out.ToolCallID = block.ToolUseID
		out.Name = block.Name
		out.FilePath = block.FilePath
		out.Content = block.ContentText
		if out.Content == "" {
			out.Content = block.Text
		}
		out.IsError = block.IsError
		out.Structured = sessionStructuredToolResultFromWorker(block.StructuredResult)
	case worker.BlockKindInteraction:
		out.Interaction = structuredInteraction(block.Interaction)
	case worker.BlockKindImage:
		out.Text = block.Text
		out.FilePath = block.FilePath
		out.ImageURL = block.ImageURL
		out.MIMEType = block.MIMEType
	default:
		out.Text = block.Text
		if includeThinking {
			out.Signature = block.Signature
		}
		out.ToolCallID = block.ToolUseID
		out.Name = block.Name
		out.FilePath = block.FilePath
		out.ImageURL = block.ImageURL
		out.MIMEType = block.MIMEType
		out.Input = sessionStructuredToolInputFromWorker(block.StructuredInput)
		out.Content = block.ContentText
		out.IsError = block.IsError
		out.Interaction = structuredInteraction(block.Interaction)
	}
	return out
}

func sessionStructuredToolInputFromWorker(input *worker.StructuredToolInput) *SessionStructuredToolInput {
	if input == nil {
		return nil
	}
	out := &SessionStructuredToolInput{
		Kind:          sessionStructuredToolInputKind(input.Kind),
		Text:          input.Text,
		Command:       input.Command,
		LinkedCommand: input.LinkedCommand,
		Code:          input.Code,
		Patch:         input.Patch,
		FilePath:      input.FilePath,
		Language:      input.Language,
		URL:           input.URL,
		Prompt:        input.Prompt,
		TaskID:        input.TaskID,
		TaskType:      input.TaskType,
		TaskStatus:    input.TaskStatus,
		Description:   input.Description,
		Question:      input.Question,
		Options:       append([]string(nil), input.Options...),
		Query:         input.Query,
		Pattern:       input.Pattern,
		Plan:          input.Plan,
		Explanation:   input.Explanation,
		Steps:         sessionStructuredPlanStepsFromWorker(input.Steps),
		Todos:         sessionStructuredTodosFromWorker(input.Todos),
	}
	if len(input.Arguments) > 0 {
		out.Arguments = sessionStructuredArgumentsFromWorker(input.Arguments)
	}
	return narrowSessionStructuredToolInput(out)
}

func narrowSessionStructuredToolInput(input *SessionStructuredToolInput) *SessionStructuredToolInput {
	if input == nil || input.Kind == "unknown" {
		return input
	}
	out := &SessionStructuredToolInput{Kind: input.Kind}
	switch input.Kind {
	case "command":
		out.Command, out.Arguments = input.Command, input.Arguments
	case "stdin":
		out.TaskID, out.Text, out.LinkedCommand = input.TaskID, input.Text, input.LinkedCommand
	case "code":
		out.Code, out.Language = input.Code, input.Language
	case "patch":
		out.Patch, out.FilePath, out.Language = input.Patch, input.FilePath, input.Language
	case "write":
		out.FilePath, out.Language, out.Text = input.FilePath, input.Language, input.Text
	case "glob":
		out.Pattern, out.Query, out.FilePath, out.Arguments = input.Pattern, input.Query, input.FilePath, input.Arguments
	case "fetch":
		out.URL, out.Prompt = input.URL, input.Prompt
	case "search":
		out.Query, out.Pattern, out.FilePath, out.Command = input.Query, input.Pattern, input.FilePath, input.Command
		out.Arguments = input.Arguments
	case "file":
		out.FilePath, out.Language, out.Command = input.FilePath, input.Language, input.Command
	case "todo":
		out.Todos = input.Todos
	case "plan":
		out.Plan, out.Explanation, out.Steps = input.Plan, input.Explanation, input.Steps
	case "question":
		out.Question, out.Options = input.Question, input.Options
	case "task":
		out.TaskID, out.TaskType, out.TaskStatus = input.TaskID, input.TaskType, input.TaskStatus
		out.Description, out.Prompt = input.Description, input.Prompt
	case "text":
		out.Text = input.Text
	case "arguments":
		out.Arguments = input.Arguments
	default:
		return &SessionStructuredToolInput{Kind: "unknown"}
	}
	return out
}

func sessionStructuredArgumentsFromWorker(args []worker.StructuredArgument) []SessionStructuredArgument {
	if len(args) == 0 {
		return nil
	}
	out := make([]SessionStructuredArgument, 0, len(args))
	for _, arg := range args {
		out = append(out, SessionStructuredArgument{
			Name:  arg.Name,
			Value: arg.Value,
		})
	}
	return out
}

func sessionStructuredToolResultFromWorker(result *worker.StructuredToolResult) *SessionStructuredToolResult {
	if result == nil {
		return nil
	}
	out := &SessionStructuredToolResult{
		Kind:              sessionStructuredToolResultKind(result.Kind),
		Text:              result.Text,
		Command:           result.Command,
		Stdout:            result.Stdout,
		Stderr:            result.Stderr,
		ExitCode:          result.ExitCode,
		Interrupted:       result.Interrupted,
		Truncated:         result.Truncated,
		IsImage:           result.IsImage,
		Mode:              result.Mode,
		Query:             result.Query,
		URL:               result.URL,
		TaskID:            result.TaskID,
		TaskType:          result.TaskType,
		TaskStatus:        result.TaskStatus,
		Description:       result.Description,
		TotalDurationMs:   result.TotalDurationMs,
		TotalTokens:       result.TotalTokens,
		TotalToolUseCount: result.TotalToolUseCount,
		Output:            result.Output,
		Question:          result.Question,
		Questions:         sessionStructuredQuestionsFromWorker(result.Questions),
		Answer:            result.Answer,
		Options:           append([]string(nil), result.Options...),
		Answers:           sessionStructuredArgumentsFromWorker(result.Answers),
		Counts:            sessionStructuredArgumentsFromWorker(result.Counts),
		StatusCode:        result.StatusCode,
		StatusText:        result.StatusText,
		Bytes:             result.Bytes,
		Filenames:         append([]string(nil), result.Filenames...),
		NumFiles:          result.NumFiles,
		NumResults:        result.NumResults,
		DurationMs:        result.DurationMs,
		AppliedLimit:      result.AppliedLimit,
		StdoutLines:       result.StdoutLines,
		StderrLines:       result.StderrLines,
		Timestamp:         result.Timestamp,
		ResultItems:       sessionStructuredSearchResultItemsFromWorker(result.ResultItems),
		Content:           result.Content,
		NumLines:          result.NumLines,
		FilePath:          result.FilePath,
		FilePaths:         append([]string(nil), result.FilePaths...),
		Language:          result.Language,
		Code:              result.Code,
		Plan:              result.Plan,
		Explanation:       result.Explanation,
		Steps:             sessionStructuredPlanStepsFromWorker(result.Steps),
		Patch:             result.Patch,
		PatchHunks:        sessionStructuredPatchHunksFromWorker(result.PatchHunks),
		OldString:         result.OldString,
		NewString:         result.NewString,
		OriginalFile:      result.OriginalFile,
		ReplaceAll:        result.ReplaceAll,
		UserModified:      result.UserModified,
		OldTodos:          sessionStructuredTodosFromWorker(result.OldTodos),
		NewTodos:          sessionStructuredTodosFromWorker(result.NewTodos),
		StartLine:         result.StartLine,
		TotalLines:        result.TotalLines,
		Error:             sessionStructuredToolErrorFromWorker(result.Error),
	}
	return narrowSessionStructuredToolResult(out)
}

func narrowSessionStructuredToolResult(result *SessionStructuredToolResult) *SessionStructuredToolResult {
	if result == nil || result.Kind == "unknown" {
		return result
	}
	out := &SessionStructuredToolResult{Kind: result.Kind}
	switch result.Kind {
	case "bash":
		out.Text, out.Command, out.Stdout, out.Stderr = result.Text, result.Command, result.Stdout, result.Stderr
		out.ExitCode, out.Interrupted, out.Truncated, out.IsImage = result.ExitCode, result.Interrupted, result.Truncated, result.IsImage
		out.TaskID, out.TaskStatus = result.TaskID, result.TaskStatus
		out.StdoutLines, out.StderrLines, out.Timestamp = result.StdoutLines, result.StderrLines, result.Timestamp
		out.Content, out.NumLines, out.Error = result.Content, result.NumLines, result.Error
	case "python":
		out.Text, out.Code, out.Stdout, out.Stderr = result.Text, result.Code, result.Stdout, result.Stderr
		out.ExitCode, out.Interrupted, out.Truncated, out.IsImage = result.ExitCode, result.Interrupted, result.Truncated, result.IsImage
		out.Error = result.Error
	case "read":
		out.FilePath, out.Language, out.Content = result.FilePath, result.Language, result.Content
		out.NumLines, out.StartLine, out.TotalLines, out.Error = result.NumLines, result.StartLine, result.TotalLines, result.Error
	case "glob":
		out.Filenames, out.NumFiles, out.DurationMs = result.Filenames, result.NumFiles, result.DurationMs
		out.Truncated, out.Content, out.NumLines, out.Error = result.Truncated, result.Content, result.NumLines, result.Error
	case "grep", "search":
		out.Mode, out.Query, out.Filenames = result.Mode, result.Query, result.Filenames
		out.NumFiles, out.NumResults, out.Counts = result.NumFiles, result.NumResults, result.Counts
		out.DurationMs, out.AppliedLimit, out.ResultItems = result.DurationMs, result.AppliedLimit, result.ResultItems
		out.Content, out.NumLines, out.Error = result.Content, result.NumLines, result.Error
	case "fetch":
		out.Text, out.URL, out.StatusCode, out.StatusText = result.Text, result.URL, result.StatusCode, result.StatusText
		out.Bytes, out.DurationMs, out.Content, out.NumLines = result.Bytes, result.DurationMs, result.Content, result.NumLines
		out.Error = result.Error
	case "todo":
		out.Text, out.Content, out.OldTodos, out.NewTodos = result.Text, result.Content, result.OldTodos, result.NewTodos
		out.Error = result.Error
	case "plan":
		out.Text, out.Content, out.Plan, out.Explanation = result.Text, result.Content, result.Plan, result.Explanation
		out.Steps, out.Error = result.Steps, result.Error
	case "question":
		out.Text, out.Content, out.Question = result.Text, result.Content, result.Question
		out.Questions, out.Answer, out.Options, out.Answers = result.Questions, result.Answer, result.Options, result.Answers
		out.Error = result.Error
	case "stdin":
		out.Text, out.TaskID, out.Content, out.NumLines = result.Text, result.TaskID, result.Content, result.NumLines
		out.Error = result.Error
	case "task":
		out.Text, out.TaskID, out.TaskType, out.TaskStatus = result.Text, result.TaskID, result.TaskType, result.TaskStatus
		out.Description, out.TotalDurationMs, out.TotalTokens = result.Description, result.TotalDurationMs, result.TotalTokens
		out.TotalToolUseCount, out.Output = result.TotalToolUseCount, result.Output
		out.Stdout, out.Stderr, out.ExitCode, out.Content = result.Stdout, result.Stderr, result.ExitCode, result.Content
		out.Error = result.Error
	case "write":
		out.Text, out.FilePath, out.FilePaths, out.Language = result.Text, result.FilePath, result.FilePaths, result.Language
		out.Content, out.NumLines, out.Patch, out.PatchHunks = result.Content, result.NumLines, result.Patch, result.PatchHunks
		out.StartLine, out.TotalLines, out.Error = result.StartLine, result.TotalLines, result.Error
	case "edit":
		out.FilePath, out.FilePaths, out.Patch, out.PatchHunks = result.FilePath, result.FilePaths, result.Patch, result.PatchHunks
		out.OldString, out.NewString, out.OriginalFile = result.OldString, result.NewString, result.OriginalFile
		out.ReplaceAll, out.UserModified, out.Content, out.Error = result.ReplaceAll, result.UserModified, result.Content, result.Error
	case "text":
		out.Text, out.Content, out.Error = result.Text, result.Content, result.Error
	default:
		return &SessionStructuredToolResult{Kind: "unknown"}
	}
	return out
}

func sessionStructuredToolErrorFromWorker(err *worker.StructuredToolError) *SessionStructuredToolError {
	if err == nil {
		return nil
	}
	return &SessionStructuredToolError{
		Category:   err.Category,
		Message:    err.Message,
		UserReason: err.UserReason,
	}
}

func sessionStructuredQuestionsFromWorker(questions []worker.StructuredQuestion) []SessionStructuredQuestion {
	if len(questions) == 0 {
		return nil
	}
	out := make([]SessionStructuredQuestion, 0, len(questions))
	for _, question := range questions {
		out = append(out, SessionStructuredQuestion{
			Question:    question.Question,
			Header:      question.Header,
			Options:     sessionStructuredQuestionOptionsFromWorker(question.Options),
			MultiSelect: question.MultiSelect,
		})
	}
	return out
}

func sessionStructuredQuestionOptionsFromWorker(options []worker.StructuredQuestionOption) []SessionStructuredQuestionOption {
	if len(options) == 0 {
		return nil
	}
	out := make([]SessionStructuredQuestionOption, 0, len(options))
	for _, option := range options {
		out = append(out, SessionStructuredQuestionOption{
			Label:       option.Label,
			Description: option.Description,
		})
	}
	return out
}

func sessionStructuredSearchResultItemsFromWorker(items []worker.StructuredSearchResultItem) []SessionStructuredSearchResultItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]SessionStructuredSearchResultItem, 0, len(items))
	for _, item := range items {
		out = append(out, SessionStructuredSearchResultItem{
			Title:   item.Title,
			URL:     item.URL,
			Snippet: item.Snippet,
		})
	}
	return out
}

func sessionStructuredPlanStepsFromWorker(steps []worker.StructuredPlanStep) []SessionStructuredPlanStep {
	if len(steps) == 0 {
		return nil
	}
	out := make([]SessionStructuredPlanStep, 0, len(steps))
	for _, step := range steps {
		out = append(out, SessionStructuredPlanStep{
			Step:   step.Step,
			Status: step.Status,
		})
	}
	return out
}

func sessionStructuredPatchHunksFromWorker(hunks []worker.StructuredPatchHunk) []SessionStructuredPatchHunk {
	if len(hunks) == 0 {
		return nil
	}
	out := make([]SessionStructuredPatchHunk, 0, len(hunks))
	for _, hunk := range hunks {
		out = append(out, SessionStructuredPatchHunk{
			FilePath: hunk.FilePath,
			OldStart: hunk.OldStart,
			OldLines: hunk.OldLines,
			NewStart: hunk.NewStart,
			NewLines: hunk.NewLines,
			Lines:    append([]string(nil), hunk.Lines...),
		})
	}
	return out
}

func sessionStructuredTodosFromWorker(todos []worker.StructuredTodoItem) []SessionStructuredTodoItem {
	if len(todos) == 0 {
		return nil
	}
	out := make([]SessionStructuredTodoItem, 0, len(todos))
	for _, todo := range todos {
		out = append(out, SessionStructuredTodoItem{
			ID:         todo.ID,
			Content:    todo.Content,
			Status:     todo.Status,
			ActiveForm: todo.ActiveForm,
			Priority:   todo.Priority,
		})
	}
	return out
}

func structuredInteraction(in *worker.HistoryInteraction) *SessionStructuredInteraction {
	if in == nil {
		return nil
	}
	return &SessionStructuredInteraction{
		RequestID: in.RequestID,
		Kind:      in.Kind,
		State:     string(in.State),
		Prompt:    in.Prompt,
		Options:   append([]string(nil), in.Options...),
		Action:    in.Action,
	}
}
