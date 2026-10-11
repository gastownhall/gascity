package api

// PendingInteractionPoker is the optional State extension through which the
// API asks the controller to re-probe a session's pending interaction at once.
// The controller detects pending interactions and records session.pending /
// session.pending_cleared on the city event log (cmd/gc
// pending_interaction_lane.go, internal/pendingwatch); the API only reads and
// streams that log. A successful POST .../respond pokes it so the clear does
// not wait for the controller's cadence.
type PendingInteractionPoker interface {
	PokePendingInteractions(sessionID string)
}

// pokePendingInteractions pokes the controller's pending-interaction
// detection for sessionID, when the State offers it.
func (s *Server) pokePendingInteractions(sessionID string) {
	if p, ok := s.state.(PendingInteractionPoker); ok {
		p.PokePendingInteractions(sessionID)
	}
}
