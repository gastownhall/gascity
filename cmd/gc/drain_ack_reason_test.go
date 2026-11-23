package main

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// drainAckSeed creates the work a drain-acking seat keeps and returns the
// metadata to stamp on the seat's row (its started records), given the
// seat's ID and the work bead's ID.
type drainAckSeed struct {
	status  string
	labels  []string
	rowMeta func(sessionID, beadID string) map[string]string
	restart bool // the row carries a pending restart request
}

// create makes the kept bead, assigned to seatID, in the seed's status
// (a store creates every bead open).
func (seed drainAckSeed) create(t *testing.T, store beads.Store, seatID string) beads.Bead {
	t.Helper()
	work, err := store.Create(beads.Bead{Title: "kept work", Type: "task", Assignee: seatID, Labels: seed.labels})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	if seed.status != work.Status {
		status := seed.status
		if err := store.Update(work.ID, beads.UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("Update(work %s) to %s: %v", work.ID, status, err)
		}
		work.Status = status
	}
	return work
}

// drainAckAnomalyPayloads decodes every session.drain_acked_with_assigned_work
// payload in evs.
func drainAckAnomalyPayloads(t *testing.T, evs []events.Event) []api.SessionDrainAckedWithAssignedWorkPayload {
	t.Helper()
	var out []api.SessionDrainAckedWithAssignedWorkPayload
	for _, ev := range evs {
		if ev.Type != events.SessionDrainAckedWithAssignedWork {
			continue
		}
		var p api.SessionDrainAckedWithAssignedWorkPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatalf("decoding %s payload %s: %v", ev.Type, ev.Payload, err)
		}
		out = append(out, p)
	}
	return out
}

// legacyDrainAckAnomaly drives a worker seat that keeps seed's bead through
// the legacy drain-ack finalize (finalizeDrainAckStoppedSession) and returns
// the anomaly payloads it recorded, the seat's ID and the bead's ID.
func legacyDrainAckAnomaly(t *testing.T, seed drainAckSeed) ([]api.SessionDrainAckedWithAssignedWorkPayload, string, string) {
	t.Helper()
	return legacyDrainAckStop(t, seed, nil)
}

// legacyDrainAckStop is legacyDrainAckAnomaly with ack, when set, run as the
// seat's own gc runtime drain-ack on the seeded store just before the
// controller sees the ack.
func legacyDrainAckStop(t *testing.T, seed drainAckSeed, ack func(env *reconcilerTestEnv, seat, work beads.Bead)) ([]api.SessionDrainAckedWithAssignedWorkPayload, string, string) {
	t.Helper()
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	fake := events.NewFake()
	env.rec = fake

	seat := env.createSessionBead("worker", "worker")
	env.markSessionActive(&seat)
	work := seed.create(t, env.store, seat.ID)
	if seed.rowMeta != nil {
		env.setSessionMetadata(&seat, seed.rowMeta(seat.ID, work.ID))
	}
	if seed.restart {
		env.setSessionMetadata(&seat, map[string]string{"restart_requested": "true"})
	}

	if ack != nil {
		ack(env, seat, work)
	}
	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}
	reconcileSessionBeadsAtPath(
		context.Background(), env.city, []beads.Bead{seat}, env.desiredState, map[string]bool{"worker": true}, env.cfg, env.sp,
		env.store, dops, nil, nil, nil, env.dt, nil, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
		withAsyncDrainAckStopTracker(env.stops),
	)
	env.reconcileStopPendingToTerminal(t, env.sp, seat, dops, map[string]bool{"worker": true})
	return drainAckAnomalyPayloads(t, fake.Events), seat.ID, work.ID
}

// v2DrainAckAnomaly runs the stop verb on a signaled row that keeps seed's
// bead and returns the anomaly payloads its finalize recorded, the row's ID
// and the bead's ID.
func v2DrainAckAnomaly(t *testing.T, seed drainAckSeed) ([]api.SessionDrainAckedWithAssignedWorkPayload, string, string) {
	t.Helper()
	var seatID, beadID string
	var extra []string
	if seed.restart {
		extra = append(extra, "restart_requested", "true")
	}
	s, _, _ := stopRun(t, signaled(drainOrphaned, extra...), undesired(drainOrphaned), func(_ *effectPass, _ *fenceLeaf, store beads.Store, id string) {
		work := seed.create(t, store, id)
		seatID, beadID = id, work.ID
		if seed.rowMeta != nil {
			if err := store.SetMetadataBatch(id, seed.rowMeta(id, work.ID)); err != nil {
				t.Fatalf("stamping the row: %v", err)
			}
		}
	})
	if s.Outcome != settledLanded {
		t.Fatalf("stop verb settled %+v, want landed", s)
	}
	return drainAckAnomalyPayloads(t, s.Facts.Events), seatID, beadID
}

// startedBy stamps the seat's execution_claim_started record naming the bead.
func startedBy(_, beadID string) map[string]string {
	return map[string]string{executionClaimStartedKey: "tok-3|" + beadID}
}

// TestDrainAckAnomalyReasonShapes pins the shape discriminator both
// emitters of session.drain_acked_with_assigned_work set in the payload's
// reason (AC-1..AC-3): started_claim_kept for a started claim the stop kept,
// restart_requested for a stop that completed a restart request, and the
// generic drain_acked_with_assigned_work for work the row never started.
func TestDrainAckAnomalyReasonShapes(t *testing.T) {
	type emitter func(*testing.T, drainAckSeed) ([]api.SessionDrainAckedWithAssignedWorkPayload, string, string)
	for _, c := range []struct {
		name   string
		seed   drainAckSeed
		reason string
	}{
		{"AC-1 a started in_progress claim", drainAckSeed{status: "in_progress", rowMeta: startedBy}, "started_claim_kept"},
		{"AC-2 a started claim kept by a restart", drainAckSeed{status: "in_progress", rowMeta: startedBy, restart: true}, "restart_requested"},
		{"AC-2 open work kept by a restart", drainAckSeed{status: "open", restart: true}, "restart_requested"},
		{"AC-3 an open preassigned row", drainAckSeed{status: "open", rowMeta: startedBy}, "drain_acked_with_assigned_work"},
		{"AC-3 an in_progress claim the row never started", drainAckSeed{status: "in_progress"}, "drain_acked_with_assigned_work"},
		{"AC-3 an in_progress claim started records name another bead", drainAckSeed{status: "in_progress", rowMeta: func(string, string) map[string]string {
			return map[string]string{executionClaimStartedKey: "tok-3|gc-elsewhere"}
		}}, "drain_acked_with_assigned_work"},
		{"AC-3 a started claim on a dispatch hold", drainAckSeed{status: "in_progress", labels: []string{"hold:mayor"}, rowMeta: startedBy}, "drain_acked_with_assigned_work"},
	} {
		for _, e := range []struct {
			name string
			run  emitter
		}{{"legacy", legacyDrainAckAnomaly}, {"v2", v2DrainAckAnomaly}} {
			t.Run(c.name+"/"+e.name, func(t *testing.T) {
				got, seatID, beadID := e.run(t, c.seed)
				if len(got) != 1 {
					t.Fatalf("%s payloads = %+v, want exactly one", events.SessionDrainAckedWithAssignedWork, got)
				}
				p := got[0]
				if p.Reason != c.reason {
					t.Errorf("reason = %q, want %q", p.Reason, c.reason)
				}
				if p.SessionID != seatID || p.BeadID != beadID || p.Template != "worker" || p.BeadStatus != c.seed.status {
					t.Errorf("payload %+v, want session %q, bead %q, template worker, status %q", p, seatID, beadID, c.seed.status)
				}
			})
		}
	}
	// A work_started cell the release cannot decode reads as naming every
	// claim (recordedUnread); the cell is a CAS-only key, so the row is
	// built, not written.
	t.Run("AC-1 a claim an unreadable work_started cell may name", func(t *testing.T) {
		kept := beads.Bead{ID: "gc-9", Status: "in_progress"}
		if got := drainAckAnomalyReason(sessionpkg.Info{ID: "gc-1", WorkStartedRaw: "{not json"}, kept, false); got != api.DrainAckReasonStartedClaimKept {
			t.Errorf("reason = %q, want %q", got, api.DrainAckReasonStartedClaimKept)
		}
	})
}

// TestDrainAckParkPathReleasesAHeldStartedClaim (AC-7) pins the park path the
// drain-ack loop escalation prescribes: a seat holding a started claim that is
// then put on hold:mayor runs its own drain-ack; the ack succeeds (the release
// runs with the started refusal armed, as the seat's own ack runs it, and keeps
// nothing), releases the claim (open, unassigned, the hold kept) and the
// finalized stop records no session.drain_acked_with_assigned_work.
func TestDrainAckParkPathReleasesAHeldStartedClaim(t *testing.T) {
	var stderr bytes.Buffer
	got, _, _ := legacyDrainAckStop(t, drainAckSeed{status: "in_progress", labels: []string{beadmeta.HoldMayorLabel}, rowMeta: startedBy},
		func(env *reconcilerTestEnv, seat, work beads.Bead) {
			row, err := env.store.Get(seat.ID)
			if err != nil {
				t.Fatalf("reading the session bead: %v", err)
			}
			refused := releaseDrainAckClaims(testWorkLegs("", servingCity("worker"), env.store, nil), row, true, drainAckReleaseBudget, &stderr)
			if len(refused) != 0 {
				t.Fatalf("the seat's own drain-ack was refused over %v; a started claim on %s does not hold its seat's ack (stderr %s)", refused, beadmeta.HoldMayorLabel, stderr.String())
			}
			kept, err := env.store.Get(work.ID)
			if err != nil {
				t.Fatalf("reading the claim back: %v", err)
			}
			if kept.Status != "open" || kept.Assignee != "" || !slices.Contains(kept.Labels, beadmeta.HoldMayorLabel) {
				t.Errorf("after the seat's own ack, claim %s is status=%q assignee=%q labels=%q, want open, unassigned, %s kept; stderr=%s",
					kept.ID, kept.Status, kept.Assignee, kept.Labels, beadmeta.HoldMayorLabel, stderr.String())
			}
		})
	if len(got) != 0 {
		t.Errorf("%s payloads = %+v, want none once the held claim is released", events.SessionDrainAckedWithAssignedWork, got)
	}
}
