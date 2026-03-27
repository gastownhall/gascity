package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/shellquote"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
)

var errSessionTemplateNotFound = errors.New("session template not found")

type sessionCreateRequest struct {
	// Kind discriminates the session target: "agent" or "provider".
	Kind              string            `json:"kind,omitempty"`
	Name              string            `json:"name,omitempty"`
	Alias             string            `json:"alias,omitempty"`
	LegacySessionName *string           `json:"session_name,omitempty"`
	Message           string            `json:"message,omitempty"`
	Async             bool              `json:"async,omitempty"`
	Options           map[string]string `json:"options,omitempty"`
	// ProjectID is an opaque identifier for the MC project context.
	// Stored in bead metadata for session-to-project association.
	ProjectID string `json:"project_id,omitempty"`
	Title     string `json:"title,omitempty"`
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

type sessionRawTranscriptResponse struct {
	ID         string                     `json:"id"`
	Template   string                     `json:"template"`
	Format     string                     `json:"format"`
	Messages   []json.RawMessage          `json:"messages"`
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
	workDir, err := workdirutil.ResolveWorkDirPathStrict(
		s.state.CityPath(),
		workdirutil.CityName(s.state.CityPath(), cfg),
		agentCfg.QualifiedName(),
		agentCfg,
		cfg.Rigs,
	)
	if err != nil {
		return nil, "", "", "", err
	}
	if workDir == "" {
		workDir = s.state.CityPath()
	}
	return resolved, workDir, agentCfg.Session, agentCfg.QualifiedName(), nil
}

func (s *Server) buildSessionResume(info session.Info) (string, runtime.Config) {
	cmd := session.BuildResumeCommand(info)

	// Check persisted kind to avoid agent/provider name collisions.
	// If kind is "provider", skip the agent template lookup entirely.
	kind := s.sessionKind(info.ID)

	if kind != "provider" {
		resolved, workDir, _, _, err := s.resolveSessionTemplate(info.Template)
		if err == nil {
			if info.WorkDir != "" {
				workDir = info.WorkDir
			}
			return cmd, sessionResumeHints(resolved, workDir)
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
	return cmd, sessionResumeHints(resolved, workDir)
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

func writeSessionManagerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, session.ErrInvalidSessionName):
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
	case errors.Is(err, session.ErrSessionNameExists):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, session.ErrInvalidSessionAlias):
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
	case errors.Is(err, session.ErrSessionAliasExists):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, session.ErrInteractionUnsupported):
		writeError(w, http.StatusNotImplemented, "unsupported", err.Error())
	case errors.Is(err, session.ErrPendingInteraction):
		writeError(w, http.StatusConflict, "pending_interaction", err.Error())
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
	if body.LegacySessionName != nil {
		writeError(w, http.StatusBadRequest, "invalid", "session_name is no longer accepted; use alias")
		return
	}

	kind := body.Kind
	name := body.Name
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid", "name is required")
		return
	}
	if kind != "agent" && kind != "provider" {
		writeError(w, http.StatusBadRequest, "invalid_kind", "kind must be 'agent' or 'provider'")
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

	var resolved *config.ResolvedProvider
	var workDir, transport, template string
	var extraArgs []string
	var optMeta map[string]string

	switch kind {
	case "agent":
		if len(body.Options) > 0 {
			s.idem.unreserve(idemKey)
			writeError(w, http.StatusBadRequest, "invalid", "options are not supported for agent sessions; use kind=provider to specify options")
			return
		}
		var err error
		resolved, workDir, transport, template, err = s.resolveSessionTemplate(name)
		if err != nil {
			if errors.Is(err, errSessionTemplateNotFound) {
				s.idem.unreserve(idemKey)
				writeError(w, http.StatusNotFound, "agent_not_found", "agent '"+name+"' not found")
				return
			}
			s.idem.unreserve(idemKey)
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		// Agent track: command comes from the agent config as-is.
		// Do NOT inject OptionsSchema defaults — agents encode their own CLI flags.

	case "provider":
		s.createProviderSession(w, r, store, body, name, idemKey, bodyHash)
		return
	}

	title := body.Title
	if title == "" {
		title = template
	}
	if body.Async && strings.TrimSpace(body.Message) != "" {
		s.idem.unreserve(idemKey)
		writeError(w, http.StatusBadRequest, "invalid", "message is not supported with async session creation; create the session, then POST /v0/session/{id}/messages")
		return
	}

	resume := session.ProviderResume{
		ResumeFlag:    resolved.ResumeFlag,
		ResumeStyle:   resolved.ResumeStyle,
		ResumeCommand: resolved.ResumeCommand,
		SessionIDFlag: resolved.SessionIDFlag,
	}
	alias, err := session.ValidateAlias(body.Alias)
	if err != nil {
		s.idem.unreserve(idemKey)
		writeSessionManagerError(w, err)
		return
	}

	// Merge extra args from options into the command string.
	command := resolved.CommandString()
	if len(extraArgs) > 0 {
		command = command + " " + shellquote.Join(extraArgs)
	}

	mgr := s.sessionManager(store)
	hints := sessionCreateHints(resolved)
	var info session.Info
	err = session.WithCitySessionAliasLock(s.state.CityPath(), alias, func() error {
		if err := session.EnsureAliasAvailableWithConfig(store, s.state.Config(), alias, ""); err != nil {
			return err
		}
		var createErr error
		if body.Async {
			info, createErr = mgr.CreateAliasedBeadOnlyNamed(
				alias,
				"",
				template,
				title,
				command,
				workDir,
				resolved.Name,
				transport,
				resolved.Env,
				resume,
			)
		} else {
			info, createErr = mgr.CreateAliasedNamedWithTransport(
				r.Context(),
				alias,
				"",
				template,
				title,
				command,
				workDir,
				resolved.Name,
				transport,
				resolved.Env,
				resume,
				hints,
			)
		}
		return createErr
	})
	if err != nil {
		s.idem.unreserve(idemKey)
		writeSessionManagerError(w, err)
		return
	}

	// Persist kind, option metadata, and project_id on the bead.
	s.persistSessionMeta(store, info.ID, "agent", body.ProjectID, optMeta)
	if body.Async {
		s.state.Poke()
	}

	// Deliver initial message if provided.
	if msg := strings.TrimSpace(body.Message); msg != "" {
		resumeCommand, nudgeHints := s.buildSessionResume(info)
		if sendErr := mgr.Send(r.Context(), info.ID, msg, resumeCommand, nudgeHints); sendErr != nil {
			log.Printf("session %s: initial message delivery failed: %v", info.ID, sendErr)
			s.idem.unreserve(idemKey)
			writeError(w, http.StatusInternalServerError, "message_delivery_failed",
				fmt.Sprintf("session created but initial message failed: %v", sendErr))
			return
		}
	}

	resp := sessionToResponse(info, s.state.Config())
	resp.Kind = "agent"
	s.enrichSessionResponse(&resp, info, s.state.Config(), s.state.SessionProvider(), false)
	statusCode := http.StatusCreated
	if body.Async {
		statusCode = http.StatusAccepted
	}
	s.idem.storeResponse(idemKey, bodyHash, statusCode, resp)
	writeJSON(w, statusCode, resp)
}

// createProviderSession handles the "provider" kind session creation.
// Resolves a bare provider (not an agent template) and creates a session.
func (s *Server) createProviderSession(w http.ResponseWriter, r *http.Request, store beads.Store, body sessionCreateRequest, providerName, idemKey, bodyHash string) {
	cfg := s.state.Config()
	if cfg == nil {
		s.idem.unreserve(idemKey)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "city config not loaded yet")
		return
	}
	resolved, err := config.ResolveProvider(
		&config.Agent{Provider: providerName},
		&cfg.Workspace,
		cfg.Providers,
		exec.LookPath,
	)
	if err != nil {
		s.idem.unreserve(idemKey)
		if errors.Is(err, config.ErrProviderNotInPATH) {
			writeError(w, http.StatusServiceUnavailable, "provider_unavailable", err.Error())
			return
		}
		if errors.Is(err, config.ErrProviderNotFound) {
			writeError(w, http.StatusNotFound, "provider_not_found", "provider '"+providerName+"' not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	// Resolve options against the provider's schema.
	var extraArgs []string
	var optMeta map[string]string
	if len(body.Options) > 0 && len(resolved.OptionsSchema) == 0 {
		s.idem.unreserve(idemKey)
		writeError(w, http.StatusBadRequest, "unknown_option", "provider '"+providerName+"' does not accept options")
		return
	}
	if len(resolved.OptionsSchema) > 0 {
		var optErr error
		extraArgs, optMeta, optErr = config.ResolveOptions(resolved.OptionsSchema, body.Options)
		if optErr != nil {
			s.idem.unreserve(idemKey)
			if errors.Is(optErr, config.ErrUnknownOption) {
				writeError(w, http.StatusBadRequest, "unknown_option", optErr.Error())
				return
			}
			writeError(w, http.StatusBadRequest, "invalid_option_value", optErr.Error())
			return
		}
	}

	template := providerName
	title := body.Title
	if title == "" {
		title = resolved.Name
	}
	if body.Async && strings.TrimSpace(body.Message) != "" {
		s.idem.unreserve(idemKey)
		writeError(w, http.StatusBadRequest, "invalid", "message is not supported with async session creation; create the session, then POST /v0/session/{id}/messages")
		return
	}
	if body.Async {
		s.idem.unreserve(idemKey)
		writeError(w, http.StatusBadRequest, "invalid", "async session creation is only supported for configured agent templates")
		return
	}

	workDir := s.state.CityPath()

	resume := session.ProviderResume{
		ResumeFlag:    resolved.ResumeFlag,
		ResumeStyle:   resolved.ResumeStyle,
		ResumeCommand: resolved.ResumeCommand,
		SessionIDFlag: resolved.SessionIDFlag,
	}
	alias, err := session.ValidateAlias(body.Alias)
	if err != nil {
		s.idem.unreserve(idemKey)
		writeSessionManagerError(w, err)
		return
	}

	command := resolved.CommandString()
	if len(extraArgs) > 0 {
		command = command + " " + shellquote.Join(extraArgs)
	}

	mgr := s.sessionManager(store)
	hints := sessionCreateHints(resolved)
	var info session.Info
	err = session.WithCitySessionAliasLock(s.state.CityPath(), alias, func() error {
		if err := session.EnsureAliasAvailableWithConfig(store, s.state.Config(), alias, ""); err != nil {
			return err
		}
		var createErr error
		info, createErr = mgr.CreateAliasedNamedWithTransport(
			r.Context(),
			alias,
			"",
			template,
			title,
			command,
			workDir,
			resolved.Name,
			"",
			resolved.Env,
			resume,
			hints,
		)
		return createErr
	})
	if err != nil {
		s.idem.unreserve(idemKey)
		writeSessionManagerError(w, err)
		return
	}

	// Persist kind, option metadata, and project_id on the bead.
	s.persistSessionMeta(store, info.ID, "provider", body.ProjectID, optMeta)
	if body.Async {
		s.state.Poke()
	}

	// Deliver initial message if provided.
	if msg := strings.TrimSpace(body.Message); msg != "" {
		resumeCommand, nudgeHints := s.buildSessionResume(info)
		if sendErr := mgr.Send(r.Context(), info.ID, msg, resumeCommand, nudgeHints); sendErr != nil {
			log.Printf("session %s: initial message delivery failed: %v", info.ID, sendErr)
			s.idem.unreserve(idemKey)
			writeError(w, http.StatusInternalServerError, "message_delivery_failed",
				fmt.Sprintf("session created but initial message failed: %v", sendErr))
			return
		}
	}

	resp := sessionToResponse(info, s.state.Config())
	resp.Kind = "provider"
	s.enrichSessionResponse(&resp, info, s.state.Config(), s.state.SessionProvider(), false)
	statusCode := http.StatusCreated
	s.idem.storeResponse(idemKey, bodyHash, statusCode, resp)
	writeJSON(w, statusCode, resp)
}

// persistSessionMeta writes kind, option metadata, and project_id to the session bead.
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

func (s *Server) handleSessionTranscript(w http.ResponseWriter, r *http.Request) {
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

	mgr := s.sessionManager(store)
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

	wantRaw := r.URL.Query().Get("format") == "raw"

	if path != "" {
		tail := 0
		if v := r.URL.Query().Get("tail"); v != "" {
			if n, convErr := strconv.Atoi(v); convErr == nil && n >= 0 {
				tail = n
			}
		}
		before := r.URL.Query().Get("before")

		if wantRaw {
			// Raw format uses ReadFileRaw (no display-type filtering) so
			// all entry types are returned — consistent with the raw
			// stream and snapshot paths.
			var rawSess *sessionlog.Session
			if before != "" {
				rawSess, err = sessionlog.ReadProviderFileRawOlder(info.Provider, path, tail, before)
			} else {
				rawSess, err = sessionlog.ReadProviderFileRaw(info.Provider, path, tail)
			}
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal", "reading session log: "+err.Error())
				return
			}
			msgs := make([]json.RawMessage, 0, len(rawSess.Messages))
			for _, entry := range rawSess.Messages {
				if len(entry.Raw) > 0 {
					msgs = append(msgs, entry.Raw)
				}
			}
			writeJSON(w, http.StatusOK, sessionRawTranscriptResponse{
				ID:         info.ID,
				Template:   info.Template,
				Format:     "raw",
				Messages:   msgs,
				Pagination: rawSess.Pagination,
			})
			return
		}

		var sess *sessionlog.Session
		if before != "" {
			sess, err = sessionlog.ReadProviderFileOlder(info.Provider, path, tail, before)
		} else {
			sess, err = sessionlog.ReadProviderFile(info.Provider, path, tail)
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

	if wantRaw {
		writeJSON(w, http.StatusOK, sessionRawTranscriptResponse{
			ID:       info.ID,
			Template: info.Template,
			Format:   "raw",
			Messages: []json.RawMessage{},
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

	id, err := s.resolveSessionIDMaterializingNamed(store, r.PathValue("id"))
	if err != nil {
		s.idem.unreserve(idemKey)
		writeResolveError(w, err)
		return
	}

	if err := s.sendMessageToSession(r.Context(), store, id, body.Message); err != nil {
		s.idem.unreserve(idemKey)
		writeSessionManagerError(w, err)
		return
	}

	resp := map[string]string{"status": "accepted", "id": id}
	s.idem.storeResponse(idemKey, bodyHash, http.StatusAccepted, resp)
	writeJSON(w, http.StatusAccepted, resp)
}

func (s *Server) handleSessionKill(w http.ResponseWriter, r *http.Request) {
	store := s.state.CityBeadStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	id, err := s.resolveSessionIDWithConfig(store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}

	mgr := s.sessionManager(store)
	if err := mgr.Kill(id); err != nil {
		writeSessionManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "id": id})
}

func (s *Server) handleSessionStop(w http.ResponseWriter, r *http.Request) {
	store := s.state.CityBeadStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	id, err := s.resolveSessionIDWithConfig(store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}

	mgr := s.sessionManager(store)
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

	id, err := s.resolveSessionIDWithConfig(store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}

	mgr := s.sessionManager(store)
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

	id, err := s.resolveSessionIDWithConfig(store, r.PathValue("id"))
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

	mgr := s.sessionManager(store)
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

	id, err := s.resolveSessionIDAllowClosedWithConfig(store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}

	mgr := s.sessionManager(store)
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
	format := r.URL.Query().Get("format")
	if info.Closed {
		if format == "raw" {
			s.emitClosedSessionSnapshotRaw(w, info, path)
		} else {
			s.emitClosedSessionSnapshot(w, info, path)
		}
		return
	}
	switch {
	case path != "":
		if format == "raw" {
			s.streamSessionTranscriptLogRaw(ctx, w, info, path)
		} else {
			s.streamSessionTranscriptLog(ctx, w, info, path)
		}
	case format == "raw":
		// No log file yet. If the session is running, poll tmux pane content
		// and wrap it as a fake raw JSONL assistant message so MC's existing
		// rendering pipeline shows terminal output (e.g. OAuth prompts).
		if running {
			s.streamSessionPeekRaw(ctx, w, info)
		} else {
			data, _ := json.Marshal(sessionRawTranscriptResponse{
				ID:       info.ID,
				Template: info.Template,
				Format:   "raw",
				Messages: []json.RawMessage{},
			})
			writeSSE(w, "message", 1, data)
		}
		return
	default:
		s.streamSessionPeek(ctx, w, info)
	}
}

func (s *Server) emitClosedSessionSnapshot(w http.ResponseWriter, info session.Info, logPath string) {
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
	// Closed session is definitionally idle.
	actData, _ := json.Marshal(map[string]string{"activity": "idle"})
	writeSSE(w, "activity", 2, actData)
}

func (s *Server) emitClosedSessionSnapshotRaw(w http.ResponseWriter, info session.Info, logPath string) {
	if logPath == "" {
		return
	}
	sess, err := sessionlog.ReadProviderFileRaw(info.Provider, logPath, 0)
	if err != nil {
		return
	}

	rawMessages := make([]json.RawMessage, 0, len(sess.Messages))
	for _, entry := range sess.Messages {
		if len(entry.Raw) == 0 {
			continue
		}
		rawMessages = append(rawMessages, entry.Raw)
	}
	if len(rawMessages) == 0 {
		return
	}

	data, err := json.Marshal(sessionRawTranscriptResponse{
		ID:       info.ID,
		Template: info.Template,
		Format:   "raw",
		Messages: rawMessages,
	})
	if err != nil {
		return
	}
	writeSSE(w, "message", 1, data)
	// Closed session is definitionally idle.
	actData, _ := json.Marshal(map[string]string{"activity": "idle"})
	writeSSE(w, "activity", 2, actData)
}

func (s *Server) streamSessionTranscriptLogRaw(ctx context.Context, w http.ResponseWriter, info session.Info, logPath string) {
	lw := newLogFileWatcher(logPath)
	defer lw.Close()

	var lastSize int64
	var lastSentUUID string
	var seq uint64
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

		rawMessages := make([]json.RawMessage, 0, len(sess.Messages))
		uuids := make([]string, 0, len(sess.Messages))
		for _, entry := range sess.Messages {
			if len(entry.Raw) == 0 {
				continue
			}
			rawMessages = append(rawMessages, entry.Raw)
			uuids = append(uuids, entry.UUID)
		}

		// Emit messages if there are new ones.
		if len(rawMessages) > 0 {
			var toSend []json.RawMessage

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
				data, err := json.Marshal(sessionRawTranscriptResponse{
					ID:       info.ID,
					Template: info.Template,
					Format:   "raw",
					Messages: toSend,
				})
				if err == nil {
					writeSSE(w, "message", seq, data)
				}
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
			actData, _ := json.Marshal(map[string]string{"activity": activity})
			writeSSE(w, "activity", seq, actData)
		}
	}

	lw.Run(ctx, readAndEmit, func() { writeSSEComment(w) })
}

func (s *Server) streamSessionTranscriptLog(ctx context.Context, w http.ResponseWriter, info session.Info, logPath string) {
	lw := newLogFileWatcher(logPath)
	defer lw.Close()

	var lastSize int64
	var lastSentUUID string
	var seq uint64
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
				data, err := json.Marshal(sessionTranscriptResponse{
					ID:       info.ID,
					Template: info.Template,
					Format:   "conversation",
					Turns:    toSend,
				})
				if err == nil {
					writeSSE(w, "turn", seq, data)
				}
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
			actData, _ := json.Marshal(map[string]string{"activity": activity})
			writeSSE(w, "activity", seq, actData)
		}
	}

	lw.Run(ctx, readAndEmit, func() { writeSSEComment(w) })
}

// streamSessionPeekRaw polls tmux pane content and wraps it as format=raw
// messages so MC's JSONL rendering pipeline can display terminal output
// (e.g. OAuth prompts, startup screens) when no transcript log exists yet.
func (s *Server) streamSessionPeekRaw(ctx context.Context, w http.ResponseWriter, info session.Info) {
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

		if output == "" {
			return
		}

		// Wrap as a fake assistant message in raw JSONL format so MC's
		// translate_transcript_response handles it like a normal transcript.
		fakeMsg, _ := json.Marshal(map[string]interface{}{
			"role": "assistant",
			"content": []map[string]string{
				{"type": "text", "text": output},
			},
		})
		data, err := json.Marshal(sessionRawTranscriptResponse{
			ID:       info.ID,
			Template: info.Template,
			Format:   "raw",
			Messages: []json.RawMessage{fakeMsg},
		})
		if err != nil {
			return
		}
		writeSSE(w, "message", seq, data)
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
