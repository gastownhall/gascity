package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
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

	id, err := session.ResolveSessionIDAllowClosed(store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}

	mgr := s.sessionManager(store)
	logPath, err := mgr.TranscriptPath(id, s.sessionLogPaths())
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	if logPath == "" {
		writeJSON(w, http.StatusOK, map[string]any{"agents": []any{}})
		return
	}

	mappings, err := sessionlog.FindAgentMappings(logPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to list agents")
		return
	}
	if mappings == nil {
		mappings = []sessionlog.AgentMapping{}
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

	id, err := session.ResolveSessionIDAllowClosed(store, r.PathValue("id"))
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

	mgr := s.sessionManager(store)
	logPath, err := mgr.TranscriptPath(id, s.sessionLogPaths())
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	if logPath == "" {
		writeError(w, http.StatusNotFound, "not_found", "no transcript found for session "+id)
		return
	}

	agentSession, err := sessionlog.ReadAgentSession(logPath, agentID)
	if err != nil {
		if errors.Is(err, sessionlog.ErrAgentNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "agent not found")
		} else {
			writeError(w, http.StatusInternalServerError, "internal", "failed to read agent transcript")
		}
		return
	}

	// Build raw message array for API pass-through (same as raw transcript).
	rawMessages := make([]json.RawMessage, 0, len(agentSession.Messages))
	for _, entry := range agentSession.Messages {
		if len(entry.Raw) > 0 {
			rawMessages = append(rawMessages, entry.Raw)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"messages": rawMessages,
		"status":   agentSession.Status,
	})
}
