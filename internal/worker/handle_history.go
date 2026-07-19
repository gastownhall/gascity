package worker

import (
	"context"
	"encoding/json"
	"strings"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TranscriptPath resolves the provider-native transcript path for the worker.
func (h *SessionHandle) TranscriptPath(_ context.Context) (string, error) {
	id := h.currentSessionID()
	if id == "" {
		return "", ErrHistoryUnavailable
	}
	path, err := h.manager.TranscriptPath(id, h.adapter.SearchPaths)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(path) == "" {
		return "", ErrHistoryUnavailable
	}
	return path, nil
}

// Transcript loads the provider-native transcript through the worker boundary.
func (h *SessionHandle) Transcript(ctx context.Context, req TranscriptRequest) (*TranscriptResult, error) {
	id := h.currentSessionID()
	if id == "" {
		return nil, ErrHistoryUnavailable
	}
	info, err := h.manager.Get(id)
	if err != nil {
		return nil, err
	}
	path, err := h.TranscriptPath(ctx)
	if err != nil {
		return nil, err
	}
	readReq := req
	readReq.Provider = h.historyProvider(info)
	readReq.TranscriptPath = path
	return h.adapter.ReadTranscript(readReq)
}

// AgentMappings returns subagent mappings discovered from the worker's
// transcript stream.
func (h *SessionHandle) AgentMappings(ctx context.Context) ([]AgentMapping, error) {
	path, err := h.TranscriptPath(ctx)
	if err != nil {
		return nil, err
	}
	return h.adapter.AgentMappings(path)
}

// AgentTranscript returns a subagent transcript derived from the worker's
// primary transcript stream.
func (h *SessionHandle) AgentTranscript(ctx context.Context, agentID string) (*AgentTranscriptResult, error) {
	path, err := h.TranscriptPath(ctx)
	if err != nil {
		return nil, err
	}
	return h.adapter.ReadAgentTranscript(path, agentID)
}

// History returns the normalized worker transcript.
func (h *SessionHandle) History(ctx context.Context, req HistoryRequest) (*HistorySnapshot, error) {
	event := h.beginOperationEvent(ctx, workerOperationHistory)
	var err error
	defer func() { event.finish(err) }()

	snapshot, err := h.historyWithRequest(req)
	return snapshot, err
}

func (h *SessionHandle) historyWithRequest(req HistoryRequest) (*HistorySnapshot, error) {
	id := h.currentSessionID()
	if id == "" {
		return nil, ErrHistoryUnavailable
	}

	info, err := h.manager.Get(id)
	if err != nil {
		return nil, err
	}
	path, err := h.manager.TranscriptPath(id, h.adapter.SearchPaths)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(path) == "" {
		return nil, ErrHistoryUnavailable
	}

	gcSessionID := strings.TrimSpace(info.SessionKey)
	if gcSessionID == "" {
		gcSessionID = info.ID
	}
	snapshot, err := h.adapter.LoadHistory(LoadRequest{
		Provider:              h.historyProvider(info),
		TranscriptPath:        path,
		GCSessionID:           gcSessionID,
		LogicalConversationID: strings.TrimSpace(req.LogicalID),
		TailCompactions:       req.TailCompactions,
		BeforeEntryID:         req.BeforeEntryID,
		AfterEntryID:          req.AfterEntryID,
	})
	if err != nil {
		return nil, err
	}
	h.maybePersistDerivedSessionKey(id, info, snapshot)
	// After any session-key persist, so the keyed transcript path can resolve.
	h.writeTranscriptSessionMeta()
	// Cursor and tail requests are bounded views, not authoritative continuity
	// snapshots. Returning them through the generation cache can replace a
	// successful page with a previously cached full (or different) view when
	// the underlying transcript generation has not changed.
	if req.TailCompactions > 0 || strings.TrimSpace(req.BeforeEntryID) != "" || strings.TrimSpace(req.AfterEntryID) != "" {
		return cloneHistorySnapshot(snapshot), nil
	}
	return h.mergeLoadedHistorySnapshot(snapshot), nil
}

func (h *SessionHandle) maybePersistDerivedSessionKey(id string, info sessionpkg.Info, snapshot *HistorySnapshot) {
	if snapshot == nil || strings.TrimSpace(info.SessionKey) != "" {
		return
	}
	sessionKey := derivedResumeSessionKey(h.historyProvider(info), snapshot.ProviderSessionID)
	if sessionKey == "" {
		return
	}
	if err := h.manager.PersistSessionKey(id, sessionKey); err != nil {
		return
	}
	snapshot.GCSessionID = sessionKey
	snapshot.LogicalConversationID = sessionKey
}

func (h *SessionHandle) mergeLoadedHistorySnapshot(current *HistorySnapshot) *HistorySnapshot {
	if current == nil {
		return nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	raw := historyGeneration{
		TranscriptStreamID: strings.TrimSpace(current.TranscriptStreamID),
		GenerationID:       strings.TrimSpace(current.Generation.ID),
	}
	if h.history != nil && raw == h.historyRaw {
		return cloneHistorySnapshot(h.history)
	}

	merged := mergeConversationHistorySnapshots(h.history, current)
	h.history = cloneHistorySnapshot(merged)
	h.historyRaw = raw
	return cloneHistorySnapshot(h.history)
}

func mergeConversationHistorySnapshots(previous, current *HistorySnapshot) *HistorySnapshot {
	if current == nil {
		return cloneHistorySnapshot(previous)
	}
	merged := cloneHistorySnapshot(current)
	if previous == nil || !sameHistoryConversation(previous, current) {
		return merged
	}
	// A later generation of the same provider stream is authoritative: some
	// structured transcripts are rewritten in place. Apply that in-place
	// replacement only for a genuine single-file rewrite. A retained snapshot
	// that already stitched a rotation reports the current stream ID even though
	// it still carries pre-rotation entries from the prior file; letting the
	// shared stream ID trigger replacement would drop that stitched history.
	previousStreamID := strings.TrimSpace(previous.TranscriptStreamID)
	if previousStreamID != "" && previousStreamID == strings.TrimSpace(current.TranscriptStreamID) &&
		!historySnapshotSpansRotation(previous) {
		return merged
	}

	priorComparable := historyComparableEntries(previous.Entries)
	if len(priorComparable) == 0 || historyContainsSubsequence(merged.Entries, priorComparable) {
		return merged
	}

	merged.Entries = mergeHistoryEntries(previous.Entries, current.Entries)
	if merged.GCSessionID == "" {
		merged.GCSessionID = previous.GCSessionID
	}
	if merged.LogicalConversationID == "" {
		merged.LogicalConversationID = previous.LogicalConversationID
	}
	if merged.ProviderSessionID == "" {
		merged.ProviderSessionID = previous.ProviderSessionID
	}
	if merged.Cursor.AfterEntryID == "" && len(merged.Entries) > 0 {
		merged.Cursor.AfterEntryID = merged.Entries[len(merged.Entries)-1].ID
	}
	if merged.TailState.LastEntryID == "" {
		merged.TailState.LastEntryID = merged.Cursor.AfterEntryID
	}
	return merged
}

// historySnapshotSpansRotation reports whether snapshot carries entries stitched
// from a transcript stream other than the one it now identifies as. normalizeEntry
// stamps each entry's Provenance.TranscriptPath from the same cleaned path used
// for the snapshot's TranscriptStreamID, so within a single read they always
// match; an entry whose source path differs was retained across a file rotation.
// Such a snapshot reports the current stream ID yet still holds pre-rotation
// history, so it must not take the same-stream in-place replacement path.
func historySnapshotSpansRotation(snapshot *HistorySnapshot) bool {
	if snapshot == nil {
		return false
	}
	streamID := strings.TrimSpace(snapshot.TranscriptStreamID)
	if streamID == "" {
		return false
	}
	for _, entry := range snapshot.Entries {
		source := strings.TrimSpace(entry.Provenance.TranscriptPath)
		if source != "" && source != streamID {
			return true
		}
	}
	return false
}

func sameHistoryConversation(previous, current *HistorySnapshot) bool {
	if previous == nil || current == nil {
		return false
	}
	previousLogical := strings.TrimSpace(previous.LogicalConversationID)
	currentLogical := strings.TrimSpace(current.LogicalConversationID)
	if previousLogical != "" && currentLogical != "" {
		return previousLogical == currentLogical
	}
	previousSession := strings.TrimSpace(previous.GCSessionID)
	currentSession := strings.TrimSpace(current.GCSessionID)
	return previousSession != "" && previousSession == currentSession
}

func historyComparableEntries(entries []HistoryEntry) []HistoryEntry {
	out := make([]HistoryEntry, 0, len(entries))
	for _, entry := range entries {
		if historyEntryIsTransient(entry) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func historyEntryIsTransient(entry HistoryEntry) bool {
	if entry.Provenance.RawType != "system" || len(entry.Provenance.Raw) == 0 {
		return false
	}
	var raw struct {
		Subtype string `json:"subtype"`
	}
	if err := json.Unmarshal(entry.Provenance.Raw, &raw); err != nil {
		return false
	}
	return raw.Subtype == "stop_hook_summary"
}

func historyContainsSubsequence(after, before []HistoryEntry) bool {
	if len(before) == 0 {
		return true
	}
	match := 0
	for _, entry := range after {
		if !historyEntryEquivalent(entry, before[match]) {
			continue
		}
		match++
		if match == len(before) {
			return true
		}
	}
	return false
}

func mergeHistoryEntries(previous, current []HistoryEntry) []HistoryEntry {
	prev := cloneHistoryEntries(previous)
	curr := cloneHistoryEntries(current)
	overlap := historyEntryOverlap(prev, curr)
	prev = append(prev, curr[overlap:]...)
	merged := prev
	for idx := range merged {
		merged[idx].Order = idx
	}
	return merged
}

func historyEntryOverlap(previous, current []HistoryEntry) int {
	limit := len(previous)
	if len(current) < limit {
		limit = len(current)
	}
	for overlap := limit; overlap > 0; overlap-- {
		match := true
		for idx := 0; idx < overlap; idx++ {
			if !historyEntryEquivalent(previous[len(previous)-overlap+idx], current[idx]) {
				match = false
				break
			}
		}
		if match {
			return overlap
		}
	}
	return 0
}

func historyEntryEquivalent(a, b HistoryEntry) bool {
	if strings.TrimSpace(a.ID) != "" && strings.TrimSpace(b.ID) != "" && a.ID == b.ID {
		return true
	}
	return historyEntrySignature(a) == historyEntrySignature(b)
}

func historyEntrySignature(entry HistoryEntry) string {
	parts := []string{
		string(entry.Actor),
		entry.Kind,
		strings.TrimSpace(entry.Text),
	}
	for _, block := range entry.Blocks {
		parts = append(parts,
			string(block.Kind),
			strings.TrimSpace(block.Text),
			strings.TrimSpace(block.Signature),
			strings.TrimSpace(block.ToolUseID),
			strings.TrimSpace(block.Name),
		)
	}
	return strings.Join(parts, "\x1f")
}

func cloneHistorySnapshot(snapshot *HistorySnapshot) *HistorySnapshot {
	if snapshot == nil {
		return nil
	}
	cloned := *snapshot
	cloned.Diagnostics = append([]HistoryDiagnostic(nil), snapshot.Diagnostics...)
	cloned.TailState.OpenToolUseIDs = append([]string(nil), snapshot.TailState.OpenToolUseIDs...)
	cloned.TailState.PendingInteractionIDs = append([]string(nil), snapshot.TailState.PendingInteractionIDs...)
	cloned.Pagination = cloneTranscriptPagination(snapshot.Pagination)
	cloned.Entries = cloneHistoryEntries(snapshot.Entries)
	return &cloned
}

func cloneTranscriptPagination(pagination *TranscriptPagination) *TranscriptPagination {
	if pagination == nil {
		return nil
	}
	cloned := *pagination
	return &cloned
}

func cloneHistoryEntries(entries []HistoryEntry) []HistoryEntry {
	if len(entries) == 0 {
		return nil
	}
	cloned := make([]HistoryEntry, len(entries))
	for idx, entry := range entries {
		cloned[idx] = entry
		if entry.Timestamp != nil {
			ts := entry.Timestamp.UTC()
			cloned[idx].Timestamp = &ts
		}
		if entry.Usage != nil {
			usage := *entry.Usage
			cloned[idx].Usage = &usage
		}
		cloned[idx].Blocks = cloneHistoryBlocks(entry.Blocks)
		cloned[idx].Provenance.Raw = cloneHistoryRaw(entry.Provenance.Raw)
	}
	return cloned
}

func cloneHistoryBlocks(blocks []HistoryBlock) []HistoryBlock {
	if len(blocks) == 0 {
		return nil
	}
	cloned := make([]HistoryBlock, len(blocks))
	for idx, block := range blocks {
		cloned[idx] = block
		cloned[idx].Input = cloneHistoryRaw(block.Input)
		cloned[idx].Content = cloneHistoryRaw(block.Content)
		if block.StructuredInput != nil {
			input := *block.StructuredInput
			input.Arguments = append([]StructuredArgument(nil), block.StructuredInput.Arguments...)
			input.Options = append([]string(nil), block.StructuredInput.Options...)
			input.Steps = append([]StructuredPlanStep(nil), block.StructuredInput.Steps...)
			input.Todos = append([]StructuredTodoItem(nil), block.StructuredInput.Todos...)
			cloned[idx].StructuredInput = &input
		}
		if block.StructuredResult != nil {
			result := *block.StructuredResult
			result.Filenames = append([]string(nil), block.StructuredResult.Filenames...)
			result.FilePaths = append([]string(nil), block.StructuredResult.FilePaths...)
			result.ResultItems = append([]StructuredSearchResultItem(nil), block.StructuredResult.ResultItems...)
			result.Questions = cloneStructuredQuestions(block.StructuredResult.Questions)
			result.Options = append([]string(nil), block.StructuredResult.Options...)
			result.Answers = append([]StructuredArgument(nil), block.StructuredResult.Answers...)
			result.Counts = append([]StructuredArgument(nil), block.StructuredResult.Counts...)
			result.PatchHunks = cloneStructuredPatchHunks(block.StructuredResult.PatchHunks)
			result.Steps = append([]StructuredPlanStep(nil), block.StructuredResult.Steps...)
			result.OldTodos = append([]StructuredTodoItem(nil), block.StructuredResult.OldTodos...)
			result.NewTodos = append([]StructuredTodoItem(nil), block.StructuredResult.NewTodos...)
			cloned[idx].StructuredResult = &result
		}
		if block.Interaction != nil {
			interaction := *block.Interaction
			interaction.Options = append([]string(nil), block.Interaction.Options...)
			interaction.Metadata = cloneStringMap(block.Interaction.Metadata)
			cloned[idx].Interaction = &interaction
		}
	}
	return cloned
}

func cloneStructuredQuestions(questions []StructuredQuestion) []StructuredQuestion {
	if len(questions) == 0 {
		return nil
	}
	out := make([]StructuredQuestion, len(questions))
	for idx, question := range questions {
		out[idx] = question
		out[idx].Options = append([]StructuredQuestionOption(nil), question.Options...)
	}
	return out
}

func cloneStructuredPatchHunks(hunks []StructuredPatchHunk) []StructuredPatchHunk {
	if len(hunks) == 0 {
		return nil
	}
	cloned := make([]StructuredPatchHunk, len(hunks))
	for idx, hunk := range hunks {
		cloned[idx] = hunk
		cloned[idx].Lines = append([]string(nil), hunk.Lines...)
	}
	return cloned
}
