package main

import (
	"strings"
	"testing"
	"time"

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
