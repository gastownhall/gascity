package api

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// withdrawQueuedWaitNudges withdraws the queued wait-nudge shadow beads with the
// given ids. It takes the strongly-typed beads.NudgesStore so the nudges class is
// statically enforced at the call site; the embedded .Store is passed to the
// class-agnostic nudgequeue helper.
func withdrawQueuedWaitNudges(store beads.NudgesStore, cityPath string, ids []string) error {
	return nudgequeue.WithdrawWaitNudges(store.Store, cityPath, ids)
}
