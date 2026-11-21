package sourceworkflow

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// FailedMoleculeSweepCloseReason is the close_reason the failed-molecule sweep stamps.
const FailedMoleculeSweepCloseReason = "failed-molecule sweep: instantiation failed and no caller re-entered within the grace window"

// FailedMoleculeSweep reports one SweepFailedMolecules pass over one store.
type FailedMoleculeSweep struct {
	Chains      []string // owner IDs whose chains this pass closed, oldest first
	ChainClosed []int    // beads newly closed per entry of Chains, parallel to it
	Closed      int      // beads newly closed
	More        bool     // sweepable chains remained beyond maxChains
}

// SweepFailedMolecules closes, oldest first and at most maxChains per call,
// every chain of open molecule_failed beads whose owner is not a live workflow
// and whose failure stamp is at or before cutoff.
//
// A molecule whose instantiation failed is stamped molecule_failed and left
// open (molecule.markFailed). Its same-key caller closes it on re-entry; a
// keyless mint (an order dispatch) has none, so this sweep is its owner. A
// chain is its owner's open members: a top-level failed root with every open
// bead under it by gc.root_bead_id or by a ParentID walk through failed beads,
// or, when the owner a failed sub-DAG names by gc.root_bead_id has closed or
// is gone, those orphaned failed members. A failed sub-DAG whose owner is an
// open workflow that did not fail belongs to that workflow and is left alone,
// as is a chain whose owner is stamped gc.gc_exempt=true and every
// controller-owned row. Each chain closes in one ordered CloseAll batch with
// gc.outcome=skipped and FailedMoleculeSweepCloseReason; molecule_failed stays
// on the closed beads, so the same-key adoption lookups never take one back.
//
// A discovery read error returns at once. A per-chain read or close error is
// wrapped with the owner's ID and joined into the returned error, and the
// pass continues with the next chain. maxChains <= 0 places no cap.
func SweepFailedMolecules(store beads.MoleculeTarget, cutoff time.Time, maxChains int) (FailedMoleculeSweep, error) {
	var out FailedMoleculeSweep
	if store.Absent() {
		return out, fmt.Errorf("sweeping failed molecules: bead store unavailable")
	}
	reader := beads.LiveFor(store)
	discovered, err := reader.List(beads.ListQuery{
		Metadata: map[string]string{beadmeta.MoleculeFailedMetadataKey: "true"},
		// UpdatedBefore is exclusive; a stamp exactly at cutoff is past grace.
		UpdatedBefore: cutoff.Add(time.Nanosecond),
		TierMode:      beads.TierBoth,
		Live:          true,
		Sort:          beads.SortCreatedAsc,
	})
	if err != nil {
		return out, fmt.Errorf("sweeping failed molecules: listing molecule_failed beads: %w", err)
	}
	byID := make(map[string]beads.Bead, len(discovered))
	candidates := make([]beads.Bead, 0, len(discovered))
	for _, b := range discovered {
		if b.Status == "closed" || coordclass.ControllerOwned(b) || failureStampAfter(b, cutoff) {
			continue
		}
		byID[b.ID] = b
		candidates = append(candidates, b)
	}
	var owners []string
	grouped := make(map[string]bool)
	for _, b := range candidates {
		if owner := failedChainOwner(b, byID); !grouped[owner] {
			grouped[owner] = true
			owners = append(owners, owner)
		}
	}
	var errs []error
	for _, ownerID := range owners {
		orphaned := false
		if root, ok := byID[ownerID]; ok {
			if root.Metadata[beadmeta.GCExemptMetadataKey] == "true" {
				continue
			}
		} else {
			sweepable, err := ownerReleasedChain(reader, ownerID)
			if err != nil {
				errs = append(errs, fmt.Errorf("closing failed molecule %s: %w", ownerID, err))
				continue
			}
			if !sweepable {
				continue
			}
			orphaned = true
		}
		if maxChains > 0 && len(out.Chains) >= maxChains {
			out.More = true
			break
		}
		n, err := closeFailedChain(store, ownerID, orphaned)
		out.Closed += n
		if err != nil {
			errs = append(errs, fmt.Errorf("closing failed molecule %s: %w", ownerID, err))
			continue
		}
		out.Chains = append(out.Chains, ownerID)
		out.ChainClosed = append(out.ChainClosed, n)
	}
	return out, errors.Join(errs...)
}

// failedChainOwner is the ID of the bead that owns b's failed chain: the walk
// up from b follows a gc.root_bead_id naming another bead, else b's ParentID,
// while the bead it reaches is a sweep candidate in byID, and stops at the
// first bead it cannot follow, at the ID a gc.root_bead_id names outside
// byID, or at a cycle.
func failedChainOwner(b beads.Bead, byID map[string]beads.Bead) string {
	seen := map[string]bool{b.ID: true}
	for {
		next := strings.TrimSpace(b.Metadata[beadmeta.RootBeadIDMetadataKey])
		named := next != "" && next != b.ID
		if !named {
			next = strings.TrimSpace(b.ParentID)
		}
		up, ok := byID[next]
		if !ok || seen[up.ID] {
			if named && !ok {
				return next
			}
			return b.ID
		}
		seen[up.ID] = true
		b = up
	}
}

// ownerReleasedChain reports whether the owner a failed sub-DAG names, which
// is not itself a sweep candidate, has let its failed members go: it is gone
// or closed, and not stamped gc.gc_exempt=true. An open owner keeps them,
// whether it failed too and is still inside the grace window or is a live
// workflow that did not fail.
func ownerReleasedChain(reader beads.LiveReader, ownerID string) (bool, error) {
	owner, err := reader.Get(ownerID)
	if errors.Is(err, beads.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading owner: %w", err)
	}
	if owner.Metadata[beadmeta.GCExemptMetadataKey] == "true" {
		return false, nil
	}
	return owner.Status == "closed", nil
}

// failureStampAfter reports whether b's failure stamp, its UpdatedAt or its
// CreatedAt when UpdatedAt is zero, is after cutoff: inside the grace window.
func failureStampAfter(b beads.Bead, cutoff time.Time) bool {
	stamp := b.UpdatedAt
	if stamp.IsZero() {
		stamp = b.CreatedAt
	}
	return stamp.After(cutoff)
}

// closeFailedChain closes the open members of ownerID's failed chain in one
// ordered batch, read fresh across both tiers: the owner and every bead naming
// it by gc.root_bead_id, or, for an orphaned chain whose owner is closed or
// gone, only the failed beads naming it, each with its failed ParentID
// descendants.
func closeFailedChain(store beads.MoleculeTarget, ownerID string, orphaned bool) (int, error) {
	reader := beads.LiveFor(store)
	memberQuery := map[string]string{beadmeta.RootBeadIDMetadataKey: ownerID}
	var chain []beads.Bead
	if orphaned {
		memberQuery[beadmeta.MoleculeFailedMetadataKey] = "true"
	} else {
		root, err := reader.Get(ownerID)
		if err != nil {
			return 0, fmt.Errorf("reading root: %w", err)
		}
		chain = append(chain, root)
	}
	members, err := reader.List(beads.ListQuery{
		Metadata: memberQuery,
		TierMode: beads.TierBoth,
		Live:     true,
	})
	if err != nil {
		return 0, fmt.Errorf("listing members: %w", err)
	}
	chain = append(chain, members...)
	descendants, err := failedDescendants(reader, chain)
	if err != nil {
		return 0, fmt.Errorf("walking children: %w", err)
	}
	ids, err := orderedOpenBeads(store, append(chain, descendants...), nil)
	if err != nil {
		return 0, fmt.Errorf("ordering close batch: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	return store.CloseAll(ids, map[string]string{
		beadmeta.OutcomeMetadataKey: beadmeta.OutcomeSkipped,
		"close_reason":              FailedMoleculeSweepCloseReason,
	})
}

// failedDescendants walks ParentID links down from chain, level by level
// across both tiers, and returns the open molecule_failed beads it reaches
// that chain does not already hold. A v1 molecule's steps name their root
// only by ParentID. The walk descends only through beads it returns, and
// filters each level in memory because a backend may answer ParentIDs with
// a superset.
func failedDescendants(reader beads.LiveReader, chain []beads.Bead) ([]beads.Bead, error) {
	seen := make(map[string]bool, len(chain))
	level := make([]string, 0, len(chain))
	for _, b := range chain {
		if !seen[b.ID] {
			seen[b.ID] = true
			level = append(level, b.ID)
		}
	}
	var out []beads.Bead
	for len(level) > 0 {
		parents := make(map[string]bool, len(level))
		for _, id := range level {
			parents[id] = true
		}
		kids, err := reader.List(beads.ListQuery{ParentIDs: level, AllowScan: true, TierMode: beads.TierBoth, Live: true})
		if err != nil {
			return nil, err
		}
		level = level[:0:0]
		for _, kid := range kids {
			if seen[kid.ID] || !parents[strings.TrimSpace(kid.ParentID)] ||
				kid.Status == "closed" || kid.Metadata[beadmeta.MoleculeFailedMetadataKey] != "true" {
				continue
			}
			seen[kid.ID] = true
			out = append(out, kid)
			level = append(level, kid.ID)
		}
	}
	return out, nil
}
