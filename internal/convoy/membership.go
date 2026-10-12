package convoy

import (
	"errors"
	"fmt"
	"sort"

	"github.com/gastownhall/gascity/internal/beads"
)

// TrackingDepType is the dependency type used for convoy membership edges.
const TrackingDepType = "tracks"

const trackedStatusUnknown = "unknown"

// IsTerminalStatus reports whether a tracked item should count as complete for
// convoy progress and auto-close decisions.
func IsTerminalStatus(status string) bool {
	return status == "closed" || status == "tombstone"
}

// TrackItem records that convoyID tracks itemID, spanning only the one class
// handle store.
//
// This is the residual unnamed call shape kept for the consumer packages that
// have not yet been converted to name their classes (internal/api,
// internal/dispatch, internal/graphv2, internal/sling and the non-convoy
// cmd/gc commands). Its memberStores tail is interpreted as Work-class scopes —
// the meaning its one non-empty caller already documents
// (dispatch.ProcessOptions.MemberStores, "the work-class store tail"). New code
// names its classes and calls TrackItemIn.
func TrackItem(store beads.Store, convoyID, itemID string, memberStores ...beads.Store) error {
	return TrackItemIn(MemberClasses{Convoy: store, Work: memberStores}, convoyID, itemID)
}

// TrackItemIn records that convoyID tracks itemID without changing itemID's
// parent-child relationship.
//
// The convoy bead and the tracks dependency edge live in classes.Convoy; the
// item is resolved across every class the caller named. Ownership is proven
// before anything is written: a member owned by a class other than the convoy's
// own fails with ErrMemberNotCoResident and writes nothing, because a dep row
// cannot reference an id its own store cannot resolve. Two residences are a
// *DuplicateResidenceError, never a first-match winner.
func TrackItemIn(classes MemberClasses, convoyID, itemID string) error {
	if err := classes.requireConvoyHandle(); err != nil {
		return fmt.Errorf("tracking %s in convoy %s: %w", itemID, convoyID, err)
	}
	_, owner, err := classes.resolveMember(itemID)
	if err != nil {
		return fmt.Errorf("getting tracked item %s: %w", itemID, err)
	}
	if !sameHandle(owner.store, classes.Convoy) {
		return fmt.Errorf("tracking %s in convoy %s: member is owned by the %s class store: %w",
			itemID, convoyID, owner.class, ErrMemberNotCoResident)
	}
	if err := classes.Convoy.DepAdd(convoyID, itemID, TrackingDepType); err != nil {
		return fmt.Errorf("adding %s dependency %s -> %s: %w", TrackingDepType, convoyID, itemID, err)
	}
	return nil
}

// UntrackItem removes a convoy membership edge from convoyID to itemID.
func UntrackItem(store beads.Store, convoyID, itemID string) error {
	deps, err := store.DepList(convoyID, "down")
	if err != nil {
		return fmt.Errorf("listing convoy %s dependencies: %w", convoyID, err)
	}
	hasTrack := false
	var mixedTypes []string
	for _, dep := range deps {
		if dep.IssueID != convoyID || dep.DependsOnID != itemID {
			continue
		}
		if dep.Type == TrackingDepType {
			hasTrack = true
			continue
		}
		mixedTypes = append(mixedTypes, dep.Type)
	}
	if !hasTrack {
		return nil
	}
	if len(mixedTypes) > 0 {
		return fmt.Errorf("not removing ambiguous %s dependency %s -> %s with other dependency types: %v", TrackingDepType, convoyID, itemID, mixedTypes)
	}
	if err := store.DepRemove(convoyID, itemID); err != nil {
		return fmt.Errorf("removing %s dependency %s -> %s: %w", TrackingDepType, convoyID, itemID, err)
	}
	return nil
}

// Members returns the beads tracked by a convoy, spanning only the one class
// handle store.
//
// This is the residual unnamed call shape kept for the consumer packages that
// have not yet been converted to name their classes; see TrackItem for the
// inventory and for how the memberStores tail is interpreted. New code names
// its classes and calls MembersIn.
func Members(store beads.Store, convoyID string, includeClosed bool, memberStores ...beads.Store) ([]beads.Bead, error) {
	return MembersIn(MemberClasses{Convoy: store, Work: memberStores}, convoyID, includeClosed)
}

// MembersIn returns beads tracked by a convoy. It supports both the current
// tracks dependency relation and legacy parent-child convoy membership.
// Unresolved tracks dependencies are returned with unknown status so completion
// paths never mistake missing dependency details for completed work.
//
// The convoy's own membership (legacy parent-child List and the tracks DepList)
// is read from classes.Convoy, which owns those edges. Each tracked member bead
// is resolved across the classes the caller named, and the partial-result rule
// decides what an absent member means:
//
//   - A class the caller did not name contributes nothing. A member owned by
//     an unnamed class is reported as an unresolved placeholder — an empty
//     result for a lookup that never spanned it — and the read succeeds.
//   - A class the caller did name contributes its failures. If a named class
//     cannot be read, the error is returned with that class as provenance
//     instead of degrading the member to a placeholder, so an unreachable
//     store never looks like a deleted bead.
func MembersIn(classes MemberClasses, convoyID string, includeClosed bool) ([]beads.Bead, error) {
	if err := classes.requireConvoyHandle(); err != nil {
		return nil, fmt.Errorf("listing members of convoy %s: %w", convoyID, err)
	}
	store := classes.Convoy
	legacyChildren, err := store.List(beads.ListQuery{
		ParentID:      convoyID,
		IncludeClosed: includeClosed,
		Sort:          beads.SortCreatedAsc,
	})
	if err != nil {
		return nil, fmt.Errorf("listing legacy convoy children of %s: %w", convoyID, err)
	}

	deps, err := store.DepList(convoyID, "down")
	if err != nil {
		return nil, fmt.Errorf("listing convoy %s dependencies: %w", convoyID, err)
	}
	var tracked []beads.Bead
	for _, dep := range deps {
		if dep.Type != TrackingDepType {
			continue
		}
		item, _, err := classes.resolveMember(dep.DependsOnID)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) || errors.Is(err, beads.ErrMetadataParse) {
				// Convoy membership is an edge inventory. Keep the edge visible
				// even when the target bead cannot be projected into a Bead.
				tracked = append(tracked, unresolvedTrackedItem(dep.DependsOnID))
				continue
			}
			return nil, fmt.Errorf("getting tracked item %s: %w", dep.DependsOnID, err)
		}
		tracked = append(tracked, item)
	}
	return assembleMembers(legacyChildren, tracked, includeClosed), nil
}

// MembersBatch returns the members of each convoy in convoyIDs, keyed by convoy
// id, spanning only the one class handle store. It is the batch form of
// Members(store, id, includeClosed) with no member stores, which is how the gc
// convoy commands read children, and it answers every convoy exactly as that
// call would.
//
// On the bd store, Members costs a bd process per read: a legacy parent-child
// list (and its wisp-tier twin), a dep list and a show per tracked member, per
// convoy. On a store holding about a thousand open convoys that is hours. MembersBatch reads the same answer for
// every convoy in a fixed number of reads:
//
//   - Legacy children: one batched up read of the parent-child dependents of
//     all the convoys, then those rows by id. A row belongs to the convoy its
//     own parent field names, because that is the only row Members keeps
//     (ListQuery.Matches compares the parent field, which bd fills from the
//     parent-child edge, so a dotted child with no edge is in neither answer).
//   - Tracked members: one batched down read of every convoy's edges, then the
//     tracks targets by id.
//
// A convoy whose tracks edge names a target the by-id read did not return (a
// dangling edge, a row in another database, a row outside the tiers the store
// lists) is read again by Members alone, because those are exactly the cases where Members' own
// reads decide what the member is. A store without both batched reads, or a
// single convoy, takes Members per convoy.
func MembersBatch(store beads.Store, convoyIDs []string, includeClosed bool) (map[string][]beads.Bead, error) {
	out := make(map[string][]beads.Bead, len(convoyIDs))
	if len(convoyIDs) == 0 {
		return out, nil
	}
	edgeReader, haveEdges := beads.DepListBatchFor(store)
	dependentReader, haveDependents := beads.DependentBatchFor(store)
	// One id is not a batch: bd's dep list answers a single anchor in the
	// neighbor shape rather than as edge records.
	if isNilStore(store) || !haveEdges || !haveDependents || len(convoyIDs) < 2 {
		return membersOneByOne(store, convoyIDs, includeClosed, out)
	}

	wanted := make(map[string]bool, len(convoyIDs))
	for _, id := range convoyIDs {
		wanted[id] = true
	}

	childIDs, err := dependentReader.DependentIDsBatch(convoyIDs, "parent-child")
	if errors.Is(err, beads.ErrDepListBatchUnsupported) {
		return membersOneByOne(store, convoyIDs, includeClosed, out)
	}
	if err != nil {
		return nil, fmt.Errorf("listing legacy convoy children: %w", err)
	}
	legacy := make(map[string][]beads.Bead)
	if len(childIDs) > 0 {
		rows, err := store.List(beads.ListQuery{
			IDs:           childIDs,
			IncludeClosed: includeClosed,
			Sort:          beads.SortCreatedAsc,
		})
		if err != nil {
			return nil, fmt.Errorf("reading legacy convoy children: %w", err)
		}
		for _, b := range rows {
			if wanted[b.ParentID] {
				legacy[b.ParentID] = append(legacy[b.ParentID], b)
			}
		}
	}

	edges, err := edgeReader.DepListBatch(convoyIDs)
	if errors.Is(err, beads.ErrDepListBatchUnsupported) {
		return membersOneByOne(store, convoyIDs, includeClosed, out)
	}
	if err != nil {
		return nil, fmt.Errorf("listing convoy dependencies: %w", err)
	}
	var trackedIDs []string
	queued := make(map[string]bool)
	for _, id := range convoyIDs {
		for _, dep := range edges[id] {
			if dep.Type == TrackingDepType && !queued[dep.DependsOnID] {
				queued[dep.DependsOnID] = true
				trackedIDs = append(trackedIDs, dep.DependsOnID)
			}
		}
	}
	held := make(map[string]beads.Bead, len(trackedIDs))
	if len(trackedIDs) > 0 {
		rows, err := store.List(beads.ListQuery{IDs: trackedIDs, IncludeClosed: true})
		if err != nil {
			return nil, fmt.Errorf("reading tracked convoy members: %w", err)
		}
		for _, b := range rows {
			held[b.ID] = b
		}
	}

	for _, id := range convoyIDs {
		var tracked []beads.Bead
		readAlone := false
		for _, dep := range edges[id] {
			if dep.Type != TrackingDepType {
				continue
			}
			b, ok := held[dep.DependsOnID]
			if !ok {
				readAlone = true
				break
			}
			tracked = append(tracked, b)
		}
		if readAlone {
			members, err := Members(store, id, includeClosed)
			if err != nil {
				return nil, err
			}
			out[id] = members
			continue
		}
		out[id] = assembleMembers(legacy[id], tracked, includeClosed)
	}
	return out, nil
}

func membersOneByOne(store beads.Store, convoyIDs []string, includeClosed bool, out map[string][]beads.Bead) (map[string][]beads.Bead, error) {
	for _, id := range convoyIDs {
		members, err := Members(store, id, includeClosed)
		if err != nil {
			return nil, err
		}
		out[id] = members
	}
	return out, nil
}

// assembleMembers applies MembersIn's rules to rows already read: legacy
// children first, each id once, terminal members dropped unless includeClosed,
// then the canonical order.
func assembleMembers(legacy, tracked []beads.Bead, includeClosed bool) []beads.Bead {
	seen := make(map[string]bool, len(legacy)+len(tracked))
	members := make([]beads.Bead, 0, len(legacy)+len(tracked))
	for _, group := range [][]beads.Bead{legacy, tracked} {
		for _, b := range group {
			if seen[b.ID] {
				continue
			}
			if !includeClosed && IsTerminalStatus(b.Status) {
				continue
			}
			seen[b.ID] = true
			members = append(members, b)
		}
	}
	sortMembers(members)
	return members
}

func unresolvedTrackedItem(id string) beads.Bead {
	return beads.Bead{
		ID:     id,
		Title:  id,
		Type:   "task",
		Status: trackedStatusUnknown,
	}
}

// IsUnresolvedTrackedItem reports whether b is a synthetic placeholder for a
// dangling tracks dependency whose target bead is unavailable.
func IsUnresolvedTrackedItem(b beads.Bead) bool {
	return b.Status == trackedStatusUnknown && b.Type == "task" && b.Title == b.ID
}

// HasTrack reports whether convoyID has a tracks dependency to itemID.
//
// This spans exactly one class: membership edges are owned by the convoy's own
// class store, and the edge alone answers the question without materializing
// the member bead. It therefore takes no member classes — a caller cannot name
// a class here and be misled into thinking it was consulted.
func HasTrack(store beads.Store, convoyID, itemID string) (bool, error) {
	deps, err := store.DepList(convoyID, "down")
	if err != nil {
		return false, fmt.Errorf("listing convoy %s dependencies: %w", convoyID, err)
	}
	for _, dep := range deps {
		if dep.Type == TrackingDepType && dep.IssueID == convoyID && dep.DependsOnID == itemID {
			return true, nil
		}
	}
	return false, nil
}

// TrackingConvoysForItem returns convoy beads that track itemID via a tracks
// dependency. Dangling dependency sources are ignored.
//
// This spans exactly one class: the tracking edges and the convoy beads they
// point at are both owned by the convoy's class store, so that handle answers
// the whole lookup. It takes no member classes for the same reason HasTrack
// does not — the member's own store holds no part of this answer, and a
// parameter that is accepted but never read would let a caller believe a class
// participated when it did not.
func TrackingConvoysForItem(store beads.Store, itemID string) ([]beads.Bead, error) {
	deps, err := store.DepList(itemID, "up")
	if err != nil {
		return nil, fmt.Errorf("listing dependents of item %s: %w", itemID, err)
	}

	seen := make(map[string]bool, len(deps))
	convoys := make([]beads.Bead, 0, len(deps))
	for _, dep := range deps {
		if dep.Type != TrackingDepType || seen[dep.IssueID] {
			continue
		}
		b, err := store.Get(dep.IssueID)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("getting tracking convoy %s: %w", dep.IssueID, err)
		}
		if b.Type != "convoy" {
			continue
		}
		seen[b.ID] = true
		convoys = append(convoys, b)
	}
	sortMembers(convoys)
	return convoys, nil
}

func sortMembers(items []beads.Bead) {
	sort.SliceStable(items, func(i, j int) bool {
		left := items[i]
		right := items[j]
		if left.CreatedAt.Equal(right.CreatedAt) {
			return left.ID < right.ID
		}
		return left.CreatedAt.Before(right.CreatedAt)
	})
}
