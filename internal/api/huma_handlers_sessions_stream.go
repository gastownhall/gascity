package api

import (
	"context"
	"errors"
	"log"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/sse"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// SSE stream handlers for the session endpoint. resolveSessionStream picks
// the right transcript format and source; streamSession drives the actual
// per-request streaming loop.

func (s *Server) resolveSessionStream(ctx context.Context, input *SessionStreamInput) (*sessionStreamState, error) {
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
	handle, err := s.workerHandleForSession(store, id)
	if err != nil {
		return nil, humaSessionManagerError(err)
	}

	historyReq := worker.HistoryRequest{}
	if input.Format == "raw" && !info.Closed {
		historyReq.TailCompactions = 1
	}
	history, historyErr := handle.History(worker.WithoutOperationEvents(ctx), historyReq)
	hasHistory := historyErr == nil && history != nil
	if historyErr != nil && !errors.Is(historyErr, worker.ErrHistoryUnavailable) {
		return nil, huma.Error500InternalServerError("reading session history: " + historyErr.Error())
	}

	state, stateErr := handle.State(ctx)
	if stateErr != nil {
		return nil, humaSessionManagerError(stateErr)
	}
	running := workerPhaseHasLiveOutput(state.Phase)
	if !hasHistory && !running {
		return nil, huma.Error404NotFound("session " + id + " has no live output")
	}

	return &sessionStreamState{
		info:       info,
		handle:     handle,
		history:    history,
		historyReq: historyReq,
		hasHistory: hasHistory,
		running:    running,
	}, nil
}

// checkSessionStream is the precheck for GET /v0/session/{id}/stream.

func (s *Server) checkSessionStream(ctx context.Context, input *SessionStreamInput) error {
	state, err := s.resolveSessionStream(ctx, input)
	if err != nil {
		return err
	}
	input.resolved = state
	return nil
}

// streamSession is the SSE streaming callback for GET /v0/session/{id}/stream.

func (s *Server) streamSession(hctx huma.Context, input *SessionStreamInput, send sse.Sender) {
	reqCtx := hctx.Context()
	state := input.resolved
	if state == nil {
		var err error
		state, err = s.resolveSessionStream(reqCtx, input)
		if err != nil {
			// Invariant violation: precheck passed, body resolve failed.
			// Session vanished between precheck and streaming start, or a
			// race we didn't anticipate. Headers are already committed so
			// we can't return an HTTP error — log so the next debugger has
			// a starting point instead of a mute disconnect.
			log.Printf("api: session-stream: resolve failed after precheck city=%s id=%s: %v",
				input.CityName, input.ID, err)
			return
		}
	}
	info := state.info
	handle := state.handle
	history := state.history
	historyReq := state.historyReq
	hasHistory := state.hasHistory
	running := state.running
	format := input.Format

	// Custom session state headers.
	if info.State != "" {
		hctx.SetHeader("GC-Session-State", string(info.State))
	}
	if !running {
		hctx.SetHeader("GC-Session-Status", "stopped")
	}

	if info.Closed {
		if format == "raw" {
			s.emitClosedSessionSnapshotRawHuma(send, info, history)
		} else {
			s.emitClosedSessionSnapshotHuma(send, info, history)
		}
		return
	}
	if format == "raw" {
		_ = send(sse.Message{ID: 0, Data: SessionStreamRawMessageEvent{
			ID:       info.ID,
			Template: info.Template,
			Provider: info.Provider,
			Format:   "raw",
			Messages: []SessionRawMessageFrame{},
		}})
	}
	switch {
	case hasHistory:
		if format == "raw" {
			s.streamSessionTranscriptLogRawHuma(reqCtx, send, info, handle, history, historyReq)
		} else {
			s.streamSessionTranscriptLogHuma(reqCtx, send, info, handle, history)
		}
	case format == "raw":
		if running {
			s.streamSessionPeekRawHuma(reqCtx, send, info)
		} else {
			_ = send(sse.Message{ID: 1, Data: SessionStreamRawMessageEvent{
				ID:       info.ID,
				Template: info.Template,
				Provider: info.Provider,
				Format:   "raw",
				Messages: []SessionRawMessageFrame{},
			}})
		}
	default:
		s.streamSessionPeekHuma(reqCtx, send, info)
	}
}

func (s *Server) emitClosedSessionSnapshotHuma(send sse.Sender, info session.Info, history *worker.HistorySnapshot) {
	if history == nil {
		return
	}
	turns, _ := historySnapshotTurns(history)
	if len(turns) == 0 {
		return
	}

	_ = send(sse.Message{ID: 1, Data: SessionStreamMessageEvent{
		ID:       info.ID,
		Template: info.Template,
		Provider: info.Provider,
		Format:   "conversation",
		Turns:    turns,
	}})
	_ = send(sse.Message{ID: 2, Data: SessionActivityEvent{Activity: "idle"}})
}

func (s *Server) emitClosedSessionSnapshotRawHuma(send sse.Sender, info session.Info, history *worker.HistorySnapshot) {
	if history == nil {
		return
	}
	rawMessages, _ := historySnapshotRawMessages(history)
	if len(rawMessages) == 0 {
		return
	}

	_ = send(sse.Message{ID: 1, Data: SessionStreamRawMessageEvent{
		ID:       info.ID,
		Template: info.Template,
		Provider: info.Provider,
		Format:   "raw",
		Messages: wrapRawFrameBytes(rawMessages),
	}})
	_ = send(sse.Message{ID: 2, Data: SessionActivityEvent{Activity: "idle"}})
}
