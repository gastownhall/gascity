package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/sessionlog"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
	"github.com/gastownhall/gascity/internal/worker"
)

// outputTurn is a single conversation turn in the unified output response.
type outputTurn struct {
	Role      string `json:"role"`
	Text      string `json:"text"`
	Timestamp string `json:"timestamp,omitempty"`
}

// agentOutputResponse is the response for GET /v0/agent/{name}/output.
type agentOutputResponse struct {
	Agent      string                     `json:"agent"`
	Format     string                     `json:"format"` // "conversation" or "text"
	Turns      []outputTurn               `json:"turns"`
	Pagination *sessionlog.PaginationInfo `json:"pagination,omitempty"`
}

// handleAgentOutput returns unified conversation output for an agent.
// Tries structured session logs first, falls back to Peek().
//
// Query params:
//   - tail: number of compaction segments to return (default 1, 0 = all)
//   - before: message UUID cursor for loading older messages
func (s *Server) handleAgentOutput(w http.ResponseWriter, r *http.Request, name string) {
	cfg := s.state.Config()
	agentCfg, ok := findAgent(cfg, name)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent "+name+" not found")
		return
	}

	// Try structured session log first.
	resp, err := s.trySessionLogOutput(r, name, agentCfg)
	if err != nil {
		// Session file exists but failed to read — surface the error.
		writeError(w, http.StatusInternalServerError, "internal", "reading session log: "+err.Error())
		return
	}
	if resp != nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// No session file found — fall back to Peek() (raw terminal text).
	s.peekFallbackOutput(w, name, cfg)
}

// trySessionLogOutput attempts to read structured conversation data from
// a Claude JSONL session file. Returns (nil, nil) if no session file is
// found (expected — triggers fallback). Returns (nil, err) if the file
// exists but cannot be read (unexpected — surface to caller).
func (s *Server) trySessionLogOutput(r *http.Request, name string, agentCfg config.Agent) (*agentOutputResponse, error) {
	cfg := s.state.Config()
	workDir := s.resolveAgentWorkDir(agentCfg, name)
	if workDir == "" {
		return nil, nil
	}
	provider := strings.TrimSpace(agentCfg.Provider)
	if provider == "" && cfg != nil {
		provider = strings.TrimSpace(cfg.Workspace.Provider)
	}

	searchPaths := s.sessionLogSearchPaths
	if searchPaths == nil {
		searchPaths = sessionlog.MergeSearchPaths(cfg.Daemon.ObservePaths)
	}
	adapter := worker.SessionLogAdapter{SearchPaths: searchPaths}
	path := adapter.DiscoverTranscript(provider, workDir, "")
	if path == "" {
		return nil, nil
	}

	tail := 1
	if v := r.URL.Query().Get("tail"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			tail = n
		}
	}
	before := r.URL.Query().Get("before")

	transcript, err := adapter.ReadTranscript(worker.TranscriptRequest{
		Provider:        provider,
		TranscriptPath:  path,
		TailCompactions: tail,
		BeforeEntryID:   before,
	})
	if err != nil {
		return nil, err
	}
	sess := transcript.Session

	turns := make([]outputTurn, 0, len(sess.Messages))
	for _, e := range sess.Messages {
		turn := entryToTurn(e)
		if turn.Text == "" {
			continue
		}
		turns = append(turns, turn)
	}

	return &agentOutputResponse{
		Agent:      name,
		Format:     "conversation",
		Turns:      turns,
		Pagination: sess.Pagination,
	}, nil
}

// peekFallbackOutput returns raw terminal text wrapped as a single turn.
func (s *Server) peekFallbackOutput(w http.ResponseWriter, name string, cfg *config.City) {
	sp := s.state.SessionProvider()
	sessionName := agentSessionName(s.state.CityName(), name, cfg.Workspace.SessionTemplate)

	if !sp.IsRunning(sessionName) {
		writeError(w, http.StatusNotFound, "not_found", "agent "+name+" not running")
		return
	}

	output, err := sp.Peek(sessionName, 100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	turns := []outputTurn{}
	if output != "" {
		turns = append(turns, outputTurn{Role: "output", Text: output})
	}

	writeJSON(w, http.StatusOK, agentOutputResponse{
		Agent:  name,
		Format: "text",
		Turns:  turns,
	})
}

// resolveAgentWorkDir returns the absolute working directory for an agent,
// honoring work_dir template expansion.
func (s *Server) resolveAgentWorkDir(a config.Agent, qualifiedName string) string {
	cfg := s.state.Config()
	return workdirutil.ResolveWorkDirPath(
		s.state.CityPath(),
		workdirutil.CityName(s.state.CityPath(), cfg),
		qualifiedName,
		a,
		cfg.Rigs,
	)
}

// entryToTurn converts a sessionlog Entry to a human-readable outputTurn.
func entryToTurn(e *sessionlog.Entry) outputTurn {
	turn := outputTurn{
		Role: e.Type,
	}
	if !e.Timestamp.IsZero() {
		turn.Timestamp = e.Timestamp.Format("2006-01-02T15:04:05Z07:00")
	}

	// Try plain string content (message is a JSON object with string content).
	if text := e.TextContent(); text != "" {
		turn.Text = text
		return turn
	}

	// Try structured content blocks — extract human-readable text.
	if blocks := e.ContentBlocks(); len(blocks) > 0 {
		var parts []string
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if b.Text != "" {
					parts = append(parts, b.Text)
				}
			case "tool_use":
				if b.Name != "" {
					parts = append(parts, "["+b.Name+"]")
				}
			case "tool_result":
				text := extractToolResultText(b.Content)
				if text != "" {
					if len(text) > 500 {
						text = text[:500] + "…"
					}
					parts = append(parts, "[result] "+text)
				}
			case "thinking":
				// Redact thinking blocks — internal model reasoning
				// should not be surfaced to the UI.
				parts = append(parts, "[thinking]")
			}
		}
		turn.Text = strings.Join(parts, "\n")
		return turn
	}

	// Claude JSONL double-encodes the message field as a JSON string
	// containing JSON. Unwrap and try again.
	turn.Text = unwrapDoubleEncoded(e.Message)
	return turn
}

func historyEntryToTurn(entry worker.HistoryEntry) outputTurn {
	turn := outputTurn{
		Role: entry.Kind,
	}
	if turn.Role == "" {
		turn.Role = string(entry.Actor)
	}
	if entry.Timestamp != nil {
		turn.Timestamp = entry.Timestamp.Format("2006-01-02T15:04:05Z07:00")
	}

	if len(entry.Blocks) > 0 {
		var parts []string
		for _, block := range entry.Blocks {
			switch block.Kind {
			case worker.BlockKindText:
				if block.Text != "" {
					parts = append(parts, block.Text)
				}
			case worker.BlockKindToolUse:
				if block.Name != "" {
					parts = append(parts, "["+block.Name+"]")
				}
			case worker.BlockKindToolResult:
				text := extractToolResultText(block.Content)
				if text != "" {
					if len(text) > 500 {
						text = text[:500] + "…"
					}
					parts = append(parts, "[result] "+text)
				}
			case worker.BlockKindThinking:
				parts = append(parts, "[thinking]")
			}
		}
		turn.Text = strings.Join(parts, "\n")
		if turn.Text != "" {
			return turn
		}
	}

	if strings.TrimSpace(entry.Text) != "" {
		turn.Text = entry.Text
		return turn
	}
	if turn.Text == "" {
		turn.Text = historyRawEntryText(entry.Provenance.Raw)
	}
	return turn
}

func historySnapshotTurns(snapshot *worker.HistorySnapshot) ([]outputTurn, []string) {
	if snapshot == nil {
		return nil, nil
	}
	turns := make([]outputTurn, 0, len(snapshot.Entries))
	ids := make([]string, 0, len(snapshot.Entries))
	for _, entry := range snapshot.Entries {
		if !historyEntryVisibleInConversation(entry) {
			continue
		}
		turn := historyEntryToTurn(entry)
		if turn.Text == "" {
			continue
		}
		turns = append(turns, turn)
		ids = append(ids, entry.ID)
	}
	return turns, ids
}

func historyEntryVisibleInConversation(entry worker.HistoryEntry) bool {
	switch entry.Provenance.RawType {
	case "user", "assistant", "system", "result":
		return true
	}
	switch entry.Kind {
	case "user", "assistant", "system", "result":
		return true
	default:
		return false
	}
}

func historySnapshotRawMessages(snapshot *worker.HistorySnapshot) ([]json.RawMessage, []string) {
	if snapshot == nil {
		return nil, nil
	}
	rawMessages := make([]json.RawMessage, 0, len(snapshot.Entries))
	ids := make([]string, 0, len(snapshot.Entries))
	for _, entry := range snapshot.Entries {
		if len(entry.Provenance.Raw) == 0 {
			continue
		}
		rawMessages = append(rawMessages, entry.Provenance.Raw)
		ids = append(ids, entry.ID)
	}
	return rawMessages, ids
}

func historySnapshotActivity(snapshot *worker.HistorySnapshot) string {
	if snapshot == nil {
		return ""
	}
	switch snapshot.TailState.Activity {
	case worker.TailActivityIdle:
		return "idle"
	case worker.TailActivityInTurn:
		return "in-turn"
	default:
		return ""
	}
}

// extractToolResultText extracts human-readable text from a tool_result
// Content field (json.RawMessage). The content can be a plain string or
// an array of content blocks (e.g., [{type:"text", text:"..."}]).
func extractToolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try plain string.
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return s
	}
	// Try array of content blocks.
	var blocks []sessionlog.ContentBlock
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// outputStreamPollInterval controls how often the stream checks for new output.
const outputStreamPollInterval = 2 * time.Second

// handleAgentOutputStream streams agent output as SSE events.
// New turns are sent as they appear; keepalives are sent every 15s.
//
// SSE event format:
//
//	event: turn
//	data: {"turns": [...]}
func (s *Server) handleAgentOutputStream(w http.ResponseWriter, r *http.Request, name string) {
	cfg := s.state.Config()
	agentCfg, ok := findAgent(cfg, name)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent "+name+" not found")
		return
	}

	// Try session log streaming first, fall back to peek polling.
	workDir := s.resolveAgentWorkDir(agentCfg, name)
	provider := strings.TrimSpace(agentCfg.Provider)
	if provider == "" {
		provider = strings.TrimSpace(cfg.Workspace.Provider)
	}
	searchPaths := s.sessionLogSearchPaths
	if searchPaths == nil {
		searchPaths = sessionlog.MergeSearchPaths(cfg.Daemon.ObservePaths)
	}
	adapter := worker.SessionLogAdapter{SearchPaths: searchPaths}

	var logPath string
	if workDir != "" {
		logPath = adapter.DiscoverTranscript(provider, workDir, "")
	}

	// Check if agent is running.
	sp := s.state.SessionProvider()
	sessionName := agentSessionName(s.state.CityName(), name, cfg.Workspace.SessionTemplate)
	running := sp.IsRunning(sessionName)

	// If no session log and agent isn't running, return 404 before committing SSE headers.
	if logPath == "" && !running {
		writeError(w, http.StatusNotFound, "not_found", "agent "+name+" not running")
		return
	}

	// Commit SSE headers. Include agent status so clients can distinguish
	// live streaming from historical replay.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	if !running {
		w.Header().Set("GC-Agent-Status", "stopped")
	}
	w.WriteHeader(http.StatusOK)
	if err := http.NewResponseController(w).Flush(); err != nil {
		_ = err
	}

	ctx := r.Context()
	if logPath != "" {
		s.streamSessionLog(ctx, w, name, provider, logPath)
	} else {
		s.streamPeekOutput(ctx, w, name, cfg)
	}
}

// streamSessionLog polls a session log file and emits new turns as SSE events.
// Uses file size tracking to skip re-reads when the file hasn't grown, and
// UUID-based cursor to correctly identify new turns after DAG resolution.
func (s *Server) streamSessionLog(ctx context.Context, w http.ResponseWriter, name, provider, logPath string) {
	lw := newLogFileWatcher(logPath)
	defer lw.Close()

	var lastSize int64
	lw.onReset = func() { lastSize = 0 }
	var lastSentUUID string
	var seq uint64
	sentUUIDs := make(map[string]struct{})

	readAndEmit := func() {
		info, err := os.Stat(logPath)
		if err != nil {
			return
		}
		currentSize := info.Size()
		if currentSize == lastSize {
			return
		}

		// Use tail=1 (last compaction segment) to limit parsing scope.
		transcript, err := worker.SessionLogAdapter{}.ReadTranscript(worker.TranscriptRequest{
			Provider:        provider,
			TranscriptPath:  logPath,
			TailCompactions: 1,
		})
		if err != nil {
			return
		}
		sess := transcript.Session
		lastSize = currentSize

		turns := make([]outputTurn, 0, len(sess.Messages))
		uuids := make([]string, 0, len(sess.Messages))
		for _, e := range sess.Messages {
			turn := entryToTurn(e)
			if turn.Text == "" {
				continue
			}
			turns = append(turns, turn)
			uuids = append(uuids, e.UUID)
		}
		if len(turns) == 0 {
			return
		}

		var toSend []outputTurn

		if lastSentUUID == "" {
			// First emission: send everything.
			toSend = turns
		} else {
			found := false
			for i, uuid := range uuids {
				if uuid == lastSentUUID {
					toSend = turns[i+1:]
					found = true
					break
				}
			}
			if !found {
				// Cursor lost (DAG rewrite, compaction). Instead of
				// re-syncing from the beginning (which causes duplicate/
				// out-of-order messages on the client), emit only turns
				// we haven't previously sent.
				log.Printf("agent stream: cursor %s lost, emitting only new turns", lastSentUUID)
				for i, uuid := range uuids {
					if _, seen := sentUUIDs[uuid]; !seen {
						toSend = append(toSend, turns[i])
					}
				}
			}
		}

		// Track all current UUIDs so cursor-lost can filter correctly.
		lastSentUUID = uuids[len(uuids)-1]
		for _, uuid := range uuids {
			sentUUIDs[uuid] = struct{}{}
		}

		if len(toSend) == 0 {
			return
		}
		seq++

		data, err := json.Marshal(agentOutputResponse{
			Agent:  name,
			Format: "conversation",
			Turns:  toSend,
		})
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: turn\nid: %d\ndata: %s\n\n", seq, data) //nolint:errcheck
		if err := http.NewResponseController(w).Flush(); err != nil {
			_ = err
		}
	}

	lw.Run(ctx, readAndEmit, func() { writeSSEComment(w) })
}

// streamPeekOutput polls Peek() and emits changes as SSE events.
func (s *Server) streamPeekOutput(ctx context.Context, w http.ResponseWriter, name string, cfg *config.City) {
	sp := s.state.SessionProvider()
	sessionName := agentSessionName(s.state.CityName(), name, cfg.Workspace.SessionTemplate)

	poll := time.NewTicker(outputStreamPollInterval)
	defer poll.Stop()
	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()

	var lastOutput string
	var seq uint64

	emitPeek := func() {
		if !sp.IsRunning(sessionName) {
			return
		}
		output, err := sp.Peek(sessionName, 100)
		if err != nil || output == lastOutput {
			return
		}
		lastOutput = output
		seq++

		turns := []outputTurn{}
		if output != "" {
			turns = append(turns, outputTurn{Role: "output", Text: output})
		}
		data, err := json.Marshal(agentOutputResponse{
			Agent:  name,
			Format: "text",
			Turns:  turns,
		})
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: turn\nid: %d\ndata: %s\n\n", seq, data) //nolint:errcheck
		if err := http.NewResponseController(w).Flush(); err != nil {
			_ = err
		}
	}

	// Emit initial state immediately.
	emitPeek()

	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			emitPeek()
		case <-keepalive.C:
			writeSSEComment(w)
		}
	}
}

// unwrapDoubleEncoded handles Claude's double-encoded message format
// where the "message" field is a JSON string containing a JSON object.
// Returns the human-readable content text, or "" if not parseable.
func unwrapDoubleEncoded(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var inner string
	if err := json.Unmarshal(raw, &inner); err == nil {
		raw = []byte(inner)
	}
	var mc sessionlog.MessageContent
	if err := json.Unmarshal(raw, &mc); err != nil {
		return ""
	}
	// Try string content.
	var s string
	if err := json.Unmarshal(mc.Content, &s); err == nil && s != "" {
		return s
	}
	// Try array of content blocks.
	var blocks []sessionlog.ContentBlock
	if err := json.Unmarshal(mc.Content, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func historyRawEntryText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var entry struct {
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		return ""
	}
	return unwrapDoubleEncoded(entry.Message)
}
