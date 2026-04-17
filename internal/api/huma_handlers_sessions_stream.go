package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/sse"
	"github.com/gastownhall/gascity/internal/session"
)

// SSE stream handlers for the session endpoint. resolveSessionStream picks
// the right transcript format and source; streamSession drives the actual
// per-request streaming loop.

func (s *Server) resolveSessionStream(input *SessionStreamInput) (*sessionStreamState, error) {
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

	sp := s.state.SessionProvider()
	running := info.State == session.StateActive && sp.IsRunning(info.SessionName)
	if path == "" && !running {
		return nil, huma.Error404NotFound("session " + id + " has no live output")
	}

	return &sessionStreamState{info: info, path: path, running: running}, nil
}

// checkSessionStream is the precheck for GET /v0/session/{id}/stream.

func (s *Server) checkSessionStream(_ context.Context, input *SessionStreamInput) error {
	_, err := s.resolveSessionStream(input)
	return err
}

// streamSession is the SSE streaming callback for GET /v0/session/{id}/stream.

func (s *Server) streamSession(hctx huma.Context, input *SessionStreamInput, send sse.Sender) {
	state, err := s.resolveSessionStream(input)
	if err != nil {
		// Should not happen — precheck already succeeded.
		return
	}
	info := state.info
	path := state.path
	running := state.running
	format := input.Format

	// Custom session state headers.
	if info.State != "" {
		hctx.SetHeader("GC-Session-State", string(info.State))
	}
	if !running {
		hctx.SetHeader("GC-Session-Status", "stopped")
	}

	reqCtx := hctx.Context()
	if info.Closed {
		if format == "raw" {
			s.emitClosedSessionSnapshotRaw(send, info, path)
		} else {
			s.emitClosedSessionSnapshot(send, info, path)
		}
		return
	}
	switch {
	case path != "":
		if format == "raw" {
			s.streamSessionTranscriptLogRaw(reqCtx, send, info, path)
		} else {
			s.streamSessionTranscriptLog(reqCtx, send, info, path)
		}
	case format == "raw":
		if running {
			s.streamSessionPeekRaw(reqCtx, send, info)
		} else {
			_ = send(sse.Message{ID: 1, Data: sessionRawTranscriptResponse{
				ID:       info.ID,
				Template: info.Template,
				Format:   "raw",
				Messages: []any{},
			}})
		}
	default:
		s.streamSessionPeek(reqCtx, send, info)
	}
}

// Keep unused import references for imports needed by specific code paths.
var _ = http.StatusCreated
