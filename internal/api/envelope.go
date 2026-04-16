package api

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// Error is the JSON error response body.
type Error struct {
	Code    string       `json:"code"`
	Message string       `json:"message"`
	Details []FieldError `json:"details,omitempty"`
}

// FieldError reports a validation error for a specific field.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// listResponse wraps a collection for JSON serialization.
type listResponse struct {
	Items      any    `json:"items"`
	Total      int    `json:"total"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// writeJSON writes v as JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck // best-effort
}

// writeError writes a structured error response.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, Error{Code: code, Message: msg})
}

// writeListJSON writes a list response with X-GC-Index header.
func writeListJSON(w http.ResponseWriter, index uint64, items any, total int) {
	w.Header().Set("X-GC-Index", strconv.FormatUint(index, 10))
	writeJSON(w, http.StatusOK, listResponse{Items: items, Total: total})
}

// latestIndex returns the latest event sequence, or 0 if unavailable.
func (s *Server) latestIndex() uint64 {
	ep := s.state.EventProvider()
	if ep == nil {
		return 0
	}
	seq, _ := ep.LatestSeq()
	return seq
}
