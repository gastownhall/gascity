package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

type sessionTranscriptResponse struct {
	ID         string                       `json:"id"`
	Template   string                       `json:"template"`
	Format     string                       `json:"format"`
	Turns      []outputTurn                 `json:"turns"`
	Pagination *worker.TranscriptPagination `json:"pagination,omitempty"`
}

type sessionRawTranscriptResponse struct {
	ID         string                       `json:"id"`
	Template   string                       `json:"template"`
	Format     string                       `json:"format"`
	Messages   []SessionRawMessageFrame     `json:"messages"`
	Pagination *worker.TranscriptPagination `json:"pagination,omitempty"`
}

func (s *Server) handleSessionTranscript(w http.ResponseWriter, r *http.Request) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	id, err := s.resolveSessionIDAllowClosedWithConfig(store.Store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}

	catalog, err := s.workerSessionCatalog(store.Store)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	info, err := catalog.Get(id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	handle, err := s.workerHandleForSession(store.Store, id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	path, err := handle.TranscriptPath(r.Context())
	if err != nil && !errors.Is(err, worker.ErrHistoryUnavailable) {
		writeSessionManagerError(w, err)
		return
	}

	format := r.URL.Query().Get("format")
	wantRaw := format == "raw"
	wantStructured := format == "structured"
	includeThinking := wantStructured && queryBoolParam(r, "include_thinking")
	before := strings.TrimSpace(r.URL.Query().Get("before"))
	after := strings.TrimSpace(r.URL.Query().Get("after"))
	if before != "" && after != "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_params", "before and after are mutually exclusive")
		return
	}
	if path == "" {
		if cursorErr := transcriptCursorAbsentError(before, after); cursorErr != nil {
			writeTranscriptReadError(w, cursorErr, "reading session log")
			return
		}
	}

	if path != "" {
		tail := 0
		if v := r.URL.Query().Get("tail"); v != "" {
			if n, convErr := strconv.Atoi(v); convErr == nil && n >= 0 {
				tail = n
			}
		}
		if wantStructured {
			history, historyErr := handle.History(worker.WithoutOperationEvents(r.Context()), worker.HistoryRequest{
				TailCompactions: tail,
				BeforeEntryID:   before,
				AfterEntryID:    after,
			})
			if historyErr != nil {
				if errors.Is(historyErr, worker.ErrHistoryUnavailable) {
					writeJSON(w, http.StatusOK, legacyStructuredFallbackTranscriptResponse(r.Context(), info, handle, includeThinking))
					return
				}
				writeTranscriptReadError(w, historyErr, "reading session history")
				return
			}
			messages, _ := historySnapshotStructuredMessages(history, includeThinking)
			projection := structuredSnapshotProjection(SessionStreamStructuredMessageEvent{
				ID:                 info.ID,
				Template:           info.Template,
				Provider:           info.Provider,
				Format:             "structured",
				SchemaVersion:      sessionStructuredSchemaVersion,
				History:            structuredHistoryFromSnapshot(history),
				StructuredMessages: messages,
				Pagination:         history.Pagination,
			}, includeThinking)
			writeJSON(w, http.StatusOK, structuredTranscriptResponseFromEvent(projection))
			return
		}

		if wantRaw {
			transcript, err := handle.Transcript(r.Context(), worker.TranscriptRequest{
				TailCompactions: tail,
				BeforeEntryID:   before,
				AfterEntryID:    after,
				Raw:             true,
			})
			if err != nil {
				writeTranscriptReadError(w, err, "reading session log")
				return
			}
			writeJSON(w, http.StatusOK, sessionRawTranscriptResponse{
				ID:         info.ID,
				Template:   info.Template,
				Format:     "raw",
				Messages:   wrapRawFrameBytes(transcript.RawMessages),
				Pagination: transcript.Session.Pagination,
			})
			return
		}

		transcript, err := handle.Transcript(r.Context(), worker.TranscriptRequest{
			TailCompactions: tail,
			BeforeEntryID:   before,
			AfterEntryID:    after,
		})
		if err != nil {
			writeTranscriptReadError(w, err, "reading session log")
			return
		}
		sess := transcript.Session

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
			Messages: []SessionRawMessageFrame{},
		})
		return
	}

	if wantStructured {
		writeJSON(w, http.StatusOK, legacyStructuredFallbackTranscriptResponse(r.Context(), info, handle, includeThinking))
		return
	}

	output, peekErr := handle.Peek(r.Context(), 100)
	if peekErr != nil && !errors.Is(peekErr, session.ErrSessionInactive) {
		writeError(w, http.StatusInternalServerError, "internal", peekErr.Error())
		return
	}
	if peekErr == nil {
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

func transcriptCursorInvalidatedProblem(err error, action string) *apierr.ErrorModel {
	if !errors.Is(err, worker.ErrTranscriptCursorNotFound) && !errors.Is(err, worker.ErrTranscriptDuplicateEntryID) {
		return nil
	}
	return apierr.TranscriptCursorInvalidated.Msg(action + ": " + err.Error())
}

func transcriptCursorAbsentError(before, after string) error {
	if before != "" {
		return &worker.TranscriptCursorNotFoundError{
			Direction: worker.TranscriptCursorDirectionBefore,
			EntryID:   before,
		}
	}
	if after != "" {
		return &worker.TranscriptCursorNotFoundError{
			Direction: worker.TranscriptCursorDirectionAfter,
			EntryID:   after,
		}
	}
	return nil
}

func writeTranscriptReadError(w http.ResponseWriter, err error, action string) {
	if problem := transcriptCursorInvalidatedProblem(err, action); problem != nil {
		writeJSONWithType(w, problem.Status, "application/problem+json", problem)
		return
	}
	writeError(w, http.StatusInternalServerError, "internal", action+": "+err.Error())
}

func legacyStructuredFallbackTranscriptResponse(ctx context.Context, info session.Info, handle worker.PeekHandle, includeThinking bool) sessionTranscriptGetResponse {
	activity := string(worker.TailActivityIdle)
	output := ""
	peekOutput, peekErr := handle.Peek(ctx, 100)
	if peekErr == nil {
		activity = string(worker.TailActivityInTurn)
		output = peekOutput
	}
	projection := structuredSnapshotProjection(SessionStreamStructuredMessageEvent{
		ID:                 info.ID,
		Template:           info.Template,
		Provider:           info.Provider,
		Format:             "structured",
		SchemaVersion:      sessionStructuredSchemaVersion,
		History:            structuredFallbackHistory(info.ID, info.SessionKey, activity),
		StructuredMessages: structuredFallbackMessages(info.ID, info.Provider, output),
	}, includeThinking)
	return structuredTranscriptResponseFromEvent(projection)
}

func queryBoolParam(r *http.Request, name string) bool {
	value := strings.ToLower(strings.TrimSpace(r.URL.Query().Get(name)))
	return value == "1" || value == "true" || value == "yes" || value == "on"
}
