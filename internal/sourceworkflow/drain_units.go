package sourceworkflow

import (
	"encoding/json"
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
// control drain minted: an open convoy carrying gc.synthetic=true,
// gc.synthetic_kind=drain-unit-convoy and gc.drain_control_id=<drain's id>.
// Each unit closes with close_reason DrainUnitRetiredCloseReason and, as its
// gc.outcome, the verdict its row reached in drain's manifest, or fallback
// for a unit whose row reached none (DrainUnitVerdicts.OutcomeOr). It returns
// how many units it closed.
//
// Nothing else matches: not the drain's members (a member carries no
// synthetic kind), not its item workflow roots (which carry the drain id but
// are no synthetic convoy), not its input convoy, and not another drain's
// units. A closed unit is never listed, so a unit keeps the outcome it closed
// with, and a call that finds every unit closed writes nothing. A manifest
// that does not parse is an error, and no unit closes: an outcome guessed for
// a unit would overwrite the verdict its row recorded.
//
// A unit's only edge is a non-blocking tracks edge to its member, so the
// units close in id order, one batch per outcome.
func RetireDrainUnitConvoys(store beads.ResidentStore, drain beads.Bead, fallback string) (int, error) {
	drainID := strings.TrimSpace(drain.ID)
	if drainID == "" {
		return 0, nil
	}
	verdicts, err := ReadDrainUnitVerdicts(drain)
	if err != nil {
		return 0, err
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
		if !isOpenDrainUnitOf(unit, drainID) {
			continue
		}
		outcome := verdicts.OutcomeOr(unit, fallback)
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

// isOpenDrainUnitOf reports whether bead is an open unit convoy the drain
// control drainID minted.
func isOpenDrainUnitOf(bead beads.Bead, drainID string) bool {
	return bead.ID != "" && bead.Status != "closed" && bead.Type == "convoy" &&
		bead.Metadata[beadmeta.SyntheticMetadataKey] == "true" &&
		strings.TrimSpace(bead.Metadata[beadmeta.DrainControlIDMetadataKey]) == drainID &&
		strings.TrimSpace(bead.Metadata[beadmeta.SyntheticKindMetadataKey]) == beadmeta.SyntheticKindDrainUnitConvoy
}

// DrainUnitVerdicts are the verdicts the rows of a drain control's manifest
// (beadmeta.DrainManifestMetadataKey) reached, each row naming the unit
// convoy it stands for by id and by gc.drain_unit_key. A drain persists a
// row's verdict as the row reaches one (a shared drain while it is still
// open, a separate drain with its close), so a drain closed without retiring
// its units can leave one open whose row already succeeded or failed. Every
// path that retires units maps a row to its outcome through DrainRowVerdict,
// so a unit closes with the same verdict whichever path retires it.
type DrainUnitVerdicts struct {
	rows []drainUnitVerdictRow
}

// drainUnitVerdictRow is the part of a drain manifest row a verdict lookup
// reads, under the JSON names the manifest's writer (internal/dispatch's
// drainManifestRow) gives it.
type drainUnitVerdictRow struct {
	UnitKey      string `json:"unit_key"`
	UnitConvoyID string `json:"unit_convoy_id"`
	Status       string `json:"status"`
}

// ReadDrainUnitVerdicts reads the verdicts drain's manifest records. A drain
// with no manifest (closed before it built one) records none. A manifest that
// does not parse is an error.
func ReadDrainUnitVerdicts(drain beads.Bead) (DrainUnitVerdicts, error) {
	raw := drain.Metadata[beadmeta.DrainManifestMetadataKey]
	if strings.TrimSpace(raw) == "" {
		return DrainUnitVerdicts{}, nil
	}
	var manifest struct {
		Rows []drainUnitVerdictRow `json:"rows"`
	}
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		return DrainUnitVerdicts{}, fmt.Errorf("parsing the manifest of drain %s: %w", drain.ID, err)
	}
	return DrainUnitVerdicts{rows: manifest.Rows}, nil
}

// OutcomeOr returns the gc.outcome unit retires with: the verdict of the
// first row that names it, by its id or by its gc.drain_unit_key (a duplicate
// an earlier pass minted for a row carries that row's key), or fallback when
// that row reached no verdict or no row names unit.
func (v DrainUnitVerdicts) OutcomeOr(unit beads.Bead, fallback string) string {
	unitKey := strings.TrimSpace(unit.Metadata[beadmeta.DrainUnitKeyMetadataKey])
	for _, row := range v.rows {
		if row.UnitConvoyID != unit.ID && (unitKey == "" || row.UnitKey != unitKey) {
			continue
		}
		if verdict, ok := DrainRowVerdict(row.Status); ok {
			return verdict
		}
		return fallback
	}
	return fallback
}

// DrainRowVerdict is the gc.outcome a drain manifest row's status records:
// pass for a succeeded row and fail for a failed one. Any other status is a
// row that reached no verdict, and ok is false.
func DrainRowVerdict(status string) (outcome string, ok bool) {
	switch status {
	case "succeeded":
		return beadmeta.OutcomePass, true
	case "failed":
		return beadmeta.OutcomeFail, true
	}
	return "", false
}

// DrainControls returns the drain controls (gc.kind=drain) among members, in
// member order, each once, leaving out every member exclude reports true for
// (a nil exclude leaves none out).
func DrainControls(members []beads.Bead, exclude func(beads.Bead) bool) []beads.Bead {
	var drains []beads.Bead
	seen := make(map[string]bool)
	for _, member := range members {
		id := strings.TrimSpace(member.ID)
		if id == "" || seen[id] || strings.TrimSpace(member.Metadata[beadmeta.KindMetadataKey]) != beadmeta.KindDrain {
			continue
		}
		if exclude != nil && exclude(member) {
			continue
		}
		seen[id] = true
		drains = append(drains, member)
	}
	return drains
}
