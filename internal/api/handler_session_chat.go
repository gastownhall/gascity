package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

var errSessionTemplateNotFound = errors.New("session template not found")

type sessionCreateRequest struct {
	Template string `json:"template"`
	Title    string `json:"title,omitempty"`
}

type sessionMessageRequest struct {
	Message string `json:"message"`
}

type sessionPendingResponse struct {
	Supported bool                        `json:"supported"`
	Pending   *runtime.PendingInteraction `json:"pending,omitempty"`
}

type sessionRespondRequest struct {
	RequestID string            `json:"request_id,omitempty"`
	Action    string            `json:"action"`
	Text      string            `json:"text,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

type sessionTranscriptResponse struct {
	ID         string                     `json:"id"`
	Template   string                     `json:"template"`
	Format     string                     `json:"format"`
	Turns      []outputTurn               `json:"turns"`
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

func (s *Server) resolveSessionTemplate(template string) (*config.ResolvedProvider, string, string, error) {
	cfg := s.state.Config()
	if cfg == nil {
		return nil, "", "", errors.New("no city config loaded")
	}
	agentCfg, ok := findAgent(cfg, template)
	if !ok {
		return nil, "", "", errSessionTemplateNotFound
	}
	resolved, err := config.ResolveProvider(&agentCfg, &cfg.Workspace, cfg.Providers, exec.LookPath)
	if err != nil {
		return nil, "", "", err
	}
	workDir := s.resolveAgentWorkDir(agentCfg)
	if workDir == "" {
		workDir = s.state.CityPath()
	}
	return resolved, workDir, agentCfg.Session, nil
}

func (s *Server) buildSessionResume(info session.Info) (string, runtime.Config) {
	cmd := session.BuildResumeCommand(info)
	resolved, workDir, _, err := s.resolveSessionTemplate(info.Template)
	if err != nil {
		return cmd, runtime.Config{WorkDir: info.WorkDir}
	}
	if info.WorkDir != "" {
		workDir = info.WorkDir
	}
	return cmd, sessionResumeHints(resolved, workDir)
}

func writeSessionManagerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, session.ErrInteractionUnsupported):
		writeError(w, http.StatusNotImplemented, "unsupported", err.Error())
	case errors.Is(err, session.ErrNoPendingInteraction):
		writeError(w, http.StatusConflict, "no_pending", err.Error())
	case errors.Is(err, session.ErrInteractionMismatch):
		writeError(w, http.StatusConflict, "invalid_interaction", err.Error())
	case errors.Is(err, session.ErrSessionClosed), errors.Is(err, session.ErrResumeRequired):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, session.ErrNotSession):
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
	default:
		writeStoreError(w, err)
	}
}

func (s *Server) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	store := s.state.CityBeadStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	var body sessionCreateRequest
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	if body.Template == "" {
		writeError(w, http.StatusBadRequest, "invalid", "template is required")
		return
	}

	idemKey := scopedIdemKey(r, r.Header.Get("Idempotency-Key"))
	var bodyHash string
	if idemKey != "" {
		bodyHash = hashBody(body)
		if s.idem.handleIdempotent(w, idemKey, bodyHash) {
			return
		}
	}

	resolved, workDir, transport, err := s.resolveSessionTemplate(body.Template)
	if err != nil {
		s.idem.unreserve(idemKey)
		if errors.Is(err, errSessionTemplateNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "template "+body.Template+" not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	title := body.Title
	if title == "" {
		title = body.Template
	}

	resume := session.ProviderResume{
		ResumeFlag:    resolved.ResumeFlag,
		ResumeStyle:   resolved.ResumeStyle,
		SessionIDFlag: resolved.SessionIDFlag,
	}

	mgr := session.NewManager(store, s.state.SessionProvider())
	info, err := mgr.CreateWithTransport(
		r.Context(),
		body.Template,
		title,
		resolved.CommandString(),
		workDir,
		resolved.Name,
		transport,
		resolved.Env,
		resume,
		sessionCreateHints(resolved),
	)
	if err != nil {
		s.idem.unreserve(idemKey)
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	resp := sessionToResponse(info)
	s.idem.storeResponse(idemKey, bodyHash, http.StatusCreated, resp)
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleSessionTranscript(w http.ResponseWriter, r *http.Request) {
	store := s.state.CityBeadStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	id, err := session.ResolveSessionID(store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}

	mgr := session.NewManager(store, s.state.SessionProvider())
	info, err := mgr.Get(id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}

	path, err := mgr.TranscriptPath(id, s.sessionLogPaths())
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}

	if path != "" {
		tail := 1
		if v := r.URL.Query().Get("tail"); v != "" {
			if n, convErr := strconv.Atoi(v); convErr == nil && n >= 0 {
				tail = n
			}
		}
		before := r.URL.Query().Get("before")

		var sess *sessionlog.Session
		if before != "" {
			sess, err = sessionlog.ReadFileOlder(path, tail, before)
		} else {
			sess, err = sessionlog.ReadFile(path, tail)
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "reading session log: "+err.Error())
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
		writeJSON(w, http.StatusOK, sessionTranscriptResponse{
			ID:         info.ID,
			Template:   info.Template,
			Format:     "conversation",
			Turns:      turns,
			Pagination: sess.Pagination,
		})
		return
	}

	if info.State == session.StateActive && s.state.SessionProvider().IsRunning(info.SessionName) {
		output, peekErr := s.state.SessionProvider().Peek(info.SessionName, 100)
		if peekErr != nil {
			writeError(w, http.StatusInternalServerError, "internal", peekErr.Error())
			return
		}
		turns := []outputTurn{}
		if output != "" {
			turns = append(turns, outputTurn{Role: "output", Text: output})
		}
		writeJSON(w, http.StatusOK, sessionTranscriptResponse{
			ID:       info.ID,
			Template: info.Template,
			Format:   "text",
			Turns:    turns,
		})
		return
	}

	writeJSON(w, http.StatusOK, sessionTranscriptResponse{
		ID:       info.ID,
		Template: info.Template,
		Format:   "conversation",
		Turns:    []outputTurn{},
	})
}

func (s *Server) handleSessionMessage(w http.ResponseWriter, r *http.Request) {
	store := s.state.CityBeadStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	id, err := session.ResolveSessionID(store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}

	var body sessionMessageRequest
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	if strings.TrimSpace(body.Message) == "" {
		writeError(w, http.StatusBadRequest, "invalid", "message is required")
		return
	}

	idemKey := scopedIdemKey(r, r.Header.Get("Idempotency-Key"))
	var bodyHash string
	if idemKey != "" {
		bodyHash = hashBody(body)
		if s.idem.handleIdempotent(w, idemKey, bodyHash) {
			return
		}
	}

	mgr := session.NewManager(store, s.state.SessionProvider())
	info, err := mgr.Get(id)
	if err != nil {
		s.idem.unreserve(idemKey)
		writeSessionManagerError(w, err)
		return
	}

	// This is a best-effort guard to keep new turns from crossing a provider's
	// blocking approval/question state. Providers can still surface a pending
	// interaction after this check and before Send reaches the runtime.
	pending, supported, err := mgr.Pending(id)
	if err != nil {
		s.idem.unreserve(idemKey)
		writeSessionManagerError(w, err)
		return
	}
	if supported && pending != nil {
		s.idem.unreserve(idemKey)
		writeError(w, http.StatusConflict, "pending_interaction", "session has a pending interaction; use /respond")
		return
	}

	resumeCommand, hints := s.buildSessionResume(info)
	if err := mgr.Send(r.Context(), id, body.Message, resumeCommand, hints); err != nil {
		s.idem.unreserve(idemKey)
		writeSessionManagerError(w, err)
		return
	}

	resp := map[string]string{"status": "accepted", "id": id}
	s.idem.storeResponse(idemKey, bodyHash, http.StatusAccepted, resp)
	writeJSON(w, http.StatusAccepted, resp)
}

func (s *Server) handleSessionStop(w http.ResponseWriter, r *http.Request) {
	store := s.state.CityBeadStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	id, err := session.ResolveSessionID(store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}

	mgr := session.NewManager(store, s.state.SessionProvider())
	if err := mgr.StopTurn(id); err != nil {
		writeSessionManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "id": id})
}

func (s *Server) handleSessionPending(w http.ResponseWriter, r *http.Request) {
	store := s.state.CityBeadStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	id, err := session.ResolveSessionID(store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}

	mgr := session.NewManager(store, s.state.SessionProvider())
	pending, supported, err := mgr.Pending(id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sessionPendingResponse{
		Supported: supported,
		Pending:   pending,
	})
}

func (s *Server) handleSessionRespond(w http.ResponseWriter, r *http.Request) {
	store := s.state.CityBeadStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	id, err := session.ResolveSessionID(store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}

	var body sessionRespondRequest
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	if body.Action == "" {
		writeError(w, http.StatusBadRequest, "invalid", "action is required")
		return
	}

	idemKey := scopedIdemKey(r, r.Header.Get("Idempotency-Key"))
	var bodyHash string
	if idemKey != "" {
		bodyHash = hashBody(body)
		if s.idem.handleIdempotent(w, idemKey, bodyHash) {
			return
		}
	}

	mgr := session.NewManager(store, s.state.SessionProvider())
	if err := mgr.Respond(id, runtime.InteractionResponse{
		RequestID: body.RequestID,
		Action:    body.Action,
		Text:      body.Text,
		Metadata:  body.Metadata,
	}); err != nil {
		s.idem.unreserve(idemKey)
		writeSessionManagerError(w, err)
		return
	}

	resp := map[string]string{"status": "accepted", "id": id}
	s.idem.storeResponse(idemKey, bodyHash, http.StatusAccepted, resp)
	writeJSON(w, http.StatusAccepted, resp)
}

func (s *Server) handleSessionStream(w http.ResponseWriter, r *http.Request) {
	store := s.state.CityBeadStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	id, err := session.ResolveSessionID(store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}

	mgr := session.NewManager(store, s.state.SessionProvider())
	info, err := mgr.Get(id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	path, err := mgr.TranscriptPath(id, s.sessionLogPaths())
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}

	sp := s.state.SessionProvider()
	running := info.State == session.StateActive && sp.IsRunning(info.SessionName)
	if path == "" && !running {
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
	if info.State == "" {
		s.emitClosedSessionSnapshot(w, info, path)
		return
	}
	switch {
	case path != "":
		s.streamSessionTranscriptLog(ctx, w, info, path)
	default:
		s.streamSessionPeek(ctx, w, info)
	}
}

func (s *Server) emitClosedSessionSnapshot(w http.ResponseWriter, info session.Info, logPath string) {
	if logPath == "" {
		return
	}
	sess, err := sessionlog.ReadFile(logPath, 1)
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

	data, err := json.Marshal(sessionTranscriptResponse{
		ID:       info.ID,
		Template: info.Template,
		Format:   "conversation",
		Turns:    turns,
	})
	if err != nil {
		return
	}
	writeSSE(w, "turn", 1, data)
}

func (s *Server) streamSessionTranscriptLog(ctx context.Context, w http.ResponseWriter, info session.Info, logPath string) {
	poll := time.NewTicker(outputStreamPollInterval)
	defer poll.Stop()
	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()

	var lastSize int64
	var lastSentUUID string
	var seq uint64

	readAndEmit := func() {
		stat, err := os.Stat(logPath)
		if err != nil {
			return
		}
		if stat.Size() == lastSize {
			return
		}

		sess, err := sessionlog.ReadFile(logPath, 0)
		if err != nil {
			return
		}
		lastSize = stat.Size()

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
		if len(turns) == 0 {
			return
		}

		startIdx := 0
		if lastSentUUID != "" {
			for i, uuid := range uuids {
				if uuid == lastSentUUID {
					startIdx = i + 1
					break
				}
			}
		}
		if startIdx >= len(turns) {
			return
		}
		lastSentUUID = uuids[len(uuids)-1]
		seq++

		data, err := json.Marshal(sessionTranscriptResponse{
			ID:       info.ID,
			Template: info.Template,
			Format:   "conversation",
			Turns:    turns[startIdx:],
		})
		if err != nil {
			return
		}
		writeSSE(w, "turn", seq, data)
	}

	readAndEmit()

	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			readAndEmit()
		case <-keepalive.C:
			writeSSEComment(w)
		}
	}
}

func (s *Server) streamSessionPeek(ctx context.Context, w http.ResponseWriter, info session.Info) {
	sp := s.state.SessionProvider()
	poll := time.NewTicker(outputStreamPollInterval)
	defer poll.Stop()
	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()

	var lastOutput string
	var seq uint64

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
		data, err := json.Marshal(sessionTranscriptResponse{
			ID:       info.ID,
			Template: info.Template,
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
		case <-keepalive.C:
			writeSSEComment(w)
		}
	}
}
