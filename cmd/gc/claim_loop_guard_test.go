package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// claimLoopHarness drives a guard on a fake clock and keeps the alerts it
// records.
type claimLoopHarness struct {
	t      *testing.T
	guard  *claimLoopGuard
	now    time.Time
	seats  int
	alerts []events.Event
	stderr bytes.Buffer
}

func newClaimLoopHarness(t *testing.T) *claimLoopHarness {
	t.Helper()
	h := &claimLoopHarness{t: t, now: time.Date(2026, 10, 18, 19, 6, 0, 0, time.UTC)}
	h.guard = newClaimLoopGuard(func(e events.Event) { h.alerts = append(h.alerts, e) }, &h.stderr)
	h.guard.now = func() time.Time { return h.now }
	return h
}

// fail feeds one bead.claim_failed from a fresh seat, stamped now: the shape
// of a loop, where every failure is a new seat the controller spawned.
func (h *claimLoopHarness) fail(template, beadID, errText string) {
	h.t.Helper()
	h.seats++
	h.failAs(fmt.Sprintf("gcg-session-%d", h.seats), template, beadID, errText)
}

// failAs feeds one bead.claim_failed from the named session, stamped now.
func (h *claimLoopHarness) failAs(session, template, beadID, errText string) {
	h.t.Helper()
	h.failAsStart(session, 0, template, beadID, errText)
}

// failAsStart feeds one bead.claim_failed from one start (runtime epoch) of
// the named session, stamped now.
func (h *claimLoopHarness) failAsStart(session string, epoch int64, template, beadID, errText string) {
	h.t.Helper()
	payload, err := json.Marshal(events.BeadClaimFailedPayload{Template: template, SessionID: session, RuntimeEpoch: epoch, Error: errText})
	if err != nil {
		h.t.Fatal(err)
	}
	h.guard.observe(events.Event{Type: events.BeadClaimFailed, Ts: h.now, Subject: beadID, Payload: payload})
}

func (h *claimLoopHarness) advance(d time.Duration) { h.now = h.now.Add(d) }

// trip feeds the failures that hold the loop wisp for the loop template.
func (h *claimLoopHarness) trip() {
	h.t.Helper()
	for i := 0; i < claimLoopTripFailures; i++ {
		h.fail(loopTemplate, loopWisp, loopErr)
	}
}

// held reports whether the loop wisp is withheld from template's demand this
// pass.
func (h *claimLoopHarness) held(template string) bool {
	h.t.Helper()
	counts := map[string]int{template: 1}
	demand := map[string]scaleCheckDemand{template: {Count: 1, WorkBeadIDs: []string{loopWisp}}}
	gotCounts, gotDemand := h.guard.withhold(counts, demand)
	return gotCounts[template] == 0 && len(gotDemand[template].WorkBeadIDs) == 0
}

func (h *claimLoopHarness) records() int {
	h.guard.mu.Lock()
	defer h.guard.mu.Unlock()
	return len(h.guard.records)
}

const (
	loopWisp     = "gcg--9223372036838841776"
	loopTemplate = "bd.dog"
	loopErr      = "claimIssue: bd serve at https://bdserve.example.test:30635 answered 404 not_found: no issue or wisp with that id"
)

// TestClaimLoopGuardHoldsABeadAfterRepeatedIdenticalFailures is the trip: the
// incident's shape, one bead failing the same way for every seat of one
// template, stops counting as that template's spawn demand at the third seat
// and raises one alert naming the bead, the template and the error.
func TestClaimLoopGuardHoldsABeadAfterRepeatedIdenticalFailures(t *testing.T) {
	h := newClaimLoopHarness(t)
	for i := 1; i < claimLoopTripFailures; i++ {
		h.fail(loopTemplate, loopWisp, loopErr)
		h.advance(time.Minute)
		if h.held(loopTemplate) {
			t.Fatalf("held after %d failure(s), want a hold only at %d", i, claimLoopTripFailures)
		}
	}
	h.fail(loopTemplate, loopWisp, loopErr)
	trippedAt := h.now
	if !h.held(loopTemplate) {
		t.Fatalf("not held after %d identical failures", claimLoopTripFailures)
	}
	if h.held("gascity/worker") {
		t.Fatal("another template's demand for the bead was withheld; the hold is per template")
	}
	if len(h.alerts) != 1 {
		t.Fatalf("alerts = %d, want exactly one for the hold", len(h.alerts))
	}
	alert := h.alerts[0]
	var payload events.BeadClaimLoopHeldPayload
	if err := json.Unmarshal(alert.Payload, &payload); err != nil {
		t.Fatalf("alert payload: %v", err)
	}
	want := events.BeadClaimLoopHeldPayload{Template: loopTemplate, Error: loopErr, Failures: claimLoopTripFailures, HeldUntil: trippedAt.Add(claimLoopHoldBase)}
	if alert.Type != events.BeadClaimLoopHeld || alert.Subject != loopWisp || !reflect.DeepEqual(payload, want) {
		t.Fatalf("alert = %s subject=%s payload=%+v, want %s subject=%s payload=%+v", alert.Type, alert.Subject, payload, events.BeadClaimLoopHeld, loopWisp, want)
	}
	// The guard can withhold only the demand it builds bead by bead, and the
	// alert must not promise more.
	if !strings.Contains(alert.Message, "default routed-work demand") || !strings.Contains(alert.Message, "custom scale_check") {
		t.Fatalf("alert message = %q, want it to name what is withheld and what is not", alert.Message)
	}
	if !strings.Contains(h.stderr.String(), "holding "+loopWisp+" for "+loopTemplate) {
		t.Fatalf("stderr = %q, want the hold named", h.stderr.String())
	}
}

// TestClaimLoopGuardCountsSeatsNotRetries: one seat failing the same claim
// again (a retry, or the same bead on a second leg) is one seat, not a loop.
func TestClaimLoopGuardCountsSeatsNotRetries(t *testing.T) {
	h := newClaimLoopHarness(t)
	for i := 0; i < claimLoopTripFailures+2; i++ {
		h.failAs("gcg-session-one", loopTemplate, loopWisp, loopErr)
	}
	if h.held(loopTemplate) {
		t.Fatal("one seat's repeated failures held the bead")
	}
	h.fail(loopTemplate, loopWisp, loopErr)
	h.fail(loopTemplate, loopWisp, loopErr)
	if !h.held(loopTemplate) {
		t.Fatal("not held once three distinct seats had failed")
	}
}

// TestClaimLoopGuardCountsEachStartOfAReusedSession: a named singleton keeps
// its session bead when it is woken again, so the same session id failing in a
// new start (runtime epoch) is a new seat, while a retry within one start is
// not.
func TestClaimLoopGuardCountsEachStartOfAReusedSession(t *testing.T) {
	h := newClaimLoopHarness(t)
	for i := 0; i < claimLoopTripFailures; i++ {
		h.failAsStart("gcg-session-singleton", 4, loopTemplate, loopWisp, loopErr)
	}
	if h.held(loopTemplate) {
		t.Fatal("retries within one start held the bead")
	}
	h.failAsStart("gcg-session-singleton", 5, loopTemplate, loopWisp, loopErr)
	h.failAsStart("gcg-session-singleton", 6, loopTemplate, loopWisp, loopErr)
	if !h.held(loopTemplate) {
		t.Fatal("three starts of one session failing the same way did not hold the bead")
	}
}

// TestClaimLoopGuardRequiresTheSameError: failures that differ are not a loop.
// A changed error restarts the streak, so it takes the full count of the NEW
// error to hold. A server request id is not part of the error.
func TestClaimLoopGuardRequiresTheSameError(t *testing.T) {
	h := newClaimLoopHarness(t)
	h.fail(loopTemplate, loopWisp, loopErr)
	h.fail(loopTemplate, loopWisp, loopErr)
	h.fail(loopTemplate, loopWisp, "claimIssue: context deadline exceeded")
	if h.held(loopTemplate) {
		t.Fatal("held after failures with different errors")
	}
	h.fail(loopTemplate, loopWisp, "claimIssue: context deadline exceeded")
	if h.held(loopTemplate) {
		t.Fatal("held after two failures of the new error")
	}
	h.fail(loopTemplate, loopWisp, "claimIssue: context deadline exceeded")
	if !h.held(loopTemplate) {
		t.Fatal("not held after the new error repeated the full count")
	}

	h = newClaimLoopHarness(t)
	for i := 0; i < claimLoopTripFailures; i++ {
		h.fail(loopTemplate, loopWisp, fmt.Sprintf("claimIssue: bd serve at https://x answered 500 internal: internal error (request_id req-%d)", i))
	}
	if !h.held(loopTemplate) {
		t.Fatal("failures differing only by request id did not count as the same error")
	}
	var payload events.BeadClaimLoopHeldPayload
	if err := json.Unmarshal(h.alerts[0].Payload, &payload); err != nil || strings.Contains(payload.Error, "request_id") {
		t.Fatalf("alert error = %q (%v), want it without the request id", payload.Error, err)
	}
	// The failure event's message keeps the id: it is how an operator finds the
	// server's log line. Its payload, the form the guard compares, does not.
	e, ok := claimFailedEvent(loopWisp, hookClaimOptions{Env: []string{"GC_TEMPLATE=" + loopTemplate}}, errors.New("answered 500 internal: internal error (request_id req-9)"))
	var failed events.BeadClaimFailedPayload
	if !ok || json.Unmarshal(e.Payload, &failed) != nil || !strings.Contains(e.Message, "(request_id req-9)") || failed.Error != "answered 500 internal: internal error" {
		t.Fatalf("bead.claim_failed message = %q, error = %q; want the request id in the message only", e.Message, failed.Error)
	}
}

// TestClaimLoopGuardTripsOnALongErrorWhoseRequestIDsDiffer round-trips the
// guard's input from its source, claimFailedEvent, for a repeating server
// error longer than the bound. Bounding the text before removing the request
// id would leave each seat a different partial id, or, when ids differ in
// length, a different amount of the text after the id, and the guard would
// never see the same error twice.
func TestClaimLoopGuardTripsOnALongErrorWhoseRequestIDsDiffer(t *testing.T) {
	for name, render := range map[string]func(seat int) string{
		// The bound cuts the id's closing paren off and keeps its digits.
		"id cut by the bound": func(seat int) string {
			id := fmt.Sprintf(" (request_id srv-%06d)", seat*111111)
			return strings.Repeat("e", claimLoopErrorMax-len(id)+1) + id
		},
		// The id fits, but its length decides how much of the text after it
		// the bound keeps.
		"text after an id of varying length": func(seat int) string {
			return "answered 500 internal: internal error (request_id srv-" + strings.Repeat("7", seat) + ") " + strings.Repeat("h", claimLoopErrorMax)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newClaimLoopHarness(t)
			for seat := 1; seat <= claimLoopTripFailures; seat++ {
				opts := hookClaimOptions{Env: []string{"GC_TEMPLATE=" + loopTemplate, fmt.Sprintf("GC_SESSION_ID=gcg-session-%d", seat)}}
				e, ok := claimFailedEvent(loopWisp, opts, errors.New(render(seat)))
				if !ok {
					t.Fatal("no event for a templated seat's claim failure")
				}
				if !strings.Contains(e.Message, "(request_id srv-") {
					t.Fatalf("message = %q, want the request id kept for the operator", e.Message)
				}
				e.Ts = h.now
				h.guard.observe(e)
			}
			if !h.held(loopTemplate) {
				t.Fatalf("not held after %d seats failed with one error differing only by request id", claimLoopTripFailures)
			}
		})
	}
}

// TestClaimLoopGuardStreakMustFitTheWindow: failures spread wider than the
// window are not consecutive evidence of a loop.
func TestClaimLoopGuardStreakMustFitTheWindow(t *testing.T) {
	h := newClaimLoopHarness(t)
	h.fail(loopTemplate, loopWisp, loopErr)
	h.advance(claimLoopWindow / 2)
	h.fail(loopTemplate, loopWisp, loopErr)
	h.advance(claimLoopWindow/2 + time.Minute)
	h.fail(loopTemplate, loopWisp, loopErr)
	if h.held(loopTemplate) {
		t.Fatal("held although the first and third failures are wider apart than the window")
	}
	h.advance(time.Minute)
	h.fail(loopTemplate, loopWisp, loopErr)
	h.advance(time.Minute)
	h.fail(loopTemplate, loopWisp, loopErr)
	if !h.held(loopTemplate) {
		t.Fatal("not held after a fresh streak inside the window")
	}
}

// TestClaimLoopGuardSeatsInFlightDoNotLengthenTheHold: seats already running
// when the hold begins still fail after it; they are not probes, so the hold,
// and the alert's held_until, stand.
func TestClaimLoopGuardSeatsInFlightDoNotLengthenTheHold(t *testing.T) {
	h := newClaimLoopHarness(t)
	h.trip()
	h.advance(time.Minute)
	h.fail(loopTemplate, loopWisp, loopErr)
	h.fail(loopTemplate, loopWisp, loopErr)
	h.advance(claimLoopHoldBase - time.Minute - time.Second)
	if !h.held(loopTemplate) {
		t.Fatal("released before the first hold ran out")
	}
	h.advance(time.Second)
	if h.held(loopTemplate) {
		t.Fatalf("still withheld %s after the trip: in-flight seats lengthened the hold", claimLoopHoldBase)
	}
	if len(h.alerts) != 1 {
		t.Fatalf("alerts = %d, want only the trip's: in-flight seats change nothing to report", len(h.alerts))
	}
}

// TestClaimLoopGuardHoldOutlastsADifferentError: a hold is about the bead, not
// the error. A seat that fails another way while the bead is held (one already
// running when the hold began, a probe timing out under load) neither ends the
// hold nor resets its backoff, and a probe that fails another way extends it
// like any failed probe. Releasing the hold on a changed error would let a loop
// whose error alternates between two texts trip once and never again.
func TestClaimLoopGuardHoldOutlastsADifferentError(t *testing.T) {
	const otherErr = "claimIssue: timed out after 10s (caller deadline)"
	h := newClaimLoopHarness(t)
	h.trip()
	h.advance(time.Minute)
	h.fail(loopTemplate, loopWisp, otherErr) // a seat already running
	if !h.held(loopTemplate) {
		t.Fatal("a seat failing another way released the hold")
	}
	h.advance(claimLoopHoldBase - time.Minute)
	if h.held(loopTemplate) {
		t.Fatal("the probe was not admitted when the hold ran out")
	}
	h.advance(time.Minute)
	h.fail(loopTemplate, loopWisp, otherErr) // the probe fails another way
	end := h.now.Add(claimLoopHold(2))
	h.now = end.Add(-time.Second)
	if !h.held(loopTemplate) {
		t.Fatal("a probe failing another way did not hold the bead again, longer")
	}
	if len(h.alerts) != 2 {
		t.Fatalf("alerts = %d, want the trip's and the failed probe's", len(h.alerts))
	}
	var payload events.BeadClaimLoopHeldPayload
	if err := json.Unmarshal(h.alerts[1].Payload, &payload); err != nil || payload.Error != otherErr || !payload.HeldUntil.Equal(end) {
		t.Fatalf("probe alert = %+v (%v), want error %q held until %s", payload, err, otherErr, end)
	}
	if !strings.Contains(h.stderr.String(), "probe of "+loopWisp+" for "+loopTemplate+" failed") {
		t.Fatalf("stderr = %q, want the failed probe named", h.stderr.String())
	}
}

// TestClaimLoopGuardOnlyTheProbeLengthensAHold: once a probe's failure has
// doubled the hold, a later seat failing the same way (one spawned before the
// hold, still finishing) leaves the new hold's end where it is.
func TestClaimLoopGuardOnlyTheProbeLengthensAHold(t *testing.T) {
	h := newClaimLoopHarness(t)
	h.trip()
	h.advance(claimLoopHoldBase)
	if h.held(loopTemplate) {
		t.Fatal("the probe was not admitted when the hold ran out")
	}
	h.advance(time.Minute)
	h.fail(loopTemplate, loopWisp, loopErr) // the probe fails: the hold doubles
	end := h.now.Add(claimLoopHold(2))
	h.advance(time.Minute)
	h.fail(loopTemplate, loopWisp, loopErr) // a straggler, not a probe
	h.now = end.Add(-time.Second)
	if !h.held(loopTemplate) {
		t.Fatal("released before the doubled hold ran out")
	}
	h.now = end
	if h.held(loopTemplate) {
		t.Fatal("a straggler after the probe lengthened the hold again")
	}
}

// TestClaimLoopGuardBacksOffWithOneProbePerHold: a hold that runs out admits
// the bead for one demand pass and then withholds it again while the probe
// reports; a probe that fails the same way doubles the hold, up to the cap,
// and raises the alert again with the hold's new end.
func TestClaimLoopGuardBacksOffWithOneProbePerHold(t *testing.T) {
	h := newClaimLoopHarness(t)
	h.trip()
	hold := claimLoopHoldBase
	for round := 0; round < 6; round++ {
		h.advance(hold - time.Second)
		if !h.held(loopTemplate) {
			t.Fatalf("round %d: admitted before the %s hold ran out", round, hold)
		}
		h.advance(time.Second)
		if h.held(loopTemplate) {
			t.Fatalf("round %d: still withheld when the %s hold ran out; one probe must be admitted", round, hold)
		}
		if !h.held(loopTemplate) {
			t.Fatalf("round %d: admitted twice in a row; a probe gets %s to report before the next", round, claimLoopProbeGrace)
		}
		h.advance(time.Minute)
		h.fail(loopTemplate, loopWisp, loopErr) // the probe failed the same way
		hold = min(hold*2, claimLoopHoldMax)
		if len(h.alerts) != round+2 {
			t.Fatalf("round %d: alerts = %d, want one more for the extended hold", round, len(h.alerts))
		}
		var payload events.BeadClaimLoopHeldPayload
		if err := json.Unmarshal(h.alerts[round+1].Payload, &payload); err != nil || !payload.HeldUntil.Equal(h.now.Add(hold)) {
			t.Fatalf("round %d: alert held_until = %s (%v), want %s", round, payload.HeldUntil, err, h.now.Add(hold))
		}
	}
}

// TestClaimLoopGuardProbeThatNeverReportsIsRetried: a probe seat that never
// spawned or never reported does not wedge the bead out of demand forever.
func TestClaimLoopGuardProbeThatNeverReportsIsRetried(t *testing.T) {
	h := newClaimLoopHarness(t)
	h.trip()
	h.advance(claimLoopHoldBase)
	if h.held(loopTemplate) {
		t.Fatal("the first probe was not admitted")
	}
	h.advance(claimLoopProbeGrace)
	if h.held(loopTemplate) {
		t.Fatalf("no probe admitted %s after one that never reported", claimLoopProbeGrace)
	}
}

// TestClaimLoopGuardResumesWhenTheBeadChanges: any journal event naming the
// bead — the claim that finally landed, an operator's edit, a close — puts it
// back in demand at once.
func TestClaimLoopGuardResumesWhenTheBeadChanges(t *testing.T) {
	for _, evtType := range []string{events.BeadUpdated, events.BeadClosed, events.BeadDeleted, events.BeadCreated} {
		t.Run(evtType, func(t *testing.T) {
			h := newClaimLoopHarness(t)
			for i := 0; i < claimLoopTripFailures; i++ {
				h.fail(loopTemplate, loopWisp, loopErr)
				h.fail("gascity/worker", loopWisp, loopErr)
			}
			h.guard.observe(events.Event{Type: evtType, Subject: "gcg--other"})
			if !h.held(loopTemplate) {
				t.Fatal("an event about another bead released the hold")
			}
			h.guard.observe(events.Event{Type: evtType, Subject: loopWisp})
			if h.held(loopTemplate) || h.held("gascity/worker") {
				t.Fatalf("still held after %s on the bead", evtType)
			}
			if !strings.Contains(h.stderr.String(), loopWisp+" counts as demand for "+loopTemplate+" again") {
				t.Fatalf("stderr = %q, want the resumption named", h.stderr.String())
			}
		})
	}
}

// TestClaimLoopGuardResumesWhenTheConfigChanges: a reload that changes the
// config may have fixed what the claims failed on.
func TestClaimLoopGuardResumesWhenTheConfigChanges(t *testing.T) {
	h := newClaimLoopHarness(t)
	h.trip()
	h.guard.configChanged()
	if h.held(loopTemplate) {
		t.Fatal("still held after a config change")
	}
	h.fail(loopTemplate, loopWisp, loopErr)
	if h.held(loopTemplate) {
		t.Fatal("one failure after a config change held the bead; the streak must restart")
	}
}

// TestAdoptConfigRevisionResetsTheClaimLoopGuardOnChange pins the reload hook:
// the same revision keeps every hold, a new one releases them.
func TestAdoptConfigRevisionResetsTheClaimLoopGuardOnChange(t *testing.T) {
	cr := &CityRuntime{configRev: "rev-1", stderr: io.Discard}
	h := newClaimLoopHarness(t)
	cr.claimLoopOnce.Do(func() { cr.claimLoop = h.guard })
	h.trip()
	cr.adoptConfigRevision("rev-1")
	if !h.held(loopTemplate) {
		t.Fatal("a reload to the same revision released the hold")
	}
	cr.adoptConfigRevision("rev-2")
	if h.held(loopTemplate) {
		t.Fatal("a reload to a new revision kept the hold")
	}
	if cr.configRev != "rev-2" {
		t.Fatalf("configRev = %q, want rev-2", cr.configRev)
	}
}

// TestClaimLoopGuardForgetsQuietRecords: a held bead that stops reporting has
// left demand without an event saying so; its record must not live forever,
// whether the next thing the guard sees is a demand pass or another failure.
func TestClaimLoopGuardForgetsQuietRecords(t *testing.T) {
	h := newClaimLoopHarness(t)
	h.trip()
	h.advance(claimLoopForgetAfter + time.Second)
	if h.held(loopTemplate) {
		t.Fatal("a record quiet for longer than the forget horizon still holds")
	}
	if n := h.records(); n != 0 {
		t.Fatalf("records = %d, want the quiet one swept by the demand pass", n)
	}

	h = newClaimLoopHarness(t)
	h.trip()
	h.advance(claimLoopForgetAfter + time.Second)
	h.fail(loopTemplate, "gcg--other", loopErr)
	if n := h.records(); n != 1 {
		t.Fatalf("records = %d, want only the new streak: a failure sweeps the quiet one too", n)
	}
}

// TestClaimLoopGuardIgnoresWhatItCannotKey: a failure with no template or no
// bead, or an undecodable payload, is no evidence about any pool.
func TestClaimLoopGuardIgnoresWhatItCannotKey(t *testing.T) {
	h := newClaimLoopHarness(t)
	for i := 0; i < claimLoopTripFailures+1; i++ {
		h.fail("", loopWisp, loopErr)
		h.fail(loopTemplate, "", loopErr)
		h.guard.observe(events.Event{Type: events.BeadClaimFailed, Subject: loopWisp, Payload: json.RawMessage(`{"template":`)})
	}
	if n := h.records(); n != 0 || len(h.alerts) != 0 {
		t.Fatalf("records=%d alerts=%d, want none", n, len(h.alerts))
	}
}

// TestClaimLoopGuardWithholdNeverEditsItsInputs: the demand maps belong to the
// pass; the guard answers with copies and only when it removes something.
func TestClaimLoopGuardWithholdNeverEditsItsInputs(t *testing.T) {
	h := newClaimLoopHarness(t)
	h.trip()
	counts := map[string]int{loopTemplate: 3, "other": 1}
	demand := map[string]scaleCheckDemand{
		loopTemplate: {Count: 3, WorkBeadIDs: []string{"gcg-a", loopWisp, "gcg-b"}},
		"other":      {Count: 1, WorkBeadIDs: []string{"gc-1"}},
	}
	gotCounts, gotDemand := h.guard.withhold(counts, demand)
	if gotCounts[loopTemplate] != 2 || !reflect.DeepEqual(gotDemand[loopTemplate].WorkBeadIDs, []string{"gcg-a", "gcg-b"}) || gotDemand[loopTemplate].Count != 2 {
		t.Fatalf("withheld demand = %v / %+v, want %s dropped from %s only", gotCounts, gotDemand[loopTemplate], loopWisp, loopTemplate)
	}
	if gotCounts["other"] != 1 || !reflect.DeepEqual(gotDemand["other"], demand["other"]) {
		t.Fatalf("another template's demand changed: %v / %+v", gotCounts, gotDemand["other"])
	}
	if counts[loopTemplate] != 3 || len(demand[loopTemplate].WorkBeadIDs) != 3 {
		t.Fatalf("withhold edited its inputs: %v / %+v", counts, demand[loopTemplate])
	}
	var nilGuard *claimLoopGuard
	if c, d := nilGuard.withhold(counts, demand); !reflect.DeepEqual(c, counts) || !reflect.DeepEqual(d, demand) {
		t.Fatal("a nil guard changed the demand")
	}
}

// TestClaimLoopGuardRegistryFollowsTheController: the demand passes find a
// city's guard by its path while its controller runs, a process with no
// controller finds none, and a stale unregistration never unpublishes the
// guard that replaced it.
func TestClaimLoopGuardRegistryFollowsTheController(t *testing.T) {
	cityPath := t.TempDir()
	if claimLoopGuardFor(cityPath) != nil {
		t.Fatal("a guard was registered before any controller ran")
	}
	first := newClaimLoopGuard(nil, io.Discard)
	unregisterFirst := registerClaimLoopGuard(context.Background(), cityPath, first)
	if claimLoopGuardFor(cityPath+"/") != first {
		t.Fatal("the registered guard is not found by its city path")
	}
	second := newClaimLoopGuard(nil, io.Discard)
	unregisterSecond := registerClaimLoopGuard(context.Background(), cityPath, second)
	unregisterFirst()
	if claimLoopGuardFor(cityPath) != second {
		t.Fatal("a stale unregistration removed the guard that replaced it")
	}
	unregisterSecond()
	if claimLoopGuardFor(cityPath) != nil {
		t.Fatal("the guard outlived its unregistration")
	}
}

// TestTickDeltaLanesFeedTheClaimLoopGuard pins the controller wiring end to
// end: starting the delta lanes publishes the runtime's guard for its city, and
// claim failures on the journal reach it through the shared feed, so both
// demand passes, which find the guard by city path, withhold the bead.
func TestTickDeltaLanesFeedTheClaimLoopGuard(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backing := events.NewFake()
	for i := 0; i < claimLoopTripFailures; i++ {
		payload, err := json.Marshal(events.BeadClaimFailedPayload{Template: loopTemplate, SessionID: fmt.Sprintf("gcg-session-%d", i), Error: loopErr})
		if err != nil {
			t.Fatal(err)
		}
		backing.Record(events.Event{Type: events.BeadClaimFailed, Ts: time.Now(), Subject: loopWisp, Payload: payload})
	}
	prov := &observedEventProvider{Provider: backing, observed: make(chan struct{}, 8), after: claimLoopTripFailures + 1}

	cr, _ := detachedOrphanRuntime(t)
	cr.cityPath = t.TempDir()
	cr.startTickDeltaLanes(ctx, prov)

	select {
	case <-prov.observed:
	case <-time.After(10 * time.Second):
		t.Fatal("the feed never consumed the claim failures")
	}
	guard := claimLoopGuardFor(cr.cityPath)
	if guard == nil || guard != cr.claimLoopGuardOf() {
		t.Fatalf("the demand passes find guard %p for the city, want the runtime's %p", guard, cr.claimLoopGuardOf())
	}
	counts, demand := guard.withhold(map[string]int{loopTemplate: 1}, map[string]scaleCheckDemand{loopTemplate: {Count: 1, WorkBeadIDs: []string{loopWisp}}})
	if counts[loopTemplate] != 0 || len(demand[loopTemplate].WorkBeadIDs) != 0 {
		t.Fatalf("withheld demand = %v / %+v, want the wisp held after the feed delivered %d failures", counts, demand[loopTemplate], claimLoopTripFailures)
	}
}

// TestDefaultDemandWithholdsAHeldBead runs both demand passes over one pool
// with three routed beads, one of them held: the legacy pass and the v2 gather
// must each count two.
func TestDefaultDemandWithholdsAHeldBead(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "gc"},
		Agents:    []config.Agent{{Name: "dog", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}},
	}
	city := beads.NewMemStore()
	var ids []string
	for i := 0; i < 3; i++ {
		b, err := city.Create(beads.Bead{Title: "routed", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "dog"}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, b.ID)
	}
	h := newClaimLoopHarness(t)
	h.guard.now = time.Now
	h.now = time.Now()
	for i := 0; i < claimLoopTripFailures; i++ {
		h.fail("dog", ids[1], loopErr)
	}
	t.Cleanup(registerClaimLoopGuard(context.Background(), cityPath, h.guard))

	legacy := buildDesiredStateWithSessionBeads("gc", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), city, nil, nil, nil, io.Discard)
	if got := legacy.ScaleCheckCounts["dog"]; got != 2 {
		t.Fatalf("legacy ScaleCheckCounts[dog] = %d, want 2 with %s held", got, ids[1])
	}
	view, err := gatherDemand(demandGatherEnv{CityName: "gc", CityPath: cityPath, Cfg: cfg, CityStore: city}, legacyDemandReads{})
	if err != nil {
		t.Fatalf("gatherDemand: %v", err)
	}
	if got := view.Collected.DefaultCounts["dog"]; got != 2 {
		t.Fatalf("v2 DefaultCounts[dog] = %d, want 2 with %s held", got, ids[1])
	}
	for _, id := range view.Collected.DefaultDemand["dog"].WorkBeadIDs {
		if id == ids[1] {
			t.Fatalf("v2 demand still lists held bead %s", id)
		}
	}
}

// TestHookClaimReportsAClaimFailureOnlyForARoutedCandidate pins the guard's
// input at its source: a routed candidate the seat skips because its claim
// write failed is reported with the error, and the seat still drains
// claims_errored. A ready assignment skipped the same way is not reported: it is
// assigned work, which wakes the seat it is assigned to and is never default
// spawn demand, so a hold could not stop anything and its alert would say it
// had.
func TestHookClaimReportsAClaimFailureOnlyForARoutedCandidate(t *testing.T) {
	writeErr := errors.New("claimIssue: bd serve at https://bdserve.example.test:30635 answered 503 busy")
	type failure struct {
		beadID string
		err    error
	}
	var got []failure
	ops := hookClaimOps{
		Claim: func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
			return beads.Bead{}, false, writeErr
		},
		EmitClaimFailed: func(beadID string, _ hookClaimOptions, err error) {
			got = append(got, failure{beadID, err})
		},
		DrainAck: func(io.Writer) error { return nil },
	}
	opts := hookClaimOptions{
		Assignee:           "bd.dog-1",
		IdentityCandidates: []string{"bd.dog-1"},
		RouteTargets:       []string{"bd.dog"},
		Env:                []string{"GC_TEMPLATE=bd.dog"},
		JSON:               true,
		DrainAck:           true,
	}
	routed := []beads.Bead{{ID: "gcg-1", Status: "open", Metadata: map[string]string{"gc.routed_to": "bd.dog"}}}
	var stdout, stderr bytes.Buffer
	if res := claimFirstEligibleHookCandidate(routed, opts, ops, "/city", &stdout, &stderr); res.terminal || !res.claimsErrored {
		t.Fatalf("routed tier = %+v, want a non-terminal claims_errored skip", res)
	}
	if len(got) != 1 || got[0].beadID != "gcg-1" || !errors.Is(got[0].err, writeErr) {
		t.Fatalf("claim failures reported = %+v, want gcg-1 with the write's error", got)
	}
	got = nil
	assigned := []beads.Bead{{ID: "gcg-2", Status: "open", Assignee: "bd.dog-1"}}
	ops.Claim = func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
		return beads.Bead{}, false, beads.ErrNotFound
	}
	if res := claimFirstReadyHookAssignment(assigned, opts, ops, "/city", &stdout, &stderr); res.terminal || !res.claimsErrored {
		t.Fatalf("ready tier = %+v, want a non-terminal claims_errored skip", res)
	}
	if len(got) != 0 {
		t.Fatalf("the ready tier reported %+v; assigned work is not spawn demand the guard can hold", got)
	}
}

// TestClaimFailedEventNamesTheSeatTemplateAndTheError pins the event the guard
// decodes, and that a hook outside any template's seat records nothing.
func TestClaimFailedEventNamesTheSeatTemplateAndTheError(t *testing.T) {
	long := strings.Repeat("x", claimLoopErrorMax+100)
	opts := hookClaimOptions{
		Assignee:     "bd.dog-1",
		Env:          []string{"GC_TEMPLATE=bd.dog", "GC_SESSION_ID=gcg-session-1"},
		StartedFence: hookStartedFence{Token: "token", Gen: 7, HasGen: true},
	}
	e, ok := claimFailedEvent(loopWisp, opts, errors.New("  "+long+"  "))
	if !ok {
		t.Fatal("no event for a templated seat's claim failure")
	}
	var payload events.BeadClaimFailedPayload
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if e.Type != events.BeadClaimFailed || e.Subject != loopWisp || e.SessionID != "gcg-session-1" || e.Actor != "bd.dog-1" || e.Message != long[:claimLoopErrorMax] ||
		payload.Template != "bd.dog" || payload.SessionID != "gcg-session-1" || payload.RuntimeEpoch != 7 || payload.Error != long[:claimLoopErrorMax] {
		t.Fatalf("event = %+v payload = %+v, want bead.claim_failed for %s by bd.dog with the bounded error", e, payload, loopWisp)
	}
	for name, tc := range map[string]struct {
		opts   hookClaimOptions
		beadID string
		err    error
	}{
		"no template": {hookClaimOptions{Env: []string{"GC_SESSION_ID=gcg-session-1"}}, loopWisp, errors.New("boom")},
		"no bead":     {opts, " ", errors.New("boom")},
		"no error":    {opts, loopWisp, nil},
	} {
		if _, ok := claimFailedEvent(tc.beadID, tc.opts, tc.err); ok {
			t.Errorf("%s: an event was built", name)
		}
	}
	opened := 0
	emitClaimFailedTo(func() events.Recorder { opened++; return events.Discard }, loopWisp, hookClaimOptions{}, errors.New("boom"))
	if opened != 0 {
		t.Fatal("the event log was opened for a failure with nothing to record")
	}
	if got := claimLoopErrorText("  héllo  "); got != "héllo" {
		t.Fatalf("claimLoopErrorText trims to %q", got)
	}
	cut := claimLoopErrorText(strings.Repeat("a", claimLoopErrorMax-1) + "é")
	if !strings.HasSuffix(cut, "a") || len(cut) != claimLoopErrorMax-1 {
		t.Fatalf("claimLoopErrorText split a rune: len %d", len(cut))
	}
}

// TestReloadThatChangesTheConfigReleasesClaimLoopHolds pins the reload hook in
// place: a reload that adopts a changed config releases every hold, so a fix
// made in city.toml is tried at once rather than after the hold.
func TestReloadThatChangesTheConfigReleasesClaimLoopHolds(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeClaimLoopCityConfig(t, tomlPath, 2)
	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := runtime.NewFake()
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)
	cr.sessionDrains = newDrainTracker()

	guard := cr.claimLoopGuardOf()
	for i := 0; i < claimLoopTripFailures; i++ {
		payload, err := json.Marshal(events.BeadClaimFailedPayload{Template: "test-city/dog", SessionID: fmt.Sprintf("gcg-session-%d", i), Error: loopErr})
		if err != nil {
			t.Fatal(err)
		}
		guard.observe(events.Event{Type: events.BeadClaimFailed, Ts: time.Now(), Subject: loopWisp, Payload: payload})
	}
	held := func() bool {
		counts, demand := guard.withhold(map[string]int{"test-city/dog": 1}, map[string]scaleCheckDemand{"test-city/dog": {Count: 1, WorkBeadIDs: []string{loopWisp}}})
		return counts["test-city/dog"] == 0 && len(demand["test-city/dog"].WorkBeadIDs) == 0
	}
	if !held() {
		t.Fatal("fixture: the wisp is not held before the reload")
	}
	before := cr.configRev

	writeClaimLoopCityConfig(t, tomlPath, 3)
	lastProviderName := "fake"
	cr.reloadConfig(context.Background(), &lastProviderName, cityPath)

	if cr.configRev == before {
		t.Fatalf("configRev stayed %q: the reload did not adopt the changed config", before)
	}
	if held() {
		t.Fatal("the hold survived a reload that changed the config")
	}
}

// writeClaimLoopCityConfig writes a one-pool city whose pool size is the only
// thing a test changes between reloads.
func writeClaimLoopCityConfig(t *testing.T, tomlPath string, maxActive int) {
	t.Helper()
	clearInheritedBeadsEnv(t)
	requireNoLeakedDoltAfterForPaths(t, filepath.Dir(tomlPath))
	data := fmt.Sprintf("[workspace]\nname = \"test-city\"\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"fake\"\n\n[[agent]]\nname = \"dog\"\nstart_command = \"true\"\nmax_active_sessions = %d\n", maxActive)
	if err := os.WriteFile(tomlPath, []byte(data), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}
