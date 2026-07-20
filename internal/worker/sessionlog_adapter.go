package worker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

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
	BeforeEntryID         string
	AfterEntryID          string
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

// TailMetaForProvider reads model/context metadata using the provider's
// transcript schema. TailMeta remains the Claude-shaped compatibility path.
func (a SessionLogAdapter) TailMetaForProvider(provider, path string) (*sessionlog.TailMeta, error) {
	if sessionlog.ProviderFamily(provider) == "codex" {
		return sessionlog.ExtractCodexTailMetaFromSearchPaths(a.SearchPaths, path)
	}
	return a.TailMeta(path)
}

// TailUsage reads per-invocation token usage entries from the tail of a
// discovered transcript path, validating it against the search-path roots.
func (a SessionLogAdapter) TailUsage(path string) ([]sessionlog.TailUsage, error) {
	return sessionlog.ExtractTailUsageFromSearchPaths(a.SearchPaths, path)
}

// CodexTailUsage reads per-invocation token usage from the tail of a codex
// rollout transcript. Validation merges the codex default roots
// (~/.codex/sessions) on top of the configured search paths, because
// a.SearchPaths alone holds claude-style roots that would reject real codex
// rollout locations.
func (a SessionLogAdapter) CodexTailUsage(path string) ([]sessionlog.TailUsage, error) {
	return sessionlog.ExtractCodexTailUsageFromSearchPaths(a.SearchPaths, path)
}

// InvocationUsage reads per-invocation token usage from a discovered
// transcript using the SAME extractor the prompt-op telemetry gate uses for
// the provider's invocation-usage family (invocationUsageSpecs). It returns
// (nil, nil) for families without invocation-telemetry support, so callers can
// treat "no extractor" and "no usage" uniformly.
func (a SessionLogAdapter) InvocationUsage(provider, path string) ([]sessionlog.TailUsage, error) {
	family, ok := InvocationUsageFamily(provider)
	if !ok {
		return nil, nil
	}
	return invocationUsageSpecs[family].extract(a, path)
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
		RawMessages:    rawMessagesFromEntries(sess.Messages),
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

	beforeID, afterID, err := transcriptPageEntryIDs(req.BeforeEntryID, req.AfterEntryID)
	if err != nil {
		return nil, err
	}
	var sess *sessionlog.Session
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
		result.RawMessages = rawMessagesFromEntries(sess.Messages)
	}
	return result, nil
}

func rawMessagesFromEntries(entries []*sessionlog.Entry) []json.RawMessage {
	rawMessages := make([]json.RawMessage, 0, len(entries))
	seenRecords := make(map[string]struct{})
	for _, entry := range entries {
		if entry == nil || len(entry.Raw) == 0 {
			continue
		}
		if entry.RawRecordID != "" {
			if _, seen := seenRecords[entry.RawRecordID]; seen {
				continue
			}
			seenRecords[entry.RawRecordID] = struct{}{}
		}
		rawMessages = append(rawMessages, entry.Raw)
	}
	return rawMessages
}

// LoadHistory loads and normalizes a provider transcript.
func (a SessionLogAdapter) LoadHistory(req LoadRequest) (*HistorySnapshot, error) {
	path := strings.TrimSpace(req.TranscriptPath)
	if path == "" {
		return nil, fmt.Errorf("transcript path is required")
	}

	beforeID, afterID, err := transcriptPageEntryIDs(req.BeforeEntryID, req.AfterEntryID)
	if err != nil {
		return nil, err
	}
	fullSession, err := sessionlog.ReadProviderFileRaw(req.Provider, path, 0)
	if err != nil {
		return nil, err
	}
	session := fullSession
	paged := req.TailCompactions > 0 || beforeID != "" || afterID != ""
	if paged {
		session, err = sessionlog.PageSession(fullSession, req.TailCompactions, beforeID, afterID)
		if err != nil {
			return nil, err
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat transcript: %w", err)
	}

	entries := normalizeHistoryEntries(req.Provider, path, session.ID, session.Messages)
	contextEntries := entries
	if paged {
		// Pair tool_result blocks whose tool_use is off the current page against
		// the full session (already read above — no extra I/O), so paginated
		// structured pages keep typed command/diff/read/task results instead of
		// degrading to plain text at page boundaries.
		contextEntries = normalizeHistoryEntries(req.Provider, path, fullSession.ID, fullSession.Messages)
	}
	entries = attachStructuredToolDataWithContext(entries, contextEntries)
	if beforeID == "" {
		// Detached (Codex) usage is extracted from the file tail — the newest
		// turns. It belongs only to a page that includes the tail. On an older
		// "before" page the tail usages are for newer, off-page turns and would
		// be mis-attributed onto earlier assistants, so skip attachment there;
		// those older turns have no tail-extractable usage to show anyway.
		entries, err = attachDetachedProviderUsage(req.Provider, path, entries)
		if err != nil {
			return nil, err
		}
	}
	compactionCount, lastEntryID, pendingIDs := transcriptGlobalFacts(fullSession.Messages)

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

	openToolUseIDs := sortedKeys(fullSession.OrphanedToolUseIDs)
	diagnostics := historyDiagnostics(fullSession.Diagnostics)
	continuity := Continuity{
		Status:          ContinuityStatusContinuous,
		CompactionCount: compactionCount,
		HasBranches:     fullSession.HasBranches,
	}
	if compactionCount > 0 {
		continuity.Status = ContinuityStatusCompacted
	}
	if len(fullSession.Messages) == 0 {
		continuity.Status = ContinuityStatusUnknown
	}
	if len(diagnostics) > 0 {
		continuity.Note = diagnostics[0].Message
		if len(fullSession.Messages) > 0 {
			continuity.Status = ContinuityStatusDegraded
		}
	}
	tailDegradedReason := tailDegradedReason(fullSession.Diagnostics)

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
		Pagination:  session.Pagination,
		Entries:     entries,
	}, nil
}

func normalizeHistoryEntries(provider, path, sessionID string, messages []*sessionlog.Entry) []HistoryEntry {
	entries := make([]HistoryEntry, 0, len(messages))
	for idx, entry := range messages {
		entries = append(entries, normalizeEntry(provider, path, sessionID, idx, entry))
	}
	return entries
}

func transcriptGlobalFacts(messages []*sessionlog.Entry) (int, string, []string) {
	compactionCount := 0
	lastEntryID := ""
	pending := make(map[string]bool)
	for idx, entry := range messages {
		lastEntryID = normalizedHistoryEntryID(entry, idx)
		if entry.IsCompactBoundary() {
			compactionCount++
		}
		// Avoid decoding potentially large off-page tool results. Provider
		// readers normalize interaction records to content blocks with this
		// discriminator before they reach the worker boundary.
		if !bytes.Contains(entry.Message, []byte(`"interaction"`)) {
			continue
		}
		for _, block := range entry.ContentBlocks() {
			if normalizeBlockKind(block.Type) != BlockKindInteraction {
				continue
			}
			id := strings.TrimSpace(firstNonEmpty(block.RequestID, block.ID, block.ToolUseID))
			if id == "" {
				continue
			}
			switch normalizeInteractionState(block.State) {
			case InteractionStateOpened, InteractionStatePending, InteractionStateResumedAfterRestart:
				pending[id] = true
			case InteractionStateResolved, InteractionStateDismissed:
				delete(pending, id)
			}
		}
	}
	return compactionCount, lastEntryID, sortedKeys(pending)
}

func transcriptPageEntryIDs(beforeEntryID, afterEntryID string) (string, string, error) {
	beforeID := strings.TrimSpace(beforeEntryID)
	afterID := strings.TrimSpace(afterEntryID)
	if beforeID != "" && afterID != "" {
		return "", "", ErrTranscriptCursorConflict
	}
	return beforeID, afterID, nil
}

func normalizeEntry(provider, path, sessionID string, order int, entry *sessionlog.Entry) HistoryEntry {
	provenance := Provenance{
		Provider:          provider,
		TranscriptPath:    filepath.Clean(path),
		ProviderSessionID: sessionID,
		RawEntryID:        entry.UUID,
		RawType:           entry.Type,
		Raw:               cloneRaw(entry.Raw),
		RawRecordID:       entry.RawRecordID,
	}

	normalized := HistoryEntry{
		ID:         normalizedHistoryEntryID(entry, order),
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
	normalized.Model, normalized.StopReason, normalized.Usage = historyEntryMetadata(entry)
	normalized.SystemEvent = historySystemEventFromSessionLog(entry.SystemEvent)

	blocks := normalizeBlocks(entry)
	normalized.Blocks = blocks
	if normalized.Text == "" {
		normalized.Text = firstText(blocks)
	}
	if normalized.Kind == "user" && normalized.Actor == ActorUser {
		normalized.UserPrompt = parseHistoryUserPrompt(normalized.Text)
	}
	return normalized
}

func normalizedHistoryEntryID(entry *sessionlog.Entry, order int) string {
	return firstNonEmpty(entry.UUID, fmt.Sprintf("derived-%d", order))
}

func historySystemEventFromSessionLog(event *sessionlog.SystemEvent) *HistorySystemEvent {
	if event == nil {
		return nil
	}
	return &HistorySystemEvent{
		Kind:     event.Kind,
		Category: event.Category,
		Code:     event.Code,
		Message:  event.Message,
	}
}

func attachDetachedProviderUsage(provider, path string, entries []HistoryEntry) ([]HistoryEntry, error) {
	family, supported := InvocationUsageFamily(provider)
	if !supported || family != "codex" {
		return entries, nil
	}
	usages, err := sessionlog.ExtractCodexTailUsage(path)
	if err != nil {
		return nil, fmt.Errorf("extract codex tail usage: %w", err)
	}
	return attachTailUsageToAssistantEntries(entries, usages), nil
}

func attachTailUsageToAssistantEntries(entries []HistoryEntry, usages []sessionlog.TailUsage) []HistoryEntry {
	if len(entries) == 0 || len(usages) == 0 {
		return entries
	}
	nextStart := 0
	for _, usage := range usages {
		usageTime, ok := tailUsageTimestamp(usage)
		if !ok {
			continue
		}
		target := latestAssistantEntryBefore(entries, nextStart, usageTime)
		if target < 0 {
			target = latestAssistantEntryBefore(entries, 0, usageTime)
		}
		if target < 0 {
			continue
		}
		if entries[target].Model == "" {
			entries[target].Model = usage.Model
		}
		if entries[target].Usage == nil {
			entries[target].Usage = historyUsageFromTailUsage(usage)
			enrichUsageContext(entries[target].Usage, entries[target].Model)
		}
		nextStart = target + 1
	}
	return entries
}

func latestAssistantEntryBefore(entries []HistoryEntry, start int, usageTime time.Time) int {
	target := -1
	for idx := start; idx < len(entries); idx++ {
		entry := entries[idx]
		if entry.Timestamp != nil && entry.Timestamp.After(usageTime) {
			break
		}
		if entry.Actor != ActorAssistant || entry.Timestamp == nil {
			continue
		}
		if entry.Usage == nil || entry.Model == "" {
			target = idx
		}
	}
	return target
}

func tailUsageTimestamp(usage sessionlog.TailUsage) (time.Time, bool) {
	if strings.TrimSpace(usage.EntryUUID) == "" {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, usage.EntryUUID)
	if err != nil {
		return time.Time{}, false
	}
	return ts.UTC(), true
}

func historyUsageFromTailUsage(usage sessionlog.TailUsage) *HistoryUsage {
	out := &HistoryUsage{
		InputTokens:         usage.InputTokens,
		OutputTokens:        usage.OutputTokens,
		ReasoningTokens:     usage.ReasoningTokens,
		CacheReadTokens:     usage.CacheReadTokens,
		CacheCreationTokens: usage.CacheCreationTokens,
		ContextWindowTokens: usage.ContextWindowTokens,
	}
	if out.InputTokens == 0 && out.OutputTokens == 0 && out.ReasoningTokens == 0 && out.CacheReadTokens == 0 && out.CacheCreationTokens == 0 && out.ContextWindowTokens == 0 {
		return nil
	}
	return out
}

func historyEntryMetadata(entry *sessionlog.Entry) (string, string, *HistoryUsage) {
	if entry == nil {
		return "", "", nil
	}
	messageMeta := historyMetadataFromRaw(entry.Message)
	rawMeta := historyMetadataFromRaw(entry.Raw)
	if len(entry.Raw) > 0 {
		var rawEntry struct {
			Message json.RawMessage `json:"message"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(entry.Raw, &rawEntry) == nil {
			rawMessageMeta := historyMetadataFromRaw(rawEntry.Message)
			rawPayloadMeta := historyMetadataFromRaw(rawEntry.Payload)
			rawMeta.Model = firstNonEmpty(rawMeta.Model, rawMessageMeta.Model, rawPayloadMeta.Model)
			rawMeta.StopReason = firstNonEmpty(rawMeta.StopReason, rawMessageMeta.StopReason, rawPayloadMeta.StopReason)
			rawMeta.Usage = firstNonNilUsage(rawMeta.Usage, rawMessageMeta.Usage, rawPayloadMeta.Usage)
		}
	}
	model := firstNonEmpty(messageMeta.Model, rawMeta.Model)
	stopReason := firstNonEmpty(messageMeta.StopReason, rawMeta.StopReason)
	usage := firstNonNilUsage(messageMeta.Usage, rawMeta.Usage)
	if usage != nil {
		enrichUsageContext(usage, model)
	}
	return model, stopReason, usage
}

type historyMetadata struct {
	Model      string
	StopReason string
	Usage      *HistoryUsage
}

func historyMetadataFromRaw(raw json.RawMessage) historyMetadata {
	return historyMetadataFromRawDepth(raw, 0)
}

func historyMetadataFromRawDepth(raw json.RawMessage, depth int) historyMetadata {
	if len(raw) == 0 {
		return historyMetadata{}
	}
	if depth > 4 {
		return historyMetadata{}
	}
	raw = unwrapJSONStringRaw(raw)
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return historyMetadata{}
	}
	metadata := historyMetadata{
		Model:      firstNonEmpty(jsonLiteralString(object, "model", "model_id", "modelID"), historyModelFromObjectField(object, "model")),
		StopReason: jsonLiteralString(object, "stop_reason", "stopReason"),
	}
	metadata.Usage = historyUsageFromObjectField(object, "usage", "tokens")
	for _, field := range []string{"info"} {
		if nested, ok := object[field]; ok {
			nestedMeta := historyMetadataFromRawDepth(nested, depth+1)
			metadata.Model = firstNonEmpty(metadata.Model, nestedMeta.Model)
			metadata.StopReason = firstNonEmpty(metadata.StopReason, nestedMeta.StopReason)
			metadata.Usage = firstNonNilUsage(metadata.Usage, nestedMeta.Usage)
		}
	}
	return metadata
}

func historyModelFromObjectField(object map[string]json.RawMessage, names ...string) string {
	for _, name := range names {
		raw, ok := object[name]
		if !ok || len(raw) == 0 {
			continue
		}
		raw = unwrapJSONStringRaw(raw)
		var modelObject map[string]json.RawMessage
		if json.Unmarshal(raw, &modelObject) != nil || len(modelObject) == 0 {
			continue
		}
		if model := jsonLiteralString(modelObject, "model_id", "modelID", "id", "name"); model != "" {
			return model
		}
	}
	return ""
}

func historyUsageFromObjectField(object map[string]json.RawMessage, names ...string) *HistoryUsage {
	for _, name := range names {
		raw, ok := object[name]
		if !ok || len(raw) == 0 {
			continue
		}
		if usage := historyUsageFromRaw(raw); usage != nil {
			return usage
		}
	}
	return nil
}

func historyUsageFromRaw(raw json.RawMessage) *HistoryUsage {
	raw = unwrapJSONStringRaw(raw)
	var usage struct {
		InputTokens              int `json:"input_tokens"`
		Input                    int `json:"input"`
		PromptTokens             int `json:"prompt_tokens"`
		OutputTokens             int `json:"output_tokens"`
		Output                   int `json:"output"`
		CompletionTokens         int `json:"completion_tokens"`
		ReasoningTokens          int `json:"reasoning_tokens"`
		ReasoningOutputTokens    int `json:"reasoning_output_tokens"`
		Reasoning                int `json:"reasoning"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CachedInputTokens        int `json:"cached_input_tokens"`
		CacheReadTokens          int `json:"cache_read_tokens"`
		CacheRead                int `json:"cacheRead"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheCreationTokens      int `json:"cache_creation_tokens"`
		CacheWrite               int `json:"cacheWrite"`
		ContextWindowTokens      int `json:"context_window_tokens"`
		ContextWindow            int `json:"contextWindow"`
		ContextUsedTokens        int `json:"context_used_tokens"`
		ContextUsed              int `json:"contextUsed"`
		ContextPercent           int `json:"context_percent"`
		ContextPercentage        int `json:"contextPercentage"`
		Percentage               int `json:"percentage"`
		Cache                    struct {
			Read  int `json:"read"`
			Write int `json:"write"`
		} `json:"cache"`
	}
	if json.Unmarshal(raw, &usage) != nil {
		return nil
	}
	out := &HistoryUsage{
		InputTokens:         firstPositiveInt(usage.InputTokens, usage.Input, usage.PromptTokens),
		OutputTokens:        firstPositiveInt(usage.OutputTokens, usage.Output, usage.CompletionTokens),
		ReasoningTokens:     firstPositiveInt(usage.ReasoningTokens, usage.ReasoningOutputTokens, usage.Reasoning),
		CacheReadTokens:     firstPositiveInt(usage.CacheReadInputTokens, usage.CachedInputTokens, usage.CacheReadTokens, usage.CacheRead, usage.Cache.Read),
		CacheCreationTokens: firstPositiveInt(usage.CacheCreationInputTokens, usage.CacheCreationTokens, usage.CacheWrite, usage.Cache.Write),
		ContextWindowTokens: firstPositiveInt(usage.ContextWindowTokens, usage.ContextWindow),
		ContextUsedTokens:   firstPositiveInt(usage.ContextUsedTokens, usage.ContextUsed),
		ContextPercent:      firstPositiveInt(usage.ContextPercent, usage.ContextPercentage, usage.Percentage),
	}
	if out.InputTokens == 0 && out.OutputTokens == 0 && out.ReasoningTokens == 0 && out.CacheReadTokens == 0 && out.CacheCreationTokens == 0 && out.ContextWindowTokens == 0 && out.ContextUsedTokens == 0 && out.ContextPercent == 0 {
		return nil
	}
	return out
}

func enrichUsageContext(usage *HistoryUsage, model string) {
	if usage == nil {
		return
	}
	if usage.ContextUsedTokens == 0 {
		usage.ContextUsedTokens = usage.InputTokens + usage.CacheReadTokens + usage.CacheCreationTokens
	}
	if usage.ContextWindowTokens == 0 {
		usage.ContextWindowTokens = sessionlog.ModelContextWindow(model)
	}
	if usage.ContextPercent == 0 && usage.ContextWindowTokens > 0 && usage.ContextUsedTokens > 0 {
		usage.ContextPercent = usage.ContextUsedTokens * 100 / usage.ContextWindowTokens
		if usage.ContextPercent > 100 {
			usage.ContextPercent = 100
		}
	}
}

func firstNonNilUsage(values ...*HistoryUsage) *HistoryUsage {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func unwrapJSONStringRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) > 0 && raw[0] == '"' {
		var value string
		if json.Unmarshal(raw, &value) == nil {
			return json.RawMessage(value)
		}
	}
	return raw
}

func jsonLiteralString(object map[string]json.RawMessage, names ...string) string {
	for _, name := range names {
		raw, ok := object[name]
		if !ok || len(raw) == 0 {
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) == nil {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstPositiveInt(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func normalizeBlocks(entry *sessionlog.Entry) []HistoryBlock {
	blocks := entry.ContentBlocks()
	if len(blocks) > 0 {
		result := make([]HistoryBlock, 0, len(blocks))
		for _, block := range blocks {
			kind := normalizeBlockKind(block.Type)
			text := block.Text
			signature := ""
			if kind == BlockKindThinking {
				text = firstNonEmpty(block.Thinking, block.Text)
				signature = strings.TrimSpace(block.Signature)
			}
			var interaction *HistoryInteraction
			if kind == BlockKindInteraction {
				interaction = normalizeInteractionBlock(block)
			}
			toolUseID := firstNonEmpty(block.ToolUseID, block.ID)
			if kind == BlockKindInteraction {
				toolUseID = ""
			}
			content := cloneRaw(block.Content)
			if kind == BlockKindToolResult {
				content = toolResultContentWithEvidence(entry, content)
			}
			contentText := structuredJSONText(content)
			result = append(result, HistoryBlock{
				Kind:        kind,
				Text:        text,
				Signature:   signature,
				ToolUseID:   toolUseID,
				Name:        block.Name,
				FilePath:    strings.TrimSpace(block.FilePath),
				ImageURL:    strings.TrimSpace(block.ImageURL),
				MIMEType:    strings.TrimSpace(block.MIMEType),
				Input:       cloneRaw(block.Input),
				Content:     content,
				ContentText: contentText,
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

func toolResultContentWithEvidence(entry *sessionlog.Entry, content json.RawMessage) json.RawMessage {
	evidence := toolResultEvidence(entry)
	if len(evidence) == 0 {
		return cloneRaw(content)
	}
	payload := struct {
		Content    json.RawMessage `json:"content,omitempty"`
		ToolResult json.RawMessage `json:"tool_result,omitempty"`
	}{
		Content:    cloneRaw(content),
		ToolResult: evidence,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return cloneRaw(content)
	}
	return raw
}

func toolResultEvidence(entry *sessionlog.Entry) json.RawMessage {
	return cloneRaw(entry.ToolResultEvidence())
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
