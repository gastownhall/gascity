package api

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2/sse"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/sessionlog"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
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

// trySessionLogOutputHuma is the Huma-compatible variant of trySessionLogOutput.
// It accepts tail and before as string parameters (from query params) instead
// of reading them from *http.Request.
func (s *Server) trySessionLogOutputHuma(name string, agentCfg config.Agent, tailStr, before string) (*agentOutputResponse, error) {
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
	path := sessionlog.FindSessionFileForProvider(searchPaths, provider, workDir)
	if path == "" {
		return nil, nil
	}

	tail := 1
	if tailStr != "" {
		if n, err := strconv.Atoi(tailStr); err == nil && n >= 0 {
			tail = n
		}
	}

	var sess *sessionlog.Session
	var err error
	if before != "" {
		sess, err = sessionlog.ReadProviderFileOlder(provider, path, tail, before)
	} else {
		sess, err = sessionlog.ReadProviderFile(provider, path, tail)
	}
	if err != nil {
		return nil, err
	}

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

// streamSessionLog polls a session log file and emits new turns as SSE events.
// Uses file size tracking to skip re-reads when the file hasn't grown, and
// UUID-based cursor to correctly identify new turns after DAG resolution.
func (s *Server) streamSessionLog(ctx context.Context, send sse.Sender, name string, logPath string) {
	// Derive provider from agent config for session log parsing.
	cfg := s.state.Config()
	agentCfg, _ := findAgent(cfg, name)
	provider := strings.TrimSpace(agentCfg.Provider)
	if provider == "" && cfg != nil {
		provider = strings.TrimSpace(cfg.Workspace.Provider)
	}
	lw := newLogFileWatcher(logPath)
	defer lw.Close()

	var lastSize int64
	lw.onReset = func() { lastSize = 0 }
	var lastSentUUID string
	var seq int
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
		sess, err := sessionlog.ReadProviderFile(provider, logPath, 1)
		if err != nil {
			return
		}
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

		_ = send(sse.Message{ID: seq, Data: agentOutputResponse{
			Agent:  name,
			Format: "conversation",
			Turns:  toSend,
		}})
	}

	lw.Run(ctx, readAndEmit, func() {
		_ = send.Data(HeartbeatEvent{Timestamp: time.Now().UTC().Format(time.RFC3339)})
	})
}

// streamPeekOutput polls Peek() and emits changes as SSE events.
func (s *Server) streamPeekOutput(ctx context.Context, send sse.Sender, name string, cfg *config.City) {
	sp := s.state.SessionProvider()
	sessionName := agentSessionName(s.state.CityName(), name, cfg.Workspace.SessionTemplate)

	poll := time.NewTicker(outputStreamPollInterval)
	defer poll.Stop()
	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()

	var lastOutput string
	var seq int

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
		_ = send(sse.Message{ID: seq, Data: agentOutputResponse{
			Agent:  name,
			Format: "text",
			Turns:  turns,
		}})
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
			_ = send.Data(HeartbeatEvent{Timestamp: time.Now().UTC().Format(time.RFC3339)})
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
	// Try to unwrap: raw might be a JSON string like "{\"role\":...}"
	var inner string
	if err := json.Unmarshal(raw, &inner); err != nil {
		return ""
	}
	// Now inner is the JSON object as a string. Parse it.
	var mc sessionlog.MessageContent
	if err := json.Unmarshal([]byte(inner), &mc); err != nil {
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
