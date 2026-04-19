package api

import (
	"net/http"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

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

	id, err := s.resolveSessionIDMaterializingNamedWithContext(r.Context(), store, r.PathValue("id"))
	if err != nil {
		s.idem.unreserve(idemKey)
		writeResolveError(w, err)
		return
	}

	if err := s.sendUserMessageToSession(r.Context(), store, id, body.Message); err != nil {
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

	handle, err := s.workerHandleForSession(store, id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	if err := handle.Kill(r.Context()); err != nil {
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

	handle, err := s.workerHandleForSession(store, id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	if err := handle.Interrupt(r.Context(), worker.InterruptRequest{}); err != nil {
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

	handle, err := s.workerHandleForSession(store, id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	pending, supported, err := handle.PendingStatus(r.Context())
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	var pendingResp *runtime.PendingInteraction
	if pending != nil {
		pendingResp = &runtime.PendingInteraction{
			RequestID: pending.RequestID,
			Kind:      pending.Kind,
			Prompt:    pending.Prompt,
			Options:   append([]string(nil), pending.Options...),
			Metadata:  cloneStringMap(pending.Metadata),
		}
	}
	writeJSON(w, http.StatusOK, sessionPendingResponse{
		Supported: supported,
		Pending:   pendingResp,
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

	handle, err := s.workerHandleForSession(store, id)
	if err != nil {
		s.idem.unreserve(idemKey)
		writeSessionManagerError(w, err)
		return
	}
	if err := handle.Respond(r.Context(), worker.InteractionResponse{
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
