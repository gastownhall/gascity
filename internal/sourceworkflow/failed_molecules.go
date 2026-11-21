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
	var errs []error
	for _, root := range discovered {
		if root.Status == "closed" || coordclass.ControllerOwned(root) || failureStampAfter(root, cutoff) {
			continue
		}
		if owner := strings.TrimSpace(root.Metadata[beadmeta.RootBeadIDMetadataKey]); owner != "" && owner != root.ID {
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
	ids, err := orderedOpenBeads(store, append([]beads.Bead{root}, members...), nil)
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
