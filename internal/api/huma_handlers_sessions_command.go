package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
)

// Command-side session handlers (create, patch, submit, message, stop, kill,
// respond, suspend, close, wake, rename). Split out of huma_handlers_sessions.go
// to isolate mutation logic from reads and streaming.

func (s *Server) humaHandleSessionCreate(ctx context.Context, input *SessionCreateInput) (*SessionCreateOutput, error) {
	store := s.state.CityBeadStore()
	if store == nil {
		return nil, huma.Error503ServiceUnavailable("no bead store configured")
	}

	body := input.Body
	if body.LegacySessionName != nil {
		return nil, huma.Error400BadRequest("session_name is no longer accepted; use alias")
	}

	kind := body.Kind
	name := body.Name
	if name == "" {
		return nil, huma.Error400BadRequest("name is required")
	}
	if kind != "agent" && kind != "provider" {
		return nil, huma.Error400BadRequest("kind must be 'agent' or 'provider'")
	}

	if kind == "provider" {
		return s.humaCreateProviderSession(ctx, store, body, name)
	}

	// Agent track.
	resolved, workDir, transport, template, err := s.resolveSessionTemplateWithBareNameFallback(name)
	if err != nil {
		if errors.Is(err, errSessionTemplateNotFound) {
			return nil, huma.Error404NotFound("agent '" + name + "' not found")
		}
		return nil, huma.Error500InternalServerError(err.Error())
	}

	if len(body.Options) > 0 {
		if len(resolved.OptionsSchema) == 0 {
			return nil, huma.Error400BadRequest("agent '" + name + "' does not accept options")
		}
		if _, optErr := config.ResolveExplicitOptions(resolved.OptionsSchema, body.Options); optErr != nil {
			if errors.Is(optErr, config.ErrUnknownOption) {
				return nil, huma.Error400BadRequest(optErr.Error())
			}
			return nil, huma.Error400BadRequest(optErr.Error())
		}
	}

	title := body.Title
	if title == "" {
		title = template
	}

	resume := session.ProviderResume{
		ResumeFlag:    resolved.ResumeFlag,
		ResumeStyle:   resolved.ResumeStyle,
		ResumeCommand: resolved.ResumeCommand,
		SessionIDFlag: resolved.SessionIDFlag,
	}
	alias, err := session.ValidateAlias(body.Alias)
	if err != nil {
		return nil, humaSessionManagerError(err)
	}
	cfg := s.state.Config()
	if cfg == nil {
		return nil, huma.Error500InternalServerError("no city config loaded")
	}
	agentCfg, ok := resolveSessionTemplateAgent(cfg, template)
	if !ok {
		return nil, huma.Error500InternalServerError("resolved agent template disappeared: " + template)
	}
	if alias != "" && agentCfg.SupportsMultipleSessions() {
		alias = workdirutil.SessionQualifiedName(s.state.CityPath(), agentCfg, cfg.Rigs, alias, "")
	}
	explicitName, err := sessionExplicitNameForCreate(agentCfg, alias)
	if err != nil {
		return nil, humaSessionManagerError(err)
	}
	workDirQualifiedName := workdirutil.SessionQualifiedName(s.state.CityPath(), agentCfg, cfg.Rigs, alias, explicitName)
	workDir, err = s.resolveSessionWorkDir(agentCfg, workDirQualifiedName)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}

	command := sessionCreateAgentCommand(resolved)
	extraMeta := sessionTemplateOverridesMetadata(body.Options, body.Message)

	mgr := s.sessionManager(store)
	var info session.Info
	reservationIDs := []string{alias, explicitName}
	reserveConcreteIdentity := agentCfg.SupportsMultipleSessions() && strings.TrimSpace(workDirQualifiedName) != ""
	if reserveConcreteIdentity {
		reservationIDs = append(reservationIDs, workDirQualifiedName)
	}
	err = session.WithCitySessionIdentifierLocks(s.state.CityPath(), reservationIDs, func() error {
		if aliasErr := session.EnsureAliasAvailableWithConfig(store, s.state.Config(), alias, ""); aliasErr != nil {
			return aliasErr
		}
		if reserveConcreteIdentity && workDirQualifiedName != alias {
			if aliasErr := session.EnsureAliasAvailableWithConfig(store, s.state.Config(), workDirQualifiedName, ""); aliasErr != nil {
				return aliasErr
			}
		}
		if nameErr := session.EnsureSessionNameAvailableWithConfig(store, s.state.Config(), explicitName, ""); nameErr != nil {
			return nameErr
		}
		if extraMeta == nil {
			extraMeta = make(map[string]string)
		}
		extraMeta["agent_name"] = workDirQualifiedName
		extraMeta["session_origin"] = "manual"
		var createErr error
		info, createErr = mgr.CreateAliasedBeadOnlyNamedWithMetadata(
			alias,
			explicitName,
			template,
			title,
			command,
			workDir,
			resolved.Name,
			transport,
			resume,
			extraMeta,
		)
		return createErr
	})
	if err != nil {
		return nil, humaSessionManagerError(err)
	}

	s.persistSessionMeta(store, info.ID, "agent", body.ProjectID, nil)
	s.state.Poke()

	titleProvider := s.resolveTitleProvider()
	MaybeGenerateTitleAsync(store, info.ID, body.Title, body.Message, titleProvider, info.WorkDir, func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "session %s: "+format+"\n", append([]any{info.ID}, args...)...)
	})

	resp := sessionToResponse(info, s.state.Config())
	resp.Kind = "agent"
	if caps, capErr := s.sessionManager(store).SubmissionCapabilities(info.ID); capErr == nil {
		resp.SubmissionCapabilities = caps
	}
	s.enrichSessionResponse(&resp, info, s.state.Config(), s.state.SessionProvider(), false)

	out := &SessionCreateOutput{Status: http.StatusAccepted}
	out.Body = resp
	return out, nil
}

// humaCreateProviderSession handles the "provider" kind session creation.

func (s *Server) humaCreateProviderSession(ctx context.Context, store beads.Store, body sessionCreateBody, providerName string) (*SessionCreateOutput, error) {
	cfg := s.state.Config()
	if cfg == nil {
		return nil, huma.Error503ServiceUnavailable("city config not loaded yet")
	}
	resolved, err := config.ResolveProvider(
		&config.Agent{Provider: providerName},
		&cfg.Workspace,
		cfg.Providers,
		exec.LookPath,
	)
	if err != nil {
		if errors.Is(err, config.ErrProviderNotInPATH) {
			return nil, huma.Error503ServiceUnavailable(err.Error())
		}
		if errors.Is(err, config.ErrProviderNotFound) {
			return nil, huma.Error404NotFound("provider '" + providerName + "' not found")
		}
		return nil, huma.Error500InternalServerError(err.Error())
	}

	var optMeta map[string]string
	if len(body.Options) > 0 && len(resolved.OptionsSchema) == 0 {
		return nil, huma.Error400BadRequest("provider '" + providerName + "' does not accept options")
	}
	if len(resolved.OptionsSchema) > 0 {
		var optErr error
		_, optMeta, optErr = config.ResolveOptions(resolved.OptionsSchema, body.Options, resolved.EffectiveDefaults)
		if optErr != nil {
			if errors.Is(optErr, config.ErrUnknownOption) {
				return nil, huma.Error400BadRequest(optErr.Error())
			}
			return nil, huma.Error400BadRequest(optErr.Error())
		}
	}

	template := providerName
	title := body.Title
	if title == "" {
		title = resolved.Name
	}
	if body.Async && strings.TrimSpace(body.Message) != "" {
		return nil, huma.Error400BadRequest("message is not supported with async session creation; create the session, then POST /v0/session/{id}/messages")
	}
	if body.Async {
		return nil, huma.Error400BadRequest("async session creation is only supported for configured agent templates")
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
		return nil, humaSessionManagerError(err)
	}

	launchCommand, err := config.BuildProviderLaunchCommand(s.state.CityPath(), resolved, body.Options)
	if err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}
	command := launchCommand.Command

	mgr := s.sessionManager(store)
	hints := sessionCreateHints(resolved)
	var info session.Info
	err = session.WithCitySessionAliasLock(s.state.CityPath(), alias, func() error {
		if aliasErr := session.EnsureAliasAvailableWithConfig(store, s.state.Config(), alias, ""); aliasErr != nil {
			return aliasErr
		}
		var createErr error
		info, createErr = mgr.CreateAliasedNamedWithTransport(
			ctx,
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
		return nil, humaSessionManagerError(err)
	}

	s.persistSessionMeta(store, info.ID, "provider", body.ProjectID, optMeta)

	titleProvider := s.resolveTitleProvider()
	MaybeGenerateTitleAsync(store, info.ID, body.Title, body.Message, titleProvider, info.WorkDir, func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "session %s: "+format+"\n", append([]any{info.ID}, args...)...)
	})

	if msg := strings.TrimSpace(body.Message); msg != "" {
		if _, sendErr := s.submitMessageToSession(ctx, store, info.ID, msg, session.SubmitIntentDefault); sendErr != nil {
			log.Printf("session %s: initial message delivery failed: %v", info.ID, sendErr)
			if rollbackErr := s.rollbackCreatedSession(store, info.ID); rollbackErr != nil {
				return nil, huma.Error500InternalServerError(
					fmt.Sprintf("initial message delivery failed: %v (rollback failed: %v)", sendErr, rollbackErr))
			}
			return nil, huma.Error500InternalServerError(
				fmt.Sprintf("initial message delivery failed: %v", sendErr))
		}
	}

	resp := sessionToResponse(info, s.state.Config())
	resp.Kind = "provider"
	if caps, capErr := s.sessionManager(store).SubmissionCapabilities(info.ID); capErr == nil {
		resp.SubmissionCapabilities = caps
	}
	s.enrichSessionResponse(&resp, info, s.state.Config(), s.state.SessionProvider(), false)

	out := &SessionCreateOutput{Status: http.StatusCreated}
	out.Body = resp
	return out, nil
}

// --- Session Transcript ---

// sessionTranscriptGetResponse is the union of conversation/text and raw
// transcript response shapes. When Format is "conversation" or "text",
// Turns is populated. When Format is "raw", Messages carries pre-decoded
// provider-native frames as generic JSON values. The spec describes the
// items as arbitrary JSON (any) — clients interpret shapes based on the
// session's provider.
type sessionTranscriptGetResponse struct {
	ID         string                     `json:"id"`
	Template   string                     `json:"template"`
	Provider   string                     `json:"provider" doc:"Producing provider identifier (claude, codex, gemini, open-code, etc.). Consumers use this to dispatch per-provider frame parsing."`
	Format     string                     `json:"format" doc:"conversation, text, or raw."`
	Turns      []outputTurn               `json:"turns,omitempty" doc:"Populated for conversation/text formats."`
	Messages   []SessionRawMessageFrame   `json:"messages,omitempty" doc:"Populated for raw format; provider-native frames emitted verbatim as the provider wrote them."`
	Pagination *sessionlog.PaginationInfo `json:"pagination,omitempty"`
}

// humaHandleSessionTranscript is the Huma-typed handler for GET /v0/session/{id}/transcript.

func (s *Server) humaHandleSessionPatch(_ context.Context, input *SessionPatchInput) (*IndexOutput[sessionResponse], error) {
	store := s.state.CityBeadStore()
	if store == nil {
		return nil, huma.Error503ServiceUnavailable("no bead store configured")
	}

	id, err := s.resolveSessionIDWithConfig(store, input.ID)
	if err != nil {
		return nil, humaResolveError(err)
	}

	// Huma has already validated:
	//  - `additionalProperties: false` → unknown fields (e.g. "template") are 422
	//  - `minLength:"1"` on Title → non-empty when provided
	// The handler only needs to enforce "at least one field" and the
	// alias-controller-managed rule below.
	titlePtr := input.Body.Title
	aliasPtr := input.Body.Alias

	if titlePtr == nil && aliasPtr == nil {
		return nil, huma.Error422UnprocessableEntity("at least one of 'title' or 'alias' is required")
	}

	b, err := store.Get(id)
	if err != nil {
		return nil, humaStoreError(err)
	}
	if !session.IsSessionBeadOrRepairable(b) {
		return nil, huma.Error400BadRequest(id + " is not a session")
	}
	session.RepairEmptyType(store, &b)

	mgr := s.sessionManager(store)
	updateFn := func() error {
		return mgr.UpdatePresentation(id, titlePtr, aliasPtr)
	}
	if aliasPtr != nil {
		if strings.TrimSpace(b.Metadata["agent_name"]) != "" {
			return nil, huma.Error403Forbidden("forbidden: alias is controller-managed for this session")
		}
		if lockErr := session.WithCitySessionAliasLock(s.state.CityPath(), *aliasPtr, func() error {
			if avErr := session.EnsureAliasAvailableWithConfig(store, s.state.Config(), *aliasPtr, id); avErr != nil {
				return avErr
			}
			return updateFn()
		}); lockErr != nil {
			return nil, humaSessionManagerError(lockErr)
		}
	} else if err := updateFn(); err != nil {
		return nil, humaSessionManagerError(err)
	}

	info, err := mgr.Get(id)
	if err != nil {
		return nil, humaSessionManagerError(err)
	}
	updated, _ := store.Get(id)
	presp := sessionResponseWithReason(info, &updated, s.state.Config(), strings.TrimSpace(s.state.CityPath()) != "")
	return &IndexOutput[sessionResponse]{
		Index: s.latestIndex(),
		Body:  presp,
	}, nil
}

// --- Session Submit ---

// humaHandleSessionSubmit is the Huma-typed handler for POST /v0/session/{id}/submit.

func (s *Server) humaHandleSessionSubmit(ctx context.Context, input *SessionSubmitInput) (*SessionSubmitOutput, error) {
	store := s.state.CityBeadStore()
	if store == nil {
		return nil, huma.Error503ServiceUnavailable("no bead store configured")
	}

	// Huma validates Body.Message (minLength:1) and Body.Intent (enum).
	// Handler-side guards are redundant; keep only the default-intent fill.
	intent := input.Body.Intent
	if intent == "" {
		intent = session.SubmitIntentDefault
	}

	id, err := s.resolveSessionIDMaterializingNamedWithContext(ctx, store, input.ID)
	if err != nil {
		return nil, humaResolveError(err)
	}

	outcome, err := s.submitMessageToSession(ctx, store, id, input.Body.Message, intent)
	if err != nil {
		return nil, humaSessionManagerError(err)
	}

	out := &SessionSubmitOutput{}
	out.Body.Status = "accepted"
	out.Body.ID = id
	out.Body.Queued = outcome.Queued
	out.Body.Intent = string(intent)
	return out, nil
}

// --- Session Messages ---

// humaHandleSessionMessage is the Huma-typed handler for POST /v0/session/{id}/messages.

func (s *Server) humaHandleSessionMessage(ctx context.Context, input *SessionMessageInput) (*SessionMessageOutput, error) {
	store := s.state.CityBeadStore()
	if store == nil {
		return nil, huma.Error503ServiceUnavailable("no bead store configured")
	}

	// Huma validates Body.Message (minLength:1); no handler guard needed.
	id, err := s.resolveSessionIDMaterializingNamedWithContext(ctx, store, input.ID)
	if err != nil {
		return nil, humaResolveError(err)
	}

	if err := s.sendUserMessageToSession(ctx, store, id, input.Body.Message); err != nil {
		return nil, humaSessionManagerError(err)
	}

	out := &SessionMessageOutput{}
	out.Body.Status = "accepted"
	out.Body.ID = id
	return out, nil
}

// --- Session Stop ---

// humaHandleSessionStop is the Huma-typed handler for POST /v0/session/{id}/stop.

func (s *Server) humaHandleSessionStop(_ context.Context, input *SessionIDInput) (*OKWithIDResponse, error) {
	store := s.state.CityBeadStore()
	if store == nil {
		return nil, huma.Error503ServiceUnavailable("no bead store configured")
	}

	id, err := s.resolveSessionIDWithConfig(store, input.ID)
	if err != nil {
		return nil, humaResolveError(err)
	}

	mgr := s.sessionManager(store)
	if err := mgr.StopTurn(id); err != nil {
		return nil, humaSessionManagerError(err)
	}
	out := &OKWithIDResponse{}
	out.Body.Status = "ok"
	out.Body.ID = id
	return out, nil
}

// --- Session Kill ---

// humaHandleSessionKill is the Huma-typed handler for POST /v0/session/{id}/kill.

func (s *Server) humaHandleSessionKill(_ context.Context, input *SessionIDInput) (*OKWithIDResponse, error) {
	store := s.state.CityBeadStore()
	if store == nil {
		return nil, huma.Error503ServiceUnavailable("no bead store configured")
	}

	id, err := s.resolveSessionIDWithConfig(store, input.ID)
	if err != nil {
		return nil, humaResolveError(err)
	}

	mgr := s.sessionManager(store)
	if err := mgr.Kill(id); err != nil {
		return nil, humaSessionManagerError(err)
	}
	out := &OKWithIDResponse{}
	out.Body.Status = "ok"
	out.Body.ID = id
	return out, nil
}

// --- Session Respond ---

// humaHandleSessionRespond is the Huma-typed handler for POST /v0/session/{id}/respond.

func (s *Server) humaHandleSessionRespond(_ context.Context, input *SessionRespondInput) (*SessionRespondOutput, error) {
	store := s.state.CityBeadStore()
	if store == nil {
		return nil, huma.Error503ServiceUnavailable("no bead store configured")
	}

	id, err := s.resolveSessionIDWithConfig(store, input.ID)
	if err != nil {
		return nil, humaResolveError(err)
	}

	// Huma validates Body.Action (minLength:1); no handler guard needed.
	mgr := s.sessionManager(store)
	if err := mgr.Respond(id, runtime.InteractionResponse{
		RequestID: input.Body.RequestID,
		Action:    input.Body.Action,
		Text:      input.Body.Text,
		Metadata:  input.Body.Metadata,
	}); err != nil {
		return nil, humaSessionManagerError(err)
	}

	out := &SessionRespondOutput{}
	out.Body.Status = "accepted"
	out.Body.ID = id
	return out, nil
}

// --- Session Suspend ---

// humaHandleSessionSuspend is the Huma-typed handler for POST /v0/session/{id}/suspend.

func (s *Server) humaHandleSessionSuspend(ctx context.Context, input *SessionIDInput) (*OKResponse, error) {
	store := s.state.CityBeadStore()
	if store == nil {
		return nil, huma.Error503ServiceUnavailable("no bead store configured")
	}
	mgr := s.sessionManager(store)

	id, err := s.resolveSessionIDMaterializingNamedWithContext(ctx, store, input.ID)
	if err != nil {
		return nil, humaResolveError(err)
	}
	if err := mgr.Suspend(id); err != nil {
		return nil, humaSessionManagerError(err)
	}
	out := &OKResponse{}
	out.Body.Status = "ok"
	return out, nil
}

// --- Session Close ---

// humaHandleSessionClose is the Huma-typed handler for POST /v0/session/{id}/close.

func (s *Server) humaHandleSessionClose(_ context.Context, input *SessionCloseInput) (*OKResponse, error) {
	store := s.state.CityBeadStore()
	if store == nil {
		return nil, huma.Error503ServiceUnavailable("no bead store configured")
	}
	mgr := s.sessionManager(store)

	id, err := s.resolveSessionIDWithConfig(store, input.ID)
	if err != nil {
		return nil, humaResolveError(err)
	}

	if b, getErr := store.Get(id); getErr == nil &&
		strings.TrimSpace(b.Metadata[apiNamedSessionMetadataKey]) == "true" &&
		strings.TrimSpace(b.Metadata[apiNamedSessionModeKey]) == "always" &&
		strings.Contains(strings.TrimSpace(b.Metadata[apiNamedSessionIdentityKey]), "/") {
		return nil, huma.Error409Conflict("configured always-on named sessions cannot be closed while config-managed")
	}
	nudgeIDs, err := session.WaitNudgeIDs(store, id)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	if err := mgr.Close(id); err != nil {
		return nil, humaSessionManagerError(err)
	}
	if err := withdrawQueuedWaitNudges(store, s.state.CityPath(), nudgeIDs); err != nil {
		log.Printf("gc api: withdrawing queued wait nudges after close %s: %v", id, err)
	}

	// Optional: permanently delete the bead after closing.
	if input.Delete {
		if err := store.Delete(id); err != nil {
			log.Printf("gc api: deleting bead after close %s: %v", id, err)
			return nil, huma.Error500InternalServerError("closed but delete failed: " + err.Error())
		}
	}

	out := &OKResponse{}
	out.Body.Status = "ok"
	return out, nil
}

// --- Session Wake ---

// humaHandleSessionWake is the Huma-typed handler for POST /v0/session/{id}/wake.

func (s *Server) humaHandleSessionWake(ctx context.Context, input *SessionIDInput) (*OKWithIDResponse, error) {
	store := s.state.CityBeadStore()
	if store == nil {
		return nil, huma.Error503ServiceUnavailable("no bead store configured")
	}

	id, err := s.resolveSessionIDMaterializingNamedWithContext(ctx, store, input.ID)
	if err != nil {
		return nil, humaResolveError(err)
	}

	b, err := store.Get(id)
	if err != nil {
		return nil, humaStoreError(err)
	}
	if !session.IsSessionBeadOrRepairable(b) {
		return nil, huma.Error400BadRequest(id + " is not a session")
	}
	session.RepairEmptyType(store, &b)
	if b.Status == "closed" {
		return nil, huma.Error409Conflict("session " + id + " is closed")
	}

	nudgeIDs, err := session.WakeSession(store, b, time.Now().UTC())
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	if err := withdrawQueuedWaitNudges(store, s.state.CityPath(), nudgeIDs); err != nil {
		log.Printf("gc api: withdrawing queued wait nudges after wake %s: %v", id, err)
	}
	sessionName := b.Metadata["session_name"]
	if sessionName != "" {
		s.state.ClearCrashHistory(sessionName)
	}

	out := &OKWithIDResponse{}
	out.Body.Status = "ok"
	out.Body.ID = id
	return out, nil
}

// --- Session Rename ---

// humaHandleSessionRename is the Huma-typed handler for POST /v0/session/{id}/rename.

func (s *Server) humaHandleSessionRename(_ context.Context, input *SessionRenameInput) (*IndexOutput[sessionResponse], error) {
	store := s.state.CityBeadStore()
	if store == nil {
		return nil, huma.Error503ServiceUnavailable("no bead store configured")
	}

	id, err := s.resolveSessionIDWithConfig(store, input.ID)
	if err != nil {
		return nil, humaResolveError(err)
	}

	// Huma validates Body.Title (minLength:1); no handler guard needed.
	b, err := store.Get(id)
	if err != nil {
		return nil, humaStoreError(err)
	}
	if !session.IsSessionBeadOrRepairable(b) {
		return nil, huma.Error400BadRequest(id + " is not a session")
	}
	session.RepairEmptyType(store, &b)

	mgr := s.sessionManager(store)
	if err := mgr.Rename(id, input.Body.Title); err != nil {
		return nil, humaSessionManagerError(err)
	}

	info, err := mgr.Get(id)
	if err != nil {
		return nil, humaSessionManagerError(err)
	}
	updated, _ := store.Get(id)
	rresp := sessionResponseWithReason(info, &updated, s.state.Config(), strings.TrimSpace(s.state.CityPath()) != "")
	return &IndexOutput[sessionResponse]{
		Index: s.latestIndex(),
		Body:  rresp,
	}, nil
}

// --- Session Agent List ---

// sessionAgentListResponse is the response for GET /v0/session/{id}/agents.
type sessionAgentListResponse struct {
	Agents []sessionlog.AgentMapping `json:"agents"`
}

// sessionAgentGetResponse is the response for GET /v0/session/{id}/agents/{agentId}.
// Messages carries pre-decoded provider-native transcript frames as
// generic JSON values (arbitrary JSON per spec). Same pattern as
// sessionTranscriptGetResponse.Messages.
type sessionAgentGetResponse struct {
	Messages []any                  `json:"messages"`
	Status   sessionlog.AgentStatus `json:"status,omitempty"`
}

// humaHandleSessionAgentList is the Huma-typed handler for GET /v0/session/{id}/agents.
