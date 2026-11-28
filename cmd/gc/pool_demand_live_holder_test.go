package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// A rig-scoped pool whose work is filed in the CITY store: on a city that
// relocates nothing, the template-scoped reach test rejects every city-store
// row for a rig agent. The wake filter still accepts a holder's own claim on
// such a row (its exact assignee identity under assignedWorkClaimRefs), so pool
// demand has to as well. Otherwise the busy holder gets no resume request,
// looks reusable, and the next queued bead's new-tier request is realized onto
// it: one session, no create.

// liveHolderSessionName is the holder's exact assignee identity in the filter
// rows below.
const liveHolderSessionName = "test-city--worker-1"

// liveHolderFilterFixture is the single-store rig pool fixture with its one
// open riga/worker session in state.
func liveHolderFilterFixture(t *testing.T, state string) (*config.City, string, []sessionpkg.Info) {
	t.Helper()
	cfg, cityPath, _ := rigScopedPoolDemandFixture(t)
	infos := sessionInfosFromBeads([]beads.Bead{{
		ID:     "gcs-1",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"template":     "riga/worker",
			"session_name": liveHolderSessionName,
			"state":        state,
			"pool_managed": "true",
		},
	}})
	seedNoRoutes(t, cityPath)
	return cfg, cityPath, infos
}

// liveHolderWork is an in-progress claim by assignee routed to riga/worker.
func liveHolderWork(id, assignee string) beads.Bead {
	return beads.Bead{
		ID:       id,
		Status:   "in_progress",
		Assignee: assignee,
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "riga/worker"},
	}
}

func TestPoolDemandFilterKeepsALiveHoldersCityStoreClaim(t *testing.T) {
	for _, state := range []string{"active", "awake", "creating", "draining"} {
		t.Run(state, func(t *testing.T) {
			cfg, cityPath, infos := liveHolderFilterFixture(t, state)
			work := []beads.Bead{liveHolderWork("gcg-1", liveHolderSessionName)}

			kept, keptRefs := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, beads.NewMemStore(), infos, work, []string{""})

			if ids := beadIDs(kept); !slices.Equal(ids, []string{"gcg-1"}) || !slices.Equal(keptRefs, []string{""}) {
				t.Fatalf("kept = %v refs %q, want [gcg-1] [\"\"]: a live holder's own city-store claim is pool demand", ids, keptRefs)
			}
		})
	}
}

func TestPoolDemandFilterKeepsALiveHoldersClaimAlignedInAMixedSlice(t *testing.T) {
	cfg, cityPath, infos := liveHolderFilterFixture(t, "active")
	work := []beads.Bead{
		liveHolderWork("gcg-rig", ""),
		liveHolderWork("gcg-unassigned", ""),
		liveHolderWork("gcg-held", liveHolderSessionName),
	}
	work[0].Status = "open"
	work[1].Status = "open"

	kept, keptRefs := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, beads.NewMemStore(), infos, work, []string{"riga", "", ""})

	if ids := beadIDs(kept); !slices.Equal(ids, []string{"gcg-rig", "gcg-held"}) {
		t.Fatalf("kept IDs = %v, want [gcg-rig gcg-held]", ids)
	}
	if !slices.Equal(keptRefs, []string{"riga", ""}) {
		t.Fatalf("kept refs = %q, want [riga \"\"] aligned with the kept rows", keptRefs)
	}
}

func TestPoolDemandFilterDropsANonLiveHoldersCityStoreClaim(t *testing.T) {
	for _, tc := range []struct {
		state  string
		closed bool
	}{
		{state: "asleep"},
		{state: "drained"},
		{state: "suspended"},
		{state: "active", closed: true},
	} {
		name := tc.state
		if tc.closed {
			name = "closed"
		}
		t.Run(name, func(t *testing.T) {
			cfg, cityPath, infos := liveHolderFilterFixture(t, tc.state)
			infos[0].Closed = tc.closed
			work := []beads.Bead{liveHolderWork("gcg-1", liveHolderSessionName)}

			kept, keptRefs := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, beads.NewMemStore(), infos, work, []string{""})

			if len(kept) != 0 || len(keptRefs) != 0 {
				t.Fatalf("kept = %v refs %q, want the claim dropped: only a live holder's city-store claim reaches a rig pool", beadIDs(kept), keptRefs)
			}
		})
	}
}

func TestPoolDemandFilterLiveHolderWideningStaysExact(t *testing.T) {
	routedElsewhere := liveHolderWork("gcg-1", liveHolderSessionName)
	routedElsewhere.Metadata[beadmeta.RoutedToMetadataKey] = "rigb/worker"
	for _, tc := range []struct {
		name string
		work beads.Bead
		ref  string
	}{
		{name: "routed to another template", work: routedElsewhere, ref: ""},
		{name: "resident in another rig's store", work: liveHolderWork("gcg-1", liveHolderSessionName), ref: "rigb"},
		{name: "unassigned", work: liveHolderWork("gcg-1", ""), ref: ""},
		{name: "assigned to the template identity", work: liveHolderWork("gcg-1", "riga/worker"), ref: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, cityPath, infos := liveHolderFilterFixture(t, "active")

			kept, keptRefs := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, beads.NewMemStore(), infos, []beads.Bead{tc.work}, []string{tc.ref})

			if len(kept) != 0 || len(keptRefs) != 0 {
				t.Fatalf("kept = %v refs %q, want the row dropped exactly as before", beadIDs(kept), keptRefs)
			}
		})
	}
}

// liveHolderDesiredName is the busy holder's session name in the
// desired-state scenarios.
const liveHolderDesiredName = "worker-gc-1"

// rigPoolLiveHolderDesired builds desired state for the reported shape: a
// single-store city whose rig pool riga/worker (max 5, scale_check 1) has one
// active session holding in-progress city-store bead A, beside open unassigned
// city-store bead B. It returns the riga/worker desired session names, sorted.
func rigPoolLiveHolderDesired(t *testing.T, heldPriority, queuedPriority int) []string {
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
			"session_name": liveHolderDesiredName,
			"state":        "active",
			"pool_managed": "true",
		},
	}); err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	held, err := cityStore.Create(beads.Bead{
		Title:    "held city work",
		Type:     "task",
		Priority: intPtr(heldPriority),
		Assignee: liveHolderDesiredName,
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "riga/worker"},
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
		Priority: intPtr(queuedPriority),
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "riga/worker"},
	}); err != nil {
		t.Fatalf("create queued bead: %v", err)
	}
	sessionSnapshot, err := loadSessionBeadSnapshot(cityStore)
	if err != nil {
		t.Fatalf("load session snapshot: %v", err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), liveHolderDesiredName, runtime.Config{}); err != nil {
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
	slices.Sort(names)
	return names
}

func TestBuildDesiredState_RigPoolCountsALiveHoldersCityStoreClaim(t *testing.T) {
	names := rigPoolLiveHolderDesired(t, 2, 2)

	if len(names) != 2 || !slices.Contains(names, liveHolderDesiredName) {
		t.Fatalf("riga/worker desired sessions = %v, want 2: the busy holder %q plus a fresh session for the queued bead", names, liveHolderDesiredName)
	}
}

func TestBuildDesiredState_RigPoolHigherPriorityQueuedWorkDoesNotTakeTheBusyHolder(t *testing.T) {
	names := rigPoolLiveHolderDesired(t, 4, 0)

	if len(names) != 2 || !slices.Contains(names, liveHolderDesiredName) {
		t.Fatalf("riga/worker desired sessions = %v, want 2: the P0 queued bead must get a fresh session, not the busy holder %q holding P4 work", names, liveHolderDesiredName)
	}
}

// liveRigHolderDiffCity is the reported shape for the allocation differential:
// a rig-scoped custom-count pool on a single-store city, one active row holding
// an in-progress city-store claim, and scale_check reporting one more.
func liveRigHolderDiffCity(t *testing.T) diffCity {
	t.Helper()
	worker := customPool("worker", "echo 1", 4)
	worker.Dir = "riga"
	cfg := diffConfig(worker)
	cfg.Rigs = []config.Rig{{Name: "riga", Path: t.TempDir()}}
	return diffCity{
		name: "a live rig-pool holder's city-store claim is demand beside a custom count",
		cfg:  cfg,
		beads: []beads.Bead{
			poolRow("dr-1", "riga/worker", 1, "active"),
			{ID: "dw-1", Title: "dw-1", Type: "task", Status: "in_progress", Assignee: "s-dr-1", CreatedAt: diffNow.Add(-time.Hour), Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "riga/worker"}},
		},
		alive:      []string{"s-dr-1"},
		scaleCheck: true,
	}
}

func TestV2AllocationCountsALiveRigHoldersCityStoreClaim(t *testing.T) {
	out := v2Allocation(t, liveRigHolderDiffCity(t))

	if got := out.accepted["riga/worker"]; got != 2 {
		t.Fatalf("v2 accepted riga/worker = %d, want 2: the holder's resume plus the scale_check's new request (accepted %v)", got, out.accepted)
	}
}
