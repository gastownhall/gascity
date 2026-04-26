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
	Provider   string                     `json:"provider" doc:"Producing provider identifier (claude, codex, gemini, open-code, etc.)."`
	Format     string                     `json:"format"`
	Turns      []outputTurn               `json:"turns"`
	Pagination *sessionlog.PaginationInfo `json:"pagination,omitempty"`
}

// SessionStreamRawMessageEvent carries provider-native transcript frames on
// the session SSE stream.
type SessionStreamRawMessageEvent struct {
	ID         string                     `json:"id"`
	Template   string                     `json:"template"`
	Provider   string                     `json:"provider" doc:"Producing provider identifier (claude, codex, gemini, open-code, etc.). Consumers use this to dispatch per-provider frame parsing."`
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

func (s *Server) handleSessionStream(w http.ResponseWriter, r *http.Request) {
	store := s.state.CityBeadStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	id, err := s.resolveSessionIDAllowClosedWithConfig(store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}

	catalog, err := s.workerSessionCatalog(store)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	info, err := catalog.Get(id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	handle, err := s.workerHandleForSession(store, id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	historyReq := worker.HistoryRequest{}
	if r.URL.Query().Get("format") == "raw" && !info.Closed {
		historyReq.TailCompactions = 1
	}
	history, historyErr := handle.History(worker.WithoutOperationEvents(r.Context()), historyReq)
	hasHistory := historyErr == nil && history != nil
	if historyErr != nil && !errors.Is(historyErr, worker.ErrHistoryUnavailable) {
		writeError(w, http.StatusInternalServerError, "internal", "reading session history: "+historyErr.Error())
		return
	}

	state, stateErr := handle.State(r.Context())
	if stateErr != nil {
		writeSessionManagerError(w, stateErr)
		return
	}
	running := workerPhaseHasLiveOutput(state.Phase)
	if !hasHistory && !running {
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
	format := r.URL.Query().Get("format")
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
		if format == "raw" {
			s.emitClosedSessionSnapshotRaw(w, info, history)
		} else {
			s.emitClosedSessionSnapshot(w, info, history)
		}
		return
	}
	switch {
	case hasHistory:
		if format == "raw" {
			s.streamSessionTranscriptHistoryRaw(ctx, w, info, handle, history, historyReq)
		} else {
			s.streamSessionTranscriptHistory(ctx, w, info, handle, history)
		}
	case format == "raw":
		// No log file yet. If the session is running, poll tmux pane content
		// and wrap it as a fake raw JSONL assistant message so a real-world app's existing
		// rendering pipeline shows terminal output (e.g. OAuth prompts).
		if running {
			s.streamSessionPeekRaw(ctx, w, info, handle)
		} else {
			data, _ := json.Marshal(SessionStreamRawMessageEvent{
				ID:       info.ID,
				Template: info.Template,
				Provider: info.Provider,
				Format:   "raw",
				Messages: []SessionRawMessageFrame{},
			})
			writeSSE(w, "message", 1, data)
		}
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
		if pending.RequestID == lastPendingID {
			return false
		}
		lastPendingID = pending.RequestID
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

	_ = emitSnapshot(initial)
	if logPath != "" {
		poll.Stop()
		keepalive.Stop()
		lw = newLogFileWatcher(logPath)
		defer lw.Close()
		lw.Run(ctx, reloadSnapshot, func() { writeSSEComment(w) }, RunOpts{
			OnStall:      func() { _ = emitPending() },
			StallTimeout: sessionStreamPendingStallTimeout,
			Wake:         workerOps,
		})
		return
	}

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

	_ = emitSnapshot(initial)
	if logPath != "" {
		poll.Stop()
		keepalive.Stop()
		lw = newLogFileWatcher(logPath)
		defer lw.Close()
		lw.Run(ctx, reloadSnapshot, func() { writeSSEComment(w) }, RunOpts{Wake: workerOps})
		return
	}

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

	emitPending := func() {
		pending, pErr := handle.Pending(ctx)
		if pErr == nil && pending != nil && pending.RequestID != lastPeekPendingID {
			lastPeekPendingID = pending.RequestID
			seq++
			pendingData, _ := json.Marshal(pending)
			writeSSE(w, "pending", seq, pendingData)
		} else if pending == nil && lastPeekPendingID != "" {
			lastPeekPendingID = ""
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
				fakeMsg, _ := json.Marshal(syntheticAssistantFrame{
					Role:    "assistant",
					Content: []syntheticContentBlock{{Type: "text", Text: output}},
				})
				data, err := json.Marshal(SessionStreamRawMessageEvent{
					ID:       info.ID,
					Template: info.Template,
					Provider: info.Provider,
					Format:   "raw",
					Messages: wrapRawFrameBytes([]json.RawMessage{fakeMsg}),
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
		if pending.RequestID == lastPendingID {
			return false
		}
		lastPendingID = pending.RequestID
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

	_ = emitSnapshot(initial)
	if logPath != "" {
		poll.Stop()
		keepalive.Stop()
		lw = newLogFileWatcher(logPath)
		defer lw.Close()
		lw.Run(ctx, reloadSnapshot, func() {
			_ = send.Data(HeartbeatEvent{Timestamp: time.Now().UTC().Format(time.RFC3339)})
		}, RunOpts{
			OnStall:      func() { _ = emitPending() },
			StallTimeout: sessionStreamPendingStallTimeout,
			Wake:         workerOps,
		})
		return
	}

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

	_ = emitSnapshot(initial)
	if logPath != "" {
		poll.Stop()
		keepalive.Stop()
		lw = newLogFileWatcher(logPath)
		defer lw.Close()
		lw.Run(ctx, reloadSnapshot, func() {
			_ = send.Data(HeartbeatEvent{Timestamp: time.Now().UTC().Format(time.RFC3339)})
		}, RunOpts{Wake: workerOps})
		return
	}

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

func (s *Server) streamSessionPeekRawHuma(ctx context.Context, send sse.Sender, info session.Info) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	send = cancelOnSendError(send, cancel)

	handle, err := s.workerHandleForSession(s.state.CityBeadStore(), info.ID)
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

	emitPending := func() {
		pending, err := handle.Pending(ctx)
		if err == nil && pending != nil && pending.RequestID != lastPendingID {
			lastPendingID = pending.RequestID
			seq++
			_ = send(sse.Message{ID: seq, Data: runtimePendingInteraction(pending)})
		} else if pending == nil && lastPendingID != "" {
			lastPendingID = ""
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
			fakeMsg, err := json.Marshal(syntheticAssistantFrame{
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
					Messages: wrapRawFrameBytes([]json.RawMessage{fakeMsg}),
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

func (s *Server) streamSessionPeekHuma(ctx context.Context, send sse.Sender, info session.Info) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	send = cancelOnSendError(send, cancel)

	handle, err := s.workerHandleForSession(s.state.CityBeadStore(), info.ID)
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
