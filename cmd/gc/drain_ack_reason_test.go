package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
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
}
