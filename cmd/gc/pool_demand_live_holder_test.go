package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// A rig-scoped pool whose work sits in the CITY store (station beads filed in
// the city store, routed to a rig pool). The pool-demand filter gates a rig
// agent's work to its own rig's store, so a LIVE pool session's city-store
// claim was dropped: it produced no resume request, the next unclaimed bead's
// new demand was realized onto that busy session, and the pool never ran a
// second session beside it.
//
// The holder's own claim is admitted under the refs the wake filter accepts
// for that holder (assignedWorkClaimRefs), and only while the holder claims a
// live runtime. Unassigned city-store work, work held by an asleep session
// (TestBuildDesiredState_RigPoolIgnoresAssignedWorkInUnreachableStore), work
// held by another template's session, and work in another rig's store stay
// dropped.

func liveHolderPoolDemandFixture(t *testing.T, state string) (*config.City, string, []sessionpkg.Info) {
	t.Helper()
	cfg, cityPath, _ := rigScopedPoolDemandFixture(t)
	seedNoRoutes(t, cityPath)
	sessions := []beads.Bead{{
		ID:     "gcs-1",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"template":     "riga/worker",
			"session_name": "test-city--worker-1",
			"state":        state,
			"pool_managed": "true",
		},
	}}
	return cfg, cityPath, sessionInfosFromBeads(sessions)
}

func TestPoolDemandFilterKeepsALiveHoldersCityStoreClaim(t *testing.T) {
	cfg, cityPath, infos := liveHolderPoolDemandFixture(t, "active")
	workBeads := []beads.Bead{routedPoolWorkBead()}

	kept, _ := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, beads.NewMemStore(), infos, workBeads, []string{""})
	if len(kept) != 1 || kept[0].ID != "gcg-1" {
		t.Fatalf("filtered work = %#v, want the live holder's city-store claim kept so it resumes its holder", kept)
	}
}

func TestPoolDemandFilterDropsAnAsleepHoldersCityStoreClaim(t *testing.T) {
	cfg, cityPath, infos := liveHolderPoolDemandFixture(t, "asleep")
	workBeads := []beads.Bead{routedPoolWorkBead()}

	kept, _ := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, beads.NewMemStore(), infos, workBeads, []string{""})
	if len(kept) != 0 {
		t.Fatalf("filtered work = %#v, want an asleep holder's city-store claim still dropped", kept)
	}
}

func TestPoolDemandFilterDropsALiveHoldersClaimInAnotherRigsStore(t *testing.T) {
	cfg, cityPath, infos := liveHolderPoolDemandFixture(t, "active")
	workBeads := []beads.Bead{routedPoolWorkBead()}

	kept, _ := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, beads.NewMemStore(), infos, workBeads, []string{"rigb"})
	if len(kept) != 0 {
		t.Fatalf("filtered work = %#v, want work in another rig's store still dropped", kept)
	}
}

func TestPoolDemandFilterDropsCityStoreWorkHeldByAnotherTemplate(t *testing.T) {
	cfg, cityPath, infos := liveHolderPoolDemandFixture(t, "active")
	wb := routedPoolWorkBead()
	wb.Metadata = map[string]string{beadmeta.RoutedToMetadataKey: "rigb/worker"}

	kept, _ := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, beads.NewMemStore(), infos, []beads.Bead{wb}, []string{""})
	if len(kept) != 0 {
		t.Fatalf("filtered work = %#v, want city-store work routed to another template dropped though its assignee is live", kept)
	}
}

func TestPoolDemandFilterDropsUnassignedCityStoreWork(t *testing.T) {
	cfg, cityPath, infos := liveHolderPoolDemandFixture(t, "active")
	wb := routedPoolWorkBead()
	wb.Status, wb.Assignee = "open", ""

	kept, _ := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, beads.NewMemStore(), infos, []beads.Bead{wb}, []string{""})
	if len(kept) != 0 {
		t.Fatalf("filtered work = %#v, want unassigned city-store work still dropped for a rig pool", kept)
	}
}

// The reported shape end to end: a working session holds a city-store claim and
// a second city-store bead is queued for the same rig pool. The working
// session must be resumed and the queued bead must get a fresh session, not be
// realized onto the busy one.
func TestBuildDesiredState_RigPoolLiveHolderOfCityStoreWorkGetsASecondSession(t *testing.T) {
	assertLiveHolderGetsASecondSession(t, nil, nil)
}

// The queued bead outranks the held one, so its new request sorts ahead of the
// holder's resume request. It must still get a fresh session rather than take
// the busy holder.
func TestBuildDesiredState_RigPoolHigherPriorityQueuedWorkDoesNotTakeTheBusyHolder(t *testing.T) {
	p0, p4 := 0, 4
	assertLiveHolderGetsASecondSession(t, &p4, &p0)
}

func assertLiveHolderGetsASecondSession(t *testing.T, claimedPriority, queuedPriority *int) {
	t.Helper()
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "riga")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("create rig dir: %v", err)
	}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	holder, err := cityStore.Create(beads.Bead{
		Title:  "working rig worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:riga/worker"},
		Metadata: map[string]string{
			"template":     "riga/worker",
			"session_name": "worker-gc-1",
			"state":        "active",
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	claimed, err := cityStore.Create(beads.Bead{
		Title:    "station run in progress",
		Type:     "task",
		Priority: claimedPriority,
		Assignee: holder.ID,
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "riga/worker"},
	})
	if err != nil {
		t.Fatalf("create claimed work: %v", err)
	}
	if err := cityStore.Update(claimed.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("set claimed work in_progress: %v", err)
	}
	if _, err := cityStore.Create(beads.Bead{
		Title:    "station run queued",
		Type:     "task",
		Priority: queuedPriority,
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "riga/worker"},
	}); err != nil {
		t.Fatalf("create queued work: %v", err)
	}
	sessionSnapshot, err := loadSessionBeadSnapshot(cityStore)
	if err != nil {
		t.Fatalf("load session snapshot: %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "riga", Path: rigPath}},
		Agents: []config.Agent{{
			Name:              "worker",
			Dir:               "riga",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(6),
			ScaleCheck:        "printf 1",
		}},
	}

	dsResult := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		cityStore, map[string]beads.Store{"riga": rigStore}, sessionSnapshot, nil, io.Discard,
	)
	var names []string
	for name, tp := range dsResult.State {
		if tp.TemplateName == "riga/worker" {
			names = append(names, name)
		}
	}
	if len(names) != 2 {
		t.Fatalf("riga/worker desired sessions = %v, want 2: the live holder resumed plus a fresh session for the queued bead", names)
	}
	if _, ok := dsResult.State["worker-gc-1"]; !ok {
		t.Fatalf("riga/worker desired sessions = %v, want the live holder worker-gc-1 among them", names)
	}
}
