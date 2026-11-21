package dispatch

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
	"github.com/gastownhall/gascity/internal/storeref"
)

// retireWorkflowDrainUnitConvoys retires, for finalizer, the finalizer bead of
// the terminal workflow rootID, the open unit convoys of drains: the drain
// controls its terminal sweep reached, which the sweep closed or found closed.
// Each unit closes with the verdict its row reached in its drain's manifest,
// and with fallback, the sweep's outcome, when its row reached none
// (sourceworkflow.RetireDrainUnitConvoys).
//
// A drain closed through closeDrainControl already retired its units, so this
// finds none of them. It catches a drain closed some other way (the root-state
// gate, a scope abort, the sweep itself, an operator) and retires its units as
// the workflow settles, where `gc convoy check` (DrainUnitConvoyRetirement)
// would retire them only on its next pass: a unit carries no gc.root_bead_id,
// so no sweep of the workflow's members lists it.
//
// A unit convoy is bookkeeping, as closeDrainControl treats it: a transient
// error is returned, so the finalizer stays open and its retry closes the
// rest, and any other error is traced and left to `gc convoy check`, so the
// workflow still settles.
func retireWorkflowDrainUnitConvoys(store beads.GraphStore, finalizer beads.Bead, rootID string, drains []beads.Bead, fallback string, opts ProcessOptions) error {
	retired, err := retireDrainUnitConvoysWhereMinted(store, drains, fallback, opts)
	if retired > 0 {
		opts.tracef("workflow-finalize bead=%s root=%s retired-units=%d", finalizer.ID, rootID, retired)
	}
	if err == nil || IsTransientControllerError(err) {
		return err
	}
	opts.tracef("workflow-finalize bead=%s root=%s retire-units-deferred err=%s", finalizer.ID, rootID, strings.ReplaceAll(err.Error(), "\n", "; "))
	return nil
}

// retireDrainUnitConvoysWhereMinted retires the open unit convoys of drains in
// every store a drain could have minted them in, and returns how many it
// closed.
//
// Those stores are the RoutedWork plan of the drain's own frame
// (drainResidency). A drain on a city whose graph class relocated runs with
// the member tail (controlMemberTail) and mints each unit in the store that
// owns its member: the scope work store, or the binding. Only the kinds that
// read beads they did not create are dispatched with the tail, so a caller of
// another kind, the workflow finalizer, takes it here on the same test: Work
// names a store other than the graph store.
func retireDrainUnitConvoysWhereMinted(store beads.GraphStore, drains []beads.Bead, fallback string, opts ProcessOptions) (int, error) {
	if len(drains) == 0 {
		return 0, nil
	}
	frame := opts
	frame.MemberTail = opts.MemberTail || (opts.Work != (beads.WorkStore{}) && !beads.SameStore(opts.Work, store))
	topo, err := drainResidency(store, frame)
	if err != nil {
		return 0, err
	}
	plan, err := storeref.Plan(storeref.RoutedWork{}, topo)
	if err != nil {
		return 0, err
	}
	closed := 0
	res, err := storeref.Sweep(plan, func(_ storeref.TypedLeg, leg beads.ResidentStore) error {
		for _, drain := range drains {
			n, err := sourceworkflow.RetireDrainUnitConvoys(leg, drain, fallback)
			closed += n
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return closed, err
	}
	if res.Partial || len(res.LegErrors) > 0 {
		legErrs := make([]error, 0, len(res.LegErrors))
		for _, legErr := range res.LegErrors {
			legErrs = append(legErrs, fmt.Errorf("%s: %w", legErr.Ref, legErr.Err))
		}
		return closed, fmt.Errorf("sweep read only part of its plan: %w", errors.Join(legErrs...))
	}
	return closed, nil
}
