package api

import (
	"net/http"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// eventEmitRequest is the JSON body for POST /v0/events.
type eventEmitRequest struct {
	Type    string `json:"type"`
	Actor   string `json:"actor"`
	Subject string `json:"subject,omitempty"`
	Message string `json:"message,omitempty"`
}

func (s *Server) handleEventEmit(w http.ResponseWriter, r *http.Request) {
	ep := s.state.EventProvider()
	if ep == nil {
		writeError(w, http.StatusServiceUnavailable, "internal", "events not enabled")
		return
	}

	var body eventEmitRequest
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	if body.Type == "" {
		writeError(w, http.StatusBadRequest, "invalid", "type is required")
		return
	}
	if body.Actor == "" {
		writeError(w, http.StatusBadRequest, "invalid", "actor is required")
		return
	}

	ep.Record(events.Event{
		Type:    body.Type,
		Actor:   body.Actor,
		Subject: body.Subject,
		Message: body.Message,
	})
	writeJSON(w, http.StatusCreated, map[string]string{"status": "recorded"})
}

func (s *Server) handleEventList(w http.ResponseWriter, r *http.Request) {
	bp := parseBlockingParams(r)
	if bp.isBlocking() {
		waitForChange(r.Context(), s.state.EventProvider(), bp)
	}

	ep := s.state.EventProvider()
	if ep == nil {
		writeListJSON(w, 0, []any{}, 0)
		return
	}

	q := r.URL.Query()
	filter := events.Filter{
		Type:  q.Get("type"),
		Actor: q.Get("actor"),
	}
	if v := q.Get("since"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			filter.Since = time.Now().Add(-d)
		}
	}

	evts, err := ep.List(filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if evts == nil {
		evts = []events.Event{}
	}
	pp := parsePagination(r, 100)
	if !pp.IsPaging {
		if pp.Limit < len(evts) {
			evts = evts[:pp.Limit]
		}
		writeListJSON(w, s.latestIndex(), evts, len(evts))
		return
	}
	page, total, nextCursor := paginate(evts, pp)
	if page == nil {
		page = []events.Event{}
	}
	writePagedJSON(w, s.latestIndex(), page, total, nextCursor)
}

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
