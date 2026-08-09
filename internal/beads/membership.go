package beads

import (
	"errors"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// Membership names the rule a projection uses to decide which beads belong to
// the workflow (molecule) rooted at a given bead. Any surface that answers
// "what is in this molecule?" declares one, because the rules disagree and the
// disagreement is invisible in the answer: on one graph they return the same
// count, on the next one they do not, and nothing about the response says
// which question was asked.
//
// # The measurement this vocabulary exists for
//
// Live city, molecule root gcg-arn:
//
//	beads carrying gc.root_bead_id == gcg-arn ... 60  (61 with the root)
//	gc bd graph gcg-arn --json ................. 61
//	gc bd dep tree gcg-arn --json .............. 48
//	in graph but not in dep tree ............... 13, every one gc.kind=spec
//
// 60 - 13 + 1 = 48, so dependency reachability returned exactly the set the
// adopt-pr driver wanted. It did so by coincidence. Spec sidecars are built
// with no dependency edges at all — formula.newSourceSpecStep clears
// DependsOn, Needs and WaitsFor — and this molecule linked none of them by
// hand, so the set dep-reachability dropped happened to equal the set the
// consumer was filtering out anyway.
//
// That is a property of one graph's shape, not a contract, and it fails in
// both directions:
//
//   - Link one spec bead and dep-reachability answers 49 while direct
//     membership still answers 61. The consumer gets a plausible wrong number
//     rather than an empty one, which is worse, because it does not look
//     wrong.
//   - Dep-reachability also escapes the molecule. A drain projects `blocks`
//     edges from every member onto an out-of-molecule blocker (dispatch's
//     ensureDrainWorkflowBlocksOn), so the walk admits beads that carry a
//     different root id, or none.
//
// Gas City's answer is DIRECT membership: everything carrying the root id, and
// consumers filter. The projection does not guess which subset a caller wants.
// TestMoleculeMembershipPinsTheMeasuredGcgArnShape pins the numbers above
// against this package's implementations.
//
// # Which surface implements which rule
//
//	beads.DirectMembers .............................. MembershipDirectRootID
//	dispatch fan-out / retry / ralph / drain / scope .. MembershipDirectRootID
//	                                                    (via DirectMembers)
//	api snapshotFromStore + workflowSQLQueryWorkflowBeads
//	                                                 .. MembershipDirectRootID
//	                                                    (re-expressed inline and in SQL;
//	                                                    same rule, different read handle)
//	storebinding graphHasOpenDescendants ............. MembershipDirectRootID first,
//	                                                    parent/dep walk only as a fallback
//	molecule.ListSubtree ............................. MembershipRootIDAndParentClosure
//	api collectBeadGraph, GET .../beads/graph/{root} . MembershipRootIDAndParentClosure,
//	                                                    or MembershipRootIDParentClosureAndConvoy
//	                                                    when the root is a container;
//	                                                    declared on the wire in
//	                                                    BeadGraphResponse.Membership
//	runproj.RunMembers ............................... MembershipDirectRootID plus two
//	                                                    documented extras, see its doc
//	bd dep tree ...................................... MembershipDepReachable
//	bd graph ......................................... MembershipDirectRootID
//	gc graph ......................................... none: the named ids, with only
//	                                                    convoys expanded
type Membership string

const (
	// MembershipDirectRootID is the root bead plus every bead whose
	// gc.root_bead_id metadata equals the root's id, open and closed alike.
	// It is what materialization stamps (molecule.Instantiate writes the key
	// on every step, InstantiateFragment on every fan-out fragment bead), so
	// it is the only rule that is complete by construction rather than by
	// the shape of the edges a formula happened to author.
	MembershipDirectRootID Membership = "direct-root-id"

	// MembershipDepReachable is the transitive closure of dependency edges
	// from the root. No Gas City projection implements it; it is named
	// because `bd dep tree` does, because it is the rule a reader is most
	// likely to assume, and because a surface that silently swapped to it
	// would keep returning a plausible number. It is neither a subset nor a
	// superset of MembershipDirectRootID: it drops dependency-isolated
	// members (every gc.kind=spec sidecar) and admits out-of-molecule beads
	// that a member merely blocks on.
	MembershipDepReachable Membership = "dep-reachable"

	// MembershipRootIDAndParentClosure is MembershipDirectRootID unioned with
	// the transitive parent-child closure of the root. It is a superset:
	// materialization sets ParentID from parent-child edges as well as
	// stamping the root id, so the closure adds only beads reparented onto
	// the molecule after the fact.
	MembershipRootIDAndParentClosure Membership = "direct-root-id+parent-closure"

	// MembershipRootIDParentClosureAndConvoy is
	// MembershipRootIDAndParentClosure plus the members of the root when the
	// root is a container (convoy) bead. Convoy membership is a tracks-edge
	// relation, not a root-id relation, so it is named separately rather than
	// folded into the parent closure.
	MembershipRootIDParentClosureAndConvoy Membership = "direct-root-id+parent-closure+convoy-members"
)

// String returns the wire spelling of the membership rule.
func (m Membership) String() string { return string(m) }

// DirectMembers returns the MembershipDirectRootID member set of the workflow
// rooted at rootID: the root bead first, then every bead whose
// gc.root_bead_id metadata equals rootID, closed beads included. Members are
// read through the store's LIVE handle, so a caller that just wrote a member
// sees it rather than a cached snapshot.
//
// A missing root is not an error — the metadata members are still returned, so
// a molecule whose root has been relocated or removed does not silently become
// an empty molecule. Any other Get failure is returned.
//
// This is the fan-out membership: dispatch's fan-out, retry, ralph, drain and
// scope paths all resolve their member set here, and findSpecBead depends on
// it, because a gc.kind=spec sidecar has no dependency edges and is reachable
// by no other rule. See Membership for why the alternative rules are wrong for
// these consumers.
func DirectMembers(store Store, rootID string) ([]Bead, error) {
	all, err := HandlesFor(store).Live.List(ListQuery{
		Metadata:      map[string]string{beadmeta.RootBeadIDMetadataKey: rootID},
		IncludeClosed: true,
	})
	if err != nil {
		return nil, err
	}

	result := make([]Bead, 0, len(all)+1)
	seen := make(map[string]bool, len(all)+1)
	if root, err := store.Get(rootID); err == nil {
		result = append(result, root)
		seen[root.ID] = true
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	for _, bead := range all {
		if seen[bead.ID] {
			continue
		}
		result = append(result, bead)
		seen[bead.ID] = true
	}
	return result, nil
}
