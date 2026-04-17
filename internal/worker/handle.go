package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

var (
	// ErrHandleConfig reports that a worker handle was constructed with an
	// incomplete or invalid configuration.
	ErrHandleConfig = errors.New("worker handle configuration is invalid")
	// ErrHistoryUnavailable reports that the worker has no discoverable
	// transcript yet.
	ErrHistoryUnavailable = errors.New("worker history is unavailable")
)

// Handle is the canonical in-memory worker API.
type Handle interface {
	Start(context.Context) error
	Stop(context.Context) error

	State(context.Context) (State, error)

	Message(context.Context, MessageRequest) (MessageResult, error)
	Interrupt(context.Context, InterruptRequest) error
	Nudge(context.Context, NudgeRequest) error

	History(context.Context, HistoryRequest) (*HistorySnapshot, error)

	Pending(context.Context) (*PendingInteraction, error)
	Respond(context.Context, InteractionResponse) error
}

// Phase captures the worker-level lifecycle state surfaced by [Handle.State].
type Phase string

const (
	PhaseUnknown  Phase = "unknown"
	PhaseStarting Phase = "starting"
	PhaseReady    Phase = "ready"
	PhaseBusy     Phase = "busy"
	PhaseBlocked  Phase = "blocked"
	PhaseStopping Phase = "stopping"
	PhaseStopped  Phase = "stopped"
	PhaseFailed   Phase = "failed"
)

// State is the worker-level lifecycle view.
type State struct {
	Phase       Phase               `json:"phase"`
	SessionID   string              `json:"session_id,omitempty"`
	SessionName string              `json:"session_name,omitempty"`
	Provider    string              `json:"provider,omitempty"`
	Detail      string              `json:"detail,omitempty"`
	Pending     *PendingInteraction `json:"pending,omitempty"`
}

// DeliveryIntent controls how a message should be delivered.
type DeliveryIntent string

const (
	DeliveryIntentDefault      DeliveryIntent = "default"
	DeliveryIntentFollowUp     DeliveryIntent = "follow_up"
	DeliveryIntentInterruptNow DeliveryIntent = "interrupt_now"
)

// MessageRequest submits a user turn to the worker.
type MessageRequest struct {
	Text     string         `json:"text"`
	Delivery DeliveryIntent `json:"delivery,omitempty"`
}

// MessageResult reports whether a worker turn was queued or delivered now.
type MessageResult struct {
	Queued bool `json:"queued"`
}

// InterruptRequest is reserved for future interrupt controls.
type InterruptRequest struct{}

// NudgeRequest delivers a best-effort wake or redirect message.
type NudgeRequest struct {
	Text      string `json:"text"`
	Immediate bool   `json:"immediate,omitempty"`
}

// HistoryRequest scopes transcript loading for a worker.
type HistoryRequest struct {
	TailCompactions int    `json:"tail_compactions,omitempty"`
	LogicalID       string `json:"logical_conversation_id,omitempty"`
}

// PendingInteraction is the worker-level view of a blocking interaction.
type PendingInteraction struct {
	RequestID string            `json:"request_id,omitempty"`
	Kind      string            `json:"kind,omitempty"`
	Prompt    string            `json:"prompt,omitempty"`
	Options   []string          `json:"options,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// InteractionResponse resolves a pending interaction.
type InteractionResponse struct {
	RequestID string            `json:"request_id,omitempty"`
	Action    string            `json:"action"`
	Text      string            `json:"text,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// SessionSpec describes the concrete session materialized by a session-backed
// worker handle.
type SessionSpec struct {
	ID           string
	Profile      Profile
	Template     string
	Title        string
	Alias        string
	ExplicitName string
	Command      string
	WorkDir      string
	Provider     string
	Transport    string
	Env          map[string]string
	Resume       sessionpkg.ProviderResume
	Hints        runtime.Config
	Metadata     map[string]string
}

// SessionHandleConfig configures a [SessionHandle].
type SessionHandleConfig struct {
	Manager     *sessionpkg.Manager
	SearchPaths []string
	Adapter     SessionLogAdapter
	Session     SessionSpec
}

// SessionHandle is the production worker handle backed by session.Manager.
type SessionHandle struct {
	mu          sync.Mutex
	manager     *sessionpkg.Manager
	adapter     SessionLogAdapter
	searchPaths []string
	session     SessionSpec
	sessionID   string
	history     *HistorySnapshot
	historyRaw  historyGeneration
}

var _ Handle = (*SessionHandle)(nil)

// NewSessionHandle constructs a session-backed worker handle.
func NewSessionHandle(cfg SessionHandleConfig) (*SessionHandle, error) {
	if cfg.Manager == nil {
		return nil, fmt.Errorf("%w: manager is required", ErrHandleConfig)
	}

	spec := cloneSessionSpec(cfg.Session)
	if spec.Provider == "" {
		spec.Provider = profileFamily(spec.Profile)
	}
	if spec.Command == "" {
		spec.Command = spec.Provider
	}
	if spec.Title == "" {
		spec.Title = spec.Template
	}
	if spec.Metadata == nil {
		spec.Metadata = map[string]string{}
	} else {
		spec.Metadata = cloneStringMap(spec.Metadata)
	}
	if strings.TrimSpace(spec.Metadata["session_origin"]) == "" {
		spec.Metadata["session_origin"] = "worker"
	}
	if spec.Profile != "" && strings.TrimSpace(spec.Metadata["worker_profile"]) == "" {
		spec.Metadata["worker_profile"] = string(spec.Profile)
	}
	if spec.ID == "" {
		switch {
		case strings.TrimSpace(spec.Template) == "":
			return nil, fmt.Errorf("%w: template is required", ErrHandleConfig)
		case strings.TrimSpace(spec.WorkDir) == "":
			return nil, fmt.Errorf("%w: work_dir is required", ErrHandleConfig)
		case strings.TrimSpace(spec.Provider) == "":
			return nil, fmt.Errorf("%w: provider is required", ErrHandleConfig)
		}
	}

	adapter := cfg.Adapter
	searchPaths := append([]string(nil), cfg.SearchPaths...)
	if len(adapter.SearchPaths) == 0 {
		adapter.SearchPaths = append([]string(nil), searchPaths...)
	}

	return &SessionHandle{
		manager:     cfg.Manager,
		adapter:     adapter,
		searchPaths: searchPaths,
		session:     spec,
		sessionID:   strings.TrimSpace(spec.ID),
	}, nil
}

// Start ensures the worker exists and its runtime is live.
func (h *SessionHandle) Start(ctx context.Context) error {
	id, err := h.ensureSessionID()
	if err != nil {
		return err
	}
	startCommand, err := h.startCommand(id)
	if err != nil {
		return err
	}
	return h.manager.Start(ctx, id, startCommand, h.runtimeHints())
}

// Stop suspends the worker runtime while preserving conversation state.
func (h *SessionHandle) Stop(context.Context) error {
	id := h.currentSessionID()
	if id == "" {
		return nil
	}
	return h.manager.Suspend(id)
}

// State returns the worker-level lifecycle view.
func (h *SessionHandle) State(ctx context.Context) (State, error) {
	id := h.currentSessionID()
	if id == "" {
		return State{Phase: PhaseStopped, Provider: h.providerLabel()}, nil
	}

	info, err := h.manager.Get(id)
	if err != nil {
		return State{}, err
	}
	state := State{
		SessionID:   info.ID,
		SessionName: info.SessionName,
		Provider:    h.providerLabel(),
		Detail:      string(info.State),
	}

	switch info.State {
	case sessionpkg.StateCreating:
		state.Phase = PhaseStarting
		return state, nil
	case sessionpkg.StateDraining:
		state.Phase = PhaseStopping
		return state, nil
	case sessionpkg.StateAsleep, sessionpkg.StateSuspended, sessionpkg.StateDrained, sessionpkg.StateArchived:
		state.Phase = PhaseStopped
		return state, nil
	case sessionpkg.StateQuarantined:
		pending, err := h.Pending(ctx)
		if err != nil {
			return State{}, err
		}
		state.Phase = PhaseBlocked
		state.Pending = pending
		return state, nil
	case sessionpkg.StateActive, sessionpkg.StateAwake:
		pending, err := h.Pending(ctx)
		if err != nil {
			return State{}, err
		}
		if pending != nil {
			state.Phase = PhaseBlocked
			state.Pending = pending
			return state, nil
		}
		state.Phase = PhaseReady
		if history, histErr := h.History(ctx, HistoryRequest{}); histErr == nil && history != nil && history.TailState.Activity == TailActivityInTurn {
			state.Phase = PhaseBusy
		}
		return state, nil
	default:
		if info.Closed {
			state.Phase = PhaseStopped
			return state, nil
		}
		state.Phase = PhaseUnknown
	}

	return state, nil
}

// Message sends a user turn to the worker.
func (h *SessionHandle) Message(ctx context.Context, req MessageRequest) (MessageResult, error) {
	if strings.TrimSpace(req.Text) == "" {
		return MessageResult{}, fmt.Errorf("message text is required")
	}
	id, err := h.ensureSessionID()
	if err != nil {
		return MessageResult{}, err
	}
	resumeCommand, err := h.startCommand(id)
	if err != nil {
		return MessageResult{}, err
	}
	outcome, err := h.manager.Submit(ctx, id, req.Text, resumeCommand, h.runtimeHints(), submitIntent(req.Delivery))
	if err != nil {
		return MessageResult{}, err
	}
	return MessageResult{Queued: outcome.Queued}, nil
}

// Interrupt soft-stops any in-flight worker turn.
func (h *SessionHandle) Interrupt(context.Context, InterruptRequest) error {
	id := h.currentSessionID()
	if id == "" {
		return nil
	}
	return h.manager.StopTurn(id)
}

// Nudge sends a best-effort redirect message to the worker.
func (h *SessionHandle) Nudge(ctx context.Context, req NudgeRequest) error {
	if strings.TrimSpace(req.Text) == "" {
		return fmt.Errorf("nudge text is required")
	}
	id, err := h.ensureSessionID()
	if err != nil {
		return err
	}
	resumeCommand, err := h.startCommand(id)
	if err != nil {
		return err
	}
	if req.Immediate {
		return h.manager.SendImmediate(ctx, id, req.Text, resumeCommand, h.runtimeHints())
	}
	return h.manager.Send(ctx, id, req.Text, resumeCommand, h.runtimeHints())
}

// History returns the normalized worker transcript.
func (h *SessionHandle) History(context.Context, HistoryRequest) (*HistorySnapshot, error) {
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
		Provider:       h.historyProvider(info),
		TranscriptPath: path,
		GCSessionID:    gcSessionID,
	})
	if err != nil {
		return nil, err
	}
	h.maybePersistDerivedSessionKey(id, info, snapshot)
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

// Pending surfaces any current blocking interaction.
func (h *SessionHandle) Pending(context.Context) (*PendingInteraction, error) {
	id := h.currentSessionID()
	if id == "" {
		return nil, nil
	}
	info, err := h.manager.Get(id)
	if err != nil {
		return nil, err
	}
	if info.Closed {
		return nil, nil
	}
	switch info.State {
	case sessionpkg.StateAsleep, sessionpkg.StateSuspended, sessionpkg.StateDrained, sessionpkg.StateArchived:
		return nil, nil
	}
	pending, supported, err := h.manager.Pending(id)
	if err != nil {
		return nil, err
	}
	if !supported || pending == nil {
		return nil, nil
	}
	return &PendingInteraction{
		RequestID: pending.RequestID,
		Kind:      pending.Kind,
		Prompt:    pending.Prompt,
		Options:   append([]string(nil), pending.Options...),
		Metadata:  cloneStringMap(pending.Metadata),
	}, nil
}

// Respond resolves the current blocking interaction.
func (h *SessionHandle) Respond(_ context.Context, req InteractionResponse) error {
	id := h.currentSessionID()
	if id == "" {
		return sessionpkg.ErrNoPendingInteraction
	}
	return h.manager.Respond(id, runtime.InteractionResponse{
		RequestID: req.RequestID,
		Action:    req.Action,
		Text:      req.Text,
		Metadata:  cloneStringMap(req.Metadata),
	})
}

func (h *SessionHandle) ensureSessionID() (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sessionID != "" {
		return h.sessionID, nil
	}
	info, err := h.manager.CreateAliasedBeadOnlyNamedWithMetadata(
		h.session.Alias,
		h.session.ExplicitName,
		h.session.Template,
		h.session.Title,
		h.session.Command,
		h.session.WorkDir,
		h.session.Provider,
		h.session.Transport,
		h.session.Resume,
		cloneStringMap(h.session.Metadata),
	)
	if err != nil {
		return "", err
	}
	h.sessionID = info.ID
	return h.sessionID, nil
}

func (h *SessionHandle) currentSessionID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessionID
}

func (h *SessionHandle) startCommand(id string) (string, error) {
	info, err := h.manager.Get(id)
	if err != nil {
		return "", err
	}
	if info.State == sessionpkg.StateCreating && h.session.Resume.SessionIDFlag != "" && strings.TrimSpace(info.SessionKey) != "" {
		command := strings.TrimSpace(info.Command)
		if command == "" {
			command = strings.TrimSpace(h.session.Command)
		}
		if command == "" {
			command = strings.TrimSpace(info.Provider)
		}
		if command == "" {
			command = strings.TrimSpace(h.session.Provider)
		}
		if command == "" {
			return "", fmt.Errorf("%w: command is required for first start", ErrHandleConfig)
		}
		return command + " " + h.session.Resume.SessionIDFlag + " " + info.SessionKey, nil
	}
	return sessionpkg.BuildResumeCommand(info), nil
}

func (h *SessionHandle) providerLabel() string {
	if h.session.Profile != "" {
		return string(h.session.Profile)
	}
	return h.session.Provider
}

func (h *SessionHandle) historyProvider(info sessionpkg.Info) string {
	if h.session.Profile != "" {
		return string(h.session.Profile)
	}
	if strings.TrimSpace(info.Provider) != "" {
		return info.Provider
	}
	return h.session.Provider
}

func (h *SessionHandle) runtimeHints() runtime.Config {
	cfg := cloneRuntimeConfig(h.session.Hints)
	cfg.Env = mergeStringMaps(cfg.Env, h.session.Env)
	return cfg
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

type historyGeneration struct {
	TranscriptStreamID string
	GenerationID       string
}

func mergeConversationHistorySnapshots(previous, current *HistorySnapshot) *HistorySnapshot {
	if current == nil {
		return cloneHistorySnapshot(previous)
	}
	merged := cloneHistorySnapshot(current)
	if previous == nil || !sameHistoryConversation(previous, current) {
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
	merged := append(prev, curr[overlap:]...)
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
	cloned.Entries = cloneHistoryEntries(snapshot.Entries)
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
		if block.Interaction != nil {
			interaction := *block.Interaction
			interaction.Options = append([]string(nil), block.Interaction.Options...)
			interaction.Metadata = cloneStringMap(block.Interaction.Metadata)
			cloned[idx].Interaction = &interaction
		}
	}
	return cloned
}

func cloneHistoryRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func submitIntent(intent DeliveryIntent) sessionpkg.SubmitIntent {
	switch intent {
	case DeliveryIntentFollowUp:
		return sessionpkg.SubmitIntentFollowUp
	case DeliveryIntentInterruptNow:
		return sessionpkg.SubmitIntentInterruptNow
	default:
		return sessionpkg.SubmitIntentDefault
	}
}

func profileFamily(profile Profile) string {
	switch profile {
	case ProfileCodexTmuxCLI:
		return "codex"
	case ProfileGeminiTmuxCLI:
		return "gemini"
	case ProfileClaudeTmuxCLI:
		return "claude"
	default:
		return ""
	}
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func mergeStringMaps(base, extra map[string]string) map[string]string {
	switch {
	case len(base) == 0 && len(extra) == 0:
		return nil
	case len(base) == 0:
		return cloneStringMap(extra)
	case len(extra) == 0:
		return cloneStringMap(base)
	}
	out := cloneStringMap(base)
	for key, value := range extra {
		out[key] = value
	}
	return out
}

func cloneRuntimeConfig(cfg runtime.Config) runtime.Config {
	cfg.Env = cloneStringMap(cfg.Env)
	cfg.ProcessNames = append([]string(nil), cfg.ProcessNames...)
	cfg.PreStart = append([]string(nil), cfg.PreStart...)
	cfg.SessionSetup = append([]string(nil), cfg.SessionSetup...)
	cfg.SessionLive = append([]string(nil), cfg.SessionLive...)
	cfg.PackOverlayDirs = append([]string(nil), cfg.PackOverlayDirs...)
	cfg.CopyFiles = append([]runtime.CopyEntry(nil), cfg.CopyFiles...)
	cfg.FingerprintExtra = cloneStringMap(cfg.FingerprintExtra)
	return cfg
}

func cloneSessionSpec(spec SessionSpec) SessionSpec {
	spec.Env = cloneStringMap(spec.Env)
	spec.Metadata = cloneStringMap(spec.Metadata)
	spec.Hints = cloneRuntimeConfig(spec.Hints)
	return spec
}
