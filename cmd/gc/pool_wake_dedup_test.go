package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// Regression for review finding 3 on PR #4322.
//
// Wake dedup collapses several beads for one template into a single wake
// request, but it kept whichever bead it saw FIRST and dropped the rest. Dedup
// runs before the canonical sort, so a P0 arriving behind a P2 was discarded and
// the template competed for scarce capacity as a P2 — losing a shared cap to a
// P1 template that should have ranked behind it. The surviving request must be
// the one the pool's policy ranks best, not the first one enumerated.

func wakeWorkBead(id, status string, priority int, created time.Time) beads.Bead {
	b := workBead(id, "rig/claude", "rig/claude", status, priority)
	b.CreatedAt = created
	return b
}

func wakeRequestFor(t *testing.T, states []PoolDesiredState, template string) SessionRequest {
	t.Helper()
	var found []SessionRequest
	for _, ds := range states {
		for _, req := range ds.Requests {
			if req.Tier == "wake-known-identity" && req.Template == template {
				found = append(found, req)
			}
		}
	}
	if len(found) != 1 {
		t.Fatalf("wake requests for %q = %d, want exactly 1 (dedup must collapse to one)", template, len(found))
	}
	return found[0]
}

func TestWakeDedupKeepsBestBeadNotFirstBead(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	cfg := &config.City{Agents: []config.Agent{poolAgent("claude", "rig", nil, 0)}}

	// P2 is enumerated first; the P0 behind it is the bead the pool would
	// actually claim next.
	work := []beads.Bead{
		wakeWorkBead("w-p2", "in_progress", 2, base),
		wakeWorkBead("w-p0", "open", 0, base.Add(time.Hour)),
	}
	closed := closedPoolSessionBead()

	req := wakeRequestFor(t, ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads([]beads.Bead{closed}), nil), "rig/claude")

	if req.WorkBeadID != "w-p0" {
		t.Fatalf("wake request carries %q (P%d); want w-p0: dedup kept the first bead instead of the best, so the template competes for capacity as a P2",
			req.WorkBeadID, beads.PriorityValue(req.BeadPriority))
	}
	if beads.PriorityValue(req.BeadPriority) != 0 {
		t.Fatalf("wake request priority = P%d, want P0", beads.PriorityValue(req.BeadPriority))
	}
}

// The surviving request must carry its OWN bead's routing context. Keeping the
// best bead's id while leaving another bead's pack/workspace/brain-parent
// attached would bind the woken session to the wrong work context.
func TestWakeDedupCarriesTheWinningBeadsContext(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	cfg := &config.City{Agents: []config.Agent{poolAgent("claude", "rig", nil, 0)}}

	first := wakeWorkBead("w-p2", "in_progress", 2, base)
	first.Metadata["gc.pack"] = "pack-p2"
	best := wakeWorkBead("w-p0", "open", 0, base.Add(time.Hour))
	best.Metadata["gc.pack"] = "pack-p0"

	closed := closedPoolSessionBead()
	req := wakeRequestFor(t, ComputePoolDesiredStates(cfg, []beads.Bead{first, best}, sessionInfosFromBeads([]beads.Bead{closed}), nil), "rig/claude")

	if req.WorkPack != "pack-p0" {
		t.Fatalf("wake request pack = %q, want pack-p0: the request must carry the winning bead's context, not the first bead's", req.WorkPack)
	}
}

// Under fifo the best bead is the oldest one, not the most urgent — dedup must
// resolve through the pool's policy rather than a hardcoded priority order.
func TestWakeDedupHonorsFIFOPolicy(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	agent := poolAgent("claude", "rig", nil, 0)
	agent.SchedulingPolicy = "fifo"
	cfg := &config.City{Agents: []config.Agent{agent}}

	work := []beads.Bead{
		wakeWorkBead("w-new-p0", "in_progress", 0, base.Add(time.Hour)),
		wakeWorkBead("w-old-p2", "open", 2, base),
	}
	closed := closedPoolSessionBead()

	req := wakeRequestFor(t, ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads([]beads.Bead{closed}), nil), "rig/claude")

	if req.WorkBeadID != "w-old-p2" {
		t.Fatalf("fifo wake request carries %q, want w-old-p2: fifo must keep the oldest bead, not the most urgent", req.WorkBeadID)
	}
}
