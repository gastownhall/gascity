package api

// latestIndex returns the latest event sequence, or 0 if unavailable.
// Used by every Huma handler that emits an IndexOutput / ListOutput to
// populate the X-GC-Index response header. Capture it before reading the
// state the response reports (after any blocking wait): a client resumes
// events or blocks after X-GC-Index, so an index read after the snapshot can
// cover a concurrent change the body lacks, and that change is never seen.
// Capturing first can only replay an event the body already reflects, for
// changes made through this server. Another process appends its events to the
// shared log before the bead-event watcher applies them to this server's
// stores, so in that window the head can cover a change the body lacks
// (ga-nahrxi).
func (s *Server) latestIndex() uint64 {
	ep := s.state.EventProvider()
	if ep == nil {
		return 0
	}
	seq, _ := ep.LatestSeq()
	return seq
}
