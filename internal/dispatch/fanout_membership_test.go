package dispatch

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestFanoutMembershipIsDirectRootIDNotDepReachability pins the membership the
// control dispatcher runs on. Fan-out, retry, ralph, drain and scope all
// resolve their member set through beads.DirectMembers
// (beads.MembershipDirectRootID), and this is why: a gc.kind=spec sidecar
// carries the root id and NOTHING else — formula.newSourceSpecStep builds it as
// a fresh Step literal that never sets DependsOn, Needs or WaitsFor — so a
// dependency walk cannot see it, while
// findSpecBead must. Swapping the fan-out's membership for dependency
// reachability would not empty the member set; it would silently shrink it,
// and the first symptom would be a retry that cannot find the step it is
// retrying.
//
// The second half pins the other direction: a drain projects `blocks` edges
// from molecule beads onto an out-of-molecule blocker
// (ensureDrainWorkflowBlocksOn), which a dependency walk follows and direct
// membership does not.
func TestFanoutMembershipIsDirectRootIDNotDepReachability(t *testing.T) {
	store := beads.NewMemStore()

	root := mustCreate(t, store, beads.Bead{
		Title:    "workflow root",
		Type:     "molecule",
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
	})
	step := mustCreate(t, store, beads.Bead{
		Title: "work step",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: root.ID,
			beadmeta.StepRefMetadataKey:    "mol.work",
		},
	})
	if err := store.DepAdd(root.ID, step.ID, "blocks"); err != nil {
		t.Fatalf("wire root -> step: %v", err)
	}

	// The spec sidecar: root id, no edges. This is the shape the compiler
	// emits, not a degenerate fixture.
	spec := mustCreate(t, store, beads.Bead{
		Title: "Step spec for work step",
		Type:  "spec",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: root.ID,
			beadmeta.KindMetadataKey:       beadmeta.KindSpec,
			beadmeta.SpecForMetadataKey:    "work",
			beadmeta.SpecForRefMetadataKey: "mol.work",
		},
	})

	// The drain-shaped out-of-molecule blocker.
	outsider := mustCreate(t, store, beads.Bead{Title: "blocker outside the molecule"})
	if err := store.DepAdd(step.ID, outsider.ID, "blocks"); err != nil {
		t.Fatalf("wire step -> outsider: %v", err)
	}

	members, err := beads.DirectMembers(store, root.ID)
	if err != nil {
		t.Fatalf("beads.DirectMembers: %v", err)
	}
	inMembers := make(map[string]bool, len(members))
	for _, m := range members {
		inMembers[m.ID] = true
	}
	if !inMembers[spec.ID] {
		t.Fatalf("the fan-out member set is missing spec sidecar %s; %s is the only rule that reaches a bead with no edges, and findSpecBead depends on it",
			spec.ID, beads.MembershipDirectRootID)
	}
	if inMembers[outsider.ID] {
		t.Errorf("the fan-out member set contains %s, which carries no gc.root_bead_id — %s does not follow edges out of the molecule",
			outsider.ID, beads.MembershipDirectRootID)
	}

	reachable := dispatchDepReachable(t, store, root.ID)
	if reachable[spec.ID] {
		t.Errorf("the spec sidecar %s is dependency-reachable in this fixture, so it no longer demonstrates what direct membership buys the dispatcher", spec.ID)
	}
	if !reachable[outsider.ID] {
		t.Errorf("the out-of-molecule blocker %s is not dependency-reachable in this fixture, so it no longer demonstrates that a dep walk escapes the molecule", outsider.ID)
	}

	// The real consumer, exercised end to end.
	control := mustCreate(t, store, beads.Bead{
		Title: "retry control",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:       "retry",
			beadmeta.StepIDMetadataKey:     "work",
			beadmeta.StepRefMetadataKey:    "mol.work",
			beadmeta.RootBeadIDMetadataKey: root.ID,
		},
	})
	got, err := findSpecBead(store, control)
	if err != nil {
		t.Fatalf("findSpecBead over the fan-out member set: %v", err)
	}
	if got.ID != spec.ID {
		t.Fatalf("findSpecBead = %s, want the dependency-isolated sidecar %s", got.ID, spec.ID)
	}
}

// dispatchDepReachable is the maximally generous dependency walk: both
// directions, every edge type. A bead it misses is missed by any dep walk.
func dispatchDepReachable(t *testing.T, store beads.Store, rootID string) map[string]bool {
	t.Helper()
	seen := map[string]bool{rootID: true}
	queue := []string{rootID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, direction := range []string{"down", "up"} {
			deps, err := store.DepList(id, direction)
			if err != nil {
				t.Fatalf("DepList(%s, %s): %v", id, direction, err)
			}
			for _, dep := range deps {
				for _, next := range []string{dep.IssueID, dep.DependsOnID} {
					if next == "" || seen[next] {
						continue
					}
					seen[next] = true
					queue = append(queue, next)
				}
			}
		}
	}
	return seen
}
