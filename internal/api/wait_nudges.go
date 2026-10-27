package api

import (
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/opjournal"
)

// waitNoticePrincipal is the principal of a wait's notice on the
// conversation lane (cmd/gc's wait_lane_notice.go), which withdraws it.
const waitNoticePrincipal = "gc:wait"

// withdrawQueuedWaitNudges withdraws the wait nudges ids: an id that is an op
// id (opjournal.IsOpID) is a wait's lane notice, canceled in the op journal
// as gc:wait; any other id goes through the city's nudge queue, which closes
// its shadow bead in the nudges-class store.
func (s *Server) withdrawQueuedWaitNudges(ids []string) error {
	var ops, queued []string
	for _, id := range ids {
		if opjournal.IsOpID(id) {
			ops = append(ops, id)
		} else {
			queued = append(queued, id)
		}
	}
	err := s.state.NudgeQueue().WithdrawQueuedWaitNudges(queued)
	if len(ops) == 0 {
		return err
	}
	st, openErr := s.conversationOpStore()
	if openErr != nil {
		return errors.Join(err, openErr)
	}
	errs := []error{err}
	for _, id := range ops {
		if _, _, cerr := st.Cancel(id, waitNoticePrincipal, "its wait was withdrawn"); cerr != nil && !errors.Is(cerr, beads.ErrNotFound) {
			errs = append(errs, fmt.Errorf("withdrawing wait notice %s: %w", id, cerr))
		}
	}
	return errors.Join(errs...)
}
