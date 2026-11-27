package main

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// memberReadFailStore fails the workflow member read for one root and serves
// every other read from the embedded store.
type memberReadFailStore struct {
	*beads.MemStore
	failRoot string
}

func (s memberReadFailStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Metadata[beadmeta.RootBeadIDMetadataKey] == s.failRoot {
		return nil, errors.New("member read outage")
	}
	return s.MemStore.List(q)
}

func createAwaitingFinalizeBead(t *testing.T, store beads.Store, b beads.Bead, status string) beads.Bead {
	t.Helper()
	created, err := store.Create(b)
	if err != nil {
		t.Fatalf("create %q: %v", b.Title, err)
	}
	if status != "open" {
		if err := store.Update(created.ID, beads.UpdateOpts{Status: stringPtr(status)}); err != nil {
			t.Fatalf("set %q %s: %v", b.Title, status, err)
		}
	}
	return created
}

func createAwaitingFinalizeRoot(t *testing.T, store beads.Store, title string) beads.Bead {
	t.Helper()
	return createAwaitingFinalizeBead(t, store, beads.Bead{
		Title: title, Type: "task", Assignee: "worker-1",
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
	}, "in_progress")
}

func createAwaitingFinalizeMember(t *testing.T, store beads.Store, rootID, title, kind, status string) {
	t.Helper()
	createAwaitingFinalizeBead(t, store, beads.Bead{
		Title: title, Type: "task",
		Metadata: map[string]string{beadmeta.KindMetadataKey: kind, beadmeta.RootBeadIDMetadataKey: rootID},
	}, status)
}

// TestCollectAssignedWorkBeadsFinishedWorkflowRootCarriesNoReadiness pins the
// controller half of the finished-root fix: an in_progress workflow root whose
// worker steps have all closed, and which waits only on its finalize control,
// stays in the assigned-work snapshot but carries no wake readiness. A live
// root, a root-only root and a root whose member read failed stay ready.
func TestCollectAssignedWorkBeadsFinishedWorkflowRootCarriesNoReadiness(t *testing.T) {
	mem := beads.NewMemStore()
	store := memberReadFailStore{MemStore: mem}

	finished := createAwaitingFinalizeRoot(t, store, "finished root")
	createAwaitingFinalizeMember(t, store, finished.ID, "finished step", "task", "closed")
	createAwaitingFinalizeMember(t, store, finished.ID, "finished finalizer", beadmeta.KindWorkflowFinalize, "open")

	live := createAwaitingFinalizeRoot(t, store, "live root")
	createAwaitingFinalizeMember(t, store, live.ID, "live done step", "task", "closed")
	createAwaitingFinalizeMember(t, store, live.ID, "live open step", "task", "open")
	createAwaitingFinalizeMember(t, store, live.ID, "live finalizer", beadmeta.KindWorkflowFinalize, "open")

	rootOnly := createAwaitingFinalizeRoot(t, store, "root-only root")

	unread := createAwaitingFinalizeRoot(t, store, "unread root")
	createAwaitingFinalizeMember(t, store, unread.ID, "unread step", "task", "closed")
	createAwaitingFinalizeMember(t, store, unread.ID, "unread finalizer", beadmeta.KindWorkflowFinalize, "open")
	store.failRoot = unread.ID

	got, _, storeRefs, readyAssigned, partial := collectAssignedWorkBeadsWithStores("", &config.City{}, store, nil, nil, nil)
	if partial {
		t.Fatal("partial = true, want false: a failed member read fails open, it does not lose the snapshot")
	}
	ready := map[string]bool{}
	inSnapshot := map[string]bool{}
	for i, b := range got {
		inSnapshot[b.ID] = true
		ready[b.ID] = readyAssigned[storeScopedBeadKey{StoreRef: storeRefs[i], ID: b.ID}]
	}
	for _, tc := range []struct {
		name      string
		id        string
		wantReady bool
	}{
		{"finished root", finished.ID, false},
		{"live root", live.ID, true},
		{"root-only root", rootOnly.ID, true},
		{"root whose member read failed", unread.ID, true},
	} {
		if !inSnapshot[tc.id] {
			t.Errorf("%s %s missing from the assigned-work snapshot %v", tc.name, tc.id, ids(got))
			continue
		}
		if ready[tc.id] != tc.wantReady {
			t.Errorf("%s %s ready = %v, want %v", tc.name, tc.id, ready[tc.id], tc.wantReady)
		}
	}
}

// TestBuildAwakeInputFromReconciler_InProgressAwaitingFinalize pins how the
// bridge carries the controller verdict: an in_progress workflow root whose
// supplied readiness flag is false is awaiting finalize and does not wake. An
// omitted flag slice, and an in_progress row of any other kind, keep today's
// wake-on-claim behavior.
func TestBuildAwakeInputFromReconciler_InProgressAwaitingFinalize(t *testing.T) {
	for _, tt := range []struct {
		name         string
		kind         string
		flags        []bool
		wantAwaiting bool
		wantWake     bool
	}{
		{"workflow root with a false flag is awaiting finalize", beadmeta.KindWorkflow, []bool{false}, true, false},
		{"workflow root with a true flag wakes", beadmeta.KindWorkflow, []bool{true}, false, true},
		{"workflow root with omitted flags still wakes", beadmeta.KindWorkflow, nil, false, true},
		{"non-workflow claim with a false flag still wakes", "task", []bool{false}, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now().UTC()
			cfg := &config.City{Agents: []config.Agent{{Name: "gc.run-operator"}}}
			sessionBead := beads.Bead{
				ID: "mc-session-1", Status: "open", Type: "session",
				Metadata: map[string]string{
					"state": "active", "session_name": "gc__run-operator-mc-1", "template": "gc.run-operator",
				},
			}
			work := beads.Bead{
				ID: "wf-root", Status: "in_progress", Assignee: "gc__run-operator-mc-1",
				Metadata: map[string]string{beadmeta.KindMetadataKey: tt.kind},
			}
			input := buildAwakeInputFromReconciler(cfg, "",
				[]session.Info{sessiontest.SeedBead(t, sessionBead)},
				nil, nil, nil, nil, nil, []beads.Bead{work}, tt.flags, nil, runtime.NewFake(), now)
			if len(input.WorkBeads) != 1 {
				t.Fatalf("WorkBeads = %+v, want exactly one bead", input.WorkBeads)
			}
			if got := input.WorkBeads[0].AwaitingFinalize; got != tt.wantAwaiting {
				t.Errorf("WorkBeads[0].AwaitingFinalize = %v, want %v", got, tt.wantAwaiting)
			}
			if got := workBeadHasAwakeDemand(input.WorkBeads[0]); got != tt.wantWake {
				t.Errorf("workBeadHasAwakeDemand = %v, want %v", got, tt.wantWake)
			}
		})
	}
}

// TestInProgressWorkAwaitingFinalizeDoesNotWakeOrFillScaleSlot pins the
// awake-set half: a session whose only assigned work is a finished workflow
// root does not wake and holds no scale slot, so another session can take it.
func TestInProgressWorkAwaitingFinalizeDoesNotWakeOrFillScaleSlot(t *testing.T) {
	result := computeAwakeSetChecked(t, AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-p1", SessionName: "polecat-mc-p1", Template: "hello-world/polecat", State: "asleep"},
			{ID: "mc-p2", SessionName: "polecat-mc-p2", Template: "hello-world/polecat", State: "active"},
		},
		WorkBeads:        []AwakeWorkBead{{ID: "wf-root", Assignee: "mc-p1", Status: "in_progress", AwaitingFinalize: true}},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 1},
		Now:              now,
	})
	assertAsleep(t, result, "polecat-mc-p1")
	assertAwake(t, result, "polecat-mc-p2")
	assertReason(t, result, "polecat-mc-p2", "scaled:demand")
}
