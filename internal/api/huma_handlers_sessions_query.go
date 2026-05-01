package api

import (
	"context"
	"errors"
	"log"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/worker"
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

	all, partialErrors, err := sessionReadModelRows(store)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	listResult := mgr.ListFullFromBeads(all, input.State, input.Template)
	sessions := listResult.Sessions

	// Build bead index for reason enrichment.
	beadIndex := make(map[string]*beads.Bead)
	for i := range listResult.Beads {
		beadIndex[listResult.Beads[i].ID] = &listResult.Beads[i]
	}

	wantPeek := input.Peek
	hasDeferredQueue := strings.TrimSpace(s.state.CityPath()) != ""
	items := make([]sessionResponse, len(sessions))
	for i, sess := range sessions {
		items[i] = sessionResponseWithReason(sess, beadIndex[sess.ID], cfg, hasDeferredQueue)
		s.enrichSessionResponse(&items[i], sess, cfg, s.runtimeSessionResponseHandle(sess), wantPeek, false, false)
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
		// No pagination cursor — capture the full match count BEFORE truncating
		// so clients can tell how many items exist vs. how many fit the page.
		total := len(items)
		if pp.Limit < len(items) {
			items = items[:pp.Limit]
		}
		return &ListOutput[sessionResponse]{
			Index: s.latestIndex(),
			Body: ListBody[sessionResponse]{
				Items:         items,
				Total:         total,
				Partial:       len(partialErrors) > 0,
				PartialErrors: partialErrors,
			},
		}, nil
	}

	page, total, nextCursor := paginate(items, pp)
	if page == nil {
		page = []sessionResponse{}
	}
	return &ListOutput[sessionResponse]{
		Index: s.latestIndex(),
		Body: ListBody[sessionResponse]{
			Items:         page,
			Total:         total,
			NextCursor:    nextCursor,
			Partial:       len(partialErrors) > 0,
			PartialErrors: partialErrors,
		},
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
	wantPeek := input.Peek
	resp := sessionResponseWithReason(info, &b, cfg, strings.TrimSpace(s.state.CityPath()) != "")
	s.enrichSessionResponse(&resp, info, cfg, sp, wantPeek, true, true)
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
		// Compactions() returns (n, provided). When the client omitted
		// ?tail the transcript endpoint has historically returned all
		// entries, so default to 0 (sessionlog's "no pagination"
		// sentinel) rather than 1 compaction.
		tail, _ := input.Compactions()
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
					Provider:   info.Provider,
					Format:     "raw",
					Messages:   wrapRawFrameBytes(rawSess.RawPayloadBytes()),
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
				Provider:   info.Provider,
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
				Provider: info.Provider,
				Format:   "raw",
				Messages: []SessionRawMessageFrame{},
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
				Provider: info.Provider,
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
			Provider: info.Provider,
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
		log.Printf("gc api: session %s agent mapping failed for %s: %v", id, logPath, err)
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
// streamSession needs. The Huma input caches it per request so the stream
// body can reuse the initial History/State resolution instead of reloading
// the transcript before the first byte is written.
type sessionStreamState struct {
	info       session.Info
	handle     worker.Handle
	history    *worker.HistorySnapshot
	historyReq worker.HistoryRequest
	hasHistory bool
	running    bool
}

// resolveSessionStream is the shared resolution logic used by both the
// precheck and the stream callback. It returns the resolved state or an
// error suitable for HTTP response.
