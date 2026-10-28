package api

import (
	"errors"
	"fmt"
	"log"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/opjournal"
)

// waitNoticePrincipal is the principal of a wait's notice on the
// conversation lane (cmd/gc's wait_lane_notice.go), which withdraws it.
const waitNoticePrincipal = "gc:wait"

// withdrawQueuedWaitNudges withdraws the wait nudges ids: an id that is an op
// id (opjournal.IsOpID) is a wait's lane notice, canceled in the op journal
// as gc:wait; any other id goes through the city's nudge queue, which closes
// its shadow bead in the nudges-class store. A notice already being
// delivered cannot be stopped: its cancel is recorded
// (opjournal.CancelRequested), the lane's outcome says whether the delivery
// landed, and the withdrawal logs that. A notice this build cannot read is
// left as it is, and logged: the lane quarantines such a row and never
// delivers it, so there is nothing to withdraw.
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
		result, _, cerr := st.Cancel(id, waitNoticePrincipal, "its wait was withdrawn")
		switch {
		case opjournal.Undecodable(cerr):
			log.Printf("gc api: wait notice %s is unreadable, so it is not withdrawn; the lane never delivers a notice it cannot read: %v", id, cerr)
		case cerr != nil && !errors.Is(cerr, beads.ErrNotFound):
			errs = append(errs, fmt.Errorf("withdrawing wait notice %s: %w", id, cerr))
		case result == opjournal.CancelRequested:
			log.Printf("gc api: wait notice %s is being delivered; its withdrawal is recorded, and the lane's outcome says whether the delivery landed", id)
		}
	}
	return errors.Join(errs...)
}
