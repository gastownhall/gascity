package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// Interrupted closes (#2085). A store without an atomic terminal close commits
// a session close as two writes: the ClosePatch metadata, then the status
// Close. When the Close is lost the bead stays open carrying this binary's own
// close code as its state. These tests pin that the reconciler finishes such a
// close instead of skipping the row as an unknown state forever, and that every
// other unrecognized state keeps the notify-only skip.

// halfCloseSessionBead writes the metadata half of a session close, as a
// two-write close whose status Close was lost leaves it.
func halfCloseSessionBead(t *testing.T, store beads.Store, id, code string, now time.Time, extra map[string]string) {
	t.Helper()
	patch := map[string]string(sessionpkg.ClosePatch(now, code))
	for k, v := range extra {
		patch[k] = v
	}
	if err := store.SetMetadataBatch(id, patch); err != nil {
		t.Fatalf("half-close %s as %q: %v", id, code, err)
	}
}

// newInterruptedClosePoolCity is a max=1 pool city whose only seat is the
// canonical pool identity, with ready work routed to the pool's template.
func newInterruptedClosePoolCity(t *testing.T, now time.Time) (string, *config.City, beads.Store, *runtime.Fake, beads.Bead) {
	t.Helper()
	cityPath := t.TempDir()
	writeCityTOML(t, cityPath, "trace-town", "worker")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "trace-town"},
		Session:   config.SessionConfig{Provider: "fake"},
		Agents: []config.Agent{{
			Name:              "worker",
			Dir:               "repo",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(1),
		}},
	}
	store := beads.NewMemStore()
	seat := createCanonicalPoolSession(t, store, &cfg.Agents[0], now, 1)
	setPoolSessionActive(t, store, seat.ID)
	return cityPath, cfg, store, runtime.NewFake(), seat
}

// A max=1 pool's only seat, half-closed in any of this binary's own close
// codes, closes on the first complete tick with the close it was interrupted
// in, and the pool's canonical identity then starts a replacement seat for the
// routed work it was holding back.
func TestInterruptedCloseFinishesPoolSeatAndReplacementStarts(t *testing.T) {
	cases := []struct {
		code  string
		extra map[string]string
	}{
		{code: "duplicate"},
		{code: "reconfigured"},
		{code: "gc_swept"},
		{code: "stale-session"},
		{code: "dead-runtime"},
		{code: "stranded-repair"},
		{code: "idle-timeout", extra: map[string]string{"sleep_reason": "idle-timeout"}},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			now := time.Now().UTC()
			cityPath, cfg, store, sp, seat := newInterruptedClosePoolCity(t, now)
			if _, err := store.Create(beads.Bead{
				Title:    "ready work behind the half-closed seat",
				Type:     "task",
				Status:   "open",
				Metadata: map[string]string{"gc.routed_to": "repo/worker"},
			}); err != nil {
				t.Fatalf("create routed work: %v", err)
			}
			halfCloseSessionBead(t, store, seat.ID, tc.code, now.Add(-time.Hour), tc.extra)

			dops := newDrainOps(sp)
			dt := newDrainTracker()
			tracker := &asyncStartTracker{}
			clk := &clock.Fake{Time: now}
			var out strings.Builder
			replacementRan := false
			for tick := 0; tick < 6 && !replacementRan; tick++ {
				drainAckRewakeTick(t, cityPath, cfg, sp, store, dops, dt, tracker, clk, &out)
				dumpDrainAckRewakeState(t, store, sp, tick)
				if tick == 0 {
					got, err := store.Get(seat.ID)
					if err != nil {
						t.Fatalf("reload seat: %v", err)
					}
					if got.Status != "closed" {
						t.Fatalf("half-closed %q seat %s status = %q after the first tick, want closed\n%s", tc.code, seat.ID, got.Status, out.String())
					}
					if got.Metadata["state"] != tc.code {
						t.Errorf("closed seat state = %q, want %q", got.Metadata["state"], tc.code)
					}
					if want := sessionpkg.CanonicalCloseReason(tc.code); got.Metadata["close_reason"] != want {
						t.Errorf("closed seat close_reason = %q, want %q", got.Metadata["close_reason"], want)
					}
				}
				open, err := loadSessionBeads(store)
				if err != nil {
					t.Fatalf("loadSessionBeads: %v", err)
				}
				for _, b := range open {
					if b.ID != seat.ID && b.Metadata["template"] == "repo/worker" && sp.IsRunning(b.Metadata["session_name"]) {
						replacementRan = true
					}
				}
				clk.Advance(30 * time.Second)
			}
			if !replacementRan {
				t.Fatalf("no replacement seat started for the pool's routed work after finishing the %q close\n%s", tc.code, out.String())
			}
		})
	}
}

// An always-mode configured named session whose bead was half-closed as
// stale-session closes on the first complete tick, stays closed, and the named
// identity comes back up on a fresh bead.
func TestInterruptedCloseFinishesNamedSeatAndIdentityRestarts(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "lead",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{Template: "lead", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "lead")
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	now := time.Now().UTC()
	clk := &clock.Fake{Time: now}
	sp := runtime.NewFake()
	seat, err := store.Create(beads.Bead{
		Title:  sessionName,
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               sessionName,
			"alias":                      "lead",
			"template":                   "lead",
			"state":                      "active",
			"generation":                 "2",
			"instance_token":             "tok-1",
			"continuation_epoch":         "1",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "lead",
			namedSessionModeMetadata:     "always",
		},
	})
	if err != nil {
		t.Fatalf("create named seat: %v", err)
	}
	halfCloseSessionBead(t, store, seat.ID, "stale-session", now.Add(-time.Hour), nil)

	dops := newDrainOps(sp)
	dt := newDrainTracker()
	tracker := &asyncStartTracker{}
	var out strings.Builder
	restarted := false
	for tick := 0; tick < 6 && !restarted; tick++ {
		drainAckRewakeTick(t, cityPath, cfg, sp, store, dops, dt, tracker, clk, &out)
		dumpDrainAckRewakeState(t, store, sp, tick)
		got, err := store.Get(seat.ID)
		if err != nil {
			t.Fatalf("reload seat: %v", err)
		}
		if got.Status != "closed" {
			t.Fatalf("tick %d: half-closed named seat %s status = %q, want closed from the first tick on\n%s", tick, seat.ID, got.Status, out.String())
		}
		open, err := loadSessionBeads(store)
		if err != nil {
			t.Fatalf("loadSessionBeads: %v", err)
		}
		for _, b := range open {
			if b.ID != seat.ID && b.Metadata["session_name"] == sessionName && sp.IsRunning(sessionName) {
				restarted = true
			}
		}
		clk.Advance(30 * time.Second)
	}
	if !restarted {
		t.Fatalf("named identity %s never came back up on a fresh bead after its interrupted close was finished\n%s", sessionName, out.String())
	}
}

// createAssignedInProgressWork creates in_progress work routed to template and
// assigned to the seat's bead ID.
func createAssignedInProgressWork(t *testing.T, store beads.Store, template, assignee string) beads.Bead {
	t.Helper()
	work, err := store.Create(beads.Bead{
		Title:    "work held by the half-closed seat",
		Type:     "task",
		Assignee: assignee,
		Metadata: map[string]string{"gc.routed_to": template},
	})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	status := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("mark work in_progress: %v", err)
	}
	return work
}

// A finished close keeps the gate of the close it finishes: a duplicate close
// is work-gated, so a half-closed duplicate holding in_progress work stays
// open and keeps it; a stale-session close releases the seat's work, so the
// half-closed stale seat closes and its work returns to the pool unassigned.
func TestInterruptedCloseKeepsTheInterruptedCloseGate(t *testing.T) {
	t.Run("duplicate_is_work_gated", func(t *testing.T) {
		now := time.Now().UTC()
		cityPath, cfg, store, sp, seat := newInterruptedClosePoolCity(t, now)
		work := createAssignedInProgressWork(t, store, "repo/worker", seat.ID)
		halfCloseSessionBead(t, store, seat.ID, "duplicate", now.Add(-time.Hour), nil)

		var out strings.Builder
		drainAckRewakeTick(t, cityPath, cfg, sp, store, newDrainOps(sp), newDrainTracker(), &asyncStartTracker{}, &clock.Fake{Time: now}, &out)

		got, err := store.Get(seat.ID)
		if err != nil {
			t.Fatalf("reload seat: %v", err)
		}
		if got.Status == "closed" {
			t.Fatalf("half-closed duplicate seat %s closed while it holds in_progress work %s; a duplicate close is work-gated\n%s", seat.ID, work.ID, out.String())
		}
		gotWork, err := store.Get(work.ID)
		if err != nil {
			t.Fatalf("reload work: %v", err)
		}
		if gotWork.Assignee != seat.ID || gotWork.Status != "in_progress" {
			t.Fatalf("work = status %q assignee %q, want in_progress assigned to %s", gotWork.Status, gotWork.Assignee, seat.ID)
		}
	})
	t.Run("stale_session_releases", func(t *testing.T) {
		now := time.Now().UTC()
		cityPath, cfg, store, sp, seat := newInterruptedClosePoolCity(t, now)
		work := createAssignedInProgressWork(t, store, "repo/worker", seat.ID)
		halfCloseSessionBead(t, store, seat.ID, "stale-session", now.Add(-time.Hour), nil)

		var out strings.Builder
		drainAckRewakeTick(t, cityPath, cfg, sp, store, newDrainOps(sp), newDrainTracker(), &asyncStartTracker{}, &clock.Fake{Time: now}, &out)

		got, err := store.Get(seat.ID)
		if err != nil {
			t.Fatalf("reload seat: %v", err)
		}
		if got.Status != "closed" {
			t.Fatalf("half-closed stale-session seat %s status = %q, want closed: a stale-session close releases its work\n%s", seat.ID, got.Status, out.String())
		}
		gotWork, err := store.Get(work.ID)
		if err != nil {
			t.Fatalf("reload work: %v", err)
		}
		if gotWork.Assignee != "" || gotWork.Status != "open" {
			t.Fatalf("work = status %q assignee %q, want open and unassigned", gotWork.Status, gotWork.Assignee)
		}
	})
}

// interruptedCloseTick runs one traced reconcile pass over the env's open
// session beads with sp as the provider.
func (e *reconcilerTestEnv) interruptedCloseTick(t *testing.T, sp runtime.Provider, storeQueryPartial bool, opts ...startExecutionOption) *sessionReconcilerTraceCycle {
	t.Helper()
	sessions, err := loadSessionBeads(e.store)
	if err != nil {
		t.Fatalf("loadSessionBeads: %v", err)
	}
	trace := newPoolDesiredStateTestTrace("worker")
	reconcileSessionBeadsTraced(
		context.Background(), e.city, sessions, e.desiredState, configuredSessionNames(e.cfg, "", e.store), e.cfg, sp,
		e.store, nil, nil, nil, nil, e.dt, map[string]int{}, storeQueryPartial, nil, "",
		nil, e.clk, e.rec, 0, 0, &e.stdout, &e.stderr, trace,
		append(append([]startExecutionOption{}, e.startOptions...), opts...)...,
	)
	return trace
}

// unknownStateEventsFor returns rec's session.unknown_state events for id.
func unknownStateEventsFor(t *testing.T, rec *capturingRecorder, id string) []api.SessionUnknownStatePayload {
	t.Helper()
	var out []api.SessionUnknownStatePayload
	for _, e := range rec.unknownStateEvents() {
		if e.Subject != id {
			continue
		}
		var payload api.SessionUnknownStatePayload
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatalf("decoding session.unknown_state payload: %v", err)
		}
		out = append(out, payload)
	}
	return out
}

// providerStops counts the Stop calls the fake provider received.
func providerStops(sp *runtime.Fake) int {
	n := 0
	for _, call := range sp.SnapshotCalls() {
		if call.Method == "Stop" {
			n++
		}
	}
	return n
}

// Every unrecognized state other than this binary's interrupted close codes
// keeps the notify-only skip: the row is never closed or otherwise written
// beyond the unknown-state markers, and the diagnostic still fires on first
// sight and escalates once past the threshold.
func TestInterruptedCloseLeavesUnrecognizedStatesToTheSkip(t *testing.T) {
	cases := []struct {
		name  string
		state string
		close bool
		extra map[string]string
	}{
		{name: "archived", state: "archived"},
		{name: "draining", state: "draining"},
		{name: "newer_binary_state", state: "hibernating"},
		{name: "padded_close_code", state: " duplicate", close: true},
		{name: "retire_state_reason", state: "duplicate-repair", close: true},
		{name: "passthrough_on_named_pool_row", state: "idle-timeout", close: true, extra: map[string]string{
			"sleep_reason":          "idle-timeout",
			"pool_managed":          "true",
			namedSessionMetadataKey: "true",
		}},
		{name: "passthrough_on_non_pool_row", state: "idle-timeout", close: true, extra: map[string]string{
			"sleep_reason": "idle-timeout",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newReconcilerTestEnv(t)
			rec := &capturingRecorder{}
			env.rec = rec
			b := env.createSessionBead("worker-held", "worker")
			patch := map[string]string{"state": tc.state}
			if tc.close {
				patch = map[string]string(sessionpkg.ClosePatch(env.clk.Now().Add(-time.Hour), tc.state))
			}
			for k, v := range tc.extra {
				patch[k] = v
			}
			if err := env.store.SetMetadataBatch(b.ID, patch); err != nil {
				t.Fatalf("seed %q: %v", tc.state, err)
			}
			before, err := env.store.Get(b.ID)
			if err != nil {
				t.Fatalf("reload seed: %v", err)
			}

			for tick := 0; tick < 3; tick++ {
				trace := env.interruptedCloseTick(t, env.sp, false)
				skipped := false
				for _, r := range trace.records {
					if r.SessionName == "worker-held" && r.ReasonCode == TraceReasonUnknownStateSkipped {
						skipped = true
					}
				}
				if !skipped {
					t.Fatalf("tick %d: no %s decision for %q; records: %+v", tick, TraceReasonUnknownStateSkipped, tc.state, trace.records)
				}
				if tick == 0 {
					env.clk.Advance(unknownStateEscalationAge + time.Minute)
				}
			}

			got, err := env.store.Get(b.ID)
			if err != nil {
				t.Fatalf("reload: %v", err)
			}
			if got.Status == "closed" {
				t.Fatalf("unrecognized state %q was closed; only this binary's interrupted close codes are finished", tc.state)
			}
			markers := map[string]bool{unknownStateFirstSeenKey: true, unknownStateValueKey: true, unknownStateEscalatedKey: true}
			for k, v := range got.Metadata {
				if markers[k] {
					continue
				}
				if before.Metadata[k] != v {
					t.Errorf("metadata %q = %q, want %q unchanged: the skip writes only its markers", k, v, before.Metadata[k])
				}
			}
			for k := range before.Metadata {
				if _, ok := got.Metadata[k]; !ok {
					t.Errorf("metadata %q removed, want unchanged", k)
				}
			}
			if got.Metadata[unknownStateEscalatedKey] == "" {
				t.Errorf("unknown-state escalation marker missing after %s unrecognized", unknownStateEscalationAge+time.Minute)
			}
			events := unknownStateEventsFor(t, rec, b.ID)
			if len(events) != 2 || events[0].Escalated || !events[1].Escalated {
				t.Fatalf("session.unknown_state events = %+v, want first sight then one escalation", events)
			}
			if n := providerStops(env.sp); n != 0 {
				t.Fatalf("provider Stop calls = %d, want 0", n)
			}
		})
	}
}

// An interrupted close is finished only once the row's runtime is confirmed not
// running: a live runtime, or a liveness probe that cannot tell, keeps the row
// open with its state unchanged, stops nothing, and leaves the row to the
// unknown-state skip exactly as before.
func TestInterruptedCloseKeepsLiveOrUnobservedRuntimeOpen(t *testing.T) {
	cases := []struct {
		name    string
		outcome TraceOutcomeCode
		sp      func(*runtime.Fake) runtime.Provider
	}{
		{name: "runtime_alive", outcome: TraceOutcomeKeptOpen, sp: func(f *runtime.Fake) runtime.Provider { return f }},
		{name: "liveness_error", outcome: TraceOutcomeSkippedLivenessError, sp: func(f *runtime.Fake) runtime.Provider {
			return &startUnavailableLivenessProvider{Fake: f}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newReconcilerTestEnv(t)
			rec := &capturingRecorder{}
			env.rec = rec
			b := env.createSessionBead("worker-zombie", "worker")
			halfCloseSessionBead(t, env.store, b.ID, "stale-session", env.clk.Now().Add(-time.Hour), nil)
			if err := env.sp.Start(context.Background(), "worker-zombie", runtime.Config{}); err != nil {
				t.Fatalf("start runtime: %v", err)
			}

			trace := env.interruptedCloseTick(t, tc.sp(env.sp), false)

			got, err := env.store.Get(b.ID)
			if err != nil {
				t.Fatalf("reload: %v", err)
			}
			if got.Status == "closed" || got.Metadata["state"] != "stale-session" {
				t.Fatalf("bead status=%q state=%q, want open with state stale-session", got.Status, got.Metadata["state"])
			}
			var finisher, skipped int
			for _, r := range trace.records {
				if r.SessionName != "worker-zombie" {
					continue
				}
				if r.SiteCode == TraceSiteCode("reconciler.session.finish_interrupted_close") &&
					r.ReasonCode == TraceReasonCode("interrupted_close") && r.OutcomeCode == tc.outcome {
					finisher++
				}
				if r.ReasonCode == TraceReasonUnknownStateSkipped {
					skipped++
				}
			}
			if finisher != 1 || skipped != 1 {
				t.Fatalf("finisher %s records = %d, unknown_state_skipped records = %d, want 1 each; records: %+v", tc.outcome, finisher, skipped, trace.records)
			}
			if events := unknownStateEventsFor(t, rec, b.ID); len(events) != 1 || events[0].Escalated {
				t.Fatalf("session.unknown_state events = %+v, want one first-sight event", events)
			}
			if n := providerStops(env.sp); n != 0 {
				t.Fatalf("provider Stop calls = %d, want 0", n)
			}
			if !env.sp.IsRunning("worker-zombie") {
				t.Fatal("runtime worker-zombie stopped, want it left running")
			}
		})
	}
}
