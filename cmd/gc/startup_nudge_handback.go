package main

import (
	"errors"
	"fmt"
	"log"
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
// typed and never fails the start (design §10.2 row A20); an exec pack's
// un-welded launch hands back every first turn, which its Start never types.
// It waits in the session's queue as a user's submit, stamped with the
// started row's continuation, held until the nudge's NotBefore, and bound to the launch that stands
// (session.StartupTurnLaunchID: the leaf's live token, else startToken, the
// one the start carried), so the queue delivers it to that launch alone,
// ahead of the session's other items, and drops it once another launch
// replaces it. The row's one-shot initial message is split off as its own
// submit (session.StartupHandbackItems). In legacy dispatcher mode it starts
// the session's poller, since nothing else may
// (startNudgePollerUnlessDispatched). A
// start that failed has its handback taken and dropped, and a leaf that
// handed nothing back queues nothing.
func queueStartupNudgeHandback(cityPath string, cfg *config.City, leaf runtime.Provider, info session.Info, name, startToken string, started bool, now time.Time) error {
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
	overrides, err := session.ParseTemplateOverridesFromInfo(info)
	if err != nil {
		log.Printf("session %s: reading its initial message to split from the startup nudge: %v", info.ID, err)
	}
	turn := newQueuedNudgeWithOptions(target.agentKey(), nudge.Text, nudgequeue.SourceSubmit, now, queuedNudgeOptionsFromTarget(target))
	items := session.StartupHandbackItems(turn, nudge.NotBefore, session.StartupTurnLaunchID(leaf, name, startToken), overrides[session.InitialMessageOverrideKey], newQueuedNudgeID())
	queue := openNudgeQueue(cityPath, beads.NudgesStore{}).Queue
	var errs []error
	for _, item := range items {
		if err := queue.EnqueueDeferred(item); err != nil {
			errs = append(errs, fmt.Errorf("queueing session %s's handed-back startup nudge %s: %w", info.ID, item.ID, err))
		}
	}
	if len(errs) < len(items) {
		pingNudgeWakeSocket(cityPath)
		startNudgePollerUnlessDispatched(target)
	}
	return errors.Join(errs...)
}
