package beads

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// The live-city measurement this file pins. Molecule root gcg-arn, read four
// ways on the same graph at the same moment:
//
//	gcgArnDirectMembers  beads carrying gc.root_bead_id == gcg-arn
//	gcgArnGraphSize      what `gc bd graph gcg-arn --json` returned
//	gcgArnDepTreeSize    what `gc bd dep tree gcg-arn --json` returned
//	gcgArnSpecSidecars   in graph but not in dep tree; every one gc.kind=spec
//
// gcgArnDirectMembers - gcgArnSpecSidecars + 1 == gcgArnDepTreeSize, which is
// exactly the trap: a dependency walk returned the set the adopt-pr driver
// wanted, so the wrong membership looked right. The subtests below hold the
// four numbers together on a reconstruction of that graph and then break the
// identity three ways, so a future change that swaps one surface's membership
// for another's reddens instead of returning a plausible number.
const (
	gcgArnDirectMembers = 60
	gcgArnGraphSize     = 61
	gcgArnDepTreeSize   = 48
	gcgArnSpecSidecars  = 13
)

// gcgArnRootID is the live molecule root the fixture reconstructs.
const gcgArnRootID = "gcg-arn"

// buildGcgArnShape reconstructs the measured gcg-arn molecule: one root, 60
// beads carrying gc.root_bead_id == root, of which 13 are gc.kind=spec
// sidecars with no dependency edge of any kind — the shape
// formula.newSourceSpecStep produces, since it builds the sidecar as a fresh
// Step literal and never sets DependsOn, Needs or WaitsFor on it. The
// remaining 47 hang off the root
// through the blocks/parent-child edges molecule.Instantiate authors, so a
// dependency walk from the root reaches 1 + 47 == gcgArnDepTreeSize beads.
//
// Half the members are closed. A molecule is read long after most of it has
// finished, and a membership rule that quietly dropped closed beads would
// return a smaller plausible number rather than an error.
func buildGcgArnShape(t *testing.T) *MemStore {
	t.Helper()
	store := NewMemStore()
	store.IDPrefix = "gcg"
	store.HonorExplicitIDs = true

	mustCreate(t, store, Bead{
		ID:       gcgArnRootID,
		Title:    "adopt-pr molecule root",
		Type:     "molecule",
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
	})

	linked := gcgArnDirectMembers - gcgArnSpecSidecars
	prev := gcgArnRootID
	for i := range linked {
		id := fmt.Sprintf("%s.step.%d", gcgArnRootID, i+1)
		mustCreate(t, store, Bead{
			ID:       id,
			Title:    fmt.Sprintf("step %d", i+1),
			ParentID: prev,
			Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: gcgArnRootID},
		})
		mustDepAdd(t, store, prev, id, "blocks")
		mustDepAdd(t, store, id, prev, "parent-child")
		if i%2 == 0 {
			mustClose(t, store, id)
		}
		prev = id
	}

	for i := range gcgArnSpecSidecars {
		mustCreate(t, store, Bead{
			ID:    fmt.Sprintf("%s.step.%d.spec", gcgArnRootID, i+1),
			Title: fmt.Sprintf("Step spec for step %d", i+1),
			Type:  "spec",
			Metadata: map[string]string{
				beadmeta.RootBeadIDMetadataKey: gcgArnRootID,
				beadmeta.KindMetadataKey:       beadmeta.KindSpec,
				beadmeta.SpecForMetadataKey:    fmt.Sprintf("%s.step.%d", gcgArnRootID, i+1),
			},
		})
	}
	return store
}

// TestMoleculeMembershipPinsTheMeasuredGcgArnShape holds the four live numbers
// against this package's implementations and proves the arithmetic that makes
// them agree is a coincidence.
func TestMoleculeMembershipPinsTheMeasuredGcgArnShape(t *testing.T) {
	t.Run("fixture-reproduces-the-measurement", func(t *testing.T) {
		store := buildGcgArnShape(t)
		carrying, err := store.List(ListQuery{
			Metadata:      map[string]string{beadmeta.RootBeadIDMetadataKey: gcgArnRootID},
			IncludeClosed: true,
		})
		if err != nil {
			t.Fatalf("list by gc.root_bead_id: %v", err)
		}
		if len(carrying) != gcgArnDirectMembers {
			t.Fatalf("beads carrying gc.root_bead_id == %s = %d, want %d — the fixture no longer reproduces the live shape the rest of this test reasons about",
				gcgArnRootID, len(carrying), gcgArnDirectMembers)
		}
	})

	t.Run("direct-membership-is-the-root-plus-everything-carrying-the-root-id", func(t *testing.T) {
		store := buildGcgArnShape(t)
		members, err := DirectMembers(store, gcgArnRootID)
		if err != nil {
			t.Fatalf("DirectMembers: %v", err)
		}
		if len(members) != gcgArnGraphSize {
			t.Fatalf("DirectMembers(%s) = %d beads, want %d (the root plus %d members) — %s answers %q and that count is the contract, not an implementation detail",
				gcgArnRootID, len(members), gcgArnGraphSize, gcgArnDirectMembers, "MembershipDirectRootID", MembershipDirectRootID)
		}
		if members[0].ID != gcgArnRootID {
			t.Errorf("DirectMembers[0] = %q, want the root %q first — callers that need the root separately rely on it", members[0].ID, gcgArnRootID)
		}
		specs := 0
		closed := 0
		for _, m := range members {
			if m.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindSpec {
				specs++
			}
			if m.Status == "closed" {
				closed++
			}
		}
		if specs != gcgArnSpecSidecars {
			t.Errorf("DirectMembers returned %d gc.kind=spec sidecars, want %d — direct membership must NOT filter; consumers filter (pr_review.py's is_frontier_candidate already excludes specs itself)", specs, gcgArnSpecSidecars)
		}
		if closed == 0 {
			t.Errorf("DirectMembers returned no closed members; the fixture closes half of them and IncludeClosed must be honored, or a finished molecule reads as a smaller live one")
		}
	})

	t.Run("dep-reachability-drops-exactly-the-spec-sidecars", func(t *testing.T) {
		store := buildGcgArnShape(t)
		members, err := DirectMembers(store, gcgArnRootID)
		if err != nil {
			t.Fatalf("DirectMembers: %v", err)
		}
		reachable := depReachable(t, store)
		if len(reachable) != gcgArnDepTreeSize {
			t.Fatalf("dependency reachability from %s = %d beads, want %d — this is the number `gc bd dep tree` returned on the live molecule",
				gcgArnRootID, len(reachable), gcgArnDepTreeSize)
		}

		var missed []string
		for _, m := range members {
			if !reachable[m.ID] {
				missed = append(missed, m.ID)
				if kind := m.Metadata[beadmeta.KindMetadataKey]; kind != beadmeta.KindSpec {
					t.Errorf("dep-reachability dropped member %s with gc.kind=%q, not %q — the live measurement found the dropped set to be ALL spec sidecars; a non-spec drop means %s is now losing real work",
						m.ID, kind, beadmeta.KindSpec, MembershipDepReachable)
				}
			}
		}
		if len(missed) != gcgArnSpecSidecars {
			t.Fatalf("dep-reachability dropped %d of the %d direct members, want %d: %v", len(missed), len(members), gcgArnSpecSidecars, missed)
		}
		if gcgArnDirectMembers-gcgArnSpecSidecars+1 != gcgArnDepTreeSize {
			t.Fatalf("the measured identity no longer holds: %d - %d + 1 != %d", gcgArnDirectMembers, gcgArnSpecSidecars, gcgArnDepTreeSize)
		}
	})

	// The three arms below each break the identity that made dep-reachability
	// look correct. None of them changes what DirectMembers returns.
	t.Run("identity-breaks-when-one-spec-is-dependency-linked", func(t *testing.T) {
		store := buildGcgArnShape(t)
		mustDepAdd(t, store, gcgArnRootID, gcgArnRootID+".step.1.spec", "blocks")

		members, err := DirectMembers(store, gcgArnRootID)
		if err != nil {
			t.Fatalf("DirectMembers: %v", err)
		}
		if len(members) != gcgArnGraphSize {
			t.Errorf("DirectMembers = %d, want %d — direct membership does not move when an edge is added; only the dep walk does", len(members), gcgArnGraphSize)
		}
		reachable := depReachable(t, store)
		if len(reachable) == gcgArnDepTreeSize {
			t.Fatalf("dep-reachability still returns %d after linking one spec bead; the fixture no longer demonstrates that %d was a property of the graph's shape rather than a contract",
				gcgArnDepTreeSize, gcgArnDepTreeSize)
		}
		if len(reachable) != gcgArnDepTreeSize+1 {
			t.Fatalf("dep-reachability = %d after linking one spec bead, want %d", len(reachable), gcgArnDepTreeSize+1)
		}
	})

	t.Run("dep-reachability-drops-a-non-spec-member-that-carries-the-root-id", func(t *testing.T) {
		store := buildGcgArnShape(t)
		// A member created before its edges were wired: the fan-out control
		// bead persists gc.fanout_state=spawning, instantiates fragments, and
		// wires the sink blockers afterwards, so a crash in between leaves an
		// ordinary work bead carrying the root id and nothing else.
		orphan := mustCreate(t, store, Bead{
			ID:       gcgArnRootID + ".item.99",
			Title:    "fan-out fragment whose blockers were never wired",
			Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: gcgArnRootID},
		})

		members, err := DirectMembers(store, gcgArnRootID)
		if err != nil {
			t.Fatalf("DirectMembers: %v", err)
		}
		if !containsBead(members, orphan.ID) {
			t.Fatalf("DirectMembers dropped %s, which carries gc.root_bead_id == %s — that is the whole rule", orphan.ID, gcgArnRootID)
		}
		if reachable := depReachable(t, store); reachable[orphan.ID] {
			t.Fatalf("dep-reachability found %s, which has no dependency edges; the fixture no longer shows that a dep walk loses real, non-spec work", orphan.ID)
		}
	})

	t.Run("dep-reachability-admits-a-bead-that-is-not-a-member", func(t *testing.T) {
		store := buildGcgArnShape(t)
		// dispatch.ensureDrainWorkflowBlocksOn projects a `blocks` edge from
		// every molecule bead onto a blocker OUTSIDE the molecule, so a walk
		// over dependency edges escapes the molecule in the other direction.
		outsider := mustCreate(t, store, Bead{ID: "gcg-outside-1", Title: "drain blocker outside the molecule"})
		mustDepAdd(t, store, gcgArnRootID+".step.1", outsider.ID, "blocks")

		members, err := DirectMembers(store, gcgArnRootID)
		if err != nil {
			t.Fatalf("DirectMembers: %v", err)
		}
		if containsBead(members, outsider.ID) {
			t.Fatalf("DirectMembers returned %s, which carries no gc.root_bead_id — direct membership must not follow edges", outsider.ID)
		}
		if reachable := depReachable(t, store); !reachable[outsider.ID] {
			t.Fatalf("dep-reachability missed %s; the fixture no longer shows that a dep walk admits out-of-molecule beads", outsider.ID)
		}
	})
}

// TestDirectMembersReturnsMembersWhenTheRootIsGone pins the deliberate
// non-error on a missing root: a molecule whose root was relocated to another
// class store must not read as an empty molecule, because an empty answer is
// indistinguishable from a finished one.
func TestDirectMembersReturnsMembersWhenTheRootIsGone(t *testing.T) {
	store := buildGcgArnShape(t)
	if err := store.Delete(gcgArnRootID); err != nil {
		t.Fatalf("delete root: %v", err)
	}
	members, err := DirectMembers(store, gcgArnRootID)
	if err != nil {
		t.Fatalf("DirectMembers with a missing root: %v", err)
	}
	if len(members) != gcgArnDirectMembers {
		t.Fatalf("DirectMembers with a missing root = %d, want %d (every member, no root)", len(members), gcgArnDirectMembers)
	}
	if containsBead(members, gcgArnRootID) {
		t.Fatalf("DirectMembers returned the deleted root %s", gcgArnRootID)
	}
}

// TestMembershipConstantsAreDistinctWireValues keeps the vocabulary usable as a
// wire contract: BeadGraphResponse.Membership serializes these strings and the
// OpenAPI enum lists them, so two rules collapsing onto one spelling would make
// the declaration unable to distinguish what it exists to distinguish.
func TestMembershipConstantsAreDistinctWireValues(t *testing.T) {
	all := []Membership{
		MembershipDirectRootID,
		MembershipDepReachable,
		MembershipRootIDAndParentClosure,
		MembershipRootIDParentClosureAndConvoy,
	}
	seen := make(map[string]bool, len(all))
	for _, m := range all {
		if strings.TrimSpace(m.String()) == "" {
			t.Errorf("membership constant has an empty wire spelling")
			continue
		}
		if seen[m.String()] {
			t.Errorf("membership wire spelling %q is used by more than one rule", m)
		}
		seen[m.String()] = true
	}
}

// depReachable models the membership `bd dep tree gcg-arn` implements: the
// transitive closure of dependency EDGES from the fixture's root. It follows
// edges in BOTH directions and accepts every edge type, which is the most
// generous reading a dep walk can have — so a bead this misses is missed by any
// dep walk, not merely by one direction of one edge kind.
func depReachable(t *testing.T, store Store) map[string]bool {
	t.Helper()
	seen := map[string]bool{gcgArnRootID: true}
	queue := []string{gcgArnRootID}
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

func containsBead(list []Bead, id string) bool {
	for _, b := range list {
		if b.ID == id {
			return true
		}
	}
	return false
}

func mustCreate(t *testing.T, store *MemStore, b Bead) Bead {
	t.Helper()
	created, err := store.Create(b)
	if err != nil {
		t.Fatalf("create %s: %v", b.ID, err)
	}
	return created
}

func mustDepAdd(t *testing.T, store *MemStore, issueID, dependsOnID, depType string) {
	t.Helper()
	if err := store.DepAdd(issueID, dependsOnID, depType); err != nil {
		t.Fatalf("DepAdd(%s -> %s, %s): %v", issueID, dependsOnID, depType, err)
	}
}

func mustClose(t *testing.T, store *MemStore, id string) {
	t.Helper()
	if err := store.Close(id); err != nil {
		t.Fatalf("close %s: %v", id, err)
	}
}
