package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// wakeReasons are the awake-set reasons the wake record carries: the five
// the architect ruling (ga-j8b1coz) names.
var wakeReasons = []string{"routed-demand", "named-demand", "work-query", "pending-create", "explicit-wake"}

// Kills a legacy commit that drops the awake-set reason: session.woke
// carries the candidate's reason as its envelope Message, for each reason the
// awake set gives, and a start the awake set never decided (no reason on its
// candidate) records an empty Message and does not panic.
func TestCommitStartResultRecordsAwakeReasonAsSessionWokeMessage(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 3, 18, 12, 0, 1, 0, time.UTC)}
	for _, reason := range append([]string{""}, wakeReasons...) {
		t.Run("reason="+reason, func(t *testing.T) {
			store := beads.NewMemStore()
			row, err := store.Create(beads.Bead{
				Title:    "helper",
				Type:     sessionBeadType,
				Labels:   []string{sessionBeadLabel},
				Metadata: map[string]string{"session_name": "sky", "state": "creating"},
			})
			if err != nil {
				t.Fatal(err)
			}
			result := startResult{
				prepared: preparedStart{
					candidate: startCandidate{
						info:        sessiontest.SeedBead(t, row),
						tp:          TemplateParams{SessionName: "sky", TemplateName: "helper"},
						awakeReason: reason,
					},
					coreHash: "core",
					liveHash: "live",
				},
				outcome:  "success",
				started:  time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC),
				finished: time.Date(2026, 3, 18, 12, 0, 1, 0, time.UTC),
			}
			rec := events.NewFake()
			if !commitStartResult(result, sessionFrontDoor(store), clk, rec, 0, ioDiscard{}, ioDiscard{}) {
				t.Fatal("commitStartResult returned false for a successful start")
			}
			woke, err := rec.List(events.Filter{Type: events.SessionWoke})
			if err != nil {
				t.Fatal(err)
			}
			if len(woke) != 1 || woke[0].Message != reason {
				t.Fatalf("session.woke events = %+v, want exactly one with Message %q", woke, reason)
			}
		})
	}
}

// Kills a v2 launch that drops the awake-set reason: the effect records
// session.woke with the start intent's reason as its Message once the commit
// lands, for each reason, and a start whose intent carries none records an
// empty Message.
func TestLaunchRecordsAwakeReasonAsSessionWokeMessage(t *testing.T) {
	for _, reason := range append([]string{""}, wakeReasons...) {
		t.Run("reason="+reason, func(t *testing.T) {
			k, _ := launchKit(t, "asleep")
			k.it.AwakeReason = reason
			rec := events.NewFake()
			k.p.held.start.rec = rec
			if s := k.runKind(); s.Outcome != settledLanded {
				t.Fatalf("settlement %+v, want landed", s)
			}
			if len(rec.Events) != 1 || rec.Events[0].Type != events.SessionWoke || rec.Events[0].Message != reason {
				t.Fatalf("recorded %+v, want one session.woke with Message %q", rec.Events, reason)
			}
		})
	}
}

// Kills a wake-decision payload that reports a reason the awake set never
// gave: awake_reason is the decision's reason only when the awake set decided
// the row, and is omitted otherwise (no empty key, no panic on a nil payload).
func TestWithAwakeReason(t *testing.T) {
	for _, reason := range wakeReasons {
		got := withAwakeReason(traceRecordPayload{"should_wake": true}, AwakeDecision{ShouldWake: true, Reason: reason}, true)
		if got["awake_reason"] != reason || got["should_wake"] != true {
			t.Fatalf("decided %q: payload %v, want awake_reason %q and the existing keys kept", reason, got, reason)
		}
	}
	for name, tc := range map[string]struct {
		payload traceRecordPayload
		d       AwakeDecision
		hasDec  bool
	}{
		"no decision":           {traceRecordPayload{"should_wake": true}, AwakeDecision{Reason: "routed-demand"}, false},
		"decision, no reason":   {traceRecordPayload{"should_wake": true}, AwakeDecision{ShouldWake: true}, true},
		"no decision, nil body": {nil, AwakeDecision{}, false},
	} {
		got := withAwakeReason(tc.payload, tc.d, tc.hasDec)
		if _, present := got["awake_reason"]; present {
			t.Fatalf("%s: payload %v carries awake_reason, want it omitted", name, got)
		}
		if len(tc.payload) > 0 && got["should_wake"] != true {
			t.Fatalf("%s: payload %v lost its existing keys", name, got)
		}
	}
	// A decided row on a nil payload still records its reason.
	if got := withAwakeReason(nil, AwakeDecision{ShouldWake: true, Reason: "work-query"}, true); got["awake_reason"] != "work-query" {
		t.Fatalf("nil payload, decided: got %v, want awake_reason work-query", got)
	}
}

// Kills a reconciler whose three wake_decision records (failed_create,
// start_in_flight, start_candidate) omit the awake-set reason: each carries
// awake_reason: the reason the awake set gave the row. A pending-create row
// the awake set counts as creating under scale demand gets "scaled:creating"
// (the scaled arm supersedes "pending-create"); a failed-create row is not
// creating, so it keeps "pending-create".
func TestWakeDecisionTraceRecordsCarryAwakeReason(t *testing.T) {
	now := func(e *capacityEnv) string { return e.clk.Now().UTC().Format(time.RFC3339) }
	for _, tc := range []struct {
		name    string
		outcome TraceOutcomeCode
		extra   func(e *capacityEnv) map[string]string
		holder  bool // a live holder has the start's runtime lease: the start is in flight, not abandoned
		reason  string
	}{
		{"start_candidate", TraceOutcomeStartCandidate, func(*capacityEnv) map[string]string { return nil }, false, "scaled:creating"},
		{"failed_create", TraceOutcomeFailedCreate, func(*capacityEnv) map[string]string {
			return map[string]string{"state": string(sessionpkg.StateFailedCreate)}
		}, false, "pending-create"},
		{"start_in_flight", TraceOutcomeStartInFlight, func(e *capacityEnv) map[string]string {
			return map[string]string{"state": string(sessionpkg.StateCreating), "last_woke_at": now(e)}
		}, true, "scaled:creating"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newCapacityEnv(t, false, "a")
			row := e.pendingCreate(t, "a", "e", tc.extra(e))
			if tc.holder {
				held, err := sessionpkg.TryRuntimeLease(sessionFrontDoor(e.store), sessionpkg.RuntimeLeaseRequest{City: e.city, Name: "a", ID: row.info.ID, TTL: time.Minute})
				if err != nil {
					t.Fatal(err)
				}
				defer held.Release()
			}
			e.reconcileTraced(e.bead(t, row.info.ID))
			var seen []SessionReconcilerTraceRecord
			for _, r := range e.records(TraceSiteReconcilerWakeDecision) {
				if r.OutcomeCode == tc.outcome {
					seen = append(seen, r)
				}
			}
			if len(seen) == 0 {
				t.Fatalf("no wake_decision/%s record; records: %v", tc.name, e.records(TraceSiteReconcilerWakeDecision))
			}
			for _, r := range seen {
				if r.Fields["awake_reason"] != tc.reason {
					t.Fatalf("wake_decision/%s fields = %v, want awake_reason %q", tc.name, r.Fields, tc.reason)
				}
			}
		})
	}
}

// Kills a start path that loses the real reason between the awake set and
// session.woke: through the whole reconciler, legacy's tick and v2's pass
// each record the awake set's reason for the explicitly woken named row as the
// Message of the one session.woke they emit. That reason is "named-always": the
// named-session arm supersedes "explicit-wake" for a named row.
func TestSessionWokeCarriesTheAwakeReasonOnBothStartPaths(t *testing.T) {
	const fixture = "an explicit wake starts a parked named row"
	var f parityFixture
	for _, c := range parityFixtures() {
		if c.Name == fixture {
			f = c
		}
	}
	if f.Name == "" {
		t.Fatalf("parity fixture %q not found", fixture)
	}
	cityPath := t.TempDir()
	rows := f.converged(t, cityPath)

	legacy := newLegacyWorld(t, f, cityPath, cloneDiffBeads(rows))
	legacy.settle(t, f.Name)
	v2 := newV2World(t, f, cityPath, rows, nil, "", nil)
	v2.settle(t, f.Name)

	for path, rec := range map[string]*memRecorder{"legacy": legacy.rec, "v2": v2.rec} {
		rec.mu.Lock()
		var woke []events.Event
		for _, e := range rec.events {
			if e.Type == events.SessionWoke {
				woke = append(woke, e)
			}
		}
		rec.mu.Unlock()
		if len(woke) != 1 || woke[0].Message != "named-always" {
			t.Errorf("%s path: session.woke events = %+v, want exactly one with Message %q", path, woke, "named-always")
		}
	}
}
