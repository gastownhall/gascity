package api

// listResponse wraps a collection for JSON serialization. Used by the few
// remaining non-Huma handlers (supervisor bare listings, legacy test
// fixtures) and by the agents-cache envelope. Huma handlers use
// ListOutput[T] / ListBody[T] instead.
type listResponse struct {
	Items         any      `json:"items"`
	Total         int      `json:"total"`
	NextCursor    string   `json:"next_cursor,omitempty"`
	Partial       bool     `json:"partial,omitempty"`
	PartialErrors []string `json:"partial_errors,omitempty"`
}

// latestIndex returns the latest event sequence, or 0 if unavailable.
// Used by every Huma handler that emits an IndexOutput / ListOutput to
// populate the X-GC-Index response header.
func (s *Server) latestIndex() uint64 {
	ep := s.state.EventProvider()
	if ep == nil {
		return 0
	}
	seq, _ := ep.LatestSeq()
	return seq
}
