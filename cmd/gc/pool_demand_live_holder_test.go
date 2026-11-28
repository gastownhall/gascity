package main

// ga-6ab279: the demand-side twin of the wake filter's ownership arm.
//
// On a city that relocates nothing, filterAssignedWorkBeadsForPoolDemand kept
// a rig agent's assigned work only through rig-equality or a relocated class
// binding, so a live rig-pool session's own in-progress claim on a city-store
// bead (ref "") was dropped. The holder then counted as idle capacity, the
// queued bead's tier=new request was realized onto it, and the pool never ran
// a second session beside a working one — while the wake filter accepted the
// same claim for the same holder (assignedWorkClaimRefs includes the leading
// work arm). The rows here pin the widened demand arm: the bead is kept only
// when its ref is one the wake filter accepts for that holder, its assignee
// is an open session's own exact identity, that session's template is the
// template the bead routes to, and the session claims a live runtime.

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// liveHolderRows runs the v1 wrapper over one work row on the single-store
// fixture with the holder in the given state, and reports the kept beads and
// their refs.
func liveHolderRows(t *testing.T, holderState string, work []beads.Bead, refs []string) ([]beads.Bead, []string) {
	t.Helper()
	cfg, cityPath, infos := rigScopedPoolDemandFixtureWithHolderState(t, holderState)
	seedNoRoutes(t, cityPath)
	return filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, beads.NewMemStore(), infos, work, refs)
}

// The reported shape: a live (active) holder's own in-progress claim on a
// city-store bead, ref "", is kept with its own ref, index-aligned. Fails on
// the base: the row is dropped and the holder counts as idle.
func TestPoolDemandFilterKeepsALiveHoldersOwnCityStoreClaim(t *testing.T) {
	kept, keptRefs := liveHolderRows(t, "active", []beads.Bead{routedPoolWorkBead()}, []string{""})

	if len(kept) != 1 || kept[0].ID != "gcg-1" {
		t.Fatalf("filtered work = %#v, want the live holder's own city-store claim kept", kept)
	}
	if !slices.Equal(keptRefs, []string{""}) {
		t.Fatalf("kept refs = %q, want [\"\"]: the kept row's own ref, index-aligned", keptRefs)
	}
}

// A holder claiming a live runtime in either of its other spellings — a row
// still creating, one waiting to stop cleanly — is as much a working holder
// as an active one.
func TestPoolDemandFilterKeepsACreatingOrDrainingHoldersClaim(t *testing.T) {
	for _, state := range []string{"creating", "draining"} {
		t.Run(state, func(t *testing.T) {
			kept, _ := liveHolderRows(t, state, []beads.Bead{routedPoolWorkBead()}, []string{""})
			if len(kept) != 1 || kept[0].ID != "gcg-1" {
				t.Fatalf("filtered work = %#v, want the %s holder's own claim kept", kept, state)
			}
		})
	}
}

// A rig-resident row beside a live holder's city-store row: both kept, and
// the returned refs stay aligned with the kept beads.
func TestPoolDemandFilterKeepsALiveHolderClaimBesideARigRow(t *testing.T) {
	rigRow := routedPoolWorkBead()
	rigRow.ID = "gcg-2"
	work := []beads.Bead{rigRow, routedPoolWorkBead()}

	kept, keptRefs := liveHolderRows(t, "active", work, []string{"riga", ""})

	if ids := beadIDs(kept); !slices.Equal(ids, []string{"gcg-2", "gcg-1"}) {
		t.Fatalf("kept IDs = %v, want both the rig row and the live holder's row, in order", ids)
	}
	if !slices.Equal(keptRefs, []string{"riga", ""}) {
		t.Fatalf("kept refs = %q, want [riga \"\"] aligned with the kept beads", keptRefs)
	}
}

// ---------------------------------------------------------------------------
// buildDesiredState rows: the reported trace.
// ---------------------------------------------------------------------------

// liveHolderCity builds the reported shape: a single-store city with rig pool
// riga/worker (max_active_sessions 5, scale_check reporting 1), an active
// riga/worker session holding in_progress city-store bead A (assigned by its
// session_name) routed to riga/worker, and open unassigned city-store bead B
// routed to riga/worker. It returns the desired state's riga/worker session
// names.
func liveHolderCity(t *testing.T, heldPriority, queuedPriority *int) []string {
	t.Helper()
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "riga")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("create rig dir: %v", err)
	}
	cityStore := beads.NewMemStore()
	if _, err := cityStore.Create(beads.Bead{
		Title:  "working rig worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:riga/worker"},
		Metadata: map[string]string{
			"template":     "riga/worker",
			"session_name": "worker-gc-1",
			"state":        "active",
			"pool_managed": "true",
		},
	}); err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	held, err := cityStore.Create(beads.Bead{
		Title:    "held city work",
		Type:     "task",
		Assignee: "worker-gc-1",
		Priority: heldPriority,
		Metadata: map[string]string{"gc.routed_to": "riga/worker"},
	})
	if err != nil {
		t.Fatalf("create held bead: %v", err)
	}
	if err := cityStore.Update(held.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("set held in_progress: %v", err)
	}
	if _, err := cityStore.Create(beads.Bead{
		Title:    "queued city work",
		Type:     "task",
		Priority: queuedPriority,
		Metadata: map[string]string{"gc.routed_to": "riga/worker"},
	}); err != nil {
		t.Fatalf("create queued bead: %v", err)
	}
	sessionSnapshot, err := loadSessionBeadSnapshot(cityStore)
	if err != nil {
		t.Fatalf("load session snapshot: %v", err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker-gc-1", runtime.Config{}); err != nil {
		t.Fatalf("start fake holder: %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "riga", Path: rigPath}},
		Agents: []config.Agent{{
			Name:              "worker",
			Dir:               "riga",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(5),
			ScaleCheck:        printCountCommand(1),
		}},
	}
	dsResult := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), cfg, sp,
		cityStore, map[string]beads.Store{"riga": beads.NewMemStore()}, sessionSnapshot, nil, io.Discard,
	)
	var names []string
	for name, tp := range dsResult.State {
		if tp.TemplateName == "riga/worker" {
			names = append(names, name)
		}
	}
	return names
}

// The reported shape end to end: the holder's counted claim yields its resume
// request and the queued bead's scale_check count a fresh session, so the
// desired state holds two riga/worker sessions — the holder by its
// session_name plus one fresh session. On the base the holder's claim is
// dropped, the holder counts as reusable, and the queued bead's tier=new
// request is realized onto it: one session, no create.
func TestBuildDesiredState_RigPoolCountsALiveHoldersCityStoreClaim(t *testing.T) {
	names := liveHolderCity(t, nil, nil)

	if len(names) != 2 {
		t.Fatalf("riga/worker desired sessions = %v (%d), want 2: the busy holder plus a fresh session for the queued bead", names, len(names))
	}
	if !slices.Contains(names, "worker-gc-1") {
		t.Fatalf("riga/worker desired sessions = %v, want the holder's own session_name among them", names)
	}
}

// The same shape with the queued bead at P0 and the held one at P4: the
// concrete resume request is admitted before anonymous new demand whatever the
// bead priorities, and the used holder is refused to the queued bead's
// tier=new request, so the outcome is still the holder plus a fresh session —
// the queued bead never takes the busy holder.
func TestBuildDesiredState_RigPoolHigherPriorityQueuedWorkDoesNotTakeTheBusyHolder(t *testing.T) {
	names := liveHolderCity(t, intPtr(4), intPtr(0))

	if len(names) != 2 {
		t.Fatalf("riga/worker desired sessions = %v (%d), want 2: the busy holder plus a fresh session for the queued bead", names, len(names))
	}
	if !slices.Contains(names, "worker-gc-1") {
		t.Fatalf("riga/worker desired sessions = %v, want the holder's own session_name among them", names)
	}
}

// A dormant holder — asleep, drained or suspended — does not claim a live
// runtime, so its city-store claim stays dropped: sessionBeadClaimsLiveRuntime
// is the gate that keeps the widened arm from resuming a sleeping rig seat,
// the reachability rule TestBuildDesiredState_RigPoolIgnoresAssignedWorkInUnreachableStore
// pins and TestPoolDemandFilterUnchangedOnASingleStoreCity mirrors for the
// template-scoped arm.
func TestPoolDemandFilterStillDropsADormantHoldersCityStoreClaim(t *testing.T) {
	for _, state := range []string{"asleep", "drained", "suspended"} {
		t.Run(state, func(t *testing.T) {
			kept, _ := liveHolderRows(t, state, []beads.Bead{routedPoolWorkBead()}, []string{""})
			if len(kept) != 0 {
				t.Fatalf("filtered work = %#v, want the %s holder's city-store claim still dropped", kept, state)
			}
		})
	}
}

// A closed session resolves no assignee at all, so a claim recorded under its
// identity is dropped exactly as before the widening.
func TestPoolDemandFilterStillDropsAClosedHoldersCityStoreClaim(t *testing.T) {
	cfg, cityPath, _ := rigScopedPoolDemandFixtureWithHolderState(t, "active")
	seedNoRoutes(t, cityPath)
	closed := []beads.Bead{{
		ID:     "gcs-1",
		Status: "closed",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"template":     "riga/worker",
			"session_name": "test-city--worker-1",
			"state":        "active",
			"pool_managed": "true",
		},
	}}

	kept, _ := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, beads.NewMemStore(), sessionInfosFromBeads(closed), []beads.Bead{routedPoolWorkBead()}, []string{""})

	if len(kept) != 0 {
		t.Fatalf("filtered work = %#v, want the closed session's claim dropped", kept)
	}
}

// The widening is ownership, not scope: rows the rig-reach test rejects that
// are NOT the live holder's own matching claim stay dropped. A bead assigned
// to the live holder but routed to another template belongs to that other
// template's pool; a bead under another rig's ref sits in a store this
// holder's hook cannot claim from; unassigned city-store work reaches no rig
// pool on a single-store city; and an assignee that is only the template
// identity is a scope statement, not an ownership one, so it grants nothing.
func TestPoolDemandFilterStillDropsRowsTheHolderDoesNotOwn(t *testing.T) {
	toRigb := routedPoolWorkBead()
	toRigb.Metadata = map[string]string{"gc.routed_to": "rigb/worker"}
	foreignRef := routedPoolWorkBead()
	unassigned := routedPoolWorkBead()
	unassigned.Assignee = ""
	templateIdentity := routedPoolWorkBead()
	templateIdentity.Assignee = "riga/worker"
	for _, tc := range []struct {
		name string
		work []beads.Bead
		refs []string
	}{
		{name: "assigned to the holder, routed to another template", work: []beads.Bead{toRigb}, refs: []string{""}},
		{name: "assigned to the holder under another rig's ref", work: []beads.Bead{foreignRef}, refs: []string{"rigb"}},
		{name: "unassigned city-store work", work: []beads.Bead{unassigned}, refs: []string{""}},
		{name: "assigned only to the template identity", work: []beads.Bead{templateIdentity}, refs: []string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kept, _ := liveHolderRows(t, "active", tc.work, tc.refs)
			if len(kept) != 0 {
				t.Fatalf("filtered work = %#v, want dropped: %s", kept, tc.name)
			}
		})
	}
}

// The v2 allocator over the same shape: it threads the demand's WakeClaimRefs
// into the pool-demand filter, so it counts the live holder's city-store claim
// beside the custom count exactly as v1 does — no explained difference.
func TestPoolDemandV2AllocationCountsALiveHoldersCityStoreClaim(t *testing.T) {
	out := v2Allocation(t, liveRigPoolHolderDiffCity(t))

	if out.accepted["riga/worker"] != 2 {
		t.Fatalf("v2 accepted riga/worker = %d, want 2: the live holder's city-store claim beside the custom count", out.accepted["riga/worker"])
	}
}
