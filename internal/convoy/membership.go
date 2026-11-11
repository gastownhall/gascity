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
	if !owner.sameAs(workHandle("convoy", classes.Convoy)) {
		return fmt.Errorf("tracking %s in convoy %s: member is owned by the %s class store: %w",
			itemID, convoyID, owner.class, ErrMemberNotCoResident)
	}
	if err := classes.Convoy.DepAdd(convoyID, itemID, TrackingDepType); err != nil {
		return fmt.Errorf("adding %s dependency %s -> %s: %w", TrackingDepType, convoyID, itemID, err)
	}
	return nil
}

// UntrackItem removes a convoy membership edge from convoyID to itemID.
func UntrackItem(store beads.WorkStore, convoyID, itemID string) error {
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
	return membersFrom(classes.Convoy, convoyID, includeClosed, func(id string) (beads.Bead, error) {
		item, _, err := classes.resolveMember(id)
		return item, err
	})
}

// MembersOn is MembersIn over one read-only leg of a convoy federation (a
// storeref plan leg): the convoy's own store is the only class it spans, as
// MembersIn's is when the caller names the convoy's class alone. A member is
// resolved with one read of the leg; a read failure carries the convoy class
// as provenance, and an absent member is an unresolved placeholder, as
// MembersIn reports them.
func MembersOn(leg beads.FederatedReader, convoyID string, includeClosed bool) ([]beads.Bead, error) {
	if beads.IsNilStore(leg) {
		return nil, fmt.Errorf("listing members of convoy %s: %w", convoyID, ErrNoConvoyClass)
	}
	return membersFrom(leg, convoyID, includeClosed, func(id string) (beads.Bead, error) {
		item, err := leg.Get(id)
		switch {
		case err == nil:
			return item, nil
		case errors.Is(err, beads.ErrNotFound):
			return beads.Bead{}, fmt.Errorf("resolving %s across convoy: %w", id, err)
		default:
			return beads.Bead{}, fmt.Errorf("reading %s from the convoy class store: %w", id, err)
		}
	})
}

// convoyReads are the reads a convoy's own store answers for its membership.
type convoyReads interface {
	Get(id string) (beads.Bead, error)
	List(query beads.ListQuery) ([]beads.Bead, error)
	DepList(id, direction string) ([]beads.Dep, error)
}

// membersFrom is MembersIn's and MembersOn's walk: the legacy children and the
// tracks edges read from store, each tracked member resolved by resolve.
func membersFrom(store convoyReads, convoyID string, includeClosed bool, resolve func(id string) (beads.Bead, error)) ([]beads.Bead, error) {
	legacyChildren, err := store.List(beads.ListQuery{
		ParentID:      convoyID,
		IncludeClosed: includeClosed,
		Sort:          beads.SortCreatedAsc,
	})
	if err != nil {
		return nil, fmt.Errorf("listing legacy convoy children of %s: %w", convoyID, err)
	}

	seen := make(map[string]bool, len(legacyChildren))
	members := make([]beads.Bead, 0, len(legacyChildren))
	add := func(b beads.Bead) {
		if seen[b.ID] {
			return
		}
		if !includeClosed && IsTerminalStatus(b.Status) {
			return
		}
		seen[b.ID] = true
		members = append(members, b)
	}
	for _, child := range legacyChildren {
		add(child)
	}

	deps, err := store.DepList(convoyID, "down")
	if err != nil {
		return nil, fmt.Errorf("listing convoy %s dependencies: %w", convoyID, err)
	}
	for _, dep := range deps {
		if dep.Type != TrackingDepType {
			continue
		}
		item, err := resolve(dep.DependsOnID)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) || errors.Is(err, beads.ErrMetadataParse) {
				// Convoy membership is an edge inventory. Keep the edge visible
				// even when the target bead cannot be projected into a Bead.
				add(unresolvedTrackedItem(dep.DependsOnID))
				continue
			}
			return nil, fmt.Errorf("getting tracked item %s: %w", dep.DependsOnID, err)
		}
		add(item)
	}

	sortMembers(members)
	return members, nil
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
func HasTrack(store beads.WorkStore, convoyID, itemID string) (bool, error) {
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
func TrackingConvoysForItem(store beads.WorkStore, itemID string) ([]beads.Bead, error) {
	return trackingConvoys(store, itemID)
}

// TrackingConvoysOn is TrackingConvoysForItem over one read-only leg of a
// convoy federation (a storeref plan leg), with the same answer and errors.
func TrackingConvoysOn(leg beads.FederatedReader, itemID string) ([]beads.Bead, error) {
	if beads.IsNilStore(leg) {
		return nil, fmt.Errorf("listing dependents of item %s: %w", itemID, ErrNoConvoyClass)
	}
	return trackingConvoys(leg, itemID)
}

// trackingConvoys is TrackingConvoysForItem's walk over store.
func trackingConvoys(store convoyReads, itemID string) ([]beads.Bead, error) {
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
