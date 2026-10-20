package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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

// fail feeds one bead.claim_failed, stamped now.
func (h *claimLoopHarness) fail(template, beadID, errText string) {
	h.t.Helper()
	payload, err := json.Marshal(events.BeadClaimFailedPayload{Template: template, SessionID: "gcg-session-x", Error: errText})
	if err != nil {
		h.t.Fatal(err)
	}
	h.guard.observe(events.Event{Type: events.BeadClaimFailed, Ts: h.now, Subject: beadID, Payload: payload})
}

func (h *claimLoopHarness) advance(d time.Duration) { h.now = h.now.Add(d) }

// held reports whether the loop wisp is withheld from template's demand this
// pass.
func (h *claimLoopHarness) held(template string) bool {
	h.t.Helper()
	counts := map[string]int{template: 1}
	demand := map[string]scaleCheckDemand{template: {Count: 1, WorkBeadIDs: []string{loopWisp}}}
	gotCounts, gotDemand := h.guard.withhold(counts, demand)
	return gotCounts[template] == 0 && len(gotDemand[template].WorkBeadIDs) == 0
}

const (
	loopWisp     = "gcg--9223372036838841776"
	loopTemplate = "bd.dog"
	loopErr      = "claimIssue: bd serve at https://bdserve.example.test:30635 answered 404 not_found: no issue or wisp with that id"
)

// TestClaimLoopGuardHoldsABeadAfterRepeatedIdenticalFailures is the trip: the
// incident's shape, one bead failing the same way for every seat of one
// template, stops counting as that template's spawn demand at the third
// failure and raises one alert naming the bead, the template and the error.
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
	if !strings.Contains(h.stderr.String(), "holding "+loopWisp+" for "+loopTemplate) {
		t.Fatalf("stderr = %q, want the hold named", h.stderr.String())
	}
}

// TestClaimLoopGuardRequiresTheSameError: failures that differ are not a loop.
// A changed error restarts the streak, so it takes the full count of the NEW
// error to hold.
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

// TestClaimLoopGuardBacksOffWithOneProbePerHold: a hold that runs out admits
// the bead for one probe and then withholds it again while the probe reports;
// a probe that fails the same way doubles the hold, up to the cap.
func TestClaimLoopGuardBacksOffWithOneProbePerHold(t *testing.T) {
	h := newClaimLoopHarness(t)
	for i := 0; i < claimLoopTripFailures; i++ {
		h.fail(loopTemplate, loopWisp, loopErr)
	}
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
	}
	if len(h.alerts) != 1 {
		t.Fatalf("alerts = %d, want one: a failed probe extends the hold without a new alert", len(h.alerts))
	}
}

// TestClaimLoopGuardProbeThatNeverReportsIsRetried: a probe seat that never
// spawned or never reported does not wedge the bead out of demand forever.
func TestClaimLoopGuardProbeThatNeverReportsIsRetried(t *testing.T) {
	h := newClaimLoopHarness(t)
	for i := 0; i < claimLoopTripFailures; i++ {
		h.fail(loopTemplate, loopWisp, loopErr)
	}
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
	for i := 0; i < claimLoopTripFailures; i++ {
		h.fail(loopTemplate, loopWisp, loopErr)
	}
	h.guard.configChanged()
	if h.held(loopTemplate) {
		t.Fatal("still held after a config change")
	}
	h.fail(loopTemplate, loopWisp, loopErr)
	if h.held(loopTemplate) {
		t.Fatal("one failure after a config change held the bead; the streak must restart")
	}
}

// TestClaimLoopGuardForgetsQuietRecords: a held bead that stops reporting has
// left demand without an event saying so; its record must not live forever.
func TestClaimLoopGuardForgetsQuietRecords(t *testing.T) {
	h := newClaimLoopHarness(t)
	for i := 0; i < claimLoopTripFailures; i++ {
		h.fail(loopTemplate, loopWisp, loopErr)
	}
	h.advance(claimLoopForgetAfter + time.Second)
	if h.held(loopTemplate) {
		t.Fatal("a record quiet for longer than the forget horizon still holds")
	}
	h.guard.mu.Lock()
	n := len(h.guard.records)
	h.guard.mu.Unlock()
	if n != 0 {
		t.Fatalf("records = %d, want the quiet one swept", n)
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
	h.guard.mu.Lock()
	n := len(h.guard.records)
	h.guard.mu.Unlock()
	if n != 0 || len(h.alerts) != 0 {
		t.Fatalf("records=%d alerts=%d, want none", n, len(h.alerts))
	}
}

// TestClaimLoopGuardWithholdNeverEditsItsInputs: the demand maps belong to the
// pass; the guard answers with copies and only when it removes something.
func TestClaimLoopGuardWithholdNeverEditsItsInputs(t *testing.T) {
	h := newClaimLoopHarness(t)
	for i := 0; i < claimLoopTripFailures; i++ {
		h.fail(loopTemplate, loopWisp, loopErr)
	}
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
	for i := 0; i < claimLoopTripFailures; i++ {
		payload, _ := json.Marshal(events.BeadClaimFailedPayload{Template: "dog", Error: loopErr})
		h.guard.observe(events.Event{Type: events.BeadClaimFailed, Ts: time.Now(), Subject: ids[1], Payload: payload})
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

// TestHookClaimRecordsAClaimFailureForEverySkippedCandidate pins the guard's
// input at its source: each served candidate a seat skips because its claim
// write failed is reported with the seat's template and the error, on the
// routed tier and the ready-assignment tier alike, and the seat still drains
// claims_errored.
func TestHookClaimRecordsAClaimFailureForEverySkippedCandidate(t *testing.T) {
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
	assigned := []beads.Bead{{ID: "gcg-2", Status: "open", Assignee: "bd.dog-1"}}
	// The ready tier skips only a failure that proves the bead is elsewhere;
	// anything else is terminal there and reported differently.
	ops.Claim = func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
		return beads.Bead{}, false, beads.ErrNotFound
	}
	if res := claimFirstReadyHookAssignment(assigned, opts, ops, "/city", &stdout, &stderr); res.terminal || !res.claimsErrored {
		t.Fatalf("ready tier = %+v, want a non-terminal claims_errored skip", res)
	}
	if len(got) != 2 || got[0].beadID != "gcg-1" || !errors.Is(got[0].err, writeErr) || got[1].beadID != "gcg-2" || !errors.Is(got[1].err, beads.ErrNotFound) {
		t.Fatalf("claim failures reported = %+v, want gcg-1 (routed) then gcg-2 (ready)", got)
	}
}

// TestClaimFailedEventNamesTheSeatTemplateAndTheError pins the event the guard
// decodes, and that a hook outside any template's seat records nothing.
func TestClaimFailedEventNamesTheSeatTemplateAndTheError(t *testing.T) {
	long := strings.Repeat("x", claimLoopErrorMax+100)
	opts := hookClaimOptions{Assignee: "bd.dog-1", Env: []string{"GC_TEMPLATE=bd.dog", "GC_SESSION_ID=gcg-session-1"}}
	e, ok := claimFailedEvent(loopWisp, opts, errors.New("  "+long+"  "))
	if !ok {
		t.Fatal("no event for a templated seat's claim failure")
	}
	var payload events.BeadClaimFailedPayload
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if e.Type != events.BeadClaimFailed || e.Subject != loopWisp || e.SessionID != "gcg-session-1" || e.Actor != "bd.dog-1" ||
		payload.Template != "bd.dog" || payload.SessionID != "gcg-session-1" || payload.Error != long[:claimLoopErrorMax] {
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
	if got := claimLoopErrorText("  héllo  "); got != "héllo" {
		t.Fatalf("claimLoopErrorText trims to %q", got)
	}
	cut := claimLoopErrorText(strings.Repeat("a", claimLoopErrorMax-1) + "é")
	if !strings.HasSuffix(cut, "a") || len(cut) != claimLoopErrorMax-1 {
		t.Fatalf("claimLoopErrorText split a rune: len %d", len(cut))
	}
}
