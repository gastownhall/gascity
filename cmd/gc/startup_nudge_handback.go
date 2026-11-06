package main

import (
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// startupNudgeHandbackSource is the queued-nudge source of a startup nudge a
// start handed back.
const startupNudgeHandbackSource = "startup"

// queueStartupNudgeHandback queues the startup nudge the leaf serving
// session info's runtime name handed back after its Start
// (runtime.TakeStartupNudgeHandback): under program_status = "on", a nudge
// the session's program held back behind a dialog through the whole startup
// ladder is never typed and never fails the start (design §10.2 row A20); it
// waits in the session's nudge queue, delivered after the dialog is
// answered. It does nothing when the leaf handed nothing back.
func queueStartupNudgeHandback(cityPath string, cfg *config.City, leaf runtime.Provider, info session.Info, name string, now time.Time) error {
	nudge, ok := runtime.TakeStartupNudgeHandback(leaf, name)
	if !ok {
		return nil
	}
	if cfg == nil {
		// A pass that carries no city config resolves no agent: the item
		// is keyed by the session row alone.
		cfg = &config.City{}
	}
	target := resolveNudgeTargetFromSessionInfo(cityPath, cfg, info)
	item := newQueuedNudgeWithOptions(target.agentKey(), nudge, startupNudgeHandbackSource, now, queuedNudgeOptionsFromTarget(target))
	if err := enqueueQueuedNudge(cityPath, item); err != nil {
		return fmt.Errorf("queueing session %s's startup nudge behind its program's dialog: %w", info.ID, err)
	}
	return nil
}
