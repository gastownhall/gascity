package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/sessionlog"
	workertranscript "github.com/gastownhall/gascity/internal/worker/transcript"
)

// LoadRequest scopes a Phase 1 transcript load.
type LoadRequest struct {
	Provider              string
	TranscriptPath        string
	GCSessionID           string
	LogicalConversationID string
	TailCompactions       int
}

// TranscriptRequest scopes provider-native transcript reads that preserve raw
// pagination and entry fidelity for higher-level API/CLI adapters.
type TranscriptRequest struct {
	Provider        string
	TranscriptPath  string
	TailCompactions int
	BeforeEntryID   string
	AfterEntryID    string
	Raw             bool
}

// TranscriptResult wraps a provider-native transcript read behind the worker
// boundary so callers do not depend on sessionlog directly for file discovery.
type TranscriptResult struct {
	Provider       string
	TranscriptPath string
	Session        *sessionlog.Session
	RawMessages    []json.RawMessage
}

// AgentTranscriptResult wraps a provider-native subagent transcript so callers
// do not depend on sessionlog discovery helpers directly.
type AgentTranscriptResult struct {
	TranscriptPath string
	Session        *sessionlog.AgentSession
	RawMessages    []json.RawMessage
}

// SessionLogAdapter exposes the normalized transcript contract while keeping
// sessionlog as the only production transcript parser in Phase 1.
type SessionLogAdapter struct {
	SearchPaths []string
}

// DiscoverTranscript returns the best available transcript path for a worker.
func (a SessionLogAdapter) DiscoverTranscript(provider, workDir, gcSessionID string) string {
	if strings.TrimSpace(gcSessionID) != "" {
		if path := workertranscript.DiscoverKeyedPath(a.SearchPaths, provider, workDir, gcSessionID); path != "" {
			return path
		}
		if path := workertranscript.DiscoverFallbackPath(a.SearchPaths, provider, workDir, gcSessionID); path != "" {
			return path
		}
	}
	return workertranscript.DiscoverPath(a.SearchPaths, provider, workDir, gcSessionID)
}

// DiscoverWorkDirTranscript resolves the best provider-specific transcript for
// a workdir without requiring a stable session identifier.
func (a SessionLogAdapter) DiscoverWorkDirTranscript(provider, workDir string) string {
	return workertranscript.DiscoverPath(a.SearchPaths, provider, workDir, "")
}

// TailMeta reads model/context metadata from a discovered transcript path.
func (a SessionLogAdapter) TailMeta(path string) (*sessionlog.TailMeta, error) {
	return sessionlog.ExtractTailMetaFromSearchPaths(a.SearchPaths, path)
}

// TailActivity reads the transcript tail activity without loading full history.
func (a SessionLogAdapter) TailActivity(path string) (TailActivity, error) {
	meta, err := a.TailMeta(path)
	if err != nil {
		return TailActivityUnknown, err
	}
	if meta == nil {
		return TailActivityUnknown, nil
	}
	switch strings.TrimSpace(meta.Activity) {
	case string(TailActivityIdle):
		return TailActivityIdle, nil
	case "in-turn":
		return TailActivityInTurn, nil
	default:
		return TailActivityUnknown, nil
	}
}

// AgentMappings lists subagent transcript mappings for a parent transcript.
func (a SessionLogAdapter) AgentMappings(path string) ([]sessionlog.AgentMapping, error) {
	return sessionlog.FindAgentMappings(strings.TrimSpace(path))
}

// ReadAgentTranscript loads a subagent transcript while preserving raw
// message fidelity for worker-owned API surfaces.
func (a SessionLogAdapter) ReadAgentTranscript(path, agentID string) (*AgentTranscriptResult, error) {
	sess, err := sessionlog.ReadAgentSession(strings.TrimSpace(path), strings.TrimSpace(agentID))
	if err != nil {
		return nil, err
	}
	result := &AgentTranscriptResult{
		TranscriptPath: filepath.Clean(path),
		Session:        sess,
		RawMessages:    make([]json.RawMessage, 0, len(sess.Messages)),
	}
	for _, entry := range sess.Messages {
		if len(entry.Raw) > 0 {
			result.RawMessages = append(result.RawMessages, entry.Raw)
		}
	}
	return result, nil
}

// ReadTranscript loads a provider transcript while preserving raw pagination
// and message fidelity for worker-owned API/CLI surfaces.
func (a SessionLogAdapter) ReadTranscript(req TranscriptRequest) (*TranscriptResult, error) {
	path := strings.TrimSpace(req.TranscriptPath)
	if path == "" {
		return nil, fmt.Errorf("transcript path is required")
	}

	var (
		sess *sessionlog.Session
		err  error
	)
	beforeID := strings.TrimSpace(req.BeforeEntryID)
	afterID := strings.TrimSpace(req.AfterEntryID)
	switch {
	case req.Raw && afterID != "":
		sess, err = sessionlog.ReadProviderFileRawNewer(req.Provider, path, req.TailCompactions, afterID)
	case req.Raw && beforeID != "":
		sess, err = sessionlog.ReadProviderFileRawOlder(req.Provider, path, req.TailCompactions, beforeID)
	case req.Raw:
		sess, err = sessionlog.ReadProviderFileRaw(req.Provider, path, req.TailCompactions)
	case afterID != "":
		sess, err = sessionlog.ReadProviderFileNewer(req.Provider, path, req.TailCompactions, afterID)
	case beforeID != "":
		sess, err = sessionlog.ReadProviderFileOlder(req.Provider, path, req.TailCompactions, beforeID)
	default:
		sess, err = sessionlog.ReadProviderFile(req.Provider, path, req.TailCompactions)
	}
	if err != nil {
		return nil, err
	}

	result := &TranscriptResult{
		Provider:       req.Provider,
		TranscriptPath: filepath.Clean(path),
		Session:        sess,
	}
	if req.Raw && sess != nil {
		result.RawMessages = make([]json.RawMessage, 0, len(sess.Messages))
		for _, entry := range sess.Messages {
			if len(entry.Raw) > 0 {
				result.RawMessages = append(result.RawMessages, entry.Raw)
			}
		}
	}
	return result, nil
}

// LoadHistory loads and normalizes a provider transcript.
func (a SessionLogAdapter) LoadHistory(req LoadRequest) (*HistorySnapshot, error) {
	path := strings.TrimSpace(req.TranscriptPath)
	if path == "" {
		return nil, fmt.Errorf("transcript path is required")
	}

	session, err := sessionlog.ReadProviderFileRaw(req.Provider, path, req.TailCompactions)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat transcript: %w", err)
	}

	entries := make([]HistoryEntry, 0, len(session.Messages))
	compactionCount := 0
	lastEntryID := ""
	for idx, entry := range session.Messages {
		normalized := normalizeEntry(req.Provider, path, session.ID, idx, entry)
		if normalized.ID != "" {
			lastEntryID = normalized.ID
		}
		if entry.IsCompactBoundary() {
			compactionCount++
		}
		entries = append(entries, normalized)
	}

	tailMeta, err := sessionlog.ExtractTailMeta(path)
	if err != nil {
		return nil, err
	}
	// Tail metadata is a heuristic fast path; full parser diagnostics are the
	// authority for degradation so large valid JSONL entries do not look torn.

	logicalConversationID := strings.TrimSpace(req.LogicalConversationID)
	if logicalConversationID == "" {
		logicalConversationID = firstNonEmpty(strings.TrimSpace(req.GCSessionID), session.ID)
	}

	openToolUseIDs := sortedKeys(session.OrphanedToolUseIDs)
	pendingIDs := pendingInteractionIDs(entries)
	diagnostics := historyDiagnostics(session.Diagnostics)
	continuity := Continuity{
		Status:          ContinuityStatusContinuous,
		CompactionCount: compactionCount,
		HasBranches:     session.HasBranches,
	}
	if compactionCount > 0 {
		continuity.Status = ContinuityStatusCompacted
	}
	if len(entries) == 0 {
		continuity.Status = ContinuityStatusUnknown
	}
	if len(diagnostics) > 0 {
		continuity.Note = diagnostics[0].Message
		if len(entries) > 0 {
			continuity.Status = ContinuityStatusDegraded
		}
	}
	tailDegradedReason := tailDegradedReason(session.Diagnostics)

	return &HistorySnapshot{
		GCSessionID:           req.GCSessionID,
		LogicalConversationID: logicalConversationID,
		ProviderSessionID:     session.ID,
		TranscriptStreamID:    filepath.Clean(path),
		Generation: Generation{
			ID:         fmt.Sprintf("%d:%d", info.ModTime().UnixNano(), info.Size()),
			ObservedAt: info.ModTime().UTC(),
		},
		Cursor: Cursor{
			AfterEntryID: lastEntryID,
		},
		Continuity: continuity,
		TailState: TailState{
			Activity:              tailActivity(tailMeta),
			LastEntryID:           lastEntryID,
			OpenToolUseIDs:        openToolUseIDs,
			PendingInteractionIDs: pendingIDs,
			Degraded:              tailDegradedReason != "",
			DegradedReason:        tailDegradedReason,
		},
		Diagnostics: diagnostics,
		Entries:     entries,
	}, nil
}

func normalizeEntry(provider, path, sessionID string, order int, entry *sessionlog.Entry) HistoryEntry {
	provenance := Provenance{
		Provider:          provider,
		TranscriptPath:    filepath.Clean(path),
		ProviderSessionID: sessionID,
		RawEntryID:        entry.UUID,
		RawType:           entry.Type,
		Raw:               cloneRaw(entry.Raw),
	}

	normalized := HistoryEntry{
		ID:         firstNonEmpty(entry.UUID, fmt.Sprintf("derived-%d", order)),
		Kind:       entry.Type,
		Actor:      actorForEntry(entry),
		Order:      order,
		Status:     ResultStatusFinal,
		Provenance: provenance,
	}
	if normalized.ID != entry.UUID {
		normalized.Provenance.Derived = true
	}
	if !entry.Timestamp.IsZero() {
		ts := entry.Timestamp.UTC()
		normalized.Timestamp = &ts
	}

	blocks := normalizeBlocks(entry)
	normalized.Blocks = blocks
	if normalized.Text == "" {
		normalized.Text = firstText(blocks)
	}
	return normalized
}

func normalizeBlocks(entry *sessionlog.Entry) []HistoryBlock {
	blocks := entry.ContentBlocks()
	if len(blocks) > 0 {
		result := make([]HistoryBlock, 0, len(blocks))
		for _, block := range blocks {
			kind := normalizeBlockKind(block.Type)
			var interaction *HistoryInteraction
			if kind == BlockKindInteraction {
				interaction = normalizeInteractionBlock(block)
			}
			toolUseID := firstNonEmpty(block.ToolUseID, block.ID)
			if kind == BlockKindInteraction {
				toolUseID = ""
			}
			result = append(result, HistoryBlock{
				Kind:        kind,
				Text:        block.Text,
				ToolUseID:   toolUseID,
				Name:        block.Name,
				Input:       cloneRaw(block.Input),
				Content:     cloneRaw(block.Content),
				IsError:     block.IsError,
				Interaction: interaction,
			})
		}
		return result
	}

	if text := strings.TrimSpace(entry.TextContent()); text != "" {
		return []HistoryBlock{{Kind: BlockKindText, Text: text}}
	}

	if entry.Type == "tool_result" && entry.ToolUseID != "" {
		return []HistoryBlock{{
			Kind:      BlockKindToolResult,
			ToolUseID: entry.ToolUseID,
			Derived:   true,
		}}
	}

	return nil
}

func actorForEntry(entry *sessionlog.Entry) Actor {
	switch strings.ToLower(strings.TrimSpace(entry.Type)) {
	case "assistant":
		return ActorAssistant
	case "user", "result":
		return ActorUser
	case "tool_result":
		return ActorTool
	case "system", "error":
		return ActorSystem
	}

	if len(entry.Message) > 0 {
		var message struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(entry.Message, &message); err == nil {
			switch strings.ToLower(strings.TrimSpace(message.Role)) {
			case "assistant":
				return ActorAssistant
			case "user":
				return ActorUser
			case "system":
				return ActorSystem
			}
		}
	}
	return ActorUnknown
}

func normalizeBlockKind(kind string) BlockKind {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "text":
		return BlockKindText
	case "thinking":
		return BlockKindThinking
	case "tool_use":
		return BlockKindToolUse
	case "tool_result":
		return BlockKindToolResult
	case "interaction":
		return BlockKindInteraction
	case "image":
		return BlockKindImage
	default:
		return BlockKindUnknown
	}
}

func tailActivity(meta *sessionlog.TailMeta) TailActivity {
	if meta == nil {
		return TailActivityUnknown
	}
	switch meta.Activity {
	case "idle":
		return TailActivityIdle
	case "in-turn":
		return TailActivityInTurn
	default:
		return TailActivityUnknown
	}
}

func historyDiagnostics(session sessionlog.SessionDiagnostics) []HistoryDiagnostic {
	malformedTail := session.MalformedTail
	if session.MalformedLineCount == 0 && !malformedTail {
		return nil
	}

	var diagnostics []HistoryDiagnostic
	if malformedTail {
		diagnostics = append(diagnostics, HistoryDiagnostic{
			Code:    "malformed_tail",
			Message: "transcript tail appears torn or malformed; normalized history is degraded",
			Count:   1,
		})
	}

	malformedInteriorCount := session.MalformedLineCount
	if malformedTail && malformedInteriorCount > 0 {
		malformedInteriorCount--
	}
	if malformedInteriorCount > 0 {
		diagnostics = append(diagnostics, HistoryDiagnostic{
			Code:    "malformed_jsonl",
			Message: "transcript contained malformed JSONL before the tail; normalized history is degraded",
			Count:   malformedInteriorCount,
		})
	}
	return diagnostics
}

func normalizeInteractionBlock(block sessionlog.ContentBlock) *HistoryInteraction {
	state := normalizeInteractionState(block.State)
	return &HistoryInteraction{
		RequestID: firstNonEmpty(block.RequestID, block.ID, block.ToolUseID),
		Kind:      firstNonEmpty(block.Kind, block.Name),
		State:     state,
		Prompt:    firstNonEmpty(block.Prompt, block.Text),
		Options:   append([]string(nil), block.Options...),
		Action:    block.Action,
		Metadata:  metadataStrings(block.Metadata),
	}
}

func normalizeInteractionState(state string) InteractionState {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "opened":
		return InteractionStateOpened
	case "pending", "blocked":
		return InteractionStatePending
	case "resolved":
		return InteractionStateResolved
	case "dismissed":
		return InteractionStateDismissed
	case "resumed_after_restart":
		return InteractionStateResumedAfterRestart
	default:
		return InteractionStateUnknown
	}
}

func pendingInteractionIDs(entries []HistoryEntry) []string {
	pending := map[string]bool{}
	for _, entry := range entries {
		for _, block := range entry.Blocks {
			if block.Kind != BlockKindInteraction || block.Interaction == nil {
				continue
			}
			id := strings.TrimSpace(block.Interaction.RequestID)
			if id == "" {
				continue
			}
			switch block.Interaction.State {
			case InteractionStateOpened, InteractionStatePending, InteractionStateResumedAfterRestart:
				pending[id] = true
			case InteractionStateResolved, InteractionStateDismissed:
				delete(pending, id)
			}
		}
	}
	return sortedKeys(pending)
}

func tailDegradedReason(session sessionlog.SessionDiagnostics) string {
	if session.MalformedTail {
		return "malformed_tail"
	}
	return ""
}

func firstText(blocks []HistoryBlock) string {
	for _, block := range blocks {
		if strings.TrimSpace(block.Text) != "" {
			return block.Text
		}
	}
	return ""
}

func sortedKeys(values map[string]bool) []string {
	if len(values) == 0 {
		return nil
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func metadataStrings(raw json.RawMessage) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var values map[string]any
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		switch typed := value.(type) {
		case string:
			out[key] = typed
		case float64, bool:
			out[key] = fmt.Sprint(typed)
		default:
			data, err := json.Marshal(typed)
			if err == nil {
				out[key] = string(data)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
