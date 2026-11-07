package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/storeref"
)

// hookEmitDrainFenceUnavailable is the emitter seam, replaced in tests.
var hookEmitDrainFenceUnavailable = emitDrainFenceUnavailable

// emitDrainFenceUnavailable records that a seat's drain-pending probe could not
// read its own session row, so the F-D claim fence failed OPEN for this poll —
// or, for a standing storage refusal, failed CLOSED (hookDrainProbeFailsClosed).
//
// Failing open on a transient fault is the right call — one store hiccup must not idle every healthy
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
	reason := drainFenceUnavailableReason(err)
	rec.Record(events.Event{
		Type:    events.SessionDrainFenceUnavailable,
		Actor:   eventActor(),
		Subject: subject,
		Message: reason,
		Payload: api.SessionLifecyclePayloadJSON(sessionID, template, reason),
	})
}

// drainFenceUnavailableReason is the event's reason text, which names the way
// the fence went: hookDrainProbeFailsClosed decided it, and an operator reading
// the record must not be told "failed open" about a fence that refused.
func drainFenceUnavailableReason(err error) string {
	if hookDrainProbeFailsClosed(err) {
		return "drain-pending probe refused by the city's storage gate; claim fence failed closed: " + err.Error()
	}
	if isTransientStorageGateFault(err) {
		// Not a refusal: the gate could not read a store it needed, and the
		// next attempt may.
		return "drain-pending probe failed: the storage gate could not read a store it needs (transient); claim fence failed open: " + err.Error()
	}
	return "drain-pending probe failed; claim fence failed open: " + err.Error()
}

// hookDrainProbeFailsClosed reports whether a drain-pending probe error must
// fail the F-D fence CLOSED rather than open.
//
// The documented contract above (and TestHookClaimDrainPendingProbeErrorFailsOpen)
// is that a probe FAULT fails open: one store hiccup must not idle every
// healthy seat in the city, the drain lanes stay the backstop, and the
// SessionDrainFenceUnavailable event keeps the inert fence visible. That
// reasoning holds only for a fault that clears on its own — a locked database, a
// timeout, a flapping connection — which is exactly the population it was
// written for, so transients keep it.
//
// A standing storage refusal (storeref.IsStandingRefusal) is not that. It is
// the verdict the storage gate took about the CITY — "this city converged on
// binding ..., and the retained work store holds N infrastructure bead(s) the
// binding cannot read", a worker-site refusal, an unloadable config on a city
// that may be split — and it holds on every poll until an operator acts. Failing
// open on it switches the drain fence off for every seat, for as long as the
// city stays refused, while the session rows that would say "draining" are
// unreadable: the fleet-wide wedge the fence exists to prevent (prod
// 2026-10-18, 22 session.drain_fence_unavailable events on maintainer-city).
// So it fails closed: no claim, no discovery answer, and the store-unavailable
// exit contract (an unknown answer, not an empty queue and not a drain) so the
// seat neither idles on a false "no work" nor drains on a row nobody read.
//
// A refusal that rests on a read the gate could not make (a hosted work store
// that timed out during the containment check: storageGateTransientFault) is
// a transient by this definition, and fails open like any other store fault.
func hookDrainProbeFailsClosed(err error) bool {
	return err != nil && storeref.IsStandingRefusal(err) && !isTransientStorageGateFault(err)
}

// reportHookDrainFenceClosed renders a fail-closed fence through the shared
// work-query failure contract: the store-unavailable token on stderr and exit
// hookExitWorkQueryFailed.
func reportHookDrainFenceClosed(prefix, sessionID string, err error, stderr io.Writer) int {
	fmt.Fprintf(stderr, "%s: %s: drain-pending probe for %s refused by the storage gate, so the drain fence failed closed and nothing was claimed: %v\n", prefix, hookStoreUnavailableToken, sessionID, err) //nolint:errcheck // best-effort stderr
	return hookExitWorkQueryFailed
}
