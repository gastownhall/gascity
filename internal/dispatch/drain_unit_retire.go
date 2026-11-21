package dispatch

import (
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
	"github.com/gastownhall/gascity/internal/storeref"
)

// retireWorkflowDrainUnitConvoys closes, as outcome, the open unit convoys of
// every drain control among members, a terminal workflow's subtree. A drain
// closed through closeDrainControl already retired its units, so this finds
// none of them; it catches a drain closed some other way (the root-state gate,
// a scope abort, the finalizer's own residue sweep, an operator), whose units
// no other path reaches: a unit carries no gc.root_bead_id, so the residue
// sweep never lists it. It returns how many units it closed.
func retireWorkflowDrainUnitConvoys(store beads.GraphStore, members []beads.Bead, outcome string, opts ProcessOptions) (int, error) {
	drainIDs := sourceworkflow.DrainControlIDs(members)
	if len(drainIDs) == 0 {
		return 0, nil
	}
	// Units are synthetic convoys and so work-class beads: they live where the
	// drain minted them, which is a leg of the drain's RoutedWork plan.
	topo, err := drainResidency(store, opts)
	if err != nil {
		return 0, err
	}
	plan, err := storeref.Plan(storeref.RoutedWork{}, topo)
	if err != nil {
		return 0, err
	}
	closed := 0
	res, err := storeref.Sweep(plan, func(_ storeref.TypedLeg, leg beads.ResidentStore) error {
		for _, drainID := range drainIDs {
			n, err := sourceworkflow.RetireDrainUnitConvoys(leg, drainID, func(beads.Bead) string { return outcome })
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
