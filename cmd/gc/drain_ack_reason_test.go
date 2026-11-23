package main

import (
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// TestDrainAckAnomalyReasonShapes pins the reason shapes of
// session.drain_acked_with_assigned_work (gastownhall/gascity#6940): the
// payload's reserved reason says which shape of drain-ack-with-work a stop
// kept, so a pack-side subscriber can tell a livelock from a live
// sibling's claim and a restart handoff without re-deriving the facts. A
// stop that keeps an in_progress claim the row's own started records name
// (and no dispatch hold) is started_claim_kept; a stop that completes a
// restart-request cycle is restart_requested, whatever it keeps; every
// other kept bead — an open preassigned row, an in_progress claim the row
// never started, a started claim on a dispatch hold — keeps the generic
// drain_acked_with_assigned_work. Both reconcilers record the same shape
// for the same facts.
func TestDrainAckAnomalyReasonShapes(t *testing.T) {
	type shape struct {
		name   string
		reason func(t *testing.T) api.SessionDrainAckedWithAssignedWorkPayload
		want   string
	}
	for _, tc := range []shape{
		{
			name: "legacy keeps a started claim: started_claim_kept",
			reason: func(t *testing.T) api.SessionDrainAckedWithAssignedWorkPayload {
				payload := legacyDrainAckReason(t, seedStartedClaim)
				if payload.Template != "worker" || payload.BeadStatus != "in_progress" {
					t.Fatalf("payload %+v, want the worker template and the in_progress status unchanged", payload)
				}
				return payload
			},
			want: "started_claim_kept",
		},
		{
			name: "v2 keeps a started claim: started_claim_kept",
			reason: func(t *testing.T) api.SessionDrainAckedWithAssignedWorkPayload {
				return api.SessionDrainAckedWithAssignedWorkPayload{Reason: v2DrainAckReason(t, nil, v2SeedStartedClaim)}
			},
			want: "started_claim_kept",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.reason(t)
			if got.Reason != tc.want {
				t.Fatalf("reason = %q, want %q (payload %+v)", got.Reason, tc.want, got)
			}
		})
	}
}

// legacyDrainAckReason drives one legacy drain-ack finalize
// (finalizeDrainAckStoppedSession) with the work seed plants and returns
// the decoded payload of the one session.drain_acked_with_assigned_work
// event it records. The event must exist: the classifier fires on any
// assigned work the finalize keeps.
func legacyDrainAckReason(t *testing.T, seed func(t *testing.T, env *reconcilerTestEnv, row *beads.Bead) beads.Bead) api.SessionDrainAckedWithAssignedWorkPayload {
	t.Helper()
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	fake := events.NewFake()
	env.rec = fake

	row := env.createSessionBead("worker", "worker")
	kept := seed(t, env, &row)
	env.setSessionMetadata(&row, session.DrainAckStopPendingPatch(env.clk.Now().UTC()))
	info := env.sessionInfo(row.ID)
	finalizeDrainAckStoppedSession(
		env.city, env.cfg, env.store, testSeatWork(env.city, env.cfg, env.store, nil),
		info, "worker", false, newFakeDrainOps(), env.dt, env.clk, fake, &env.stderr,
	)

	var payload api.SessionDrainAckedWithAssignedWorkPayload
	matches := 0
	for i := range fake.Events {
		if fake.Events[i].Type != events.SessionDrainAckedWithAssignedWork {
			continue
		}
		matches++
		if err := json.Unmarshal(fake.Events[i].Payload, &payload); err != nil {
			t.Fatalf("decoding %s payload: %v", events.SessionDrainAckedWithAssignedWork, err)
		}
	}
	if matches != 1 {
		t.Fatalf("%s events = %d, want exactly 1 across the finalize (stderr %s)", events.SessionDrainAckedWithAssignedWork, matches, env.stderr.String())
	}
	if payload.BeadID != kept.ID || payload.SessionID != row.ID {
		t.Fatalf("payload names bead %q session %q, want the kept bead %q on session %q", payload.BeadID, payload.SessionID, kept.ID, row.ID)
	}
	return payload
}

// seedStartedClaim plants an in_progress claim assigned to the session that
// the session's own work_started cell names: the shape a drain-ack keeps
// and the OD9 fence refuses an own ack over.
func seedStartedClaim(t *testing.T, env *reconcilerTestEnv, row *beads.Bead) beads.Bead {
	t.Helper()
	kept := mustCreateDrainAckBead(t, env.store, beads.Bead{Title: "started work", Type: "task"}, "in_progress", row.ID)
	cell := session.EncodeWorkStarted(session.WorkStarted{}.With(session.WorkStartedEntry{
		Store: "city", Bead: kept.ID, Via: session.WorkStartedViaClaim,
	}))
	casSetMeta(t, env.store, row.ID, session.WorkStartedKey, cell)
	return kept
}

// v2DrainAckReason drives one v2 stop-verb finalize over a row carrying
// extraMeta whose setup plants work, and returns the reason of the one
// session.drain_acked_with_assigned_work event its facts record.
func v2DrainAckReason(t *testing.T, extraMeta []string, setup func(t *testing.T, store beads.Store, id string)) string {
	t.Helper()
	s, got, _ := stopRun(t, signaled(drainOrphaned, extraMeta...), undesired(drainOrphaned), func(_ *effectPass, _ *fenceLeaf, store beads.Store, id string) {
		setup(t, store, id)
	})
	if got.Metadata["state"] != "asleep" || got.Metadata["sleep_reason"] != "idle" {
		t.Fatalf("row %v, want the work-keeping drain to sleep the seat idle", got.Metadata)
	}
	var payload api.SessionDrainAckedWithAssignedWorkPayload
	matches := 0
	for _, ev := range s.Facts.Events {
		if ev.Type != events.SessionDrainAckedWithAssignedWork {
			continue
		}
		matches++
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			t.Fatalf("decoding %s payload: %v", events.SessionDrainAckedWithAssignedWork, err)
		}
	}
	if matches != 1 {
		t.Fatalf("%s events = %d, want exactly 1 in the stop's facts", events.SessionDrainAckedWithAssignedWork, matches)
	}
	return payload.Reason
}

// v2SeedStartedClaim plants the v2 stop's started-claim shape: an
// in_progress bead assigned to the row whose own work_started cell names
// it.
func v2SeedStartedClaim(t *testing.T, store beads.Store, id string) {
	t.Helper()
	kept := mustCreateDrainAckBead(t, store, beads.Bead{Title: "started work", Type: "task"}, "in_progress", id)
	casSetMeta(t, store, id, session.WorkStartedKey, session.EncodeWorkStarted(session.WorkStarted{}.With(session.WorkStartedEntry{
		Store: "city", Bead: kept.ID, Via: session.WorkStartedViaClaim,
	})))
}
