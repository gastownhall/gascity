package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/graphv2"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

const (
	drainManifestMetadataKey = beadmeta.DrainManifestMetadataKey
	defaultDrainMaxUnits     = 100
)

type drainManifest struct {
	Version        int                `json:"version"`
	Context        string             `json:"context"`
	ParentConvoyID string             `json:"parent_convoy_id"`
	Formula        string             `json:"formula"`
	Rows           []drainManifestRow `json:"rows"`
}

type drainManifestRow struct {
	Index        int    `json:"index"`
	MemberID     string `json:"member_id"`
	UnitKey      string `json:"unit_key"`
	UnitConvoyID string `json:"unit_convoy_id,omitempty"`
	ItemRootKey  string `json:"item_root_key"`
	ItemRootID   string `json:"item_root_id,omitempty"`
	Status       string `json:"status"`
	OutcomeBead  string `json:"outcome_bead_id,omitempty"`
	OutcomeKind  string `json:"outcome_kind,omitempty"`
	Failure      string `json:"failure_reason,omitempty"`
}

func processDrain(store beads.Store, bead beads.Bead, opts ProcessOptions) (ControlResult, error) {
	switch strings.TrimSpace(bead.Metadata[beadmeta.DrainStateMetadataKey]) {
	case "", beadmeta.DrainStatePending, beadmeta.DrainStateExpanding:
		return expandDrain(store, bead, opts)
	case beadmeta.DrainStateExpanded, beadmeta.DrainStateCompleting:
		return completeDrain(store, bead, opts)
	case beadmeta.DrainStateSucceeded, beadmeta.DrainStateFailed:
		return ControlResult{}, nil
	default:
		return ControlResult{}, fmt.Errorf("%s: unsupported gc.drain_state %q", bead.ID, bead.Metadata[beadmeta.DrainStateMetadataKey])
	}
}

func expandDrain(store beads.Store, bead beads.Bead, opts ProcessOptions) (ControlResult, error) {
	if len(opts.FormulaSearchPaths) == 0 {
		return ControlResult{}, fmt.Errorf("%s: missing formula search paths", bead.ID)
	}
	rootID := strings.TrimSpace(bead.Metadata[beadmeta.RootBeadIDMetadataKey])
	if rootID == "" {
		return ControlResult{}, fmt.Errorf("%s: missing gc.root_bead_id", bead.ID)
	}
	root, err := store.Get(rootID)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: loading workflow root %s: %w", bead.ID, rootID, err)
	}
	parentConvoyID := strings.TrimSpace(root.Metadata[beadmeta.InputConvoyIDMetadataKey])
	if parentConvoyID == "" {
		return ControlResult{}, fmt.Errorf("%s: workflow root %s missing gc.input_convoy_id", bead.ID, rootID)
	}
	parentVars, err := graphv2.ParseRuntimeVarsMetadata(root.Metadata[graphv2.RuntimeVarsMetadataKey])
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: parsing graph.v2 runtime vars on root %s: %w", bead.ID, rootID, err)
	}
	itemFormula := strings.TrimSpace(bead.Metadata[beadmeta.DrainFormulaMetadataKey])
	if itemFormula == "" {
		return ControlResult{}, fmt.Errorf("%s: missing gc.drain_formula", bead.ID)
	}
	manifest, members, err := loadOrBuildDrainManifest(store, bead, parentConvoyID, itemFormula)
	if err != nil {
		if errors.Is(err, errDrainLimitExceeded) {
			scopeResult, scopeErr := reconcileClosedDrainScope(store, bead.ID, opts)
			if scopeErr != nil {
				return ControlResult{}, scopeErr
			}
			return ControlResult{Processed: true, Action: "drain-limit-exceeded", Skipped: scopeResult.Skipped}, nil
		}
		if errors.Is(err, errDrainUnresolvedMember) {
			scopeResult, scopeErr := reconcileClosedDrainScope(store, bead.ID, opts)
			if scopeErr != nil {
				return ControlResult{}, scopeErr
			}
			return ControlResult{Processed: true, Action: "drain-unresolved-member", Skipped: scopeResult.Skipped}, nil
		}
		// Validation failures above may have closed the control before
		// erroring; reconcile the scope best-effort so a closed scoped drain
		// does not strand its scope (mirrors markControllerSpawnError's tolerant
		// reconcile).
		if closed, getErr := store.Get(bead.ID); getErr == nil && closed.Status == "closed" {
			_, _ = reconcileTerminalScopedMemberWithOptions(store, closed, opts)
		}
		return ControlResult{}, err
	}
	if err := persistDrainManifest(store, bead.ID, manifest, map[string]string{beadmeta.DrainStateMetadataKey: beadmeta.DrainStateExpanding}); err != nil {
		return ControlResult{}, fmt.Errorf("%s: recording drain manifest: %w", bead.ID, err)
	}
	if manifest.Context == beadmeta.DrainContextShared {
		return advanceSharedDrain(store, bead, manifest, members, itemFormula, parentVars, opts)
	}
	if err := reserveDrainMembers(store, bead, members); err != nil {
		return closeDrainReservationFailure(store, bead, manifest, err, opts)
	}

	totalCreated := 0
	for i := range manifest.Rows {
		row := &manifest.Rows[i]
		member := members[i]
		var unit beads.Bead
		if row.UnitConvoyID == "" {
			var created bool
			var err error
			unit, created, err = ensureDrainUnitConvoy(store, bead, parentConvoyID, len(members), *row, member)
			if err != nil {
				return ControlResult{}, err
			}
			if created {
				totalCreated++
			}
			row.UnitConvoyID = unit.ID
			row.Status = "unit-created"
		} else {
			reloaded, err := store.Get(row.UnitConvoyID)
			if err != nil {
				return ControlResult{}, fmt.Errorf("%s: loading drain unit convoy %s: %w", bead.ID, row.UnitConvoyID, err)
			}
			unit = reloaded
		}

		if row.ItemRootID == "" {
			blockerIDs, err := drainProjectedBlockerIDs(store, member.ID, manifest)
			if err != nil {
				return ControlResult{}, fmt.Errorf("%s: listing source dependencies for member %s: %w", bead.ID, member.ID, err)
			}
			rootID, created, err := ensureDrainItemRoot(store, bead, unit, member, len(members), row, itemFormula, parentVars, blockerIDs, opts)
			if err != nil {
				if errors.Is(err, errDrainInvalidItemFormula) {
					return closeDrainItemFormulaFailure(store, bead, manifest, err, opts)
				}
				return ControlResult{}, err
			}
			if created {
				totalCreated++
			}
			row.ItemRootID = rootID
			row.Status = "root-created"
		}
		if err := ensureBlockingDependency(store, bead.ID, row.ItemRootID); err != nil {
			if controllerSpawnBoundaryPending(store, bead.ID, err, opts) {
				return ControlResult{}, ErrControlPending
			}
			return ControlResult{}, fmt.Errorf("%s: wiring drain item root %s: %w", bead.ID, row.ItemRootID, err)
		}
		if err := ensureDrainRowDependencyProjection(store, bead, manifest, member.ID, row.ItemRootID); err != nil {
			if controllerSpawnBoundaryPending(store, bead.ID, err, opts) {
				return ControlResult{}, ErrControlPending
			}
			return ControlResult{}, fmt.Errorf("%s: projecting drain dependencies for member %s: %w", bead.ID, member.ID, err)
		}
		row.Status = "wired"
		if err := persistDrainManifest(store, bead.ID, manifest, map[string]string{beadmeta.DrainStateMetadataKey: beadmeta.DrainStateExpanding}); err != nil {
			return ControlResult{}, fmt.Errorf("%s: recording drain progress: %w", bead.ID, err)
		}
	}
	if err := ensureDrainDependencyProjection(store, bead, manifest); err != nil {
		if controllerSpawnBoundaryPending(store, bead.ID, err, opts) {
			return ControlResult{}, ErrControlPending
		}
		return ControlResult{}, fmt.Errorf("%s: projecting drain dependencies: %w", bead.ID, err)
	}
	if err := persistDrainManifest(store, bead.ID, manifest, map[string]string{
		beadmeta.DrainStateMetadataKey:          beadmeta.DrainStateExpanded,
		beadmeta.DrainParentConvoyIDMetadataKey: parentConvoyID,
		beadmeta.DrainCountMetadataKey:          strconv.Itoa(len(manifest.Rows)),
	}); err != nil {
		return ControlResult{}, fmt.Errorf("%s: recording expanded drain: %w", bead.ID, err)
	}
	if len(manifest.Rows) == 0 {
		return completeDrain(store, mustReloadDrain(store, bead), opts)
	}
	return ControlResult{Processed: true, Action: "drain-expanded", Created: totalCreated}, nil
}

func loadOrBuildDrainManifest(store beads.Store, bead beads.Bead, parentConvoyID, itemFormula string) (drainManifest, []beads.Bead, error) {
	if strings.TrimSpace(bead.Metadata[drainManifestMetadataKey]) != "" {
		manifest, err := parseDrainManifest(bead.Metadata[drainManifestMetadataKey])
		if err != nil {
			return drainManifest{}, nil, fmt.Errorf("%s: parsing persisted drain manifest: %w", bead.ID, err)
		}
		members, err := loadDrainManifestMembers(store, bead.ID, manifest)
		if err != nil {
			return drainManifest{}, nil, err
		}
		return manifest, members, nil
	}
	members, err := convoycore.Members(store, parentConvoyID, false)
	if err != nil {
		return drainManifest{}, nil, fmt.Errorf("%s: loading convoy members for %s: %w", bead.ID, parentConvoyID, err)
	}
	if err := rejectUnresolvedDrainMembers(bead.ID, parentConvoyID, members); err != nil {
		var unresolved drainUnresolvedMemberError
		if errors.As(err, &unresolved) {
			closeMetadata := map[string]string{
				beadmeta.DrainStateMetadataKey:     beadmeta.DrainStateFailed,
				beadmeta.OutcomeMetadataKey:        beadmeta.OutcomeFail,
				beadmeta.FailureClassMetadataKey:   beadmeta.FailureClassHard,
				beadmeta.FailureReasonMetadataKey:  "unresolved_member",
				beadmeta.FailureSubjectMetadataKey: unresolved.MemberID,
			}
			if closeErr := updateMetadataAndClose(store, bead.ID, closeMetadata); closeErr != nil {
				return drainManifest{}, nil, fmt.Errorf("%s: closing unresolved-member drain: %w", bead.ID, closeErr)
			}
		}
		return drainManifest{}, nil, err
	}
	maxUnits, err := drainMaxUnits(bead)
	if err != nil {
		closeMetadata := map[string]string{
			beadmeta.DrainStateMetadataKey:    beadmeta.DrainStateFailed,
			beadmeta.OutcomeMetadataKey:       beadmeta.OutcomeFail,
			beadmeta.FailureClassMetadataKey:  beadmeta.FailureClassHard,
			beadmeta.FailureReasonMetadataKey: "drain_max_units_invalid",
		}
		if closeErr := updateMetadataAndClose(store, bead.ID, closeMetadata); closeErr != nil {
			return drainManifest{}, nil, fmt.Errorf("%s: closing invalid-max-units drain: %w", bead.ID, closeErr)
		}
		return drainManifest{}, nil, err
	}
	if len(members) > maxUnits {
		closeMetadata := map[string]string{
			beadmeta.DrainStateMetadataKey:    beadmeta.DrainStateFailed,
			beadmeta.OutcomeMetadataKey:       beadmeta.OutcomeFail,
			beadmeta.FailureClassMetadataKey:  beadmeta.FailureClassHard,
			beadmeta.FailureReasonMetadataKey: "limit_exceeded",
		}
		if err := updateMetadataAndClose(store, bead.ID, closeMetadata); err != nil {
			return drainManifest{}, nil, fmt.Errorf("%s: closing limit-exceeded drain: %w", bead.ID, err)
		}
		return drainManifest{}, nil, errDrainLimitExceeded
	}
	orderedMembers, err := orderDrainMembersByDependencies(store, members)
	if err != nil {
		return drainManifest{}, nil, fmt.Errorf("%s: ordering drain members for %s: %w", bead.ID, parentConvoyID, err)
	}
	return buildDrainManifest(bead, parentConvoyID, itemFormula, orderedMembers), orderedMembers, nil
}

var (
	errDrainLimitExceeded      = errors.New("drain limit exceeded")
	errDrainInvalidItemFormula = errors.New("invalid drain item formula")
	errDrainUnresolvedMember   = errors.New("drain unresolved member")
)

type drainUnresolvedMemberError struct {
	ControlID      string
	ParentConvoyID string
	MemberID       string
}

func (e drainUnresolvedMemberError) Error() string {
	return fmt.Sprintf("%s: parent convoy %s has unresolved or cross-store member %s", e.ControlID, e.ParentConvoyID, e.MemberID)
}

func (e drainUnresolvedMemberError) Unwrap() error {
	return errDrainUnresolvedMember
}

func loadDrainManifestMembers(store beads.Store, controlID string, manifest drainManifest) ([]beads.Bead, error) {
	members := make([]beads.Bead, 0, len(manifest.Rows))
	for _, row := range manifest.Rows {
		member, err := store.Get(row.MemberID)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) && strings.TrimSpace(row.MemberID) != "" {
				members = append(members, beads.Bead{ID: row.MemberID, Title: row.MemberID, Type: "task", Status: "unknown"})
				continue
			}
			return nil, fmt.Errorf("%s: loading persisted drain member %s: %w", controlID, row.MemberID, err)
		}
		members = append(members, member)
	}
	return members, nil
}

func rejectUnresolvedDrainMembers(controlID, parentConvoyID string, members []beads.Bead) error {
	for _, member := range members {
		if convoycore.IsUnresolvedTrackedItem(member) {
			return drainUnresolvedMemberError{ControlID: controlID, ParentConvoyID: parentConvoyID, MemberID: member.ID}
		}
	}
	return nil
}

func completeDrain(store beads.Store, bead beads.Bead, opts ProcessOptions) (ControlResult, error) {
	manifest, err := parseDrainManifest(bead.Metadata[drainManifestMetadataKey])
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: parsing drain manifest: %w", bead.ID, err)
	}
	if manifest.Context == beadmeta.DrainContextShared {
		rootID := strings.TrimSpace(bead.Metadata[beadmeta.RootBeadIDMetadataKey])
		if rootID == "" {
			return ControlResult{}, fmt.Errorf("%s: missing gc.root_bead_id", bead.ID)
		}
		root, err := store.Get(rootID)
		if err != nil {
			return ControlResult{}, fmt.Errorf("%s: loading workflow root %s: %w", bead.ID, rootID, err)
		}
		parentVars, err := graphv2.ParseRuntimeVarsMetadata(root.Metadata[graphv2.RuntimeVarsMetadataKey])
		if err != nil {
			return ControlResult{}, fmt.Errorf("%s: parsing graph.v2 runtime vars on root %s: %w", bead.ID, rootID, err)
		}
		members, err := loadDrainManifestMembers(store, bead.ID, manifest)
		if err != nil {
			return ControlResult{}, err
		}
		return advanceSharedDrain(store, bead, manifest, members, manifest.Formula, parentVars, opts)
	}
	// Re-running the projection here lets manifests whose item workflows were
	// wired to source members by earlier builds heal while the drain waits on
	// open item roots; expansion never revisits an expanded drain.
	if err := ensureDrainDependencyProjection(store, bead, manifest); err != nil {
		if controllerSpawnBoundaryPending(store, bead.ID, err, opts) {
			return ControlResult{}, ErrControlPending
		}
		return ControlResult{}, fmt.Errorf("%s: repairing drain dependency projection: %w", bead.ID, err)
	}
	if strings.TrimSpace(bead.Metadata[beadmeta.DrainStateMetadataKey]) != beadmeta.DrainStateCompleting {
		if err := store.SetMetadata(bead.ID, beadmeta.DrainStateMetadataKey, beadmeta.DrainStateCompleting); err != nil {
			if controllerSpawnBoundaryPending(store, bead.ID, err, opts) {
				return ControlResult{}, ErrControlPending
			}
			return ControlResult{}, fmt.Errorf("%s: marking drain completing: %w", bead.ID, err)
		}
	}
	failed := 0
	for i := range manifest.Rows {
		row := &manifest.Rows[i]
		if row.ItemRootID == "" {
			return ControlResult{}, ErrControlPending
		}
		root, err := store.Get(row.ItemRootID)
		if err != nil {
			return ControlResult{}, fmt.Errorf("%s: loading item root %s: %w", bead.ID, row.ItemRootID, err)
		}
		if root.Status != "closed" {
			return ControlResult{}, ErrControlPending
		}
		outcome := strings.TrimSpace(root.Metadata[beadmeta.OutcomeMetadataKey])
		if outcome == beadmeta.OutcomePass {
			row.Status = "succeeded"
		} else {
			failed++
			row.Status = "failed"
			row.Failure = root.Metadata[beadmeta.FailureReasonMetadataKey]
			if row.Failure == "" {
				row.Failure = "item_outcome_" + outcome
				if outcome == "" {
					row.Failure = "missing_item_outcome"
				}
			}
		}
		row.OutcomeBead = root.Metadata[beadmeta.OutcomeBeadIDMetadataKey]
		if row.OutcomeBead == "" {
			row.OutcomeBead = root.ID
		}
		row.OutcomeKind = outcome
	}
	closeState := "succeeded"
	outcome := beadmeta.OutcomePass
	action := "drain-succeeded"
	if failed > 0 {
		closeState = "failed"
		outcome = beadmeta.OutcomeFail
		action = "drain-failed"
	}
	metadata := map[string]string{
		beadmeta.DrainStateMetadataKey: closeState,
		beadmeta.OutcomeMetadataKey:    outcome,
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return ControlResult{}, err
	}
	metadata[drainManifestMetadataKey] = string(data)
	if err := releaseDrainReservations(store, bead.ID, manifest); err != nil {
		return ControlResult{}, err
	}
	if err := updateMetadataAndClose(store, bead.ID, metadata); err != nil {
		return ControlResult{}, fmt.Errorf("%s: closing drain: %w", bead.ID, err)
	}
	scopeResult, err := reconcileClosedDrainScope(store, bead.ID, opts)
	if err != nil {
		return ControlResult{}, err
	}
	return ControlResult{Processed: true, Action: action, Skipped: scopeResult.Skipped}, nil
}

func advanceSharedDrain(store beads.Store, bead beads.Bead, manifest drainManifest, members []beads.Bead, itemFormula string, parentVars map[string]string, opts ProcessOptions) (ControlResult, error) {
	if len(manifest.Rows) == 0 {
		return closeDrainWithManifest(store, bead.ID, manifest, "succeeded", beadmeta.OutcomePass, "drain-succeeded", opts)
	}
	// Repair materialized rows before waiting on them: a row wired to a
	// source member by an earlier build never closes (drains do not close
	// source members), and in shared mode its blocker row is not even
	// materialized until this row's root closes.
	if err := ensureDrainDependencyProjection(store, bead, manifest); err != nil {
		if controllerSpawnBoundaryPending(store, bead.ID, err, opts) {
			return ControlResult{}, ErrControlPending
		}
		return ControlResult{}, fmt.Errorf("%s: repairing shared drain dependency projection: %w", bead.ID, err)
	}
	onItemFailure := drainOnItemFailure(bead)
	for i := range manifest.Rows {
		row := &manifest.Rows[i]
		if row.ItemRootID != "" {
			root, err := store.Get(row.ItemRootID)
			if err != nil {
				return ControlResult{}, fmt.Errorf("%s: loading shared item root %s: %w", bead.ID, row.ItemRootID, err)
			}
			if root.Status != "closed" {
				if err := persistDrainManifest(store, bead.ID, manifest, map[string]string{beadmeta.DrainStateMetadataKey: beadmeta.DrainStateExpanded}); err != nil {
					return ControlResult{}, fmt.Errorf("%s: recording shared drain wait: %w", bead.ID, err)
				}
				return ControlResult{}, ErrControlPending
			}
			if !recordDrainRowOutcome(row, root) {
				if onItemFailure == beadmeta.DrainOnItemFailureSkipRemaining {
					markRemainingSharedRowsSkipped(&manifest, i+1)
					return closeDrainWithManifest(store, bead.ID, manifest, "failed", beadmeta.OutcomeFail, "drain-failed", opts)
				}
			}
			continue
		}
		if i > len(members)-1 {
			return ControlResult{}, fmt.Errorf("%s: shared drain manifest/member length mismatch", bead.ID)
		}
		member := members[i]
		if err := reserveDrainMember(store, bead, member); err != nil {
			return closeDrainReservationFailure(store, bead, manifest, err, opts)
		}
		created, err := materializeDrainRow(store, bead, manifest, members, row, member, itemFormula, parentVars, opts)
		if err != nil {
			if errors.Is(err, errDrainInvalidItemFormula) {
				return closeDrainItemFormulaFailure(store, bead, manifest, err, opts)
			}
			return ControlResult{}, err
		}
		if err := ensureDrainDependencyProjection(store, bead, manifest); err != nil {
			if controllerSpawnBoundaryPending(store, bead.ID, err, opts) {
				return ControlResult{}, ErrControlPending
			}
			return ControlResult{}, fmt.Errorf("%s: projecting shared drain dependencies: %w", bead.ID, err)
		}
		if err := persistDrainManifest(store, bead.ID, manifest, map[string]string{
			beadmeta.DrainStateMetadataKey:          beadmeta.DrainStateExpanded,
			beadmeta.DrainParentConvoyIDMetadataKey: manifest.ParentConvoyID,
			beadmeta.DrainCountMetadataKey:          strconv.Itoa(len(manifest.Rows)),
		}); err != nil {
			return ControlResult{}, fmt.Errorf("%s: recording shared drain progress: %w", bead.ID, err)
		}
		return ControlResult{Processed: true, Action: "drain-shared-advanced", Created: created}, nil
	}
	if drainManifestHasFailedRows(manifest) {
		return closeDrainWithManifest(store, bead.ID, manifest, "failed", beadmeta.OutcomeFail, "drain-failed", opts)
	}
	return closeDrainWithManifest(store, bead.ID, manifest, "succeeded", beadmeta.OutcomePass, "drain-succeeded", opts)
}

func materializeDrainRow(store beads.Store, control beads.Bead, manifest drainManifest, members []beads.Bead, row *drainManifestRow, member beads.Bead, itemFormula string, parentVars map[string]string, opts ProcessOptions) (int, error) {
	createdCount := 0
	var unit beads.Bead
	if row.UnitConvoyID == "" {
		createdUnit, created, err := ensureDrainUnitConvoy(store, control, manifest.ParentConvoyID, len(members), *row, member)
		if err != nil {
			return 0, err
		}
		unit = createdUnit
		if created {
			createdCount++
		}
		row.UnitConvoyID = unit.ID
		row.Status = "unit-created"
	} else {
		reloaded, err := store.Get(row.UnitConvoyID)
		if err != nil {
			return 0, fmt.Errorf("%s: loading drain unit convoy %s: %w", control.ID, row.UnitConvoyID, err)
		}
		unit = reloaded
	}
	if row.ItemRootID == "" {
		blockerIDs, err := drainProjectedBlockerIDs(store, member.ID, manifest)
		if err != nil {
			return 0, fmt.Errorf("%s: listing source dependencies for member %s: %w", control.ID, member.ID, err)
		}
		rootID, created, err := ensureDrainItemRoot(store, control, unit, member, len(members), row, itemFormula, parentVars, blockerIDs, opts)
		if err != nil {
			return 0, err
		}
		if created {
			createdCount++
		}
		row.ItemRootID = rootID
		row.Status = "root-created"
	}
	if err := ensureBlockingDependency(store, control.ID, row.ItemRootID); err != nil {
		if controllerSpawnBoundaryPending(store, control.ID, err, opts) {
			return 0, ErrControlPending
		}
		return 0, fmt.Errorf("%s: wiring drain item root %s: %w", control.ID, row.ItemRootID, err)
	}
	if err := ensureDrainRowDependencyProjection(store, control, manifest, member.ID, row.ItemRootID); err != nil {
		if controllerSpawnBoundaryPending(store, control.ID, err, opts) {
			return 0, ErrControlPending
		}
		return 0, fmt.Errorf("%s: projecting drain dependencies for member %s: %w", control.ID, member.ID, err)
	}
	row.Status = "wired"
	return createdCount, nil
}

func ensureDrainDependencyProjection(store beads.Store, control beads.Bead, manifest drainManifest) error {
	for _, row := range manifest.Rows {
		memberID := strings.TrimSpace(row.MemberID)
		rootID := strings.TrimSpace(row.ItemRootID)
		if memberID == "" || rootID == "" {
			continue
		}
		if err := ensureDrainRowDependencyProjection(store, control, manifest, memberID, rootID); err != nil {
			return err
		}
	}
	return nil
}

func ensureDrainRowDependencyProjection(store beads.Store, control beads.Bead, manifest drainManifest, memberID, rootID string) error {
	blockerIDs, err := drainProjectedBlockerIDs(store, memberID, manifest)
	if err != nil {
		return fmt.Errorf("%s: listing source dependencies for member %s: %w", control.ID, memberID, err)
	}
	for _, blockerID := range blockerIDs {
		if blockerID == rootID {
			continue
		}
		if err := ensureDrainWorkflowBlocksOn(store, rootID, blockerID); err != nil {
			return fmt.Errorf("%s: wiring item workflow %s for member %s to blocker %s: %w", control.ID, rootID, memberID, blockerID, err)
		}
	}
	if err := repairDrainWorkflowSourceMemberDeps(store, manifest, memberID, rootID); err != nil {
		return fmt.Errorf("%s: repairing source-member dependencies on item workflow %s for member %s: %w", control.ID, rootID, memberID, err)
	}
	return nil
}

func drainRootByMember(manifest drainManifest) map[string]string {
	rootByMember := make(map[string]string, len(manifest.Rows))
	for _, row := range manifest.Rows {
		memberID := strings.TrimSpace(row.MemberID)
		rootID := strings.TrimSpace(row.ItemRootID)
		if memberID == "" || rootID == "" {
			continue
		}
		rootByMember[memberID] = rootID
	}
	return rootByMember
}

func drainManifestMemberIDs(manifest drainManifest) map[string]bool {
	memberIDs := make(map[string]bool, len(manifest.Rows))
	for _, row := range manifest.Rows {
		memberID := strings.TrimSpace(row.MemberID)
		if memberID == "" {
			continue
		}
		memberIDs[memberID] = true
	}
	return memberIDs
}

func drainProjectedBlockerIDs(store beads.Store, memberID string, manifest drainManifest) ([]string, error) {
	rootByMember := drainRootByMember(manifest)
	manifestMembers := drainManifestMemberIDs(manifest)
	deps, err := store.DepList(memberID, "down")
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(deps))
	blockerIDs := make([]string, 0, len(deps))
	for _, dep := range deps {
		if !beads.IsReadyBlockingDependencyType(dep.Type) {
			continue
		}
		dependsOnID := strings.TrimSpace(dep.DependsOnID)
		if dependsOnID == "" || dependsOnID == memberID {
			continue
		}
		blockerID := dependsOnID
		if projectedRootID := strings.TrimSpace(rootByMember[dependsOnID]); projectedRootID != "" {
			blockerID = projectedRootID
		} else if manifestMembers[dependsOnID] {
			// An in-manifest member without a materialized item root must not
			// be embedded as a blocker: drains do not close source members,
			// so that edge would never release. Manifests persisted before
			// dependency ordering can reach this state on resume; the
			// manifest-wide sweep wires the item-root dependency once the
			// blocker's root exists.
			continue
		}
		if seen[blockerID] {
			continue
		}
		seen[blockerID] = true
		blockerIDs = append(blockerIDs, blockerID)
	}
	return blockerIDs, nil
}

func ensureDrainWorkflowBlocksOn(store beads.Store, rootID, blockerID string) error {
	rootID = strings.TrimSpace(rootID)
	blockerID = strings.TrimSpace(blockerID)
	if rootID == "" || blockerID == "" || rootID == blockerID {
		return nil
	}
	workflowBeads, err := listByWorkflowRoot(store, rootID)
	if err != nil {
		return err
	}
	for _, bead := range workflowBeads {
		if strings.TrimSpace(bead.ID) == "" || bead.ID == blockerID {
			continue
		}
		if err := ensureBlockingDependency(store, bead.ID, blockerID); err != nil {
			return err
		}
	}
	return nil
}

// repairDrainWorkflowSourceMemberDeps removes ready-blocking dependencies
// that earlier builds embedded from item workflow beads onto other manifest
// source members. Drains do not close source members, so such an edge stalls
// the item workflow permanently; the projected item-root dependency wired by
// ensureDrainRowDependencyProjection before this repair supersedes it.
func repairDrainWorkflowSourceMemberDeps(store beads.Store, manifest drainManifest, memberID, rootID string) error {
	manifestMembers := drainManifestMemberIDs(manifest)
	workflowBeads, err := listByWorkflowRoot(store, rootID)
	if err != nil {
		return err
	}
	for _, bead := range workflowBeads {
		if strings.TrimSpace(bead.ID) == "" {
			continue
		}
		deps, err := store.DepList(bead.ID, "down")
		if err != nil {
			return fmt.Errorf("listing dependencies for item workflow bead %s: %w", bead.ID, err)
		}
		for _, dep := range deps {
			if !beads.IsReadyBlockingDependencyType(dep.Type) {
				continue
			}
			dependsOnID := strings.TrimSpace(dep.DependsOnID)
			if dependsOnID == "" || dependsOnID == memberID || !manifestMembers[dependsOnID] {
				continue
			}
			if err := store.DepRemove(bead.ID, dependsOnID); err != nil {
				return fmt.Errorf("removing source-member dependency %s from item workflow bead %s: %w", dependsOnID, bead.ID, err)
			}
		}
	}
	return nil
}

func recordDrainRowOutcome(row *drainManifestRow, root beads.Bead) bool {
	outcome := strings.TrimSpace(root.Metadata[beadmeta.OutcomeMetadataKey])
	row.OutcomeBead = root.Metadata[beadmeta.OutcomeBeadIDMetadataKey]
	if row.OutcomeBead == "" {
		row.OutcomeBead = root.ID
	}
	row.OutcomeKind = outcome
	if outcome == beadmeta.OutcomePass {
		row.Status = "succeeded"
		row.Failure = ""
		return true
	}
	row.Status = "failed"
	row.Failure = root.Metadata[beadmeta.FailureReasonMetadataKey]
	if row.Failure == "" {
		row.Failure = "item_outcome_" + outcome
		if outcome == "" {
			row.Failure = "missing_item_outcome"
		}
	}
	return false
}

func drainManifestHasFailedRows(manifest drainManifest) bool {
	for _, row := range manifest.Rows {
		if row.Status == "failed" || row.Status == "skipped" {
			return true
		}
		outcome := strings.TrimSpace(row.OutcomeKind)
		if outcome != "" && outcome != beadmeta.OutcomePass {
			return true
		}
	}
	return false
}

func markRemainingSharedRowsSkipped(manifest *drainManifest, start int) {
	if manifest == nil {
		return
	}
	for i := start; i < len(manifest.Rows); i++ {
		row := &manifest.Rows[i]
		if row.ItemRootID != "" || row.Status == "succeeded" || row.Status == "failed" {
			continue
		}
		row.Status = "skipped"
		row.OutcomeKind = beadmeta.OutcomeSkipped
		row.Failure = "previous_item_failed"
	}
}

// reconcileClosedDrainScope mirrors the fanout/retry/ralph terminal-close
// behavior for drain controls: after a drain control closes, reconcile its
// enclosing scope so a drain that was the scope's last open member finalizes
// the scope (or aborts it on fail) instead of relying on another control's
// close-time backstop. Returns the scope reconciliation result for Skipped
// propagation; no-op for scope-less drains.
func reconcileClosedDrainScope(store beads.Store, beadID string, opts ProcessOptions) (ControlResult, error) {
	return reconcileClosedScopeMemberWithOptions(store, beadID, opts)
}

func closeDrainWithManifest(store beads.Store, beadID string, manifest drainManifest, closeState, outcome, action string, opts ProcessOptions) (ControlResult, error) {
	metadata := map[string]string{
		beadmeta.DrainStateMetadataKey: closeState,
		beadmeta.OutcomeMetadataKey:    outcome,
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return ControlResult{}, err
	}
	metadata[drainManifestMetadataKey] = string(data)
	if err := releaseDrainReservations(store, beadID, manifest); err != nil {
		return ControlResult{}, err
	}
	if err := updateMetadataAndClose(store, beadID, metadata); err != nil {
		return ControlResult{}, fmt.Errorf("%s: closing drain: %w", beadID, err)
	}
	scopeResult, err := reconcileClosedDrainScope(store, beadID, opts)
	if err != nil {
		return ControlResult{}, err
	}
	return ControlResult{Processed: true, Action: action, Skipped: scopeResult.Skipped}, nil
}

func buildDrainManifest(bead beads.Bead, parentConvoyID, itemFormula string, members []beads.Bead) drainManifest {
	context := strings.TrimSpace(bead.Metadata[beadmeta.DrainContextMetadataKey])
	if context == "" {
		context = beadmeta.DrainContextSeparate
	}
	rows := make([]drainManifestRow, 0, len(members))
	for i, member := range members {
		unitKey := fmt.Sprintf("drain-unit:%s:%d:%s", bead.ID, i, member.ID)
		rows = append(rows, drainManifestRow{
			Index:       i,
			MemberID:    member.ID,
			UnitKey:     unitKey,
			ItemRootKey: fmt.Sprintf("drain-item-root:%s:%d:%s", bead.ID, i, member.ID),
			Status:      "pending",
		})
	}
	return drainManifest{Version: 1, Context: context, ParentConvoyID: parentConvoyID, Formula: itemFormula, Rows: rows}
}

func orderDrainMembersByDependencies(store beads.Store, members []beads.Bead) ([]beads.Bead, error) {
	if len(members) < 2 {
		return members, nil
	}
	memberByID := make(map[string]beads.Bead, len(members))
	for _, member := range members {
		memberID := strings.TrimSpace(member.ID)
		if memberID == "" {
			continue
		}
		memberByID[memberID] = member
	}
	blockersByMember := make(map[string]map[string]bool, len(members))
	for _, member := range members {
		memberID := strings.TrimSpace(member.ID)
		if memberID == "" {
			continue
		}
		deps, err := store.DepList(memberID, "down")
		if err != nil {
			return nil, fmt.Errorf("listing source dependencies for member %s: %w", memberID, err)
		}
		for _, dep := range deps {
			if !beads.IsReadyBlockingDependencyType(dep.Type) {
				continue
			}
			blockerID := strings.TrimSpace(dep.DependsOnID)
			if blockerID == "" || blockerID == memberID {
				continue
			}
			if _, ok := memberByID[blockerID]; !ok {
				continue
			}
			if blockersByMember[memberID] == nil {
				blockersByMember[memberID] = make(map[string]bool)
			}
			blockersByMember[memberID][blockerID] = true
		}
	}
	ordered := make([]beads.Bead, 0, len(members))
	emitted := make(map[string]bool, len(members))
	for len(ordered) < len(members) {
		progressed := false
		for _, member := range members {
			memberID := strings.TrimSpace(member.ID)
			if emitted[memberID] {
				continue
			}
			blocked := false
			for blockerID := range blockersByMember[memberID] {
				if !emitted[blockerID] {
					blocked = true
					break
				}
			}
			if blocked {
				continue
			}
			ordered = append(ordered, member)
			emitted[memberID] = true
			progressed = true
		}
		if !progressed {
			cycleMembers := make([]string, 0, len(members)-len(ordered))
			for _, member := range members {
				memberID := strings.TrimSpace(member.ID)
				if memberID != "" && !emitted[memberID] {
					cycleMembers = append(cycleMembers, memberID)
				}
			}
			return nil, fmt.Errorf("source dependency cycle among drain members: %s", strings.Join(cycleMembers, ", "))
		}
	}
	return ordered, nil
}

func ensureDrainUnitConvoy(store beads.Store, control beads.Bead, parentConvoyID string, count int, row drainManifestRow, member beads.Bead) (beads.Bead, bool, error) {
	unlock := graphv2.LockKey(row.UnitKey)
	defer unlock()
	existing, err := store.ListByMetadata(map[string]string{beadmeta.DrainUnitKeyMetadataKey: row.UnitKey}, 1, beads.WithBothTiers)
	if err != nil {
		return beads.Bead{}, false, fmt.Errorf("%s: looking up unit convoy for member %s: %w", control.ID, member.ID, err)
	}
	if len(existing) > 0 {
		if err := ensureDrainUnitTrack(store, control.ID, existing[0].ID, member); err != nil {
			return beads.Bead{}, false, err
		}
		return existing[0], false, nil
	}
	metadata := map[string]string{
		beadmeta.SyntheticMetadataKey:         "true",
		beadmeta.SyntheticKindMetadataKey:     "drain-unit-convoy",
		beadmeta.ParentConvoyIDMetadataKey:    parentConvoyID,
		beadmeta.DrainControlIDMetadataKey:    control.ID,
		beadmeta.DrainIndexMetadataKey:        strconv.Itoa(row.Index),
		beadmeta.DrainCountMetadataKey:        strconv.Itoa(count),
		beadmeta.DrainMemberIDMetadataKey:     member.ID,
		beadmeta.DrainMemberAccessMetadataKey: drainMemberAccess(control),
		beadmeta.DrainUnitKeyMetadataKey:      row.UnitKey,
	}
	created, err := store.Create(beads.Bead{
		Title:    fmt.Sprintf("drain unit %d for %s", row.Index, member.ID),
		Type:     "convoy",
		Priority: member.Priority,
		Metadata: metadata,
	})
	if err != nil {
		return beads.Bead{}, false, fmt.Errorf("%s: creating unit convoy for member %s: %w", control.ID, member.ID, err)
	}
	if err := trackDrainMember(store, created.ID, member); err != nil {
		return beads.Bead{}, false, fmt.Errorf("%s: tracking member %s from unit convoy %s: %w", control.ID, member.ID, created.ID, err)
	}
	return created, true, nil
}

func ensureDrainUnitTrack(store beads.Store, controlID, unitConvoyID string, member beads.Bead) error {
	memberID := strings.TrimSpace(member.ID)
	hasTrack, err := convoycore.HasTrack(store, unitConvoyID, memberID)
	if err != nil {
		return fmt.Errorf("%s: checking unit convoy %s track for member %s: %w", controlID, unitConvoyID, memberID, err)
	}
	if hasTrack {
		return nil
	}
	if err := trackDrainMember(store, unitConvoyID, member); err != nil {
		return fmt.Errorf("%s: repairing unit convoy %s track for member %s: %w", controlID, unitConvoyID, memberID, err)
	}
	return nil
}

func trackDrainMember(store beads.Store, unitConvoyID string, member beads.Bead) error {
	if convoycore.IsUnresolvedTrackedItem(member) {
		return store.SetMetadata(unitConvoyID, beadmeta.DrainMemberUnresolvedMetadataKey, "true")
	}
	return convoycore.TrackItem(store, unitConvoyID, member.ID)
}

func ensureDrainItemRoot(store beads.Store, control, unit, member beads.Bead, count int, row *drainManifestRow, itemFormula string, parentVars map[string]string, blockerIDs []string, opts ProcessOptions) (string, bool, error) {
	unlock := graphv2.LockKey(row.ItemRootKey)
	defer unlock()
	if err := closeFailedDrainItemRoots(store, control.ID, row.ItemRootKey); err != nil {
		return "", false, err
	}
	existing, err := store.ListByMetadata(map[string]string{beadmeta.ItemRootKeyMetadataKey: row.ItemRootKey}, 0, beads.IncludeClosed, beads.WithBothTiers)
	if err != nil {
		return "", false, fmt.Errorf("%s: looking up item root %s: %w", control.ID, row.ItemRootKey, err)
	}
	for _, candidate := range existing {
		if candidate.Metadata["molecule_failed"] == "true" {
			continue
		}
		return candidate.ID, false, nil
	}
	vars := make(map[string]string, len(parentVars))
	for key, value := range parentVars {
		switch strings.TrimSpace(key) {
		case "", graphv2.ConvoyIDVar, "issue", "bead_id":
			continue
		default:
			vars[strings.TrimSpace(key)] = value
		}
	}
	vars[graphv2.ConvoyIDVar] = unit.ID
	if !convoycore.IsUnresolvedTrackedItem(member) && strings.TrimSpace(member.ID) != "" {
		// Deprecated one-release compat alias (#2941): item formulas that
		// still reference {{issue}} resolve it to the unit's tracked member.
		vars[graphv2.LegacyIssueVar] = member.ID
	}
	recipe, err := formula.CompileWithoutRuntimeVarValidation(context.Background(), itemFormula, opts.FormulaSearchPaths, vars)
	if err != nil {
		return "", false, fmt.Errorf("%w: %s: compiling drain item formula %q: %w", errDrainInvalidItemFormula, control.ID, itemFormula, err)
	}
	if !isGraphV2WorkflowRecipe(recipe) {
		return "", false, fmt.Errorf("%w: %s: drain item formula %q must declare contract = \"graph.v2\"", errDrainInvalidItemFormula, control.ID, itemFormula)
	}
	if err := molecule.ValidateRecipeRuntimeVars(recipe, molecule.Options{Vars: vars}); err != nil {
		return "", false, fmt.Errorf("%w: %s: validating drain item formula %q: %w", errDrainInvalidItemFormula, control.ID, itemFormula, err)
	}
	runtimeVars := drainItemRuntimeVars(recipe, vars)
	stampDrainItemRecipe(recipe, control, unit, member, count, row, itemFormula, runtimeVars)
	if opts.PrepareRecipe != nil {
		if err := opts.PrepareRecipe(recipe, control); err != nil {
			return "", false, fmt.Errorf("%w: %s: preparing drain item formula %q: %w", errDrainInvalidItemFormula, control.ID, itemFormula, err)
		}
	}
	result, err := molecule.Instantiate(context.Background(), store, recipe, molecule.Options{
		Vars:             runtimeVars,
		ExternalDeps:     drainWorkflowExternalDeps(recipe, blockerIDs),
		PriorityOverride: member.Priority,
	})
	if err != nil {
		if cleanupErr := closeFailedDrainItemRoots(store, control.ID, row.ItemRootKey); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
		if controllerSpawnBoundaryPending(store, control.ID, err, opts) {
			return "", false, ErrControlPending
		}
		return "", false, fmt.Errorf("%s: instantiating drain item formula %q: %w", control.ID, itemFormula, err)
	}
	return result.RootID, true, nil
}

func drainWorkflowExternalDeps(recipe *formula.Recipe, blockerIDs []string) []molecule.ExternalDep {
	if recipe == nil || len(recipe.Steps) == 0 || len(blockerIDs) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	var deps []molecule.ExternalDep
	for _, step := range recipe.Steps {
		stepID := strings.TrimSpace(step.ID)
		if stepID == "" {
			continue
		}
		for _, blockerID := range blockerIDs {
			blockerID = strings.TrimSpace(blockerID)
			if blockerID == "" {
				continue
			}
			key := stepID + "\x00" + blockerID
			if seen[key] {
				continue
			}
			seen[key] = true
			deps = append(deps, molecule.ExternalDep{
				StepID:      stepID,
				DependsOnID: blockerID,
				Type:        "blocks",
			})
		}
	}
	return deps
}

func drainItemRuntimeVars(recipe *formula.Recipe, vars map[string]string) map[string]string {
	out := make(map[string]string, len(vars))
	if recipe != nil {
		for name, def := range recipe.Vars {
			if def != nil && def.Default != nil {
				out[name] = *def.Default
			}
		}
	}
	for key, value := range vars {
		out[key] = value
	}
	if len(out) == 0 {
		return map[string]string{}
	}
	return out
}

func closeFailedDrainItemRoots(store beads.Store, controlID, itemRootKey string) error {
	itemRootKey = strings.TrimSpace(itemRootKey)
	if store == nil || itemRootKey == "" {
		return nil
	}
	matches, err := store.ListByMetadata(map[string]string{beadmeta.ItemRootKeyMetadataKey: itemRootKey}, 0, beads.WithBothTiers)
	if err != nil {
		return fmt.Errorf("%s: looking up failed drain item roots for key %s: %w", controlID, itemRootKey, err)
	}
	for _, root := range matches {
		if root.Status == "closed" || root.Metadata["molecule_failed"] != "true" {
			continue
		}
		if _, err := sourceworkflow.CloseWorkflowSubtree(store, root.ID); err != nil {
			return fmt.Errorf("%s: closing failed drain item root %s: %w", controlID, root.ID, err)
		}
	}
	return nil
}

func isGraphV2WorkflowRecipe(recipe *formula.Recipe) bool {
	if recipe == nil {
		return false
	}
	root := recipe.RootStep()
	return root != nil && root.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindWorkflow && root.Metadata[beadmeta.FormulaContractMetadataKey] == beadmeta.FormulaContractGraphV2
}

func stampDrainItemRecipe(recipe *formula.Recipe, control, unit, member beads.Bead, count int, row *drainManifestRow, itemFormula string, vars map[string]string) {
	if recipe == nil || len(recipe.Steps) == 0 {
		return
	}
	root := &recipe.Steps[0]
	if root.Metadata == nil {
		root.Metadata = make(map[string]string)
	}
	root.Metadata[beadmeta.InputConvoyIDMetadataKey] = unit.ID
	root.Metadata[beadmeta.DrainControlIDMetadataKey] = control.ID
	root.Metadata[beadmeta.DrainIndexMetadataKey] = strconv.Itoa(row.Index)
	root.Metadata[beadmeta.DrainCountMetadataKey] = strconv.Itoa(count)
	root.Metadata[beadmeta.DrainMemberIDMetadataKey] = member.ID
	root.Metadata[beadmeta.DrainMemberAccessMetadataKey] = drainMemberAccess(control)
	root.Metadata[beadmeta.ItemRootKeyMetadataKey] = row.ItemRootKey
	root.Metadata[beadmeta.Graphv2RootKeyMetadataKey] = graphv2.RootKey(unit.ID, itemFormula, vars, "drain", control.ID+":"+member.ID)
	if metadata := graphv2.RuntimeVarsMetadata(vars); metadata != "" {
		root.Metadata[graphv2.RuntimeVarsMetadataKey] = metadata
	}
	if strings.TrimSpace(control.Metadata[beadmeta.DrainContextMetadataKey]) == beadmeta.DrainContextShared {
		group := sharedDrainContinuationGroup(control)
		for i := range recipe.Steps {
			step := &recipe.Steps[i]
			if !isSharedDrainExecutableStep(step) {
				continue
			}
			if step.Metadata == nil {
				step.Metadata = make(map[string]string)
			}
			step.Metadata[beadmeta.ContinuationGroupMetadataKey] = group
			step.Metadata[beadmeta.SessionAffinityMetadataKey] = "require"
		}
	}
}

// isSharedDrainExecutableStep reports whether a drain item recipe step is
// worker-executable work that should carry shared-drain continuation
// metadata (gc.continuation_group, gc.session_affinity). Control-dispatcher
// steps (beadmeta.ControlKinds) and workflow-topology anchors
// (beadmeta.WorkflowTopologyKinds) are infrastructure the control dispatcher
// or graph routing owns, never session-affine worker work, so they are
// excluded.
func isSharedDrainExecutableStep(step *formula.RecipeStep) bool {
	if step == nil {
		return false
	}
	kind := ""
	if step.Metadata != nil {
		kind = strings.TrimSpace(step.Metadata[beadmeta.KindMetadataKey])
	}
	return !beadmeta.IsControlKind(kind) && !slices.Contains(beadmeta.WorkflowTopologyKinds, kind)
}

func sharedDrainContinuationGroup(control beads.Bead) string {
	group := "drain:" + control.ID
	if suffix := strings.TrimSpace(control.Metadata[beadmeta.DrainContinuationGroupMetadataKey]); suffix != "" {
		group += ":" + suffix
	}
	return group
}

type drainReservationError struct {
	ControlID string
	MemberID  string
	Owner     string
}

func (e drainReservationError) Error() string {
	return fmt.Sprintf("%s: member %s already reserved by drain %s", e.ControlID, e.MemberID, e.Owner)
}

func reserveDrainMember(store beads.Store, control, member beads.Bead) error {
	if drainMemberAccess(control) != beadmeta.DrainMemberAccessExclusive {
		return nil
	}
	current, err := store.Get(member.ID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("%s: loading exclusive drain member %s: %w", control.ID, member.ID, err)
	}
	owner := strings.TrimSpace(current.Metadata[beadmeta.ExclusiveDrainReservationMetadataKey])
	if owner != "" && owner != control.ID {
		return drainReservationError{ControlID: control.ID, MemberID: member.ID, Owner: owner}
	}
	if owner == control.ID {
		return nil
	}
	return store.SetMetadata(member.ID, beadmeta.ExclusiveDrainReservationMetadataKey, control.ID)
}

func reserveDrainMembers(store beads.Store, control beads.Bead, members []beads.Bead) error {
	for _, member := range members {
		if err := reserveDrainMember(store, control, member); err != nil {
			return err
		}
	}
	return nil
}

func releaseDrainReservations(store beads.Store, controlID string, manifest drainManifest) error {
	controlID = strings.TrimSpace(controlID)
	if store == nil || controlID == "" {
		return nil
	}
	seen := make(map[string]bool, len(manifest.Rows))
	for _, row := range manifest.Rows {
		memberID := strings.TrimSpace(row.MemberID)
		if memberID == "" || seen[memberID] {
			continue
		}
		seen[memberID] = true
		member, err := store.Get(memberID)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return fmt.Errorf("%s: loading drain member %s for reservation release: %w", controlID, memberID, err)
		}
		if strings.TrimSpace(member.Metadata[beadmeta.ExclusiveDrainReservationMetadataKey]) != controlID {
			continue
		}
		if err := store.SetMetadata(memberID, beadmeta.ExclusiveDrainReservationMetadataKey, ""); err != nil {
			return fmt.Errorf("%s: releasing drain reservation on %s: %w", controlID, memberID, err)
		}
	}
	return nil
}

func closeDrainReservationFailure(store beads.Store, bead beads.Bead, manifest drainManifest, err error, opts ProcessOptions) (ControlResult, error) {
	var reservationErr drainReservationError
	failureReason := "exclusive_reservation_failed"
	metadata := map[string]string{
		beadmeta.DrainStateMetadataKey:    beadmeta.DrainStateFailed,
		beadmeta.OutcomeMetadataKey:       beadmeta.OutcomeFail,
		beadmeta.FailureClassMetadataKey:  beadmeta.FailureClassHard,
		beadmeta.FailureReasonMetadataKey: failureReason,
	}
	if errors.As(err, &reservationErr) {
		failureReason = "exclusive_reservation_conflict"
		metadata[beadmeta.FailureReasonMetadataKey] = "exclusive_reservation_conflict"
		metadata[beadmeta.FailureSubjectMetadataKey] = reservationErr.MemberID
		metadata[beadmeta.FailureOwnerMetadataKey] = reservationErr.Owner
	}
	if closeErr := closeOpenDrainItemRoots(store, &manifest, failureReason); closeErr != nil {
		return ControlResult{}, fmt.Errorf("%s: closing partial drain item roots after %w: %w", bead.ID, err, closeErr)
	}
	data, marshalErr := json.Marshal(manifest)
	if marshalErr != nil {
		return ControlResult{}, marshalErr
	}
	metadata[drainManifestMetadataKey] = string(data)
	if releaseErr := releaseDrainReservations(store, bead.ID, manifest); releaseErr != nil {
		return ControlResult{}, fmt.Errorf("%s: releasing reservations after %w: %w", bead.ID, err, releaseErr)
	}
	if closeErr := updateMetadataAndClose(store, bead.ID, metadata); closeErr != nil {
		return ControlResult{}, fmt.Errorf("%s: closing reservation-failed drain after %w: %w", bead.ID, err, closeErr)
	}
	scopeResult, scopeErr := reconcileClosedDrainScope(store, bead.ID, opts)
	if scopeErr != nil {
		return ControlResult{}, scopeErr
	}
	return ControlResult{Processed: true, Action: "drain-reservation-failed", Skipped: scopeResult.Skipped}, nil
}

func closeDrainItemFormulaFailure(store beads.Store, bead beads.Bead, manifest drainManifest, err error, opts ProcessOptions) (ControlResult, error) {
	const failureReason = "invalid_drain_item_formula"
	if closeErr := closeOpenDrainItemRoots(store, &manifest, failureReason); closeErr != nil {
		return ControlResult{}, fmt.Errorf("%s: closing partial drain item roots after %w: %w", bead.ID, err, closeErr)
	}
	markIncompleteDrainRowsFailed(&manifest, failureReason)
	data, marshalErr := json.Marshal(manifest)
	if marshalErr != nil {
		return ControlResult{}, marshalErr
	}
	metadata := map[string]string{
		beadmeta.DrainStateMetadataKey:    beadmeta.DrainStateFailed,
		beadmeta.OutcomeMetadataKey:       beadmeta.OutcomeFail,
		beadmeta.FailureClassMetadataKey:  beadmeta.FailureClassHard,
		beadmeta.FailureReasonMetadataKey: failureReason,
		drainManifestMetadataKey:          string(data),
	}
	if manifest.Formula != "" {
		metadata[beadmeta.FailureSubjectMetadataKey] = manifest.Formula
	}
	if releaseErr := releaseDrainReservations(store, bead.ID, manifest); releaseErr != nil {
		return ControlResult{}, fmt.Errorf("%s: releasing reservations after %w: %w", bead.ID, err, releaseErr)
	}
	if closeErr := updateMetadataAndClose(store, bead.ID, metadata); closeErr != nil {
		return ControlResult{}, fmt.Errorf("%s: closing invalid-item-formula drain after %w: %w", bead.ID, err, closeErr)
	}
	scopeResult, scopeErr := reconcileClosedDrainScope(store, bead.ID, opts)
	if scopeErr != nil {
		return ControlResult{}, scopeErr
	}
	return ControlResult{Processed: true, Action: "drain-failed", Skipped: scopeResult.Skipped}, nil
}

func markIncompleteDrainRowsFailed(manifest *drainManifest, failureReason string) {
	if manifest == nil {
		return
	}
	for i := range manifest.Rows {
		row := &manifest.Rows[i]
		if row.Status == "succeeded" || row.OutcomeKind == beadmeta.OutcomePass {
			continue
		}
		row.Status = "failed"
		if row.OutcomeKind == "" {
			row.OutcomeKind = beadmeta.OutcomeFail
		}
		if row.Failure == "" {
			row.Failure = failureReason
		}
	}
}

func closeOpenDrainItemRoots(store beads.Store, manifest *drainManifest, failureReason string) error {
	if manifest == nil {
		return nil
	}
	for i := range manifest.Rows {
		row := &manifest.Rows[i]
		rootID := strings.TrimSpace(row.ItemRootID)
		if rootID == "" {
			continue
		}
		root, err := store.Get(rootID)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return fmt.Errorf("loading drain item root %s: %w", rootID, err)
		}
		if root.Status == "closed" {
			recordDrainRowOutcome(row, root)
			continue
		}
		row.OutcomeBead = rootID
		row.OutcomeKind = beadmeta.OutcomeFail
		row.Failure = failureReason
		if _, err := sourceworkflow.CloseWorkflowSubtree(store, rootID); err != nil {
			return fmt.Errorf("closing drain item workflow subtree %s: %w", rootID, err)
		}
		if err := store.SetMetadataBatch(rootID, map[string]string{
			beadmeta.OutcomeMetadataKey:       beadmeta.OutcomeFail,
			beadmeta.FailureClassMetadataKey:  beadmeta.FailureClassHard,
			beadmeta.FailureReasonMetadataKey: failureReason,
		}); err != nil {
			return fmt.Errorf("marking drain item root %s failed: %w", rootID, err)
		}
		row.Status = "failed"
	}
	return nil
}

func persistDrainManifest(store beads.Store, beadID string, manifest drainManifest, metadata map[string]string) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if metadata == nil {
		metadata = make(map[string]string)
	}
	metadata[drainManifestMetadataKey] = string(data)
	return store.SetMetadataBatch(beadID, metadata)
}

func parseDrainManifest(raw string) (drainManifest, error) {
	var manifest drainManifest
	if strings.TrimSpace(raw) == "" {
		return manifest, fmt.Errorf("manifest is empty")
	}
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		return manifest, err
	}
	return manifest, nil
}

func drainMaxUnits(bead beads.Bead) (int, error) {
	raw := strings.TrimSpace(bead.Metadata[beadmeta.DrainMaxUnitsMetadataKey])
	if raw == "" {
		return defaultDrainMaxUnits, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > defaultDrainMaxUnits {
		return 0, fmt.Errorf("%s: invalid gc.drain_max_units %q", bead.ID, raw)
	}
	return n, nil
}

func drainMemberAccess(bead beads.Bead) string {
	access := strings.TrimSpace(bead.Metadata[beadmeta.DrainMemberAccessMetadataKey])
	if access == "" {
		return "read"
	}
	return access
}

func drainOnItemFailure(bead beads.Bead) string {
	policy := strings.TrimSpace(bead.Metadata[beadmeta.DrainOnItemFailureMetadataKey])
	if policy != "" {
		return policy
	}
	if strings.TrimSpace(bead.Metadata[beadmeta.DrainContextMetadataKey]) == beadmeta.DrainContextShared {
		return beadmeta.DrainOnItemFailureSkipRemaining
	}
	return beadmeta.DrainOnItemFailureContinue
}

func mustReloadDrain(store beads.Store, bead beads.Bead) beads.Bead {
	reloaded, err := store.Get(bead.ID)
	if err != nil {
		return bead
	}
	return reloaded
}
