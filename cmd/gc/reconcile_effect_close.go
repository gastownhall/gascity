package main

import (
	"context"
	"errors"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The close effect (CONTRACT v5 C3, arm A21). Under the runtime name lock it
// first confirms the stop (C8.8, v5 F3): a complete, error-free fresh read
// through the composite provider, begun after the effect began, that shows
// no live pane for the row's name. A corpse counts as stopped. When the
// attach leg (L3) passes, the effect also removes the corpse under the
// corpse exception (v5 F2), by exact session object, and then reads again:
// the kill re-checks server-side, and a refused kill leaves the corpse in
// place (a multi-pane corpse is never auto-removed), so the second read alone
// decides, and only a live pane refuses. Then the premise close: the row the
// pass decided on is the premise, so a row that changed since (a wake that
// landed after the read) refuses with cause superseded and is decided again,
// never closed at its new revision. Releasing the closed row's work is C8's
// orphan-release step.

// Close refusal causes; a fresh read's are freshLiveness's.
const (
	causeLivePane   = "live-pane"  // the fresh read shows a live pane: not stopped
	causeSuperseded = "superseded" // the row changed since the pass decided
)

// closeEffect is an admitted close. Its intent's Patch is the terminal patch
// (session.ClosePatch and any clears the close kind adds) and its Event what
// a landed close records.
func closeEffect(p *effectPass, it intent) func(context.Context) settlement {
	return closeRun{pass: p, it: it, now: time.Now}.run
}

// closeRun is one close. now is the runtime's clock: a fresh read accepts
// only a refresh begun at or after the time it gives.
type closeRun struct {
	pass *effectPass
	it   intent
	now  func() time.Time
}

func (e closeRun) run(ctx context.Context) settlement {
	row, ok := e.pass.World.Census.Rows[e.it.Key]
	if !ok {
		return settlement{Outcome: settledRefused, Cause: causeRedecided}
	}
	writer, ok := e.pass.Writers[e.it.Key.Leg]
	if !ok {
		return settlement{Outcome: settledRefused, Cause: causeNoWriter, Err: errNoConditionalWriter}
	}
	name, unlock, ok := lockRuntimeName(e.pass.World, row.Info)
	switch {
	case !ok && name == "":
		return settlement{Outcome: settledRefused, Cause: causeRouteUnknown}
	case !ok:
		return settlement{Outcome: settledRefused, Cause: causeNameBusy}
	}
	defer unlock()
	if cause := e.noLivePane(ctx, name); cause != "" {
		return settlement{Outcome: settledRefused, Cause: cause}
	}
	if err := ctx.Err(); err != nil {
		return settlement{Outcome: settledFailed, Cause: causeDeadline, Err: err}
	}
	closed, err := writer.closeWithTerminalPatch(row.Info, e.it.Patch, "gc: close session "+row.Info.ID, e.pass.World.Now)
	switch {
	case errors.Is(err, errNoConditionalWriter):
		return settlement{Outcome: settledRefused, Cause: causeNoWriter, Err: err}
	case errors.Is(err, session.ErrSessionCloseSuperseded), errors.Is(err, session.ErrSessionKillPending):
		return settlement{Outcome: settledRefused, Cause: causeSuperseded, Err: err}
	case err != nil:
		return settlement{Outcome: settledFailed, Cause: causeWrite, Err: err}
	case !closed: // another writer closed it first
		return settlement{Outcome: settledNoop}
	}
	return settlement{Outcome: settledLanded, Event: e.it.Event}
}

// noLivePane is C8.8 for the close, removing a detached corpse on the way.
// It returns the refusal cause, or "" once no live pane is confirmed.
func (e closeRun) noLivePane(ctx context.Context, name string) string {
	live, cause := freshLiveness(ctx, e.pass.Runtime, name, e.now())
	switch {
	case cause != "":
		return cause
	case live.Running:
		return causeLivePane
	case !live.Corpse || !e.removeCorpse(ctx, name, live):
		return ""
	}
	if live, cause = freshLiveness(ctx, e.pass.Runtime, name, e.now()); cause == "" && live.Running {
		cause = causeLivePane // the name now holds a session the first read did not (R54)
	}
	return cause
}

// removeCorpse kills the corpse live read, under the corpse exception:
// identity waived, L3 kept. The routed leaf both answers L3 and kills, so the
// two cannot split across backends; a leaf that cannot kill by session
// object leaves the corpse. It reports whether a kill was sent.
func (e closeRun) removeCorpse(ctx context.Context, name string, live runtime.Liveness) bool {
	leaf, _, known := runtime.ResolveBackend(e.pass.Runtime, name)
	killer, kills := leaf.(runtime.SessionObjectKiller)
	if !known || !kills || ctx.Err() != nil {
		return false
	}
	if reason, _ := attachLeg(ctx, leaf, name, false, e.pass.World.Now); reason != "" {
		return false // an attached corpse stays (v5 D3)
	}
	_, _ = killer.KillCorpseObject(name, live.ObjectID, live.ObjectCreated)
	return true
}
