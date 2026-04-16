package api

import (
	"net/http"
)

func (s *Server) handleEventStream(w http.ResponseWriter, r *http.Request) {
	ep := s.state.EventProvider()
	if ep == nil {
		writeError(w, http.StatusServiceUnavailable, "internal", "events not enabled")
		return
	}

	afterSeq := parseAfterSeq(r)

	// Create watcher before committing 200 — allows returning 503 on failure.
	watcher, err := ep.Watch(r.Context(), afterSeq)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "internal", "failed to start event watcher: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	// Use ResponseController to flush through wrapped writers (e.g., logging middleware).
	if err := http.NewResponseController(w).Flush(); err != nil {
		_ = err // Flushing not supported; best-effort.
	}

	streamProjectedEventsWithWatcher(r.Context(), w, watcher, s.state)
}
