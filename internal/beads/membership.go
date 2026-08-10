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
// with no dependency edges at all — formula.newSourceSpecStep returns a fresh
// Step literal that never SETS DependsOn, Needs or WaitsFor, and
// formula.namespaceSourceSpecStep is the one place that actively nils them,
// on the clone a ralph iteration re-namespaces — and this molecule linked none
// of them by hand, so the set dep-reachability dropped happened to equal the
// set the consumer was filtering out anyway.
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
// # No rule here has a tier axis
//
// Every rule below spans BOTH bead storage tiers. That has to be stated
// because it is not what a ListQuery does by default: ListQuery's zero-value
// TierMode is TierIssues, which drops every Ephemeral row, so a projection
// that just fills in a Metadata filter answers a strictly narrower question
// than the one it names. DirectMembers avoids that by reading through
// HandlesFor(store).Live, which forces TierBoth.
//
// The tier is not an edge case for molecules. A wisp molecule materializes
// with EVERY node in ephemeral storage — the wisp bead policy resolves to
// ephemeral storage under bd-1.0.5 ready semantics, and molecule
// instantiation stamps gc.root_bead_id on every node — so a tier-scoped
// membership read of a wisp molecule returns the root and nothing else. Bead
// Get is tier-agnostic, so the root always resolves and the read always
// succeeds: the molecule reads as complete and empty rather than as missing.
// If a surface ever does want a tier restriction, this vocabulary needs a
// tier axis and the value it publishes must say so — an unqualified
// "direct-root-id" is what a consumer will assert against.
//
// # Which surface implements which rule
//
//	beads.DirectMembers .............................. MembershipDirectRootID
//	dispatch fan-out / retry / ralph / drain / scope .. MembershipDirectRootID
//	                                                    (via DirectMembers)
//	api workflowSQLQueryWorkflowBeads (Dolt fast path)
//	                                                 .. MembershipDirectRootID
//	                                                    (re-expressed in SQL, both
//	                                                    tier tables)
//	api snapshotFromStore fallback ................... MembershipDirectRootID
//	                                                    MINUS the ephemeral tier —
//	                                                    a known divergence, see below
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
//
// # The outstanding divergence between the direct-root-id implementations
//
// MembershipDirectRootID has three Go/SQL expressions: DirectMembers,
// api.workflowSQLQueryWorkflowBeads, and api.snapshotFromStore's fallback.
// They do NOT agree today, and the disagreement is not drift waiting to
// happen or a cache-staleness window — it is a permanent whole-tier set
// difference that is present right now:
//
//   - DirectMembers reads TierBoth (via the LIVE handle).
//   - workflowSQLQueryWorkflowBeads reads both the issues and wisps tables,
//     so it is TierBoth too.
//   - snapshotFromStore's fallback lists at the zero-value TierMode, so it is
//     TierIssues and cannot see a wisp molecule's members at all.
//
// The last two are the two branches of ONE endpoint, selected by whether the
// Dolt server is reachable, so that endpoint's answer for a wisp molecule
// depends on which branch ran. Collapsing the Go copies onto DirectMembers
// (with an option selecting the read handle, so a hot dashboard path is not
// silently moved onto a cache-bypassing read) plus a conformance test that
// the SQL path and the fallback return the same set is the fix; it is a
// behavior change on a dashboard read path and is deliberately not made
// here. Tracked as ga-212sl.
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
	// the transitive parent-child closure taken over that whole set, not over
	// the root alone: a member reparented outside the molecule still
	// contributes its own descendants. It is a superset of
	// MembershipDirectRootID — materialization sets ParentID from parent-child
	// edges as well as stamping the root id, so the closure adds only beads
	// attached to a member after the fact.
	//
	// Like MembershipDirectRootID it has no tier axis: the ephemeral (wisp)
	// tier is in scope, because a wisp molecule's beads are ALL ephemeral and
	// a tier-scoped answer would report one as empty rather than as missing.
	MembershipRootIDAndParentClosure Membership = "direct-root-id+parent-closure"

	// MembershipRootIDParentClosureAndConvoy is
	// MembershipRootIDAndParentClosure widened with the members of the root
	// when the root is a container (convoy) bead. Convoy membership is a
	// tracks-edge relation, not a root-id relation, so it is named separately
	// rather than folded into the parent closure — but the parent closure is
	// then taken over the convoy's members too, so their descendants come with
	// them. That is the point of the rule for a display consumer: a convoy
	// graph that stopped at each member and hid its subtree would be a
	// different, less useful answer.
	MembershipRootIDParentClosureAndConvoy Membership = "direct-root-id+parent-closure+convoy-members"
)

// String returns the wire spelling of the membership rule.
func (m Membership) String() string { return string(m) }

// AllMemberships returns every rule this vocabulary defines, in declaration
// order. It exists so a wire schema can be checked against the vocabulary
// rather than against a second hand-maintained copy of it: an OpenAPI enum
// naming a rule no constant defines is a typo that would otherwise reach a
// generated client. TestAllMembershipsCoversEveryDeclaredConstant keeps this
// list from going stale.
func AllMemberships() []Membership {
	return []Membership{
		MembershipDirectRootID,
		MembershipDepReachable,
		MembershipRootIDAndParentClosure,
		MembershipRootIDParentClosureAndConvoy,
	}
}

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
