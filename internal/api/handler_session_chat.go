package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2/sse"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
)

var errSessionTemplateNotFound = errors.New("session template not found")

type sessionPendingResponse struct {
	Supported bool                        `json:"supported"`
	Pending   *runtime.PendingInteraction `json:"pending,omitempty"`
}

type sessionTranscriptResponse struct {
	ID         string                     `json:"id"`
	Template   string                     `json:"template"`
	Format     string                     `json:"format"`
	Turns      []outputTurn               `json:"turns"`
	Pagination *sessionlog.PaginationInfo `json:"pagination,omitempty"`
}

type sessionRawTranscriptResponse struct {
	ID         string                     `json:"id"`
	Template   string                     `json:"template"`
	Format     string                     `json:"format"`
	Messages   []any                      `json:"messages" doc:"Provider-native transcript frames (arbitrary JSON)."`
	Pagination *sessionlog.PaginationInfo `json:"pagination,omitempty"`
}

func (s *Server) sessionLogPaths() []string {
	if s.sessionLogSearchPaths != nil {
		return s.sessionLogSearchPaths
	}
	cfg := s.state.Config()
	if cfg == nil {
		return sessionlog.DefaultSearchPaths()
	}
	return sessionlog.MergeSearchPaths(cfg.Daemon.ObservePaths)
}

func sessionCreateHints(resolved *config.ResolvedProvider) runtime.Config {
	return runtime.Config{
		ReadyPromptPrefix:      resolved.ReadyPromptPrefix,
		ReadyDelayMs:           resolved.ReadyDelayMs,
		ProcessNames:           resolved.ProcessNames,
		EmitsPermissionWarning: resolved.EmitsPermissionWarning,
	}
}

func sessionResumeHints(resolved *config.ResolvedProvider, workDir string) runtime.Config {
	return runtime.Config{
		WorkDir:                workDir,
		ReadyPromptPrefix:      resolved.ReadyPromptPrefix,
		ReadyDelayMs:           resolved.ReadyDelayMs,
		ProcessNames:           resolved.ProcessNames,
		EmitsPermissionWarning: resolved.EmitsPermissionWarning,
		Env:                    resolved.Env,
	}
}

func sessionExplicitNameForCreate(agentCfg config.Agent, alias string) (string, error) {
	if !agentCfg.SupportsMultipleSessions() || strings.TrimSpace(alias) != "" {
		return "", nil
	}
	return session.GenerateAdhocExplicitName(agentCfg.Name)
}

func (s *Server) resolveSessionWorkDir(agentCfg config.Agent, qualifiedName string) (string, error) {
	cfg := s.state.Config()
	if cfg == nil {
		return "", errors.New("no city config loaded")
	}
	workDir, err := workdirutil.ResolveWorkDirPathStrict(
		s.state.CityPath(),
		workdirutil.CityName(s.state.CityPath(), cfg),
		qualifiedName,
		agentCfg,
		cfg.Rigs,
	)
	if err != nil {
		return "", err
	}
	if workDir == "" {
		workDir = s.state.CityPath()
	}
	return workDir, nil
}

func (s *Server) resolveSessionTemplate(template string) (*config.ResolvedProvider, string, string, string, error) {
	cfg := s.state.Config()
	if cfg == nil {
		return nil, "", "", "", errors.New("no city config loaded")
	}
	agentCfg, ok := resolveSessionTemplateAgent(cfg, template)
	if !ok {
		return nil, "", "", "", errSessionTemplateNotFound
	}
	resolved, err := config.ResolveProvider(&agentCfg, &cfg.Workspace, cfg.Providers, exec.LookPath)
	if err != nil {
		return nil, "", "", "", err
	}
	workDir, err := s.resolveSessionWorkDir(agentCfg, agentCfg.QualifiedName())
	if err != nil {
		return nil, "", "", "", err
	}
	return resolved, workDir, agentCfg.Session, agentCfg.QualifiedName(), nil
}

func (s *Server) buildSessionResume(info session.Info) (string, runtime.Config) {
	cmd := session.BuildResumeCommand(info)

	buildResolved := func(resolved *config.ResolvedProvider, workDir string) (string, runtime.Config) {
		if resolved == nil {
			return cmd, runtime.Config{WorkDir: workDir}
		}
		resolvedInfo := info
		resolvedInfo.Command = resolved.CommandString()
		resolvedInfo.Provider = resolved.Name
		resolvedInfo.ResumeFlag = resolved.ResumeFlag
		resolvedInfo.ResumeStyle = resolved.ResumeStyle
		resolvedInfo.ResumeCommand = resolved.ResumeCommand
		return session.BuildResumeCommand(resolvedInfo), sessionResumeHints(resolved, workDir)
	}

	// Check persisted kind to avoid agent/provider name collisions.
	// If kind is "provider", skip the agent template lookup entirely.
	kind := s.sessionKind(info.ID)

	if kind != "provider" {
		resolved, workDir, _, _, err := s.resolveSessionTemplate(info.Template)
		if err == nil {
			if info.WorkDir != "" {
				workDir = info.WorkDir
			}
			return buildResolved(resolved, workDir)
		}
	}

	// Provider path (explicit kind=provider, or agent template not found).
	resolved, err := s.resolveBareProvider(info.Template)
	if err != nil {
		return cmd, runtime.Config{WorkDir: info.WorkDir}
	}
	workDir := info.WorkDir
	if workDir == "" {
		workDir = s.state.CityPath()
	}
	return buildResolved(resolved, workDir)
}

// sessionKind reads the persisted mc_session_kind from bead metadata.
func (s *Server) sessionKind(sessionID string) string {
	store := s.state.CityBeadStore()
	if store == nil {
		return ""
	}
	b, err := store.Get(sessionID)
	if err != nil {
		return ""
	}
	return b.Metadata["mc_session_kind"]
}

// resolveBareProvider resolves a provider by name without an agent template.
func (s *Server) resolveBareProvider(providerName string) (*config.ResolvedProvider, error) {
	cfg := s.state.Config()
	if cfg == nil {
		return nil, errors.New("no city config loaded")
	}
	return config.ResolveProvider(
		&config.Agent{Provider: providerName},
		&cfg.Workspace,
		cfg.Providers,
		exec.LookPath,
	)
}

func (s *Server) persistSessionMeta(store beads.Store, sessionID, kind, projectID string, optMeta map[string]string) {
	batch := make(map[string]string)
	for k, v := range optMeta {
		batch[k] = v
	}
	if kind != "" {
		batch["mc_session_kind"] = kind
	}
	if projectID != "" {
		batch["mc_project_id"] = projectID
	}
	if len(batch) > 0 {
		if err := store.SetMetadataBatch(sessionID, batch); err != nil {
			log.Printf("persistSessionMeta: session %s: %v", sessionID, err)
		}
	}
}

func (s *Server) emitClosedSessionSnapshot(send sse.Sender, info session.Info, logPath string) {
	if logPath == "" {
		return
	}
	sess, err := sessionlog.ReadProviderFile(info.Provider, logPath, 0)
	if err != nil {
		return
	}

	turns := make([]outputTurn, 0, len(sess.Messages))
	for _, entry := range sess.Messages {
		turn := entryToTurn(entry)
		if turn.Text == "" {
			continue
		}
		turns = append(turns, turn)
	}
	if len(turns) == 0 {
		return
	}

	if err := send(sse.Message{ID: 1, Data: sessionTranscriptResponse{
		ID:       info.ID,
		Template: info.Template,
		Format:   "conversation",
		Turns:    turns,
	}}); err != nil {
		return
	}
	// Closed session is definitionally idle.
	_ = send(sse.Message{ID: 2, Data: SessionActivityEvent{Activity: "idle"}})
}

func (s *Server) emitClosedSessionSnapshotRaw(send sse.Sender, info session.Info, logPath string) {
	if logPath == "" {
		return
	}
	sess, err := sessionlog.ReadProviderFileRaw(info.Provider, logPath, 0)
	if err != nil {
		return
	}

	rawMessages := sess.RawPayloads()
	if len(rawMessages) == 0 {
		return
	}

	if err := send(sse.Message{ID: 1, Data: sessionRawTranscriptResponse{
		ID:       info.ID,
		Template: info.Template,
		Format:   "raw",
		Messages: rawMessages,
	}}); err != nil {
		return
	}
	_ = send(sse.Message{ID: 2, Data: SessionActivityEvent{Activity: "idle"}})
}

func (s *Server) streamSessionTranscriptLogRaw(ctx context.Context, send sse.Sender, info session.Info, logPath string) {
	lw := newLogFileWatcher(logPath)
	defer lw.Close()

	var lastSize int64
	var lastSentUUID string
	var seq int
	var lastActivity string
	sentUUIDs := make(map[string]struct{})
	lw.onReset = func() { lastSize = 0; lastActivity = "" }

	readAndEmit := func() {
		stat, err := os.Stat(logPath)
		if err != nil {
			return
		}
		if stat.Size() == lastSize {
			return
		}

		// Use tail=1 (last compaction segment) to limit parsing scope,
		// consistent with the non-raw streaming path.
		sess, err := sessionlog.ReadProviderFileRaw(info.Provider, logPath, 1)
		if err != nil {
			return
		}
		lastSize = stat.Size()

		// Compute activity early (used after message emission).
		activity := sessionlog.InferActivityFromEntries(sess.Messages)

		rawMessages := make([]any, 0, len(sess.Messages))
		uuids := make([]string, 0, len(sess.Messages))
		for _, entry := range sess.Messages {
			if len(entry.Raw) == 0 {
				continue
			}
			var v any
			if err := json.Unmarshal(entry.Raw, &v); err != nil {
				continue
			}
			rawMessages = append(rawMessages, v)
			uuids = append(uuids, entry.UUID)
		}

		// Emit messages if there are new ones.
		if len(rawMessages) > 0 {
			var toSend []any

			if lastSentUUID == "" {
				// First emission: send everything.
				toSend = rawMessages
			} else {
				found := false
				for i, uuid := range uuids {
					if uuid == lastSentUUID {
						toSend = rawMessages[i+1:]
						found = true
						break
					}
				}
				if !found {
					// Cursor lost (DAG rewrite, compaction). Instead of
					// re-syncing from the beginning (which causes duplicate/
					// out-of-order messages on the client), emit only messages
					// we haven't previously sent.
					log.Printf("session stream raw: cursor %s lost, emitting only new messages", lastSentUUID)
					for i, uuid := range uuids {
						if _, seen := sentUUIDs[uuid]; !seen {
							toSend = append(toSend, rawMessages[i])
						}
					}
				}
			}

			if len(toSend) > 0 {
				seq++
				_ = send(sse.Message{ID: seq, Data: sessionRawTranscriptResponse{
					ID:       info.ID,
					Template: info.Template,
					Format:   "raw",
					Messages: toSend,
				}})
			}

			// Track all current UUIDs so cursor-lost can filter correctly.
			lastSentUUID = uuids[len(uuids)-1]
			for _, uuid := range uuids {
				sentUUIDs[uuid] = struct{}{}
			}
		}

		// Emit activity after content so clients receive data before state change.
		if activity != "" && activity != lastActivity {
			lastActivity = activity
			seq++
			_ = send(sse.Message{ID: seq, Data: SessionActivityEvent{Activity: activity}})
		}
	}

	// Stall detection: when the log hasn't grown for 5s, check the tmux
	// pane for a tool approval prompt. If found, emit a "pending" SSE event
	// so the UI can show the approval panel.
	var lastPendingID string
	onStall := func() {
		sp := s.state.SessionProvider()
		ip, ok := sp.(runtime.InteractionProvider)
		if !ok {
			return
		}
		pending, err := ip.Pending(info.SessionName)
		if err != nil || pending == nil {
			if lastPendingID != "" {
				// Approval cleared — emit activity update.
				lastPendingID = ""
				seq++
				_ = send(sse.Message{ID: seq, Data: SessionActivityEvent{Activity: "in-turn"}})
			}
			return
		}
		if pending.RequestID == lastPendingID {
			return // already emitted this approval
		}
		lastPendingID = pending.RequestID
		seq++
		_ = send(sse.Message{ID: seq, Data: *pending})
	}

	keepaliveTicker := time.NewTicker(sseKeepalive)
	defer keepaliveTicker.Stop()
	lw.Run(ctx, readAndEmit, func() {
		_ = send.Data(HeartbeatEvent{Timestamp: time.Now().UTC().Format(time.RFC3339)})
	}, RunOpts{
		OnStall:      onStall,
		StallTimeout: 5 * time.Second,
	})
}

func (s *Server) streamSessionTranscriptLog(ctx context.Context, send sse.Sender, info session.Info, logPath string) {
	lw := newLogFileWatcher(logPath)
	defer lw.Close()

	var lastSize int64
	var lastSentUUID string
	var seq int
	var lastActivity string
	sentUUIDs := make(map[string]struct{})
	lw.onReset = func() { lastSize = 0; lastActivity = "" }

	readAndEmit := func() {
		stat, err := os.Stat(logPath)
		if err != nil {
			return
		}
		if stat.Size() == lastSize {
			return
		}

		sess, err := sessionlog.ReadProviderFile(info.Provider, logPath, 0)
		if err != nil {
			return
		}
		lastSize = stat.Size()

		// Compute activity early (used after turn emission).
		activity := sessionlog.InferActivityFromEntries(sess.Messages)

		turns := make([]outputTurn, 0, len(sess.Messages))
		uuids := make([]string, 0, len(sess.Messages))
		for _, entry := range sess.Messages {
			turn := entryToTurn(entry)
			if turn.Text == "" {
				continue
			}
			turns = append(turns, turn)
			uuids = append(uuids, entry.UUID)
		}

		// Emit turns if there are new ones.
		if len(turns) > 0 {
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
					log.Printf("session stream: cursor %s lost, emitting only new turns", lastSentUUID)
					for i, uuid := range uuids {
						if _, seen := sentUUIDs[uuid]; !seen {
							toSend = append(toSend, turns[i])
						}
					}
				}
			}

			if len(toSend) > 0 {
				seq++
				_ = send(sse.Message{ID: seq, Data: sessionTranscriptResponse{
					ID:       info.ID,
					Template: info.Template,
					Format:   "conversation",
					Turns:    toSend,
				}})
			}

			// Track all current UUIDs so cursor-lost can filter correctly.
			lastSentUUID = uuids[len(uuids)-1]
			for _, uuid := range uuids {
				sentUUIDs[uuid] = struct{}{}
			}
		}

		// Emit activity after content so clients receive data before state change.
		if activity != "" && activity != lastActivity {
			lastActivity = activity
			seq++
			_ = send(sse.Message{ID: seq, Data: SessionActivityEvent{Activity: activity}})
		}
	}

	lw.Run(ctx, readAndEmit, func() {
		_ = send.Data(HeartbeatEvent{Timestamp: time.Now().UTC().Format(time.RFC3339)})
	})
}

// streamSessionPeekRaw polls tmux pane content and wraps it as format=raw
// messages so MC's JSONL rendering pipeline can display terminal output
// (e.g. OAuth prompts, startup screens) when no transcript log exists yet.
func (s *Server) streamSessionPeekRaw(ctx context.Context, send sse.Sender, info session.Info) {
	sp := s.state.SessionProvider()
	poll := time.NewTicker(outputStreamPollInterval)
	defer poll.Stop()
	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()

	var lastOutput string
	var seq int

	var lastPeekPendingID string

	emitPeek := func() {
		if !sp.IsRunning(info.SessionName) {
			return
		}
		output, err := sp.Peek(info.SessionName, 100)
		if err != nil || output == lastOutput {
			return
		}
		lastOutput = output
		seq++

		if output == "" {
			return
		}

		// Wrap as a fake assistant message in raw JSONL format so MC's
		// translate_transcript_response handles it like a normal transcript.
		fakeMsg := map[string]any{
			"role": "assistant",
			"content": []map[string]string{
				{"type": "text", "text": output},
			},
		}
		_ = send(sse.Message{ID: seq, Data: sessionRawTranscriptResponse{
			ID:       info.ID,
			Template: info.Template,
			Format:   "raw",
			Messages: []any{fakeMsg},
		}})

		// Check for approval prompts in the pane output we already have.
		if ip, ok := sp.(runtime.InteractionProvider); ok {
			pending, pErr := ip.Pending(info.SessionName)
			if pErr == nil && pending != nil && pending.RequestID != lastPeekPendingID {
				lastPeekPendingID = pending.RequestID
				seq++
				_ = send(sse.Message{ID: seq, Data: *pending})
			} else if pending == nil && lastPeekPendingID != "" {
				lastPeekPendingID = ""
			}
		}
	}

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

func (s *Server) streamSessionPeek(ctx context.Context, send sse.Sender, info session.Info) {
	sp := s.state.SessionProvider()
	poll := time.NewTicker(outputStreamPollInterval)
	defer poll.Stop()
	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()

	var lastOutput string
	var seq int

	emitPeek := func() {
		if !sp.IsRunning(info.SessionName) {
			return
		}
		output, err := sp.Peek(info.SessionName, 100)
		if err != nil || output == lastOutput {
			return
		}
		lastOutput = output
		seq++

		turns := []outputTurn{}
		if output != "" {
			turns = append(turns, outputTurn{Role: "output", Text: output})
		}
		_ = send(sse.Message{ID: seq, Data: sessionTranscriptResponse{
			ID:       info.ID,
			Template: info.Template,
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
		case <-keepalive.C:
			_ = send.Data(HeartbeatEvent{Timestamp: time.Now().UTC().Format(time.RFC3339)})
		}
	}
}
