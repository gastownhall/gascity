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
	Chains []string // owner IDs whose chains this pass closed, oldest first
	Closed int      // beads newly closed
	More   bool     // sweepable chains remained beyond maxChains
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
	for _, b := range discovered {
		byID[b.ID] = b
	}
	var errs []error
	for _, root := range discovered {
		if root.Status == "closed" || coordclass.ControllerOwned(root) || failureStampAfter(root, cutoff) {
			continue
		}
		if failedChainOwner(root, byID) != root.ID || root.Metadata[beadmeta.GCExemptMetadataKey] == "true" {
			continue
		}
		if maxChains > 0 && len(out.Chains) >= maxChains {
			out.More = true
			break
		}
		n, err := closeFailedChain(store, root.ID)
		out.Closed += n
		if err != nil {
			errs = append(errs, fmt.Errorf("closing failed molecule %s: %w", root.ID, err))
			continue
		}
		out.Chains = append(out.Chains, root.ID)
	}
	return out, errors.Join(errs...)
}

// failedChainOwner is the ID of the bead that owns b's failed chain: the
// first gc.root_bead_id naming another bead on the walk up b's failed
// ancestors in byID, else the topmost such ancestor. The walk stops at a
// cycle.
func failedChainOwner(b beads.Bead, byID map[string]beads.Bead) string {
	seen := map[string]bool{}
	for {
		if owner := strings.TrimSpace(b.Metadata[beadmeta.RootBeadIDMetadataKey]); owner != "" && owner != b.ID {
			return owner
		}
		seen[b.ID] = true
		parent, ok := byID[strings.TrimSpace(b.ParentID)]
		if !ok || seen[parent.ID] {
			return b.ID
		}
		b = parent
	}
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

// closeFailedChain closes the open members of the failed chain rooted at
// ownerID in one ordered batch, read fresh across both tiers.
func closeFailedChain(store beads.MoleculeTarget, ownerID string) (int, error) {
	reader := beads.LiveFor(store)
	root, err := reader.Get(ownerID)
	if err != nil {
		return 0, fmt.Errorf("reading root: %w", err)
	}
	members, err := reader.List(beads.ListQuery{
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: ownerID},
		TierMode: beads.TierBoth,
		Live:     true,
	})
	if err != nil {
		return 0, fmt.Errorf("listing members: %w", err)
	}
	chain := append([]beads.Bead{root}, members...)
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
