package main

import (
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/storeref"
)

// emitControlDispatcherScopeGapEvents records one control.dispatcher_scope_gap
// event per scope the desired-state build found owning open control work with
// no configured control-dispatcher.
//
// The build already prints one stderr line per scope, which is where this
// condition hid: the pass object is rebuilt every tick, so the "report once"
// budget resets every tick and a standing config gap becomes an endless stream
// of identical lines nobody reads — 600+ over 26h on maintainer-city before a
// factory stall was traced back to it by hand (ga-oytw9). The event carries the
// count the line cannot, so the same condition is queryable via gc events and
// alertable, and the stderr line stays for trace readers that grep it.
//
// One event per scope per build, not per suppressed row: the gap is a property
// of the city's config, not of any one bead. It is re-emitted on later builds
// while the gap stands — a level-triggered gauge, so a subscriber reads the
// current count rather than having to sum a history.
func emitControlDispatcherScopeGapEvents(rec events.Recorder, cityName string, gaps []ControlDispatcherScopeGap, now time.Time) {
	if rec == nil || len(gaps) == 0 {
		return
	}
	for _, gap := range gaps {
		rec.Record(events.Event{
			Type:    events.ControlDispatcherScopeGap,
			Ts:      now.UTC(),
			Actor:   "gc",
			Subject: controlDispatcherScopeGapSubject(cityName, gap),
			Message: formatControlDispatcherScopeGapMessage(gap),
			Payload: events.ControlDispatcherScopeGapPayloadJSON(events.ControlDispatcherScopeGapPayload{
				ScopeLabel:      gap.ScopeLabel,
				RigContext:      gap.RigContext,
				StoreRef:        gap.StoreRef,
				SuppressedCount: gap.SuppressedCount,
				SampleBeadID:    gap.SampleBeadID,
			}),
		})
	}
}

// controlDispatcherScopeGapSubject names the scope that OWNS the suppressed
// work, in the canonical store-ref vocabulary. It is deliberately not the gap's
// StoreRef: on a relocated city that leg is the class binding, which serves
// every scope and owns none, so subscribers keying on it could not tell two
// rigs' gaps apart.
func controlDispatcherScopeGapSubject(cityName string, gap ControlDispatcherScopeGap) string {
	if gap.RigContext != "" {
		return string(storeref.RigRef(gap.RigContext))
	}
	return "city:" + cityName
}

// formatControlDispatcherScopeGapMessage renders the operator-facing text for a
// control.dispatcher_scope_gap event.
func formatControlDispatcherScopeGapMessage(gap ControlDispatcherScopeGap) string {
	msg := fmt.Sprintf("%s has no configured control-dispatcher; %d control bead(s) suppressed from this tick's demand",
		gap.ScopeLabel, gap.SuppressedCount)
	if gap.SampleBeadID != "" {
		msg += " (e.g. " + gap.SampleBeadID + ")"
	}
	return msg + "; that work cannot run until the scope has a dispatcher"
}
