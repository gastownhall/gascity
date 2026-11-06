package main

import (
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// queueStartupNudgeHandback takes the startup nudge the leaf serving session
// info's runtime name handed back after its Start or Relaunch
// (runtime.TakeStartupNudgeHandback) and, when that start succeeded
// (started), queues it: under program_status = "on", a nudge the session's
// program held back behind a dialog through the whole startup ladder is never
// typed and never fails the start (design §10.2 row A20). It waits in the
// session's queue as a user's submit (nudgequeue.SourceSubmit), stamped with
// the started row's continuation, and is delivered verbatim after the dialog
// is answered. A start that failed has its handback taken and dropped, and a
// leaf that handed nothing back queues nothing.
func queueStartupNudgeHandback(cityPath string, cfg *config.City, leaf runtime.Provider, info session.Info, name string, started bool, now time.Time) error {
	nudge, ok := runtime.TakeStartupNudgeHandback(leaf, name)
	if !ok || !started {
		return nil
	}
	if cfg == nil {
		// A pass that carries no city config resolves no agent: the item
		// is keyed by the session row alone.
		cfg = &config.City{}
	}
	target := resolveNudgeTargetFromSessionInfo(cityPath, cfg, info)
	item := newQueuedNudgeWithOptions(target.agentKey(), nudge, nudgequeue.SourceSubmit, now, queuedNudgeOptionsFromTarget(target))
	if err := openNudgeQueue(cityPath, beads.NudgesStore{}).Queue.EnqueueDeferred(item); err != nil {
		return fmt.Errorf("queueing session %s's startup nudge behind its program's dialog: %w", info.ID, err)
	}
	pingNudgeWakeSocket(cityPath)
	return nil
}
