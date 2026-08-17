package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2/sse"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/worker"
)

// SessionStreamMessageEvent carries normalized conversation turns on the
// session SSE stream.
type SessionStreamMessageEvent struct {
	ID         string                     `json:"id"`
	Template   string                     `json:"template"`
	Provider   string                     `json:"provider" doc:"Producing provider identifier (claude, codex, gemini, opencode, etc.)."`
	Format     string                     `json:"format"`
	Turns      []outputTurn               `json:"turns"`
	Pagination *sessionlog.PaginationInfo `json:"pagination,omitempty"`
}

// SessionStreamRawMessageEvent carries provider-native transcript frames on
// the session SSE stream.
type SessionStreamRawMessageEvent struct {
	ID         string                     `json:"id"`
	Template   string                     `json:"template"`
	Provider   string                     `json:"provider" doc:"Producing provider identifier (claude, codex, gemini, opencode, etc.). Consumers use this to dispatch per-provider frame parsing."`
	Format     string                     `json:"format"`
	Messages   []SessionRawMessageFrame   `json:"messages" doc:"Provider-native transcript frames, emitted verbatim as the provider wrote them."`
	Pagination *sessionlog.PaginationInfo `json:"pagination,omitempty"`
}

type sessionStreamActivityPayload struct {
	Activity string `json:"activity"`
}

type syntheticContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type syntheticAssistantFrame struct {
	Role    string                  `json:"role"`
	Content []syntheticContentBlock `json:"content"`
}

var sessionStreamPendingStallTimeout = 5 * time.Second

func runtimePendingInteraction(pending *worker.PendingInteraction) runtime.PendingInteraction {
	return runtime.PendingInteraction{
		RequestID: pending.RequestID,
		Kind:      pending.Kind,
		Prompt:    pending.Prompt,
		Options:   append([]string(nil), pending.Options...),
		Metadata:  cloneStringMap(pending.Metadata),
	}
}

func pendingInteractionKey(pending *worker.PendingInteraction) string {
	if pending == nil {
		return ""
	}
	encoded, err := json.Marshal(runtimePendingInteraction(pending))
	if err != nil {
		log.Printf("session stream: pending interaction key encode failed for %s: %v", pending.RequestID, err)
		return pending.RequestID
	}
	return string(encoded)
}

func sessionStreamResumeToken(lastEventID, afterCursor string) string {
	if token := strings.TrimSpace(lastEventID); token != "" {
		return token
	}
	return strings.TrimSpace(afterCursor)
}

func (s *Server) handleSessionStream(w http.ResponseWriter, r *http.Request) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	id, err := s.resolveSessionIDAllowClosedWithConfig(store.Store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}

	catalog, err := s.workerSessionCatalog(store.Store)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	info, err := catalog.Get(id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	format := r.URL.Query().Get("format")
	includeThinking := queryBoolParam(r, "include_thinking")
	resumeToken := sessionStreamResumeToken(r.Header.Get("Last-Event-ID"), r.URL.Query().Get("after_cursor"))
	handle, err := s.workerHandleForSession(store.Store, id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	historyReq := worker.HistoryRequest{}
	if format == "raw" && !info.Closed {
		historyReq.TailCompactions = 1
	}
	history, historyErr := handle.History(worker.WithoutOperationEvents(r.Context()), historyReq)
	hasHistory := historyErr == nil && history != nil
	if historyErr != nil && !errors.Is(historyErr, worker.ErrHistoryUnavailable) {
		writeTranscriptReadError(w, historyErr, "reading session history")
		return
	}

	state, stateErr := handle.State(r.Context())
	if stateErr != nil {
		writeSessionManagerError(w, stateErr)
		return
	}
	running := workerPhaseHasLiveOutput(state.Phase)
	if !hasHistory && !running && format != "structured" {
		writeError(w, http.StatusNotFound, "not_found", "session "+id+" has no live output")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	if info.State != "" {
		w.Header().Set("GC-Session-State", string(info.State))
	}
	if !running {
		w.Header().Set("GC-Session-Status", "stopped")
	}
	w.WriteHeader(http.StatusOK)
	if err := http.NewResponseController(w).Flush(); err != nil {
		_ = err
	}

	ctx := r.Context()
	if format == "raw" && !info.Closed {
		data, _ := json.Marshal(SessionStreamRawMessageEvent{
			ID:       info.ID,
			Template: info.Template,
			Provider: info.Provider,
			Format:   "raw",
			Messages: []SessionRawMessageFrame{},
		})
		writeSSE(w, "message", 0, data)
	}
	if info.Closed {
		switch format {
		case "raw":
			s.emitClosedSessionSnapshotRaw(w, info, history)
		case "structured":
			s.emitClosedSessionSnapshotStructured(w, info, history, includeThinking, resumeToken)
		default:
			s.emitClosedSessionSnapshot(w, info, history)
		}
		return
	}
	if format == "structured" && !hasHistory && !running {
		s.emitStructuredFallbackSnapshot(w, info, "", includeThinking, resumeToken)
		return
	}
	switch {
	case hasHistory:
		switch format {
		case "raw":
			s.streamSessionTranscriptHistoryRaw(ctx, w, info, handle, history, historyReq)
		case "structured":
			s.streamSessionTranscriptHistoryStructured(ctx, w, info, handle, history, includeThinking, resumeToken, "", "")
		default:
			s.streamSessionTranscriptHistory(ctx, w, info, handle, history)
		}
	case format == "structured":
		s.streamSessionPeekStructured(ctx, w, info, handle, includeThinking, resumeToken)
		return
	case format == "raw":
		// No log file yet. If the session is running, poll tmux pane content
		// and wrap that live output as a synthetic raw JSONL assistant message
		// so a real-world app's existing rendering pipeline shows terminal
		// output (e.g. OAuth prompts).
		s.streamSessionPeekRaw(ctx, w, info, handle)
		return
	default:
		s.streamSessionPeek(ctx, w, info, handle)
	}
}

func workerPhaseHasLiveOutput(phase worker.Phase) bool {
	switch phase {
	case worker.PhaseStarting, worker.PhaseReady, worker.PhaseBusy, worker.PhaseBlocked, worker.PhaseStopping:
		return true
	default:
		return false
	}
}

func (s *Server) emitClosedSessionSnapshot(w http.ResponseWriter, info session.Info, history *worker.HistorySnapshot) {
	if history == nil {
		return
	}
	turns, _ := historySnapshotTurns(history)
	if len(turns) == 0 {
		return
	}

	data, err := json.Marshal(SessionStreamMessageEvent{
		ID:       info.ID,
		Template: info.Template,
		Provider: info.Provider,
		Format:   "conversation",
		Turns:    turns,
	})
	if err != nil {
		return
	}
	writeSSE(w, "turn", 1, data)
	actData, _ := json.Marshal(sessionStreamActivityPayload{Activity: "idle"})
	writeSSE(w, "activity", 2, actData)
}

func (s *Server) emitClosedSessionSnapshotRaw(w http.ResponseWriter, info session.Info, history *worker.HistorySnapshot) {
	if history == nil {
		return
	}
	rawMessages, _ := historySnapshotRawMessages(history)
	if len(rawMessages) == 0 {
		return
	}

	data, err := json.Marshal(SessionStreamRawMessageEvent{
		ID:       info.ID,
		Template: info.Template,
		Provider: info.Provider,
		Format:   "raw",
		Messages: wrapRawFrameBytes(rawMessages),
	})
	if err != nil {
		return
	}
	writeSSE(w, "message", 1, data)
	actData, _ := json.Marshal(sessionStreamActivityPayload{Activity: "idle"})
	writeSSE(w, "activity", 2, actData)
}

func (s *Server) emitClosedSessionSnapshotStructured(w http.ResponseWriter, info session.Info, history *worker.HistorySnapshot, includeThinking bool, resumeToken string) {
	if history == nil {
		s.emitStructuredFallbackSnapshot(w, info, "", includeThinking, resumeToken)
		return
	}
	messages, _ := historySnapshotStructuredMessages(history, includeThinking)
	projection := SessionStreamStructuredMessageEvent{
		ID:                 info.ID,
		Template:           info.Template,
		Provider:           info.Provider,
		Format:             "structured",
		SchemaVersion:      sessionStructuredSchemaVersion,
		History:            structuredHistoryFromSnapshot(history),
		StructuredMessages: messages,
		Pagination:         history.Pagination,
	}
	writeStructuredSSEUpdate(w, buildStructuredStreamUpdate(resumeToken, projection, includeThinking))
	actData, _ := json.Marshal(sessionStreamActivityPayload{Activity: "idle"})
	writeSSEWithoutID(w, "activity", actData)
}

func (s *Server) emitStructuredFallbackSnapshot(w http.ResponseWriter, info session.Info, output string, includeThinking bool, resumeToken string) {
	projection := SessionStreamStructuredMessageEvent{
		ID:                 info.ID,
		Template:           info.Template,
		Provider:           info.Provider,
		Format:             "structured",
		SchemaVersion:      sessionStructuredSchemaVersion,
		History:            structuredFallbackHistory(info.ID, info.SessionKey, string(worker.TailActivityIdle)),
		StructuredMessages: structuredFallbackMessages(info.ID, info.Provider, output),
	}
	writeStructuredSSEUpdate(w, buildStructuredStreamUpdate(resumeToken, projection, includeThinking))
	actData, _ := json.Marshal(sessionStreamActivityPayload{Activity: "idle"})
	writeSSEWithoutID(w, "activity", actData)
}

func writeStructuredSSEUpdate(w http.ResponseWriter, update *SessionStreamStructuredMessageEvent) {
	if update == nil || update.History == nil {
		return
	}
	data, err := json.Marshal(update)
	if err != nil {
		return
	}
	writeSSE(w, "structured", update.History.Cursor.ResumeToken, data)
}

func (s *Server) streamSessionTranscriptHistoryRaw(ctx context.Context, w http.ResponseWriter, info session.Info, handle interface {
	worker.HistoryHandle
	worker.InteractionHandle
}, initial *worker.HistorySnapshot, req worker.HistoryRequest,
) {
	logPath := sessionStreamTranscriptPath(ctx, handle)
	poll := time.NewTicker(outputStreamPollInterval)
	keepalive := time.NewTicker(sseKeepalive)
	workerOps := s.watchSessionWorkerOperationSignals(ctx, info)
	if logPath == "" {
		defer poll.Stop()
		defer keepalive.Stop()
	}

	var lastSentID string
	var seq uint64
	var lastActivity string
	var lastPendingID string
	var lastPendingKey string
	lastProgress := time.Now()
	sentIDs := make(map[string]struct{})
	currentActivity := historySnapshotActivity(initial)

	emitSnapshot := func(snapshot *worker.HistorySnapshot) bool {
		emitted := false
		if snapshot == nil {
			return false
		}
		currentActivity = historySnapshotActivity(snapshot)
		rawMessages, ids := historySnapshotRawMessages(snapshot)
		if len(rawMessages) > 0 {
			var toSend []json.RawMessage
			if lastSentID == "" {
				toSend = rawMessages
			} else {
				found := false
				for i, id := range ids {
					if id == lastSentID {
						toSend = rawMessages[i+1:]
						found = true
						break
					}
				}
				if !found {
					log.Printf("session stream raw: cursor %s lost, emitting only new messages", lastSentID)
					for i, id := range ids {
						if _, seen := sentIDs[id]; !seen {
							toSend = append(toSend, rawMessages[i])
						}
					}
				}
			}
			if len(toSend) > 0 {
				seq++
				data, err := json.Marshal(SessionStreamRawMessageEvent{
					ID:       info.ID,
					Template: info.Template,
					Provider: info.Provider,
					Format:   "raw",
					Messages: wrapRawFrameBytes(toSend),
				})
				if err == nil {
					writeSSE(w, "message", seq, data)
					lastProgress = time.Now()
					lastPendingID = ""
					lastPendingKey = ""
					emitted = true
				}
			}
			lastSentID = ids[len(ids)-1]
			for _, id := range ids {
				sentIDs[id] = struct{}{}
			}
		}
		if currentActivity != "" && currentActivity != lastActivity {
			lastActivity = currentActivity
			seq++
			actData, _ := json.Marshal(sessionStreamActivityPayload{Activity: currentActivity})
			writeSSE(w, "activity", seq, actData)
			lastProgress = time.Now()
			emitted = true
		}
		return emitted
	}

	emitPending := func() bool {
		if time.Since(lastProgress) < sessionStreamPendingStallTimeout {
			return false
		}
		pending, err := handle.Pending(ctx)
		if err != nil || pending == nil {
			if lastPendingID != "" {
				lastPendingID = ""
				lastPendingKey = ""
				activity := currentActivity
				if activity == "" {
					activity = "in-turn"
				}
				seq++
				actData, _ := json.Marshal(sessionStreamActivityPayload{Activity: activity})
				writeSSE(w, "activity", seq, actData)
				return true
			}
			return false
		}
		pendingKey := pendingInteractionKey(pending)
		if pendingKey == lastPendingKey {
			return false
		}
		lastPendingID = pending.RequestID
		lastPendingKey = pendingKey
		seq++
		pendingData, _ := json.Marshal(pending)
		writeSSE(w, "pending", seq, pendingData)
		return true
	}

	var lw *logFileWatcher
	reloadSnapshot := func() bool {
		emitted := false
		snapshot, err := handle.History(worker.WithoutOperationEvents(ctx), req)
		switch {
		case err == nil:
			emitted = emitSnapshot(snapshot)
		case errors.Is(err, worker.ErrHistoryUnavailable):
		default:
			log.Printf("session stream raw: history reload failed for %s: %v", info.ID, err)
		}
		emitted = emitPending() || emitted
		if lw != nil {
			lw.UpdatePath(sessionStreamTranscriptPath(ctx, handle))
		}
		return emitted
	}

	if logPath != "" {
		poll.Stop()
		keepalive.Stop()
		lw = newLogFileWatcher(logPath)
		defer lw.Close()
		_ = emitSnapshot(initial)
		lw.Run(ctx, reloadSnapshot, func() { writeSSEComment(w) }, RunOpts{
			OnStall:      func() { _ = emitPending() },
			StallTimeout: sessionStreamPendingStallTimeout,
			Wake:         workerOps,
		})
		return
	}

	_ = emitSnapshot(initial)
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			reloadSnapshot()
		case _, ok := <-workerOps:
			if !ok {
				workerOps = nil
				continue
			}
			reloadSnapshot()
		case <-keepalive.C:
			writeSSEComment(w)
		}
	}
}

func (s *Server) streamSessionTranscriptHistory(ctx context.Context, w http.ResponseWriter, info session.Info, handle worker.HistoryHandle, initial *worker.HistorySnapshot) {
	logPath := sessionStreamTranscriptPath(ctx, handle)
	poll := time.NewTicker(outputStreamPollInterval)
	keepalive := time.NewTicker(sseKeepalive)
	workerOps := s.watchSessionWorkerOperationSignals(ctx, info)
	if logPath == "" {
		defer poll.Stop()
		defer keepalive.Stop()
	}

	var lastSentID string
	var seq uint64
	var lastActivity string
	sentIDs := make(map[string]struct{})

	emitSnapshot := func(snapshot *worker.HistorySnapshot) bool {
		emitted := false
		if snapshot == nil {
			return false
		}
		turns, ids := historySnapshotTurns(snapshot)
		if len(turns) > 0 {
			var toSend []outputTurn
			if lastSentID == "" {
				toSend = turns
			} else {
				found := false
				for i, id := range ids {
					if id == lastSentID {
						toSend = turns[i+1:]
						found = true
						break
					}
				}
				if !found {
					log.Printf("session stream: cursor %s lost, emitting only new turns", lastSentID)
					for i, id := range ids {
						if _, seen := sentIDs[id]; !seen {
							toSend = append(toSend, turns[i])
						}
					}
				}
			}
			if len(toSend) > 0 {
				seq++
				data, err := json.Marshal(SessionStreamMessageEvent{
					ID:       info.ID,
					Template: info.Template,
					Provider: info.Provider,
					Format:   "conversation",
					Turns:    toSend,
				})
				if err == nil {
					writeSSE(w, "turn", seq, data)
					emitted = true
				}
			}
			lastSentID = ids[len(ids)-1]
			for _, id := range ids {
				sentIDs[id] = struct{}{}
			}
		}
		activity := historySnapshotActivity(snapshot)
		if activity != "" && activity != lastActivity {
			lastActivity = activity
			seq++
			actData, _ := json.Marshal(sessionStreamActivityPayload{Activity: activity})
			writeSSE(w, "activity", seq, actData)
			emitted = true
		}
		return emitted
	}

	var lw *logFileWatcher
	reloadSnapshot := func() bool {
		emitted := false
		snapshot, err := handle.History(worker.WithoutOperationEvents(ctx), worker.HistoryRequest{})
		switch {
		case err == nil:
			emitted = emitSnapshot(snapshot)
		case errors.Is(err, worker.ErrHistoryUnavailable):
		default:
			log.Printf("session stream: history reload failed for %s: %v", info.ID, err)
		}
		if lw != nil {
			lw.UpdatePath(sessionStreamTranscriptPath(ctx, handle))
		}
		return emitted
	}

	if logPath != "" {
		poll.Stop()
		keepalive.Stop()
		lw = newLogFileWatcher(logPath)
		defer lw.Close()
		_ = emitSnapshot(initial)
		lw.Run(ctx, reloadSnapshot, func() { writeSSEComment(w) }, RunOpts{Wake: workerOps})
		return
	}

	_ = emitSnapshot(initial)
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			reloadSnapshot()
		case _, ok := <-workerOps:
			if !ok {
				workerOps = nil
				continue
			}
			reloadSnapshot()
		case <-keepalive.C:
			writeSSEComment(w)
		}
	}
}

func (s *Server) streamSessionTranscriptHistoryStructured(ctx context.Context, w http.ResponseWriter, info session.Info, handle interface {
	worker.HistoryHandle
	worker.InteractionHandle
	worker.PeekHandle
}, initial *worker.HistorySnapshot, includeThinking bool, resumeToken, pendingRequestID, pendingKey string,
) {
	logPath := sessionStreamTranscriptPath(ctx, handle)
	poll := time.NewTicker(outputStreamPollInterval)
	keepalive := time.NewTicker(sseKeepalive)
	workerOps := s.watchSessionWorkerOperationSignals(ctx, info)
	if logPath == "" {
		defer poll.Stop()
		defer keepalive.Stop()
	}

	var lastActivity string
	lastPendingID := strings.TrimSpace(pendingRequestID)
	lastPendingKey := pendingKey
	lastProgress := time.Now()
	currentActivity := historySnapshotActivity(initial)
	currentResumeToken := resumeToken
	var hasStructuredProjection bool

	emitStructuredFallback := func() {
		if hasStructuredProjection {
			return
		}
		output, err := handle.Peek(ctx, 100)
		if errors.Is(err, session.ErrSessionInactive) {
			return
		}
		if err != nil {
			log.Printf("session stream structured: fallback peek failed for %s: %v", info.ID, err)
			output = ""
		}
		projection := SessionStreamStructuredMessageEvent{
			ID:                 info.ID,
			Template:           info.Template,
			Provider:           info.Provider,
			Format:             "structured",
			SchemaVersion:      sessionStructuredSchemaVersion,
			History:            structuredFallbackHistory(info.ID, info.SessionKey, string(worker.TailActivityInTurn)),
			StructuredMessages: structuredFallbackMessages(info.ID, info.Provider, output),
		}
		if update := buildStructuredStreamUpdate(currentResumeToken, projection, includeThinking); update != nil {
			currentResumeToken = update.History.Cursor.ResumeToken
			writeStructuredSSEUpdate(w, update)
		}
		hasStructuredProjection = true
	}

	emitSnapshot := func(snapshot *worker.HistorySnapshot) bool {
		emitted := false
		if snapshot == nil {
			return false
		}
		currentActivity = historySnapshotActivity(snapshot)
		hasStructuredProjection = true
		messages, _ := historySnapshotStructuredMessages(snapshot, includeThinking)
		projection := SessionStreamStructuredMessageEvent{
			ID:                 info.ID,
			Template:           info.Template,
			Provider:           info.Provider,
			Format:             "structured",
			SchemaVersion:      sessionStructuredSchemaVersion,
			History:            structuredHistoryFromSnapshot(snapshot),
			StructuredMessages: messages,
			Pagination:         snapshot.Pagination,
		}
		if update := buildStructuredStreamUpdate(currentResumeToken, projection, includeThinking); update != nil {
			currentResumeToken = update.History.Cursor.ResumeToken
			writeStructuredSSEUpdate(w, update)
			lastProgress = time.Now()
			emitted = true
		}
		activity := currentActivity
		if activity != "" && activity != lastActivity {
			lastActivity = activity
			actData, _ := json.Marshal(sessionStreamActivityPayload{Activity: activity})
			writeSSEWithoutID(w, "activity", actData)
			lastProgress = time.Now()
			emitted = true
		}
		return emitted
	}
	emitPending := func(force bool) bool {
		if !force && lastPendingID == "" && time.Since(lastProgress) < sessionStreamPendingStallTimeout {
			return false
		}
		pending, err := handle.Pending(ctx)
		if err != nil {
			log.Printf("session stream structured: pending read failed for %s: %v", info.ID, err)
			return false
		}
		if pending == nil {
			if lastPendingID == "" {
				return false
			}
			clearedData, _ := json.Marshal(SessionPendingClearedEvent{RequestID: lastPendingID})
			writeSSEWithoutID(w, "pending_cleared", clearedData)
			lastPendingID = ""
			lastPendingKey = ""
			activity := currentActivity
			if activity == "" {
				activity = "in-turn"
			}
			actData, _ := json.Marshal(sessionStreamActivityPayload{Activity: activity})
			writeSSEWithoutID(w, "activity", actData)
			return true
		}
		pendingKey := pendingInteractionKey(pending)
		if pendingKey == lastPendingKey {
			return false
		}
		lastPendingID = pending.RequestID
		lastPendingKey = pendingKey
		pendingData, _ := json.Marshal(runtimePendingInteraction(pending))
		writeSSEWithoutID(w, "pending", pendingData)
		return true
	}

	var lw *logFileWatcher
	reloadSnapshot := func() bool {
		emitted := false
		snapshot, err := handle.History(worker.WithoutOperationEvents(ctx), worker.HistoryRequest{})
		switch {
		case err == nil:
			emitted = emitSnapshot(snapshot)
		case errors.Is(err, worker.ErrHistoryUnavailable):
		default:
			log.Printf("session stream structured: history reload failed for %s: %v", info.ID, err)
		}
		emitted = emitPending(false) || emitted
		if lw != nil {
			lw.UpdatePath(sessionStreamTranscriptPath(ctx, handle))
		}
		return emitted
	}

	if logPath != "" {
		poll.Stop()
		keepalive.Stop()
		lw = newLogFileWatcher(logPath)
		defer lw.Close()
		_ = emitSnapshot(initial)
		emitStructuredFallback()
		_ = emitPending(true)
		lw.Run(ctx, reloadSnapshot, func() { writeSSEComment(w) }, RunOpts{
			OnStall:      func() { _ = emitPending(false) },
			StallTimeout: sessionStreamPendingStallTimeout,
			Wake:         workerOps,
		})
		return
	}

	_ = emitSnapshot(initial)
	emitStructuredFallback()
	_ = emitPending(true)
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			reloadSnapshot()
		case _, ok := <-workerOps:
			if !ok {
				workerOps = nil
				continue
			}
			reloadSnapshot()
		case <-keepalive.C:
			writeSSEComment(w)
		}
	}
}

// streamSessionPeekRaw polls tmux pane content and wraps it as format=raw
// messages so a real-world app's JSONL rendering pipeline can display terminal output
// (e.g. OAuth prompts, startup screens) when no transcript log exists yet.
func (s *Server) streamSessionPeekRaw(ctx context.Context, w http.ResponseWriter, info session.Info, handle interface {
	worker.PeekHandle
	worker.InteractionHandle
},
) {
	poll := time.NewTicker(outputStreamPollInterval)
	defer poll.Stop()
	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()
	workerOps := s.watchSessionWorkerOperationSignals(ctx, info)

	var lastOutput string
	var seq uint64
	var lastPeekPendingID string
	var lastPeekPendingKey string

	emitPending := func() {
		pending, pErr := handle.Pending(ctx)
		pendingKey := pendingInteractionKey(pending)
		if pErr == nil && pending != nil && pendingKey != lastPeekPendingKey {
			lastPeekPendingID = pending.RequestID
			lastPeekPendingKey = pendingKey
			seq++
			pendingData, _ := json.Marshal(pending)
			writeSSE(w, "pending", seq, pendingData)
		} else if pending == nil && lastPeekPendingID != "" {
			lastPeekPendingID = ""
			lastPeekPendingKey = ""
		}
	}

	emitPeek := func() {
		output, err := handle.Peek(ctx, 100)
		if errors.Is(err, session.ErrSessionInactive) {
			return
		}
		if err != nil {
			return
		}
		if output != lastOutput {
			lastOutput = output
			seq++
			if output != "" {
				syntheticMsg, _ := json.Marshal(syntheticAssistantFrame{
					Role:    "assistant",
					Content: []syntheticContentBlock{{Type: "text", Text: output}},
				})
				data, err := json.Marshal(SessionStreamRawMessageEvent{
					ID:       info.ID,
					Template: info.Template,
					Provider: info.Provider,
					Format:   "raw",
					Messages: wrapRawFrameBytes([]json.RawMessage{syntheticMsg}),
				})
				if err == nil {
					writeSSE(w, "message", seq, data)
				}
			}
		}
		emitPending()
	}
	emitPeek()

	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			emitPeek()
		case _, ok := <-workerOps:
			if !ok {
				workerOps = nil
				continue
			}
			emitPeek()
		case <-keepalive.C:
			writeSSEComment(w)
		}
	}
}

func (s *Server) streamSessionPeekStructured(ctx context.Context, w http.ResponseWriter, info session.Info, handle worker.Handle, includeThinking bool, resumeToken string,
) {
	poll := time.NewTicker(outputStreamPollInterval)
	defer poll.Stop()
	pollC := poll.C
	if s.structuredPeekPoll != nil {
		pollC = s.structuredPeekPoll
	}
	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()
	workerOps := s.watchSessionWorkerOperationSignals(ctx, info)

	var lastOutput string
	var emitted bool
	var lastPendingID string
	var lastPendingKey string
	currentResumeToken := resumeToken

	emitPending := func() {
		pending, err := handle.Pending(ctx)
		if err != nil {
			log.Printf("session stream structured: pending read failed for %s: %v", info.ID, err)
			return
		}
		pendingKey := pendingInteractionKey(pending)
		if pending != nil && pendingKey != lastPendingKey {
			lastPendingID = pending.RequestID
			lastPendingKey = pendingKey
			pendingData, _ := json.Marshal(runtimePendingInteraction(pending))
			writeSSEWithoutID(w, "pending", pendingData)
		} else if pending == nil && lastPendingID != "" {
			clearedData, _ := json.Marshal(SessionPendingClearedEvent{RequestID: lastPendingID})
			writeSSEWithoutID(w, "pending_cleared", clearedData)
			lastPendingID = ""
			lastPendingKey = ""
		}
	}

	emitPeek := func() {
		output, err := handle.Peek(ctx, 100)
		if errors.Is(err, session.ErrSessionInactive) {
			return
		}
		if err != nil || (emitted && output == lastOutput) {
			emitPending()
			return
		}
		lastOutput = output
		emitted = true
		projection := SessionStreamStructuredMessageEvent{
			ID:                 info.ID,
			Template:           info.Template,
			Provider:           info.Provider,
			Format:             "structured",
			SchemaVersion:      sessionStructuredSchemaVersion,
			History:            structuredFallbackHistory(info.ID, info.SessionKey, string(worker.TailActivityInTurn)),
			StructuredMessages: structuredFallbackMessages(info.ID, info.Provider, output),
		}
		if update := buildStructuredStreamUpdate(currentResumeToken, projection, includeThinking); update != nil {
			currentResumeToken = update.History.Cursor.ResumeToken
			writeStructuredSSEUpdate(w, update)
		}
		emitPending()
	}
	promoteToHistory := func() bool {
		snapshot, err := handle.History(worker.WithoutOperationEvents(ctx), worker.HistoryRequest{})
		switch {
		case err == nil:
			s.streamSessionTranscriptHistoryStructured(ctx, w, info, handle, snapshot, includeThinking, currentResumeToken, lastPendingID, lastPendingKey)
			return true
		case errors.Is(err, worker.ErrHistoryUnavailable):
			return false
		default:
			log.Printf("session stream structured: history promotion failed for %s: %v", info.ID, err)
			return false
		}
	}

	emitPeek()
	if promoteToHistory() {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-pollC:
			if promoteToHistory() {
				return
			}
			emitPeek()
		case _, ok := <-workerOps:
			if !ok {
				workerOps = nil
				continue
			}
			if promoteToHistory() {
				return
			}
			emitPeek()
		case <-keepalive.C:
			writeSSEComment(w)
		}
	}
}

func (s *Server) streamSessionPeek(ctx context.Context, w http.ResponseWriter, info session.Info, handle worker.PeekHandle) {
	poll := time.NewTicker(outputStreamPollInterval)
	defer poll.Stop()
	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()
	workerOps := s.watchSessionWorkerOperationSignals(ctx, info)

	var lastOutput string
	var seq uint64

	emitPeek := func() {
		output, err := handle.Peek(ctx, 100)
		if errors.Is(err, session.ErrSessionInactive) {
			return
		}
		if err != nil || output == lastOutput {
			return
		}
		lastOutput = output
		seq++

		turns := []outputTurn{}
		if output != "" {
			turns = append(turns, outputTurn{Role: "output", Text: output})
		}
		data, err := json.Marshal(SessionStreamMessageEvent{
			ID:       info.ID,
			Template: info.Template,
			Provider: info.Provider,
			Format:   "text",
			Turns:    turns,
		})
		if err != nil {
			return
		}
		writeSSE(w, "turn", seq, data)
	}

	emitPeek()

	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			emitPeek()
		case _, ok := <-workerOps:
			if !ok {
				workerOps = nil
				continue
			}
			emitPeek()
		case <-keepalive.C:
			writeSSEComment(w)
		}
	}
}

func (s *Server) streamSessionTranscriptLogRawHuma(ctx context.Context, send sse.Sender, info session.Info, handle interface {
	worker.HistoryHandle
	worker.InteractionHandle
}, initial *worker.HistorySnapshot, req worker.HistoryRequest,
) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	send = cancelOnSendError(send, cancel)

	logPath := sessionStreamTranscriptPath(ctx, handle)
	poll := time.NewTicker(outputStreamPollInterval)
	keepalive := time.NewTicker(sseKeepalive)
	workerOps := s.watchSessionWorkerOperationSignals(ctx, info)
	if logPath == "" {
		defer poll.Stop()
		defer keepalive.Stop()
	}

	var lastSentID string
	var seq int
	var lastActivity string
	var lastPendingID string
	var lastPendingKey string
	lastProgress := time.Now()
	sentIDs := make(map[string]struct{})
	currentActivity := historySnapshotActivity(initial)

	emitSnapshot := func(snapshot *worker.HistorySnapshot) bool {
		emitted := false
		if snapshot == nil {
			return false
		}
		currentActivity = historySnapshotActivity(snapshot)
		rawMessages, ids := historySnapshotRawMessages(snapshot)
		if len(rawMessages) > 0 {
			var toSend []json.RawMessage
			if lastSentID == "" {
				toSend = rawMessages
			} else {
				found := false
				for i, id := range ids {
					if id == lastSentID {
						toSend = rawMessages[i+1:]
						found = true
						break
					}
				}
				if !found {
					log.Printf("session stream raw: cursor %s lost, emitting only new messages", lastSentID)
					for i, id := range ids {
						if _, seen := sentIDs[id]; !seen {
							toSend = append(toSend, rawMessages[i])
						}
					}
				}
			}
			if len(toSend) > 0 {
				seq++
				_ = send(sse.Message{ID: seq, Data: SessionStreamRawMessageEvent{
					ID:       info.ID,
					Template: info.Template,
					Provider: info.Provider,
					Format:   "raw",
					Messages: wrapRawFrameBytes(toSend),
				}})
				lastProgress = time.Now()
				lastPendingID = ""
				lastPendingKey = ""
				emitted = true
			}
			lastSentID = ids[len(ids)-1]
			for _, id := range ids {
				sentIDs[id] = struct{}{}
			}
		}
		if currentActivity != "" && currentActivity != lastActivity {
			lastActivity = currentActivity
			seq++
			_ = send(sse.Message{ID: seq, Data: SessionActivityEvent{Activity: currentActivity}})
			lastProgress = time.Now()
			emitted = true
		}
		return emitted
	}

	emitPending := func() bool {
		if time.Since(lastProgress) < sessionStreamPendingStallTimeout {
			return false
		}
		pending, err := handle.Pending(ctx)
		if err != nil || pending == nil {
			if lastPendingID != "" {
				lastPendingID = ""
				lastPendingKey = ""
				activity := currentActivity
				if activity == "" {
					activity = "in-turn"
				}
				seq++
				_ = send(sse.Message{ID: seq, Data: SessionActivityEvent{Activity: activity}})
				return true
			}
			return false
		}
		pendingKey := pendingInteractionKey(pending)
		if pendingKey == lastPendingKey {
			return false
		}
		lastPendingID = pending.RequestID
		lastPendingKey = pendingKey
		seq++
		_ = send(sse.Message{ID: seq, Data: runtimePendingInteraction(pending)})
		return true
	}

	var lw *logFileWatcher
	reloadSnapshot := func() bool {
		emitted := false
		snapshot, err := handle.History(worker.WithoutOperationEvents(ctx), req)
		switch {
		case err == nil:
			emitted = emitSnapshot(snapshot)
		case errors.Is(err, worker.ErrHistoryUnavailable):
		default:
			log.Printf("session stream raw: history reload failed for %s: %v", info.ID, err)
		}
		emitted = emitPending() || emitted
		if lw != nil {
			lw.UpdatePath(sessionStreamTranscriptPath(ctx, handle))
		}
		return emitted
	}

	if logPath != "" {
		poll.Stop()
		keepalive.Stop()
		lw = newLogFileWatcher(logPath)
		defer lw.Close()
		_ = emitSnapshot(initial)
		lw.Run(ctx, reloadSnapshot, func() {
			_ = send.Data(HeartbeatEvent{Timestamp: time.Now().UTC().Format(time.RFC3339)})
		}, RunOpts{
			OnStall:      func() { _ = emitPending() },
			StallTimeout: sessionStreamPendingStallTimeout,
			Wake:         workerOps,
		})
		return
	}

	_ = emitSnapshot(initial)
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			reloadSnapshot()
		case _, ok := <-workerOps:
			if !ok {
				workerOps = nil
				continue
			}
			reloadSnapshot()
		case <-keepalive.C:
			_ = send.Data(HeartbeatEvent{Timestamp: time.Now().UTC().Format(time.RFC3339)})
		}
	}
}

func (s *Server) streamSessionTranscriptLogHuma(ctx context.Context, send sse.Sender, info session.Info, handle worker.HistoryHandle, initial *worker.HistorySnapshot) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	send = cancelOnSendError(send, cancel)

	logPath := sessionStreamTranscriptPath(ctx, handle)
	poll := time.NewTicker(outputStreamPollInterval)
	keepalive := time.NewTicker(sseKeepalive)
	workerOps := s.watchSessionWorkerOperationSignals(ctx, info)
	if logPath == "" {
		defer poll.Stop()
		defer keepalive.Stop()
	}

	var lastSentID string
	var seq int
	var lastActivity string
	sentIDs := make(map[string]struct{})

	emitSnapshot := func(snapshot *worker.HistorySnapshot) bool {
		emitted := false
		if snapshot == nil {
			return false
		}
		turns, ids := historySnapshotTurns(snapshot)
		if len(turns) > 0 {
			var toSend []outputTurn
			if lastSentID == "" {
				toSend = turns
			} else {
				found := false
				for i, id := range ids {
					if id == lastSentID {
						toSend = turns[i+1:]
						found = true
						break
					}
				}
				if !found {
					log.Printf("session stream: cursor %s lost, emitting only new turns", lastSentID)
					for i, id := range ids {
						if _, seen := sentIDs[id]; !seen {
							toSend = append(toSend, turns[i])
						}
					}
				}
			}

			if len(toSend) > 0 {
				seq++
				_ = send(sse.Message{ID: seq, Data: SessionStreamMessageEvent{
					ID:       info.ID,
					Template: info.Template,
					Provider: info.Provider,
					Format:   "conversation",
					Turns:    toSend,
				}})
				emitted = true
			}
			lastSentID = ids[len(ids)-1]
			for _, id := range ids {
				sentIDs[id] = struct{}{}
			}
		}

		activity := historySnapshotActivity(snapshot)
		if activity != "" && activity != lastActivity {
			lastActivity = activity
			seq++
			_ = send(sse.Message{ID: seq, Data: SessionActivityEvent{Activity: activity}})
			emitted = true
		}
		return emitted
	}

	var lw *logFileWatcher
	reloadSnapshot := func() bool {
		emitted := false
		snapshot, err := handle.History(worker.WithoutOperationEvents(ctx), worker.HistoryRequest{})
		switch {
		case err == nil:
			emitted = emitSnapshot(snapshot)
		case errors.Is(err, worker.ErrHistoryUnavailable):
		default:
			log.Printf("session stream: history reload failed for %s: %v", info.ID, err)
		}
		if lw != nil {
			lw.UpdatePath(sessionStreamTranscriptPath(ctx, handle))
		}
		return emitted
	}

	if logPath != "" {
		poll.Stop()
		keepalive.Stop()
		lw = newLogFileWatcher(logPath)
		defer lw.Close()
		_ = emitSnapshot(initial)
		lw.Run(ctx, reloadSnapshot, func() {
			_ = send.Data(HeartbeatEvent{Timestamp: time.Now().UTC().Format(time.RFC3339)})
		}, RunOpts{Wake: workerOps})
		return
	}

	_ = emitSnapshot(initial)
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			reloadSnapshot()
		case _, ok := <-workerOps:
			if !ok {
				workerOps = nil
				continue
			}
			reloadSnapshot()
		case <-keepalive.C:
			_ = send.Data(HeartbeatEvent{Timestamp: time.Now().UTC().Format(time.RFC3339)})
		}
	}
}

func (s *Server) streamSessionTranscriptLogStructuredHuma(ctx context.Context, send StringIDSender, info session.Info, handle interface {
	worker.HistoryHandle
	worker.InteractionHandle
	worker.PeekHandle
}, initial *worker.HistorySnapshot, includeThinking bool, resumeToken, pendingRequestID, pendingKey string,
) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	send = cancelOnStringIDSendError(send, cancel)

	logPath := sessionStreamTranscriptPath(ctx, handle)
	poll := time.NewTicker(outputStreamPollInterval)
	keepalive := time.NewTicker(sseKeepalive)
	workerOps := s.watchSessionWorkerOperationSignals(ctx, info)
	if logPath == "" {
		defer poll.Stop()
		defer keepalive.Stop()
	}

	var lastActivity string
	lastPendingID := strings.TrimSpace(pendingRequestID)
	lastPendingKey := pendingKey
	lastProgress := time.Now()
	currentActivity := historySnapshotActivity(initial)
	currentResumeToken := resumeToken
	var hasStructuredProjection bool

	emitStructuredFallback := func() {
		if hasStructuredProjection {
			return
		}
		output, err := handle.Peek(ctx, 100)
		if errors.Is(err, session.ErrSessionInactive) {
			return
		}
		if err != nil {
			log.Printf("session stream structured: fallback peek failed for %s: %v", info.ID, err)
			output = ""
		}
		projection := SessionStreamStructuredMessageEvent{
			ID:                 info.ID,
			Template:           info.Template,
			Provider:           info.Provider,
			Format:             "structured",
			SchemaVersion:      sessionStructuredSchemaVersion,
			History:            structuredFallbackHistory(info.ID, info.SessionKey, string(worker.TailActivityInTurn)),
			StructuredMessages: structuredFallbackMessages(info.ID, info.Provider, output),
		}
		if update := buildStructuredStreamUpdate(currentResumeToken, projection, includeThinking); update != nil {
			currentResumeToken = update.History.Cursor.ResumeToken
			_ = send(StringIDMessage{ID: currentResumeToken, Data: *update})
		}
		hasStructuredProjection = true
	}

	emitSnapshot := func(snapshot *worker.HistorySnapshot) bool {
		emitted := false
		if snapshot == nil {
			return false
		}
		currentActivity = historySnapshotActivity(snapshot)
		hasStructuredProjection = true
		messages, _ := historySnapshotStructuredMessages(snapshot, includeThinking)
		projection := SessionStreamStructuredMessageEvent{
			ID:                 info.ID,
			Template:           info.Template,
			Provider:           info.Provider,
			Format:             "structured",
			SchemaVersion:      sessionStructuredSchemaVersion,
			History:            structuredHistoryFromSnapshot(snapshot),
			StructuredMessages: messages,
			Pagination:         snapshot.Pagination,
		}
		if update := buildStructuredStreamUpdate(currentResumeToken, projection, includeThinking); update != nil {
			currentResumeToken = update.History.Cursor.ResumeToken
			_ = send(StringIDMessage{ID: currentResumeToken, Data: *update})
			lastProgress = time.Now()
			emitted = true
		}

		activity := currentActivity
		if activity != "" && activity != lastActivity {
			lastActivity = activity
			_ = send(StringIDMessage{Data: SessionActivityEvent{Activity: activity}})
			lastProgress = time.Now()
			emitted = true
		}
		return emitted
	}
	emitPending := func(force bool) bool {
		if !force && lastPendingID == "" && time.Since(lastProgress) < sessionStreamPendingStallTimeout {
			return false
		}
		pending, err := handle.Pending(ctx)
		if err != nil {
			log.Printf("session stream structured: pending read failed for %s: %v", info.ID, err)
			return false
		}
		if pending == nil {
			if lastPendingID == "" {
				return false
			}
			_ = send(StringIDMessage{Data: SessionPendingClearedEvent{RequestID: lastPendingID}})
			lastPendingID = ""
			lastPendingKey = ""
			activity := currentActivity
			if activity == "" {
				activity = "in-turn"
			}
			_ = send(StringIDMessage{Data: SessionActivityEvent{Activity: activity}})
			return true
		}
		pendingKey := pendingInteractionKey(pending)
		if pendingKey == lastPendingKey {
			return false
		}
		lastPendingID = pending.RequestID
		lastPendingKey = pendingKey
		_ = send(StringIDMessage{Data: runtimePendingInteraction(pending)})
		return true
	}

	var lw *logFileWatcher
	reloadSnapshot := func() bool {
		emitted := false
		snapshot, err := handle.History(worker.WithoutOperationEvents(ctx), worker.HistoryRequest{})
		switch {
		case err == nil:
			emitted = emitSnapshot(snapshot)
		case errors.Is(err, worker.ErrHistoryUnavailable):
		default:
			log.Printf("session stream structured: history reload failed for %s: %v", info.ID, err)
		}
		emitted = emitPending(false) || emitted
		if lw != nil {
			lw.UpdatePath(sessionStreamTranscriptPath(ctx, handle))
		}
		return emitted
	}

	if logPath != "" {
		poll.Stop()
		keepalive.Stop()
		lw = newLogFileWatcher(logPath)
		defer lw.Close()
		_ = emitSnapshot(initial)
		emitStructuredFallback()
		_ = emitPending(true)
		lw.Run(ctx, reloadSnapshot, func() {
			_ = send(StringIDMessage{Data: HeartbeatEvent{Timestamp: time.Now().UTC().Format(time.RFC3339)}})
		}, RunOpts{
			OnStall:      func() { _ = emitPending(false) },
			StallTimeout: sessionStreamPendingStallTimeout,
			Wake:         workerOps,
		})
		return
	}

	_ = emitSnapshot(initial)
	emitStructuredFallback()
	_ = emitPending(true)
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			reloadSnapshot()
		case _, ok := <-workerOps:
			if !ok {
				workerOps = nil
				continue
			}
			reloadSnapshot()
		case <-keepalive.C:
			_ = send(StringIDMessage{Data: HeartbeatEvent{Timestamp: time.Now().UTC().Format(time.RFC3339)}})
		}
	}
}

func (s *Server) streamSessionPeekRawHuma(ctx context.Context, send sse.Sender, info session.Info) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	send = cancelOnSendError(send, cancel)

	handle, err := s.workerHandleForSession(s.state.SessionsBeadStore().Store, info.ID)
	if err != nil {
		return
	}
	poll := time.NewTicker(outputStreamPollInterval)
	defer poll.Stop()
	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()
	workerOps := s.watchSessionWorkerOperationSignals(ctx, info)

	var lastOutput string
	var seq int
	var lastPendingID string
	var lastPendingKey string

	emitPending := func() {
		pending, err := handle.Pending(ctx)
		pendingKey := pendingInteractionKey(pending)
		if err == nil && pending != nil && pendingKey != lastPendingKey {
			lastPendingID = pending.RequestID
			lastPendingKey = pendingKey
			seq++
			_ = send(sse.Message{ID: seq, Data: runtimePendingInteraction(pending)})
		} else if pending == nil && lastPendingID != "" {
			lastPendingID = ""
			lastPendingKey = ""
		}
	}

	emitPeek := func() {
		output, err := handle.Peek(ctx, 100)
		if errors.Is(err, session.ErrSessionInactive) {
			return
		}
		if err != nil || output == lastOutput {
			emitPending()
			return
		}
		lastOutput = output

		if output != "" {
			syntheticMsg, err := json.Marshal(syntheticAssistantFrame{
				Role:    "assistant",
				Content: []syntheticContentBlock{{Type: "text", Text: output}},
			})
			if err == nil {
				seq++
				_ = send(sse.Message{ID: seq, Data: SessionStreamRawMessageEvent{
					ID:       info.ID,
					Template: info.Template,
					Provider: info.Provider,
					Format:   "raw",
					Messages: wrapRawFrameBytes([]json.RawMessage{syntheticMsg}),
				}})
			}
		}

		emitPending()
	}

	emitPeek()

	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			emitPeek()
		case _, ok := <-workerOps:
			if !ok {
				workerOps = nil
				continue
			}
			emitPeek()
		case <-keepalive.C:
			_ = send.Data(HeartbeatEvent{Timestamp: time.Now().UTC().Format(time.RFC3339)})
		}
	}
}

func (s *Server) streamSessionPeekStructuredHuma(ctx context.Context, send StringIDSender, info session.Info, handle worker.Handle, includeThinking bool, resumeToken string) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	send = cancelOnStringIDSendError(send, cancel)
	poll := time.NewTicker(outputStreamPollInterval)
	defer poll.Stop()
	pollC := poll.C
	if s.structuredPeekPoll != nil {
		pollC = s.structuredPeekPoll
	}
	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()
	workerOps := s.watchSessionWorkerOperationSignals(ctx, info)

	var lastOutput string
	var emitted bool
	var lastPendingID string
	var lastPendingKey string
	currentResumeToken := resumeToken

	emitPending := func() {
		pending, err := handle.Pending(ctx)
		if err != nil {
			log.Printf("session stream structured: pending read failed for %s: %v", info.ID, err)
			return
		}
		pendingKey := pendingInteractionKey(pending)
		if pending != nil && pendingKey != lastPendingKey {
			lastPendingID = pending.RequestID
			lastPendingKey = pendingKey
			_ = send(StringIDMessage{Data: runtimePendingInteraction(pending)})
		} else if pending == nil && lastPendingID != "" {
			_ = send(StringIDMessage{Data: SessionPendingClearedEvent{RequestID: lastPendingID}})
			lastPendingID = ""
			lastPendingKey = ""
		}
	}

	emitPeek := func() {
		output, err := handle.Peek(ctx, 100)
		if errors.Is(err, session.ErrSessionInactive) {
			return
		}
		if err != nil || (emitted && output == lastOutput) {
			emitPending()
			return
		}
		lastOutput = output
		emitted = true

		projection := SessionStreamStructuredMessageEvent{
			ID:                 info.ID,
			Template:           info.Template,
			Provider:           info.Provider,
			Format:             "structured",
			SchemaVersion:      sessionStructuredSchemaVersion,
			History:            structuredFallbackHistory(info.ID, info.SessionKey, string(worker.TailActivityInTurn)),
			StructuredMessages: structuredFallbackMessages(info.ID, info.Provider, output),
		}
		if update := buildStructuredStreamUpdate(currentResumeToken, projection, includeThinking); update != nil {
			currentResumeToken = update.History.Cursor.ResumeToken
			_ = send(StringIDMessage{ID: currentResumeToken, Data: *update})
		}

		emitPending()
	}
	promoteToHistory := func() bool {
		snapshot, err := handle.History(worker.WithoutOperationEvents(ctx), worker.HistoryRequest{})
		switch {
		case err == nil:
			s.streamSessionTranscriptLogStructuredHuma(ctx, send, info, handle, snapshot, includeThinking, currentResumeToken, lastPendingID, lastPendingKey)
			return true
		case errors.Is(err, worker.ErrHistoryUnavailable):
			return false
		default:
			log.Printf("session stream structured: history promotion failed for %s: %v", info.ID, err)
			return false
		}
	}

	emitPeek()
	if promoteToHistory() {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-pollC:
			if promoteToHistory() {
				return
			}
			emitPeek()
		case _, ok := <-workerOps:
			if !ok {
				workerOps = nil
				continue
			}
			if promoteToHistory() {
				return
			}
			emitPeek()
		case <-keepalive.C:
			_ = send(StringIDMessage{Data: HeartbeatEvent{Timestamp: time.Now().UTC().Format(time.RFC3339)}})
		}
	}
}

func (s *Server) streamSessionPeekHuma(ctx context.Context, send sse.Sender, info session.Info) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	send = cancelOnSendError(send, cancel)

	handle, err := s.workerHandleForSession(s.state.SessionsBeadStore().Store, info.ID)
	if err != nil {
		return
	}
	poll := time.NewTicker(outputStreamPollInterval)
	defer poll.Stop()
	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()
	workerOps := s.watchSessionWorkerOperationSignals(ctx, info)

	var lastOutput string
	var seq int

	emitPeek := func() {
		output, err := handle.Peek(ctx, 100)
		if errors.Is(err, session.ErrSessionInactive) {
			return
		}
		if err != nil || output == lastOutput {
			return
		}
		lastOutput = output
		seq++

		turns := []outputTurn{}
		if output != "" {
			turns = append(turns, outputTurn{Role: "output", Text: output})
		}
		_ = send(sse.Message{ID: seq, Data: SessionStreamMessageEvent{
			ID:       info.ID,
			Template: info.Template,
			Provider: info.Provider,
			Format:   "text",
			Turns:    turns,
		}})
	}

	emitPeek()

	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			emitPeek()
		case _, ok := <-workerOps:
			if !ok {
				workerOps = nil
				continue
			}
			emitPeek()
		case <-keepalive.C:
			_ = send.Data(HeartbeatEvent{Timestamp: time.Now().UTC().Format(time.RFC3339)})
		}
	}
}

func sessionStreamTranscriptPath(ctx context.Context, handle any) string {
	pathHandle, ok := handle.(interface {
		TranscriptPath(context.Context) (string, error)
	})
	if !ok {
		return ""
	}
	path, err := pathHandle.TranscriptPath(worker.WithoutOperationEvents(ctx))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(path)
}
