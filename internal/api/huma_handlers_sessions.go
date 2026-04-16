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

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// --- Huma error helpers for session endpoints ---

// humaResolveError maps session.ResolveSessionID errors to Huma errors.
func humaResolveError(err error) error {
	switch {
	case errors.Is(err, session.ErrAmbiguous), errors.Is(err, errConfiguredNamedSessionConflict):
		return huma.Error409Conflict(err.Error())
	case errors.Is(err, session.ErrSessionNotFound):
		return huma.Error404NotFound(err.Error())
	default:
		return huma.Error500InternalServerError(err.Error())
	}
}

// humaSessionManagerError maps session manager errors to Huma errors.
func humaSessionManagerError(err error) error {
	switch {
	case errors.Is(err, session.ErrInvalidSessionName):
		return huma.Error400BadRequest(err.Error())
	case errors.Is(err, session.ErrSessionNameExists):
		return huma.Error409Conflict(err.Error())
	case errors.Is(err, session.ErrInvalidSessionAlias):
		return huma.Error400BadRequest(err.Error())
	case errors.Is(err, session.ErrSessionAliasExists):
		return huma.Error409Conflict(err.Error())
	case errors.Is(err, session.ErrInteractionUnsupported):
		return huma.Error501NotImplemented(err.Error())
	case errors.Is(err, session.ErrPendingInteraction):
		return huma.Error409Conflict(err.Error())
	case errors.Is(err, session.ErrNoPendingInteraction):
		return huma.Error409Conflict(err.Error())
	case errors.Is(err, session.ErrInteractionMismatch):
		return huma.Error409Conflict(err.Error())
	case errors.Is(err, session.ErrSessionClosed), errors.Is(err, session.ErrResumeRequired):
		return huma.Error409Conflict(err.Error())
	case errors.Is(err, session.ErrNotSession):
		return huma.Error400BadRequest(err.Error())
	default:
		return humaStoreError(err)
	}
}

// humaStoreError maps bead store errors to Huma errors.
func humaStoreError(err error) error {
	if errors.Is(err, beads.ErrNotFound) {
		return huma.Error404NotFound(err.Error())
	}
	return huma.Error500InternalServerError(err.Error())
}

// --- Session List ---

// humaHandleSessionList is the Huma-typed handler for GET /v0/sessions.
func (s *Server) humaHandleSessionList(_ context.Context, input *SessionListInput) (*ListOutput[sessionResponse], error) {
	store := s.state.CityBeadStore()
	if store == nil {
		return nil, huma.Error503ServiceUnavailable("no bead store configured")
	}
	mgr := s.sessionManager(store)
	cfg := s.state.Config()
	sp := s.state.SessionProvider()

	sessions, err := mgr.List(input.State, input.Template)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}

	// Build bead index for reason enrichment.
	beadIndex := make(map[string]*beads.Bead)
	if all, listErr := store.List(beads.ListQuery{Label: session.LabelSession}); listErr == nil {
		for i := range all {
			beadIndex[all[i].ID] = &all[i]
		}
	}

	wantPeek := input.Peek == "true"
	hasDeferredQueue := strings.TrimSpace(s.state.CityPath()) != ""
	items := make([]sessionResponse, len(sessions))
	for i, sess := range sessions {
		items[i] = sessionResponseWithReason(sess, beadIndex[sess.ID], cfg, hasDeferredQueue)
		s.enrichSessionResponse(&items[i], sess, cfg, sp, wantPeek)
	}

	// Pagination support.
	limit := maxPaginationLimit
	if input.Limit > 0 {
		limit = input.Limit
		if limit > maxPaginationLimit {
			limit = maxPaginationLimit
		}
	}

	if input.Cursor == "" {
		// No pagination cursor -- just cap at limit.
		if limit < len(items) {
			items = items[:limit]
		}
		if items == nil {
			items = []sessionResponse{}
		}
		return &ListOutput[sessionResponse]{
			Body: ListBody[sessionResponse]{Items: items, Total: len(items)},
		}, nil
	}

	pp := pageParams{
		Offset:   decodeCursor(input.Cursor),
		Limit:    limit,
		IsPaging: true,
	}
	page, total, nextCursor := paginate(items, pp)
	if page == nil {
		page = []sessionResponse{}
	}
	return &ListOutput[sessionResponse]{
		Body: ListBody[sessionResponse]{Items: page, Total: total, NextCursor: nextCursor},
	}, nil
}

// --- Session Get ---

// humaHandleSessionGet is the Huma-typed handler for GET /v0/session/{id}.
func (s *Server) humaHandleSessionGet(_ context.Context, input *SessionGetInput) (*IndexOutput[sessionResponse], error) {
	store := s.state.CityBeadStore()
	if store == nil {
		return nil, huma.Error503ServiceUnavailable("no bead store configured")
	}
	mgr := s.sessionManager(store)
	cfg := s.state.Config()
	sp := s.state.SessionProvider()

	id, err := s.resolveSessionIDAllowClosedWithConfig(store, input.ID)
	if err != nil {
		return nil, humaResolveError(err)
	}
	info, err := mgr.Get(id)
	if err != nil {
		return nil, humaSessionManagerError(err)
	}
	b, _ := store.Get(id)
	wantPeek := input.Peek == "true"
	resp := sessionResponseWithReason(info, &b, cfg, strings.TrimSpace(s.state.CityPath()) != "")
	s.enrichSessionResponse(&resp, info, cfg, sp, wantPeek)
	return &IndexOutput[sessionResponse]{
		Index: s.latestIndex(),
		Body:  resp,
	}, nil
}

// --- Session Create ---

// humaHandleSessionCreate is the Huma-typed handler for POST /v0/sessions.
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
	resolved, workDir, transport, template, err := s.resolveSessionTemplate(name)
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

	command := resolved.CommandString()
	if len(resolved.OptionsSchema) > 0 {
		mergedOptions := make(map[string]string)
		for k, v := range resolved.EffectiveDefaults {
			mergedOptions[k] = v
		}
		for k, v := range body.Options {
			mergedOptions[k] = v
		}
		if mergedArgs, mergeErr := config.ResolveExplicitOptions(resolved.OptionsSchema, mergedOptions); mergeErr == nil && len(mergedArgs) > 0 {
			command = config.ReplaceSchemaFlags(command, resolved.OptionsSchema, mergedArgs)
		}
	}

	allOverrides := make(map[string]string)
	for k, v := range body.Options {
		allOverrides[k] = v
	}
	if msg := strings.TrimSpace(body.Message); msg != "" {
		allOverrides["initial_message"] = msg
	}
	var extraMeta map[string]string
	if len(allOverrides) > 0 {
		if overridesJSON, jsonErr := json.Marshal(allOverrides); jsonErr == nil {
			extraMeta = map[string]string{"template_overrides": string(overridesJSON)}
		}
	}

	mgr := s.sessionManager(store)
	var info session.Info
	err = session.WithCitySessionAliasLock(s.state.CityPath(), alias, func() error {
		if aliasErr := session.EnsureAliasAvailableWithConfig(store, s.state.Config(), alias, ""); aliasErr != nil {
			return aliasErr
		}
		var createErr error
		info, createErr = mgr.CreateAliasedBeadOnlyNamedWithMetadata(
			alias,
			"",
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

	out := &SessionCreateOutput{}
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

	var extraArgs []string
	var optMeta map[string]string
	if len(body.Options) > 0 && len(resolved.OptionsSchema) == 0 {
		return nil, huma.Error400BadRequest("provider '" + providerName + "' does not accept options")
	}
	if len(resolved.OptionsSchema) > 0 {
		var optErr error
		extraArgs, optMeta, optErr = config.ResolveOptions(resolved.OptionsSchema, body.Options, resolved.EffectiveDefaults)
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

	command := resolved.CommandString()
	if len(extraArgs) > 0 {
		command = command + " " + shellquote.Join(extraArgs)
	}

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
			return nil, huma.Error500InternalServerError(
				fmt.Sprintf("session created but initial message failed: %v", sendErr))
		}
	}

	resp := sessionToResponse(info, s.state.Config())
	resp.Kind = "provider"
	if caps, capErr := s.sessionManager(store).SubmissionCapabilities(info.ID); capErr == nil {
		resp.SubmissionCapabilities = caps
	}
	s.enrichSessionResponse(&resp, info, s.state.Config(), s.state.SessionProvider(), false)

	out := &SessionCreateOutput{}
	out.Body = resp
	return out, nil
}

// --- Session Transcript ---

// humaHandleSessionTranscript is the Huma-typed handler for GET /v0/session/{id}/transcript.
func (s *Server) humaHandleSessionTranscript(_ context.Context, input *SessionTranscriptInput) (*IndexOutput[json.RawMessage], error) {
	store := s.state.CityBeadStore()
	if store == nil {
		return nil, huma.Error503ServiceUnavailable("no bead store configured")
	}

	id, err := s.resolveSessionIDAllowClosedWithConfig(store, input.ID)
	if err != nil {
		return nil, humaResolveError(err)
	}

	mgr := s.sessionManager(store)
	info, err := mgr.Get(id)
	if err != nil {
		return nil, humaSessionManagerError(err)
	}

	path, err := mgr.TranscriptPath(id, s.sessionLogPaths())
	if err != nil {
		return nil, humaSessionManagerError(err)
	}

	wantRaw := input.Format == "raw"

	if path != "" {
		tail := 0
		if input.Tail != "" {
			if n, convErr := strconv.Atoi(input.Tail); convErr == nil && n >= 0 {
				tail = n
			}
		}
		before := input.Before

		if wantRaw {
			var rawSess *sessionlog.Session
			if before != "" {
				rawSess, err = sessionlog.ReadProviderFileRawOlder(info.Provider, path, tail, before)
			} else {
				rawSess, err = sessionlog.ReadProviderFileRaw(info.Provider, path, tail)
			}
			if err != nil {
				return nil, huma.Error500InternalServerError("reading session log: " + err.Error())
			}
			msgs := make([]json.RawMessage, 0, len(rawSess.Messages))
			for _, entry := range rawSess.Messages {
				if len(entry.Raw) > 0 {
					msgs = append(msgs, entry.Raw)
				}
			}
			resp := sessionRawTranscriptResponse{
				ID:         info.ID,
				Template:   info.Template,
				Format:     "raw",
				Messages:   msgs,
				Pagination: rawSess.Pagination,
			}
			raw, _ := json.Marshal(resp)
			return &IndexOutput[json.RawMessage]{
				Index: s.latestIndex(),
				Body:  raw,
			}, nil
		}

		var sess *sessionlog.Session
		if before != "" {
			sess, err = sessionlog.ReadProviderFileOlder(info.Provider, path, tail, before)
		} else {
			sess, err = sessionlog.ReadProviderFile(info.Provider, path, tail)
		}
		if err != nil {
			return nil, huma.Error500InternalServerError("reading session log: " + err.Error())
		}

		turns := make([]outputTurn, 0, len(sess.Messages))
		for _, entry := range sess.Messages {
			turn := entryToTurn(entry)
			if turn.Text == "" {
				continue
			}
			turns = append(turns, turn)
		}
		resp := sessionTranscriptResponse{
			ID:         info.ID,
			Template:   info.Template,
			Format:     "conversation",
			Turns:      turns,
			Pagination: sess.Pagination,
		}
		raw, _ := json.Marshal(resp)
		return &IndexOutput[json.RawMessage]{
			Index: s.latestIndex(),
			Body:  raw,
		}, nil
	}

	if wantRaw {
		resp := sessionRawTranscriptResponse{
			ID:       info.ID,
			Template: info.Template,
			Format:   "raw",
			Messages: []json.RawMessage{},
		}
		raw, _ := json.Marshal(resp)
		return &IndexOutput[json.RawMessage]{
			Index: s.latestIndex(),
			Body:  raw,
		}, nil
	}

	if info.State == session.StateActive && s.state.SessionProvider().IsRunning(info.SessionName) {
		output, peekErr := s.state.SessionProvider().Peek(info.SessionName, 100)
		if peekErr != nil {
			return nil, huma.Error500InternalServerError(peekErr.Error())
		}
		turns := []outputTurn{}
		if output != "" {
			turns = append(turns, outputTurn{Role: "output", Text: output})
		}
		resp := sessionTranscriptResponse{
			ID:       info.ID,
			Template: info.Template,
			Format:   "text",
			Turns:    turns,
		}
		raw, _ := json.Marshal(resp)
		return &IndexOutput[json.RawMessage]{
			Index: s.latestIndex(),
			Body:  raw,
		}, nil
	}

	resp := sessionTranscriptResponse{
		ID:       info.ID,
		Template: info.Template,
		Format:   "conversation",
		Turns:    []outputTurn{},
	}
	raw, _ := json.Marshal(resp)
	return &IndexOutput[json.RawMessage]{
		Index: s.latestIndex(),
		Body:  raw,
	}, nil
}

// --- Session Pending ---

// humaHandleSessionPending is the Huma-typed handler for GET /v0/session/{id}/pending.
func (s *Server) humaHandleSessionPending(_ context.Context, input *SessionIDInput) (*IndexOutput[sessionPendingResponse], error) {
	store := s.state.CityBeadStore()
	if store == nil {
		return nil, huma.Error503ServiceUnavailable("no bead store configured")
	}

	id, err := s.resolveSessionIDWithConfig(store, input.ID)
	if err != nil {
		return nil, humaResolveError(err)
	}

	mgr := s.sessionManager(store)
	pending, supported, err := mgr.Pending(id)
	if err != nil {
		return nil, humaSessionManagerError(err)
	}
	return &IndexOutput[sessionPendingResponse]{
		Index: s.latestIndex(),
		Body: sessionPendingResponse{
			Supported: supported,
			Pending:   pending,
		},
	}, nil
}

// --- Session Patch ---

// humaHandleSessionPatch is the Huma-typed handler for PATCH /v0/session/{id}.
func (s *Server) humaHandleSessionPatch(_ context.Context, input *SessionPatchInput) (*IndexOutput[sessionResponse], error) {
	store := s.state.CityBeadStore()
	if store == nil {
		return nil, huma.Error503ServiceUnavailable("no bead store configured")
	}

	id, err := s.resolveSessionIDWithConfig(store, input.ID)
	if err != nil {
		return nil, humaResolveError(err)
	}

	// Validate fields: only title and alias are mutable.
	if input.Body.Title == nil && input.Body.Alias == nil {
		return nil, huma.Error400BadRequest("at least one of 'title' or 'alias' is required")
	}
	if input.Body.Title != nil && *input.Body.Title == "" {
		return nil, huma.Error400BadRequest("title must be a non-empty string")
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
		return mgr.UpdatePresentation(id, input.Body.Title, input.Body.Alias)
	}
	if input.Body.Alias != nil {
		if strings.TrimSpace(b.Metadata["agent_name"]) != "" {
			return nil, huma.Error403Forbidden("alias is controller-managed for this session")
		}
		if lockErr := session.WithCitySessionAliasLock(s.state.CityPath(), *input.Body.Alias, func() error {
			if avErr := session.EnsureAliasAvailableWithConfig(store, s.state.Config(), *input.Body.Alias, id); avErr != nil {
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

	if strings.TrimSpace(input.Body.Message) == "" {
		return nil, huma.Error400BadRequest("message is required")
	}
	intent := input.Body.Intent
	if intent == "" {
		intent = session.SubmitIntentDefault
	}
	switch intent {
	case session.SubmitIntentDefault, session.SubmitIntentFollowUp, session.SubmitIntentInterruptNow:
	default:
		return nil, huma.Error400BadRequest(fmt.Sprintf("intent must be one of %q, %q, or %q", session.SubmitIntentDefault, session.SubmitIntentFollowUp, session.SubmitIntentInterruptNow))
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

	if strings.TrimSpace(input.Body.Message) == "" {
		return nil, huma.Error400BadRequest("message is required")
	}

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

	if input.Body.Action == "" {
		return nil, huma.Error400BadRequest("action is required")
	}

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
func (s *Server) humaHandleSessionSuspend(_ context.Context, input *SessionIDInput) (*OKResponse, error) {
	store := s.state.CityBeadStore()
	if store == nil {
		return nil, huma.Error503ServiceUnavailable("no bead store configured")
	}
	mgr := s.sessionManager(store)

	id, err := s.resolveSessionIDWithConfig(store, input.ID)
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

	if b, getErr := store.Get(id); getErr == nil && strings.TrimSpace(b.Metadata[apiNamedSessionMetadataKey]) == "true" && strings.TrimSpace(b.Metadata[apiNamedSessionModeKey]) == "always" {
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
	if input.Delete == "true" {
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

	if input.Body.Title == "" {
		return nil, huma.Error400BadRequest("title is required")
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

// humaHandleSessionAgentList is the Huma-typed handler for GET /v0/session/{id}/agents.
func (s *Server) humaHandleSessionAgentList(_ context.Context, input *SessionIDInput) (*IndexOutput[json.RawMessage], error) {
	store := s.state.CityBeadStore()
	if store == nil {
		return nil, huma.Error503ServiceUnavailable("no bead store configured")
	}

	id, err := s.resolveSessionIDAllowClosedWithConfig(store, input.ID)
	if err != nil {
		return nil, humaResolveError(err)
	}

	mgr := s.sessionManager(store)
	logPath, err := mgr.TranscriptPath(id, s.sessionLogPaths())
	if err != nil {
		return nil, humaSessionManagerError(err)
	}
	if logPath == "" {
		raw, _ := json.Marshal(map[string]any{"agents": []any{}})
		return &IndexOutput[json.RawMessage]{
			Index: s.latestIndex(),
			Body:  raw,
		}, nil
	}

	mappings, err := sessionlog.FindAgentMappings(logPath)
	if err != nil {
		return nil, huma.Error500InternalServerError("failed to list agents")
	}
	if mappings == nil {
		mappings = []sessionlog.AgentMapping{}
	}
	raw, _ := json.Marshal(map[string]any{"agents": mappings})
	return &IndexOutput[json.RawMessage]{
		Index: s.latestIndex(),
		Body:  raw,
	}, nil
}

// --- Session Agent Get ---

// humaHandleSessionAgentGet is the Huma-typed handler for GET /v0/session/{id}/agents/{agentId}.
func (s *Server) humaHandleSessionAgentGet(_ context.Context, input *SessionAgentGetInput) (*IndexOutput[json.RawMessage], error) {
	store := s.state.CityBeadStore()
	if store == nil {
		return nil, huma.Error503ServiceUnavailable("no bead store configured")
	}

	id, err := s.resolveSessionIDAllowClosedWithConfig(store, input.ID)
	if err != nil {
		return nil, humaResolveError(err)
	}

	if input.AgentID == "" {
		return nil, huma.Error400BadRequest("agentId is required")
	}
	if err := sessionlog.ValidateAgentID(input.AgentID); err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}

	mgr := s.sessionManager(store)
	logPath, err := mgr.TranscriptPath(id, s.sessionLogPaths())
	if err != nil {
		return nil, humaSessionManagerError(err)
	}
	if logPath == "" {
		return nil, huma.Error404NotFound("no transcript found for session " + id)
	}

	agentSession, err := sessionlog.ReadAgentSession(logPath, input.AgentID)
	if err != nil {
		if errors.Is(err, sessionlog.ErrAgentNotFound) {
			return nil, huma.Error404NotFound("agent not found")
		}
		return nil, huma.Error500InternalServerError("failed to read agent transcript")
	}

	rawMessages := make([]json.RawMessage, 0, len(agentSession.Messages))
	for _, entry := range agentSession.Messages {
		if len(entry.Raw) > 0 {
			rawMessages = append(rawMessages, entry.Raw)
		}
	}

	raw, _ := json.Marshal(map[string]any{
		"messages": rawMessages,
		"status":   agentSession.Status,
	})
	return &IndexOutput[json.RawMessage]{
		Index: s.latestIndex(),
		Body:  raw,
	}, nil
}

// Keep unused import references for imports needed by specific code paths.
var _ = http.StatusCreated
