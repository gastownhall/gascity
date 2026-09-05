package main

import (
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/events"
)

// hookEmitDrainFenceUnavailable is the emitter seam, replaced in tests.
var hookEmitDrainFenceUnavailable = emitDrainFenceUnavailable

// emitDrainFenceUnavailable records that a seat's drain-pending probe could not
// read its own session row, so the F-D claim fence failed OPEN for this poll.
//
// Failing open is the right call — one store hiccup must not idle every healthy
// seat in the city — but it is SILENT, and that silence is the hazard this
// event exists to remove. The same agent-side store fault also fails open the
// runtime-identity fence (hookClaimSessionStoreUnavailable), so a persistent one
// switches BOTH drain fences off for every seat while the reconciler keeps
// marking rows draining: draining seats claim and execute indefinitely, which is
// exactly the wedge this series closes, and the only trace would be stderr
// inside agent panes.
//
// Cadence is one event per store leg that reaches the fence, matching
// emitCityWorkQueryFailure's per-failure shape. On a federated city that is more
// than one per poll: claimHookWorkWithRunner calls tryHookClaim — and so the
// probe — once per leg inside its reselect loop. That is bounded by agent-turn
// frequency, not the reconcile hot path, and a fence that is inert on every poll
// is a fact worth a record every time it is — the volume IS the signal.
//
// Best-effort and silent on every failure: a diagnostics counter must never
// become a second failure mode on a path that has already decided to proceed.
func emitDrainFenceUnavailable(stderr io.Writer, sessionID, template string, err error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || err == nil {
		return
	}
	rec := openCityRecorder(stderr)
	if closer, ok := rec.(interface{ Close() error }); ok {
		defer closer.Close() //nolint:errcheck // best-effort event recorder cleanup
	}
	if rec == nil {
		return
	}
	template = strings.TrimSpace(template)
	subject := template
	if subject == "" {
		subject = sessionID
	}
	reason := "drain-pending probe failed; claim fence failed open: " + err.Error()
	rec.Record(events.Event{
		Type:    events.SessionDrainFenceUnavailable,
		Actor:   eventActor(),
		Subject: subject,
		Message: reason,
		Payload: api.SessionLifecyclePayloadJSON(sessionID, template, reason),
	})
}
