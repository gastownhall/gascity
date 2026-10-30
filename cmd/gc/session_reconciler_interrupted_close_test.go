package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// A finished close keeps the gate of the close it finishes: a stale-session
// close releases the seat's work, so the half-closed stale seat closes and its
// work returns to the pool unassigned. A work-gated close that its gate now
// refuses is rolled back instead
// (TestInterruptedCloseRefusedByItsGateRejoinsTheLifecycle).
func TestInterruptedCloseKeepsTheInterruptedCloseGate(t *testing.T) {
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

// A work-gated interrupted close (duplicate, gc_swept, the pool-slot
// passthrough codes, killed) whose gate refuses it now, because work reached
// the seat after the metadata half landed, is rolled back to asleep on the
// first tick instead of being held on the unknown-state skip, and the paths
// that serve a dormant seat take it from the next tick: the assigned-work wake
// restarts a seat holding in_progress work in place on its own bead, and a
// killed seat gives back its open claim, closes as killed, and a replacement
// seat starts for the claim.
func TestInterruptedCloseRefusedByItsGateRejoinsTheLifecycle(t *testing.T) {
	cases := []struct {
		code      string
		extra     map[string]string
		openClaim bool   // the seat holds an open routed claim, not in_progress work
		wantSleep string // the rolled-back row's sleep_reason
	}{
		{code: "duplicate", wantSleep: "runtime-missing"},
		{code: "gc_swept", wantSleep: "runtime-missing"},
		{code: "idle-timeout", extra: map[string]string{"sleep_reason": "idle-timeout"}, wantSleep: "idle-timeout"},
		{code: "killed", extra: map[string]string{"sleep_reason": "killed"}, openClaim: true, wantSleep: "killed"},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			now := time.Now().UTC()
			cityPath, cfg, store, sp, seat := newInterruptedClosePoolCity(t, now)
			var work beads.Bead
			wantWorkStatus := "in_progress"
			if tc.openClaim {
				wantWorkStatus = "open"
				claim, err := store.Create(beads.Bead{
					Title:    "open claim held by the half-closed seat",
					Type:     "task",
					Status:   "open",
					Assignee: seat.ID,
					Metadata: map[string]string{"gc.routed_to": "repo/worker"},
				})
				if err != nil {
					t.Fatalf("create claim: %v", err)
				}
				work = claim
			} else {
				work = createAssignedInProgressWork(t, store, "repo/worker", seat.ID)
			}
			halfCloseSessionBead(t, store, seat.ID, tc.code, now.Add(-time.Hour), tc.extra)
			seatName := seat.Metadata["session_name"]

			dops := newDrainOps(sp)
			dt := newDrainTracker()
			tracker := &asyncStartTracker{}
			clk := &clock.Fake{Time: now}
			var out strings.Builder
			drainAckRewakeTick(t, cityPath, cfg, sp, store, dops, dt, tracker, clk, &out)
			dumpDrainAckRewakeState(t, store, sp, 0)

			got, err := store.Get(seat.ID)
			if err != nil {
				t.Fatalf("reload seat: %v", err)
			}
			if got.Status == "closed" || got.Metadata["state"] != "asleep" || got.Metadata["sleep_reason"] != tc.wantSleep {
				t.Fatalf("after the first tick seat = status %q state %q sleep_reason %q, want open asleep with sleep_reason %q: a refused %q close is rolled back\n%s",
					got.Status, got.Metadata["state"], got.Metadata["sleep_reason"], tc.wantSleep, tc.code, out.String())
			}
			if got.Metadata["close_reason"] != "" || got.Metadata["closed_at"] != "" {
				t.Errorf("rolled-back seat close_reason = %q closed_at = %q, want both cleared", got.Metadata["close_reason"], got.Metadata["closed_at"])
			}
			gotWork, err := store.Get(work.ID)
			if err != nil {
				t.Fatalf("reload work: %v", err)
			}
			if gotWork.Assignee != seat.ID || gotWork.Status != wantWorkStatus {
				t.Fatalf("after the rollback work = status %q assignee %q, want %s assigned to %s", gotWork.Status, gotWork.Assignee, wantWorkStatus, seat.ID)
			}
			warn := fmt.Sprintf("WARN: reconciler: rolled back interrupted %q close of session bead %s to asleep", tc.code, seat.ID)
			if n := strings.Count(out.String(), warn); n != 1 {
				t.Fatalf("stderr has %d %q lines, want 1\n%s", n, warn, out.String())
			}

			served := false
			for tick := 1; tick <= 6 && !served; tick++ {
				clk.Advance(30 * time.Second)
				drainAckRewakeTick(t, cityPath, cfg, sp, store, dops, dt, tracker, clk, &out)
				dumpDrainAckRewakeState(t, store, sp, tick)
				got, err = store.Get(seat.ID)
				if err != nil {
					t.Fatalf("reload seat: %v", err)
				}
				gotWork, err = store.Get(work.ID)
				if err != nil {
					t.Fatalf("reload work: %v", err)
				}
				open, err := loadSessionBeads(store)
				if err != nil {
					t.Fatalf("loadSessionBeads: %v", err)
				}
				if !tc.openClaim {
					if got.Status == "closed" {
						t.Fatalf("tick %d: seat %s closed while it holds in_progress work %s\n%s", tick, seat.ID, work.ID, out.String())
					}
					for _, b := range open {
						if b.ID != seat.ID && b.Metadata["template"] == "repo/worker" {
							t.Fatalf("tick %d: second seat %s opened for the template, want the seat restarted in place\n%s", tick, b.ID, out.String())
						}
					}
					served = sp.IsRunning(seatName) && gotWork.Assignee == seat.ID && gotWork.Status == "in_progress"
					continue
				}
				if got.Status != "closed" {
					continue
				}
				if got.Metadata["state"] != tc.code || got.Metadata["close_reason"] != sessionpkg.CanonicalCloseReason(tc.code) {
					t.Fatalf("tick %d: killed seat closed with state %q close_reason %q, want state %q close_reason %q",
						tick, got.Metadata["state"], got.Metadata["close_reason"], tc.code, sessionpkg.CanonicalCloseReason(tc.code))
				}
				if gotWork.Assignee == seat.ID {
					t.Fatalf("tick %d: closed killed seat still holds claim %s", tick, work.ID)
				}
				for _, b := range open {
					if b.ID != seat.ID && b.Metadata["template"] == "repo/worker" && sp.IsRunning(b.Metadata["session_name"]) {
						served = true
					}
				}
			}
			if !served {
				t.Fatalf("the rolled-back %q seat's work was never served; seat status %q state %q, work status %q assignee %q\n%s",
					tc.code, got.Status, got.Metadata["state"], gotWork.Status, gotWork.Assignee, out.String())
			}
		})
	}
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

			// First sight, mid-window (throttled silent), then past the
			// escalation threshold.
			advances := []time.Duration{5 * time.Minute, unknownStateEscalationAge - 4*time.Minute}
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
				if tick == 1 {
					if events := unknownStateEventsFor(t, rec, b.ID); len(events) != 1 {
						t.Fatalf("mid-window session.unknown_state events = %+v, want only the first-sight event (throttled)", events)
					}
				}
				if tick < len(advances) {
					env.clk.Advance(advances[tick])
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

	// The same seat half-closed in a recognized code closes on the same tick,
	// so the cases above stay open because they are unrecognized.
	t.Run("control_recognized_close_code_closes", func(t *testing.T) {
		env := newReconcilerTestEnv(t)
		b := env.createSessionBead("worker-held", "worker")
		halfCloseSessionBead(t, env.store, b.ID, "stale-session", env.clk.Now().Add(-time.Hour), nil)

		env.interruptedCloseTick(t, env.sp, false)

		got, err := env.store.Get(b.ID)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if got.Status != "closed" {
			t.Fatalf("recognized stale-session half-close status = %q, want closed", got.Status)
		}
	})
}

// An interrupted close is finished only once the row's runtime is confirmed not
// running: a live runtime, or a liveness probe that cannot tell, keeps the row
// open with its state unchanged, stops nothing, and leaves the row to the
// unknown-state skip exactly as before.
func TestInterruptedCloseKeepsLiveOrUnobservedRuntimeOpen(t *testing.T) {
	cases := []struct {
		name       string
		outcome    TraceOutcomeCode
		hold       string // the kept_open record's hold, when the finisher holds the row
		sp         func(*runtime.Fake) runtime.Provider
		wantStderr string
	}{
		{name: "runtime_alive", outcome: TraceOutcomeKeptOpen, hold: "runtime_alive", sp: func(f *runtime.Fake) runtime.Provider { return f }},
		{name: "liveness_error", outcome: TraceOutcomeSkippedLivenessError, sp: func(f *runtime.Fake) runtime.Provider {
			return &startUnavailableLivenessProvider{Fake: f}
		}, wantStderr: "session reconciler: skipping interrupted close of 'worker-zombie': liveness observation failed"},
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
					if tc.hold != "" && r.Fields["hold"] != tc.hold {
						t.Errorf("finisher %s record hold = %v, want %q", tc.outcome, r.Fields["hold"], tc.hold)
					}
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
			if got.Metadata[unknownStateFirstSeenKey] == "" {
				t.Fatal("unknown-state first-seen marker not stamped, want the skip's first-sight marker")
			}
			if tc.wantStderr != "" && !strings.Contains(env.stderr.String(), tc.wantStderr) {
				t.Fatalf("stderr = %q, want the fail-closed line %q", env.stderr.String(), tc.wantStderr)
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

// A tick on a partial store view, or the boot pass that defers session
// closes, never finishes an interrupted close: it records the skip with both
// flags, and the next complete steady-state tick finishes it.
func TestInterruptedCloseWaitsForACompleteSteadyStateTick(t *testing.T) {
	cases := []struct {
		name    string
		partial bool
		opts    []startExecutionOption
	}{
		{name: "store_query_partial", partial: true},
		{name: "defer_session_closes_on_boot", opts: []startExecutionOption{withDeferSessionClosesOnBoot()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newReconcilerTestEnv(t)
			b := env.createSessionBead("worker-gone", "worker")
			halfCloseSessionBead(t, env.store, b.ID, "stale-session", env.clk.Now().Add(-time.Hour), nil)

			trace := env.interruptedCloseTick(t, env.sp, tc.partial, tc.opts...)

			got, err := env.store.Get(b.ID)
			if err != nil {
				t.Fatalf("reload: %v", err)
			}
			if got.Status == "closed" {
				t.Fatal("interrupted close finished on a partial or boot tick, want it left open")
			}
			var skips []map[string]any
			for _, r := range trace.records {
				if r.SessionName == "worker-gone" &&
					r.SiteCode == TraceSiteCode("reconciler.session.finish_interrupted_close") &&
					r.OutcomeCode == TraceOutcomeSkipped {
					skips = append(skips, r.Fields)
				}
			}
			if len(skips) != 1 {
				t.Fatalf("finisher skipped records = %d, want 1; records: %+v", len(skips), trace.records)
			}
			if skips[0]["store_query_partial"] != tc.partial ||
				skips[0]["defer_session_closes_on_boot"] != (len(tc.opts) > 0) {
				t.Fatalf("skipped record fields = %+v, want store_query_partial=%v defer_session_closes_on_boot=%v", skips[0], tc.partial, len(tc.opts) > 0)
			}

			env.interruptedCloseTick(t, env.sp, false)

			got, err = env.store.Get(b.ID)
			if err != nil {
				t.Fatalf("reload after steady-state tick: %v", err)
			}
			if got.Status != "closed" {
				t.Fatalf("bead status after a complete steady-state tick = %q, want closed", got.Status)
			}
		})
	}
}

// A finished interrupted close says so: one WARN line naming the bead and its
// state, a closed trace record carrying the state and gate, and no
// unknown-state signal for the row it closed.
func TestInterruptedCloseReportsTheFinishedClose(t *testing.T) {
	env := newReconcilerTestEnv(t)
	rec := &capturingRecorder{}
	env.rec = rec
	b := env.createSessionBead("worker-gone", "worker")
	halfCloseSessionBead(t, env.store, b.ID, "stale-session", env.clk.Now().Add(-time.Hour), nil)

	trace := env.interruptedCloseTick(t, env.sp, false)

	got, err := env.store.Get(b.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("bead status = %q, want closed", got.Status)
	}
	warn := `WARN: reconciler: finished interrupted "stale-session" close of session bead ` + b.ID + ` — runtime not running`
	if n := strings.Count(env.stderr.String(), warn); n != 1 {
		t.Fatalf("stderr has %d %q lines, want 1; stderr:\n%s", n, warn, env.stderr.String())
	}
	var closed []map[string]any
	for _, r := range trace.records {
		if r.SessionName != "worker-gone" {
			continue
		}
		if r.ReasonCode == TraceReasonUnknownStateSkipped {
			t.Fatalf("unknown_state_skipped recorded for the finished close: %+v", r)
		}
		if r.SiteCode == TraceSiteCode("reconciler.session.finish_interrupted_close") &&
			r.ReasonCode == TraceReasonCode("interrupted_close") && r.OutcomeCode == TraceOutcomeClosed {
			closed = append(closed, r.Fields)
		}
	}
	if len(closed) != 1 || closed[0]["state"] != "stale-session" || closed[0]["gate"] != "releasing" {
		t.Fatalf("finisher closed records = %+v, want one with state=stale-session gate=releasing", closed)
	}
	if events := unknownStateEventsFor(t, rec, b.ID); len(events) != 0 {
		t.Fatalf("session.unknown_state events = %+v, want none for the finished close", events)
	}
}

// The finisher's trace says why a row it does not close stays the way it is: a
// gate refusal is a rollback carrying the refusal's cause, a runtime that
// cannot be torn down holds the row open as runtime_teardown (a live runtime
// is runtime_alive), and neither is the other.
func TestInterruptedCloseRecordsWhyItDidNotClose(t *testing.T) {
	finisherRecords := func(trace *sessionReconcilerTraceCycle, name string) []SessionReconcilerTraceRecord {
		var out []SessionReconcilerTraceRecord
		for _, r := range trace.records {
			if r.SessionName == name && r.SiteCode == TraceSiteReconcilerFinishInterruptedClose && r.ReasonCode == TraceReasonInterruptedClose {
				out = append(out, r)
			}
		}
		return out
	}
	t.Run("gate_refusal_rolls_back_with_its_cause", func(t *testing.T) {
		env := newReconcilerTestEnv(t)
		rec := &capturingRecorder{}
		env.rec = rec
		b := env.createSessionBead("worker-held", "worker")
		createAssignedInProgressWork(t, env.store, "worker", b.ID)
		halfCloseSessionBead(t, env.store, b.ID, "duplicate", env.clk.Now().Add(-time.Hour), nil)

		trace := env.interruptedCloseTick(t, env.sp, false)

		records := finisherRecords(trace, "worker-held")
		if len(records) != 1 || records[0].OutcomeCode != TraceOutcomeRollback ||
			records[0].Fields["cause"] != causeHasWork || records[0].Fields["sleep_reason"] != "runtime-missing" {
			t.Fatalf("finisher records = %+v, want one rollback with cause=%s sleep_reason=runtime-missing", records, causeHasWork)
		}
		for _, r := range trace.records {
			if r.SessionName == "worker-held" && r.ReasonCode == TraceReasonUnknownStateSkipped {
				t.Fatalf("unknown_state_skipped recorded for the rolled-back row: %+v", r)
			}
		}
		if events := unknownStateEventsFor(t, rec, b.ID); len(events) != 0 {
			t.Fatalf("session.unknown_state events = %+v, want none for the rolled-back row", events)
		}
		if got := env.sessionInfo(b.ID); got.Closed || got.MetadataState != "asleep" {
			t.Fatalf("row closed=%v state=%q, want open and asleep", got.Closed, got.MetadataState)
		}
	})
	t.Run("runtime_teardown_holds", func(t *testing.T) {
		env := newReconcilerTestEnv(t)
		b := env.createSessionBead("worker-pool", "worker")
		name := "worker-" + b.ID
		halfCloseSessionBead(t, env.store, b.ID, "stale-session", env.clk.Now().Add(-time.Hour), map[string]string{
			"session_name": name,
			"pool_managed": "true",
		})
		env.sp.StopErrors[name] = errors.New("stop refused")

		trace := env.interruptedCloseTick(t, env.sp, false)

		records := finisherRecords(trace, name)
		if len(records) != 1 || records[0].OutcomeCode != TraceOutcomeKeptOpen || records[0].Fields["hold"] != "runtime_teardown" {
			t.Fatalf("finisher records = %+v, want one kept_open with hold=runtime_teardown", records)
		}
		if got := env.sessionInfo(b.ID); got.Closed || got.MetadataState != "stale-session" {
			t.Fatalf("row closed=%v state=%q, want open and still stale-session", got.Closed, got.MetadataState)
		}
	})
}

// lostCloseStore loses the status Close while lose is set, after the close's
// metadata half landed: the two-write close of a store without an atomic
// terminal close, interrupted between its writes.
type lostCloseStore struct {
	*beads.MemStore
	lose bool
}

// Close loses the status Close while lose is set.
func (s *lostCloseStore) Close(id string) error {
	if s.lose {
		return fmt.Errorf("status close of %s lost", id)
	}
	return s.MemStore.Close(id)
}

// Tx runs fn on the store itself, so a close's Tx loses its Close too.
func (s *lostCloseStore) Tx(_ string, fn func(beads.Tx) error) error { return fn(s) }

// A close the store really interrupted (the production close, its Close lost
// after the ClosePatch landed) is recognized by its state alone. While the
// store keeps losing the Close the finisher's own commit does not land, so
// the row keeps its close code for a later tick and is not rolled back, and
// the first tick on which the Close lands finishes it.
func TestInterruptedCloseFinishesARealInterruptedCommit(t *testing.T) {
	env := newReconcilerTestEnv(t)
	store := &lostCloseStore{MemStore: beads.NewMemStore(), lose: true}
	env.store = store
	b := env.createSessionBead("worker-gone", "worker")
	sw := testSeatWork(env.city, env.cfg, env.store, nil)
	if closeSeat(env.store, sw, env.sessionInfo(b.ID), closeWhy{reason: "stale-session", gate: closeReleasing}, env.clk.Now().UTC(), &env.stderr) {
		t.Fatal("closeSeat reported a landed close, want the lost Close to fail it")
	}
	if got := env.sessionInfo(b.ID); got.Closed || got.MetadataState != "stale-session" {
		t.Fatalf("after the interrupted close row closed=%v state=%q, want open and stale-session", got.Closed, got.MetadataState)
	}

	trace := env.interruptedCloseTick(t, env.sp, false)

	var notLanded int
	for _, r := range trace.records {
		if r.SessionName == "worker-gone" && r.SiteCode == TraceSiteReconcilerFinishInterruptedClose &&
			r.OutcomeCode == TraceOutcomeNoChange && r.Fields["cause"] == "commit_not_landed" {
			notLanded++
		}
	}
	if notLanded != 1 {
		t.Fatalf("finisher no_change commit_not_landed records = %d, want 1; records: %+v", notLanded, trace.records)
	}
	if got := env.sessionInfo(b.ID); got.Closed || got.MetadataState != "stale-session" {
		t.Fatalf("after a tick whose Close was lost row closed=%v state=%q, want open and still stale-session", got.Closed, got.MetadataState)
	}

	store.lose = false
	env.interruptedCloseTick(t, env.sp, false)

	got, err := env.store.Get(b.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Status != "closed" || got.Metadata["state"] != "stale-session" {
		t.Fatalf("after a tick whose Close landed row status=%q state=%q, want closed stale-session", got.Status, got.Metadata["state"])
	}
	warn := `WARN: reconciler: finished interrupted "stale-session" close of session bead ` + b.ID
	if n := strings.Count(env.stderr.String(), warn); n != 1 {
		t.Fatalf("stderr has %d %q lines, want 1; stderr:\n%s", n, warn, env.stderr.String())
	}
}
