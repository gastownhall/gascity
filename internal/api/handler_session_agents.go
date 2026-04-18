package api

import (
	"errors"
	"net/http"

	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/worker"
)

// handleSessionAgentList returns subagent mappings for a session.
//
//	GET /v0/session/{id}/agents
//	Response: { "agents": [{ "agent_id": "...", "parent_tool_use_id": "..." }] }
func (s *Server) handleSessionAgentList(w http.ResponseWriter, r *http.Request) {
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

	handle, err := s.workerHandleForSession(store, id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	mappings, err := handle.AgentMappings(r.Context())
	if err != nil {
		if errors.Is(err, worker.ErrHistoryUnavailable) {
			writeJSON(w, http.StatusOK, map[string]any{"agents": []any{}})
			return
		}
		writeSessionManagerError(w, err)
		return
	}
	if mappings == nil {
		mappings = []sessionlog.AgentMapping{}
	}
	if len(mappings) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"agents": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": mappings})
}

// handleSessionAgentGet returns the transcript and status of a subagent.
//
//	GET /v0/session/{id}/agents/{agentId}
//	Response: { "messages": [...], "status": "completed|running|pending|failed" }
func (s *Server) handleSessionAgentGet(w http.ResponseWriter, r *http.Request) {
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

	agentID := r.PathValue("agentId")
	if agentID == "" {
		writeError(w, http.StatusBadRequest, "invalid", "agentId is required")
		return
	}

	if err := sessionlog.ValidateAgentID(agentID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	handle, err := s.workerHandleForSession(store, id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	agentSession, err := handle.AgentTranscript(r.Context(), agentID)
	if err != nil {
		if errors.Is(err, worker.ErrHistoryUnavailable) {
			writeError(w, http.StatusNotFound, "not_found", "no transcript found for session "+id)
			return
		}
		if errors.Is(err, sessionlog.ErrAgentNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "agent not found")
		} else {
			writeError(w, http.StatusInternalServerError, "internal", "failed to read agent transcript")
		}
		return
	}

	// Build raw message array for API pass-through (same as raw transcript).
	writeJSON(w, http.StatusOK, map[string]any{
		"messages": agentSession.RawMessages,
		"status":   agentSession.Session.Status,
	})
}
