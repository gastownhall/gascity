package api

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// Query-side session handlers (list, get, transcript, pending, agent-list,
// agent-get). Split out of huma_handlers_sessions.go to isolate read-side
// logic from mutations and streaming.

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

	pp := pageParams{
		Offset:   decodeCursor(input.Cursor),
		Limit:    limit,
		IsPaging: input.cursorPresent,
	}

	if !pp.IsPaging {
		// No pagination cursor — just cap at limit.
		if pp.Limit < len(items) {
			items = items[:pp.Limit]
		}
		if items == nil {
			items = []sessionResponse{}
		}
		return &ListOutput[sessionResponse]{
			Body: ListBody[sessionResponse]{Items: items, Total: len(items)},
		}, nil
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

func (s *Server) humaHandleSessionTranscript(_ context.Context, input *SessionTranscriptInput) (*IndexOutput[sessionTranscriptGetResponse], error) {
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
			return &IndexOutput[sessionTranscriptGetResponse]{
				Index: s.latestIndex(),
				Body: sessionTranscriptGetResponse{
					ID:         info.ID,
					Template:   info.Template,
					Format:     "raw",
					Messages:   rawSess.RawPayloads(),
					Pagination: rawSess.Pagination,
				},
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
		return &IndexOutput[sessionTranscriptGetResponse]{
			Index: s.latestIndex(),
			Body: sessionTranscriptGetResponse{
				ID:         info.ID,
				Template:   info.Template,
				Format:     "conversation",
				Turns:      turns,
				Pagination: sess.Pagination,
			},
		}, nil
	}

	if wantRaw {
		return &IndexOutput[sessionTranscriptGetResponse]{
			Index: s.latestIndex(),
			Body: sessionTranscriptGetResponse{
				ID:       info.ID,
				Template: info.Template,
				Format:   "raw",
				Messages: []any{},
			},
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
		return &IndexOutput[sessionTranscriptGetResponse]{
			Index: s.latestIndex(),
			Body: sessionTranscriptGetResponse{
				ID:       info.ID,
				Template: info.Template,
				Format:   "text",
				Turns:    turns,
			},
		}, nil
	}

	return &IndexOutput[sessionTranscriptGetResponse]{
		Index: s.latestIndex(),
		Body: sessionTranscriptGetResponse{
			ID:       info.ID,
			Template: info.Template,
			Format:   "conversation",
			Turns:    []outputTurn{},
		},
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

func (s *Server) humaHandleSessionAgentList(_ context.Context, input *SessionIDInput) (*IndexOutput[sessionAgentListResponse], error) {
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
		return &IndexOutput[sessionAgentListResponse]{
			Index: s.latestIndex(),
			Body:  sessionAgentListResponse{Agents: []sessionlog.AgentMapping{}},
		}, nil
	}

	mappings, err := sessionlog.FindAgentMappings(logPath)
	if err != nil {
		return nil, huma.Error500InternalServerError("failed to list agents")
	}
	if mappings == nil {
		mappings = []sessionlog.AgentMapping{}
	}
	return &IndexOutput[sessionAgentListResponse]{
		Index: s.latestIndex(),
		Body:  sessionAgentListResponse{Agents: mappings},
	}, nil
}

// --- Session Agent Get ---

// humaHandleSessionAgentGet is the Huma-typed handler for GET /v0/session/{id}/agents/{agentId}.

func (s *Server) humaHandleSessionAgentGet(_ context.Context, input *SessionAgentGetInput) (*IndexOutput[sessionAgentGetResponse], error) {
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

	return &IndexOutput[sessionAgentGetResponse]{
		Index: s.latestIndex(),
		Body: sessionAgentGetResponse{
			Messages: agentSession.RawPayloads(),
			Status:   agentSession.Status,
		},
	}, nil
}

// --- Session Stream (SSE) ---

// sessionStreamState holds the state resolved by checkSessionStream that
// streamSession needs. It's not passed through registerSSE; instead both
// functions re-resolve from the input, which is cheap (map lookups).
type sessionStreamState struct {
	info    session.Info
	path    string
	running bool
}

// resolveSessionStream is the shared resolution logic used by both the
// precheck and the stream callback. It returns the resolved state or an
// error suitable for HTTP response.
