package main

import (
	"os"

	"github.com/gastownhall/gascity/internal/events"
)

// hookEmitNudgeDialogBlocked is the emitter seam, replaced in tests.
var hookEmitNudgeDialogBlocked = emitNudgeDialogBlocked

// emitNudgeDialogBlocked records that the queued-nudge delivery gate deferred
// because a dialog owns the target session's pane input, rather than because
// the pane is genuinely busy. Without this signal the deferral is
// indistinguishable from ordinary quiescence: pollerSessionIdleEnough reports
// the gate as closed either way, so an operator watching the event bus
// cannot tell a session waiting out its quiescence window from one wedged
// behind an unmatched dialog that will never clear on its own.
//
// BeadID is left empty: the gate check runs before any queued item is
// claimed, so no specific bead is in scope yet at this call site.
//
// Best-effort and silent on failure: a diagnostics event must never become a
// second failure mode on a gate that has already decided to defer.
func emitNudgeDialogBlocked(target nudgeTarget, kind string) {
	rec := openCityRecorderAt(target.cityPath, os.Stderr)
	if closer, ok := rec.(interface{ Close() error }); ok {
		defer closer.Close() //nolint:errcheck // best-effort event recorder cleanup
	}
	if rec == nil {
		return
	}
	rec.Record(events.Event{
		Type:      events.NudgeDialogBlocked,
		Actor:     eventActor(),
		Subject:   target.sessionName,
		Message:   "queued-nudge delivery gate deferred: session blocked by dialog kind " + kind,
		SessionID: target.sessionID,
		Payload: events.NudgeDialogBlockedPayloadJSON(events.NudgeDialogBlockedPayload{
			SessionID:  target.sessionID,
			DialogKind: kind,
		}),
	})
}
