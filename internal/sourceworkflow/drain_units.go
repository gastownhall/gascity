package sourceworkflow

import (
	"fmt"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// DrainUnitRetiredCloseReason is stamped as close_reason on each unit convoy
// retired because its drain control reached a terminal state. It is prose
// rather than a token because bd rejects close reasons shorter than 20
// characters.
const DrainUnitRetiredCloseReason = "drain unit retired: its drain control reached a terminal state"

// RetireDrainUnitConvoys closes, in store, every open unit convoy the drain
// control drainID minted: an open convoy carrying gc.synthetic=true,
// gc.synthetic_kind=drain-unit-convoy and gc.drain_control_id=drainID. Each
// unit closes with gc.outcome outcomeFor(unit) and close_reason
// DrainUnitRetiredCloseReason. It returns how many units it closed.
//
// Nothing else matches: not the drain's members (a member carries no
// synthetic kind), not its item workflow roots (which carry the drain id but
// are no synthetic convoy), not its input convoy, and not another drain's
// units. A closed unit is never listed, so a unit keeps the outcome it closed
// with, and a call that finds every unit closed writes nothing.
//
// A unit's only edge is a non-blocking tracks edge to its member, so the
// units close in id order, one batch per outcome.
func RetireDrainUnitConvoys(store beads.ResidentStore, drainID string, outcomeFor func(beads.Bead) string) (int, error) {
	drainID = strings.TrimSpace(drainID)
	if drainID == "" {
		return 0, nil
	}
	units, err := store.ListByMetadata(map[string]string{
		beadmeta.DrainControlIDMetadataKey: drainID,
		beadmeta.SyntheticKindMetadataKey:  beadmeta.SyntheticKindDrainUnitConvoy,
	}, 0, beads.WithBothTiers)
	if err != nil {
		return 0, fmt.Errorf("listing unit convoys of drain %s: %w", drainID, err)
	}
	byOutcome := make(map[string][]string)
	for _, unit := range units {
		if unit.ID == "" || unit.Status == "closed" || unit.Type != "convoy" ||
			unit.Metadata[beadmeta.SyntheticMetadataKey] != "true" ||
			strings.TrimSpace(unit.Metadata[beadmeta.DrainControlIDMetadataKey]) != drainID ||
			strings.TrimSpace(unit.Metadata[beadmeta.SyntheticKindMetadataKey]) != beadmeta.SyntheticKindDrainUnitConvoy {
			continue
		}
		outcome := outcomeFor(unit)
		byOutcome[outcome] = append(byOutcome[outcome], unit.ID)
	}
	outcomes := make([]string, 0, len(byOutcome))
	for outcome := range byOutcome {
		outcomes = append(outcomes, outcome)
	}
	slices.Sort(outcomes)
	closed := 0
	for _, outcome := range outcomes {
		ids := byOutcome[outcome]
		slices.Sort(ids)
		n, err := store.CloseAll(ids, map[string]string{
			beadmeta.OutcomeMetadataKey: outcome,
			"close_reason":              DrainUnitRetiredCloseReason,
		})
		closed += n
		if err != nil {
			return closed, fmt.Errorf("closing unit convoys of drain %s as %s: %w", drainID, outcome, err)
		}
	}
	return closed, nil
}

// DrainControlIDs returns the ids of the drain controls (gc.kind=drain) among
// members, in member order, each once.
func DrainControlIDs(members []beads.Bead) []string {
	var ids []string
	seen := make(map[string]bool)
	for _, member := range members {
		id := strings.TrimSpace(member.ID)
		if id == "" || seen[id] || strings.TrimSpace(member.Metadata[beadmeta.KindMetadataKey]) != beadmeta.KindDrain {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}
