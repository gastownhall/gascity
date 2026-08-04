package convoy

import (
	"errors"
	"fmt"
	"sort"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storeref"
)

// TrackingDepType is the dependency type used for convoy membership edges.
const TrackingDepType = "tracks"

const trackedStatusUnknown = "unknown"

// IsTerminalStatus reports whether a tracked item should count as complete for
// convoy progress and auto-close decisions.
func IsTerminalStatus(status string) bool {
	return status == "closed" || status == "tombstone"
}

// TrackItem records that convoyID tracks itemID without changing itemID's
// parent-child relationship.
//
// The convoy bead and the tracks dependency edge always live in store. The
// tracked item itself may live in a different (per-class) store; memberStores
// supplies the additional member stores to probe when verifying that itemID
// exists. The probe order is store first, then memberStores. On origin/main
// every class collapses to the one store, so the default empty memberStores
// reproduces today's single-store store.Get(itemID) probe exactly.
func TrackItem(store beads.Store, convoyID, itemID string, memberStores ...beads.Store) error {
	if _, err := storeref.Resolve(itemID, memberProbeSet(store, memberStores)); err != nil {
		return fmt.Errorf("getting tracked item %s: %w", itemID, err)
	}
	if err := store.DepAdd(convoyID, itemID, TrackingDepType); err != nil {
		return fmt.Errorf("adding %s dependency %s -> %s: %w", TrackingDepType, convoyID, itemID, err)
	}
	return nil
}

// memberProbeSet returns the ordered store set used to resolve a tracked member
// bead: the convoy's primary store first, then any additional per-class member
// stores. Resolving through this set preserves the "probe every candidate"
// contract so a placeholder is emitted only after every store reports the bead
// absent.
func memberProbeSet(store beads.Store, memberStores []beads.Store) []beads.Store {
	probe := make([]beads.Store, 0, 1+len(memberStores))
	probe = append(probe, store)
	probe = append(probe, memberStores...)
	return probe
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

// Members returns beads tracked by a convoy. It supports both the current
// tracks dependency relation and legacy parent-child convoy membership.
// Unresolved tracks dependencies are returned with unknown status so completion
// paths never mistake missing dependency details for completed work.
//
// The convoy bead's own membership (legacy parent-child List and the tracks
// DepList) is read from store. Each tracked member bead is resolved across
// store first, then memberStores, so members that live in a different per-class
// store are still materialized. A member is replaced by an unresolved
// placeholder only after every probed store reports it absent; on origin/main
// the single store collapses memberStores to the empty default and the probe is
// the original store.Get.
func Members(store beads.Store, convoyID string, includeClosed bool, memberStores ...beads.Store) ([]beads.Bead, error) {
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
	probe := memberProbeSet(store, memberStores)
	for _, dep := range deps {
		if dep.Type != TrackingDepType {
			continue
		}
		item, err := storeref.Resolve(dep.DependsOnID, probe)
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
// Membership edges are stored on the convoy bead in store, so HasTrack reads
// only store's dependency list. memberStores is accepted for signature
// uniformity with the other membership helpers and is not probed; the tracks
// edge alone answers the question without materializing the member bead.
func HasTrack(store beads.Store, convoyID, itemID string, memberStores ...beads.Store) (bool, error) {
	_ = memberStores
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
// The dependency edges and the tracking convoy beads both live in store (the
// convoy-owning store), so the convoy Get reads from store. memberStores is
// accepted for signature uniformity with the other membership helpers and is
// not probed: it names member stores, whereas this lookup materializes convoys,
// which are co-resident with the edges in store.
func TrackingConvoysForItem(store beads.Store, itemID string, memberStores ...beads.Store) ([]beads.Bead, error) {
	_ = memberStores
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
