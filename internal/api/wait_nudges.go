package api

// withdrawQueuedWaitNudges withdraws the queued wait nudges ids through the
// city's nudge queue, which closes their shadow beads in the nudges-class store.
func (s *Server) withdrawQueuedWaitNudges(ids []string) error {
	return s.state.NudgeQueue().WithdrawQueuedWaitNudges(ids)
}
