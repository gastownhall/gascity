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
	"github.com/gastownhall/gascity/internal/storeref"
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
		return ControlResult{}, fmt.Errorf("%s: parsing formulas v2 runtime vars on root %s: %w", bead.ID, rootID, err)
	}
	itemFormula := strings.TrimSpace(bead.Metadata[beadmeta.DrainFormulaMetadataKey])
	if itemFormula == "" {
		return ControlResult{}, fmt.Errorf("%s: missing gc.drain_formula", bead.ID)
	}
	manifest, members, err := loadOrBuildDrainManifest(store, bead, parentConvoyID, itemFormula, opts)
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
	if err := reserveDrainMembers(store, bead, members, opts); err != nil {
		if retryableDrainReservationError(err) {
			return ControlResult{}, fmt.Errorf("%s: reserving drain members (retrying next pass): %w", bead.ID, err)
		}
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
			unit, created, err = ensureDrainUnitConvoy(store, bead, parentConvoyID, len(members), *row, member, opts)
			if err != nil {
				return ControlResult{}, err
			}
			if created {
				totalCreated++
			}
			row.UnitConvoyID = unit.ID
			row.Status = "unit-created"
		} else {
			reloaded, err := reloadDrainUnitConvoy(store, row.UnitConvoyID, opts)
			if err != nil {
				return ControlResult{}, fmt.Errorf("%s: loading drain unit convoy %s: %w", bead.ID, row.UnitConvoyID, err)
			}
			unit = reloaded
		}

		if row.ItemRootID == "" {
			projection, err := drainProjectedBlockerIDs(store, member.ID, manifest, opts)
			if err != nil {
				return ControlResult{}, fmt.Errorf("%s: listing source dependencies for member %s: %w", bead.ID, member.ID, err)
			}
			rootID, created, err := ensureDrainItemRoot(store, bead, unit, member, len(members), row, itemFormula, parentVars, projection.BlockerIDs, opts)
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
		if err := ensureDrainRowDependencyProjection(store, bead, manifest, member.ID, row.ItemRootID, opts); err != nil {
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
	if err := ensureDrainDependencyProjection(store, bead, manifest, opts); err != nil {
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
		reloaded, err := reloadDrain(store, bead)
		if err != nil {
			return ControlResult{}, err
		}
		return completeDrain(store, reloaded, opts)
	}
	return ControlResult{Processed: true, Action: "drain-expanded", Created: totalCreated}, nil
}

func loadOrBuildDrainManifest(store beads.Store, bead beads.Bead, parentConvoyID, itemFormula string, opts ProcessOptions) (drainManifest, []beads.Bead, error) {
	if strings.TrimSpace(bead.Metadata[drainManifestMetadataKey]) != "" {
		manifest, err := parseDrainManifest(bead.Metadata[drainManifestMetadataKey])
		if err != nil {
			return drainManifest{}, nil, fmt.Errorf("%s: parsing persisted drain manifest: %w", bead.ID, err)
		}
		members, err := loadDrainManifestMembers(store, bead.ID, manifest, opts)
		if err != nil {
			return drainManifest{}, nil, err
		}
		return manifest, members, nil
	}
	members, err := drainConvoyMembers(store, parentConvoyID, opts)
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
	orderedMembers, err := orderDrainMembersByDependencies(store, members, opts)
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

// drainMemberOwningStore returns the store that owns memberID, resolved through
// the drain's residency frame (drainResidency) rather than a hand-walked probe
// list. When no store has the member it falls back to the primary store so
// reservation reads/writes preserve their pre-seam not-found handling
// (reserveDrainMember/releaseDrainReservations treat ErrNotFound as a no-op) —
// and the fallback matters for correctness, not just parity: a reserved-namespace
// miss resolves to a NIL store, which a caller must never write through.
func drainMemberOwningStore(store beads.Store, memberID string, opts ProcessOptions) (beads.Store, error) {
	owner, found, err := resolveDrainMember(store, memberID, opts)
	if err != nil {
		return nil, err
	}
	if !found {
		return store, nil
	}
	return owner.Store, nil
}

// drainMemberDepStore returns the store to read a drain member's dependency
// edges from. A member work bead — and the dependency edges co-resident with it
// — may live in a different per-class store than the ambient graph store the
// drain control runs in. When per-class member stores are configured it resolves
// the member's owning store (drainMemberOwningStore); with none configured
// (single-store callers) it returns the ambient store WITHOUT the owning-store
// probe read, so today's behavior — including the pre-seam DepList error path —
// is byte-identical and the per-tick drain projection sweep adds no extra
// round-trip. Unlike reserveDrainMember/releaseDrainReservations (which Get the
// member anyway to read/write its reservation metadata), the projection reads
// only the member's edges, so the probe would be pure overhead in the common
// single-store case.
func drainMemberDepStore(store beads.Store, memberID string, opts ProcessOptions) (beads.Store, error) {
	if len(opts.MemberStores) == 0 {
		return store, nil
	}
	return drainMemberOwningStore(store, memberID, opts)
}

// drainConvoyMembers reads a drain's input-convoy membership across every
// candidate store rather than from the one store the drain itself runs in.
//
// opts.MemberStores resolves member beads, and that is only the TAIL of the
// lookup. The head — the convoy's own legacy children (List by ParentID) and its
// tracks edges (DepList) — is read from a single handle, and reading it from the
// ambient graph store is what silently loses a whole convoy: a store that has
// never seen the convoy answers both reads EMPTY rather than erroring, and an
// empty membership is indistinguishable from a genuinely empty input convoy, so
// the drain expands to zero rows and closes gc.outcome=pass with every member
// left open and undispatched.
//
// Which store SHOULD answer is settled: a convoy is a work bead, synthetic ones
// included, so its edges are in the work store while the drain runs in the
// binding. The work leg is therefore the authoritative one and the graph leg is
// expected to contribute nothing.
//
// It is still read as the UNION of what each candidate store records rather than
// as a single lookup, for two reasons. A city migrated under the previous
// classification has an edgeless copy of every synthetic convoy in its binding —
// the migration copied the row and importInfraSnapshot re-added only the edges
// whose both endpoints were infra — so "the store that has the convoy bead" is
// still an ambiguous question on real disks, and answering it wrong is silent.
// And the failure this guards is not a wrong answer but a green one: a drain
// that could not read its membership produces byte-for-byte the same terminal
// state as a drain that correctly found nothing to do.
//
// The union cannot be wrong in the way a single lookup can. It is order-free and
// monotone — a store that never saw the convoy contributes nothing, an edgeless
// copy contributes nothing, and no store's silence can subtract a member another
// store records — so a zero-row expansion over a non-empty convoy is structurally
// impossible rather than merely unlikely, and stays impossible even if some
// future path mints a convoy somewhere this comment does not predict. A member
// seen as an unresolved placeholder in one store and as a real bead in another
// keeps the real bead.
//
// With no member stores configured — every single-store caller — this collapses
// to the single convoycore.Members call it replaces, byte for byte.
func drainConvoyMembers(store beads.Store, convoyID string, opts ProcessOptions) ([]beads.Bead, error) {
	if len(opts.MemberStores) == 0 {
		return convoycore.Members(store, convoyID, false)
	}
	topo, err := drainResidency(store, opts)
	if err != nil {
		return nil, err
	}
	plan, err := storeref.Plan(storeref.RoutedWork{}, topo)
	if err != nil {
		return nil, err
	}
	// Deliberately un-deduped (nil id fn): Union's own first-leg-wins would drop
	// the second copy of a member id outright, and this merge needs to SEE it —
	// a placeholder recorded by the leg that answered first must still yield to
	// the real bead another leg records.
	union, err := storeref.Union[beads.Bead](plan, nil,
		func(leg storeref.Leg) ([]beads.Bead, error) {
			return convoycore.Members(leg.Store, convoyID, false, opts.MemberStores...)
		})
	if err != nil {
		return nil, err
	}
	var merged []beads.Bead
	at := make(map[string]int)
	for _, member := range union.Items {
		i, seen := at[member.ID]
		if !seen {
			at[member.ID] = len(merged)
			merged = append(merged, member)
			continue
		}
		if convoycore.IsUnresolvedTrackedItem(merged[i]) && !convoycore.IsUnresolvedTrackedItem(member) {
			merged[i] = member
		}
	}
	// Same order convoycore.MembersIn returns within one store, so the manifest
	// row order does not depend on which store answered first.
	slices.SortStableFunc(merged, func(a, b beads.Bead) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return merged, nil
}

func loadDrainManifestMembers(store beads.Store, controlID string, manifest drainManifest, opts ProcessOptions) ([]beads.Bead, error) {
	members := make([]beads.Bead, 0, len(manifest.Rows))
	for _, row := range manifest.Rows {
		owner, found, err := resolveDrainMember(store, row.MemberID, opts)
		if err != nil {
			return nil, fmt.Errorf("%s: loading persisted drain member %s: %w", controlID, row.MemberID, err)
		}
		if !found {
			// A blank id never reaches here: ByID refuses it above, which is the
			// same outcome the pre-seam probe produced for one.
			members = append(members, beads.Bead{ID: row.MemberID, Title: row.MemberID, Type: "task", Status: "unknown"})
			continue
		}
		members = append(members, owner.Bead)
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
			return ControlResult{}, fmt.Errorf("%s: parsing formulas v2 runtime vars on root %s: %w", bead.ID, rootID, err)
		}
		members, err := loadDrainManifestMembers(store, bead.ID, manifest, opts)
		if err != nil {
			return ControlResult{}, err
		}
		return advanceSharedDrain(store, bead, manifest, members, manifest.Formula, parentVars, opts)
	}
	// Re-running the projection here lets manifests whose item workflows were
	// wired to source members by earlier builds heal while the drain waits on
	// open item roots; expansion never revisits an expanded drain.
	if err := ensureDrainDependencyProjection(store, bead, manifest, opts); err != nil {
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
	if err := releaseDrainReservations(store, bead.ID, manifest, opts); err != nil {
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
	if err := ensureDrainDependencyProjection(store, bead, manifest, opts); err != nil {
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
		if err := reserveDrainMember(store, bead, member, opts); err != nil {
			if retryableDrainReservationError(err) {
				return ControlResult{}, fmt.Errorf("%s: reserving drain member %s (retrying next pass): %w", bead.ID, member.ID, err)
			}
			return closeDrainReservationFailure(store, bead, manifest, err, opts)
		}
		created, err := materializeDrainRow(store, bead, manifest, members, row, member, itemFormula, parentVars, opts)
		if err != nil {
			if errors.Is(err, errDrainInvalidItemFormula) {
				return closeDrainItemFormulaFailure(store, bead, manifest, err, opts)
			}
			return ControlResult{}, err
		}
		if err := ensureDrainDependencyProjection(store, bead, manifest, opts); err != nil {
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
		createdUnit, created, err := ensureDrainUnitConvoy(store, control, manifest.ParentConvoyID, len(members), *row, member, opts)
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
		reloaded, err := reloadDrainUnitConvoy(store, row.UnitConvoyID, opts)
		if err != nil {
			return 0, fmt.Errorf("%s: loading drain unit convoy %s: %w", control.ID, row.UnitConvoyID, err)
		}
		unit = reloaded
	}
	if row.ItemRootID == "" {
		projection, err := drainProjectedBlockerIDs(store, member.ID, manifest, opts)
		if err != nil {
			return 0, fmt.Errorf("%s: listing source dependencies for member %s: %w", control.ID, member.ID, err)
		}
		rootID, created, err := ensureDrainItemRoot(store, control, unit, member, len(members), row, itemFormula, parentVars, projection.BlockerIDs, opts)
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
	if err := ensureDrainRowDependencyProjection(store, control, manifest, member.ID, row.ItemRootID, opts); err != nil {
		if controllerSpawnBoundaryPending(store, control.ID, err, opts) {
			return 0, ErrControlPending
		}
		return 0, fmt.Errorf("%s: projecting drain dependencies for member %s: %w", control.ID, member.ID, err)
	}
	row.Status = "wired"
	return createdCount, nil
}

func ensureDrainDependencyProjection(store beads.Store, control beads.Bead, manifest drainManifest, opts ProcessOptions) error {
	for _, row := range manifest.Rows {
		memberID := strings.TrimSpace(row.MemberID)
		rootID := strings.TrimSpace(row.ItemRootID)
		if memberID == "" || rootID == "" {
			continue
		}
		if err := ensureDrainRowDependencyProjection(store, control, manifest, memberID, rootID, opts); err != nil {
			return err
		}
	}
	return nil
}

func ensureDrainRowDependencyProjection(store beads.Store, control beads.Bead, manifest drainManifest, memberID, rootID string, opts ProcessOptions) error {
	projection, err := drainProjectedBlockerIDs(store, memberID, manifest, opts)
	if err != nil {
		return fmt.Errorf("%s: listing source dependencies for member %s: %w", control.ID, memberID, err)
	}
	for _, blockerID := range projection.BlockerIDs {
		if blockerID == rootID {
			continue
		}
		if err := ensureDrainWorkflowBlocksOn(store, rootID, blockerID); err != nil {
			return fmt.Errorf("%s: wiring item workflow %s for member %s to blocker %s: %w", control.ID, rootID, memberID, blockerID, err)
		}
	}
	if err := recordUnprojectableDrainBlockers(store, control, memberID, rootID, projection.Unprojectable, opts); err != nil {
		return fmt.Errorf("%s: recording unprojectable blockers for member %s: %w", control.ID, memberID, err)
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

// drainBlockerProjection is the result of projecting one drain member's
// ready-blocking dependencies onto its item workflow.
type drainBlockerProjection struct {
	// BlockerIDs are the ids the item workflow's beads can actually depend on:
	// either another manifest row's item root, or an out-of-manifest blocker the
	// item workflow's own store can resolve.
	BlockerIDs []string

	// Unprojectable are out-of-manifest blockers that are still open and that the
	// item workflow's own store cannot resolve, so the constraint they express
	// cannot be written down at all. Sorted, and recorded on the item root by
	// ensureDrainRowDependencyProjection.
	Unprojectable []string
}

// drainBlockerResidence classifies one out-of-manifest blocker for projection.
type drainBlockerResidence int

const (
	// drainBlockerUnclassified is the zero value, returned only alongside an
	// error. It is not a decision, and the projection switch refuses it rather
	// than letting a read failure look like a dropped blocker.
	drainBlockerUnclassified drainBlockerResidence = iota

	// drainBlockerProjectable: the item workflow's own store resolves the
	// blocker, so the dependency edge is writable and means what it says.
	drainBlockerProjectable

	// drainBlockerSatisfied: the blocker is not co-resident, but it is already
	// terminal, so it constrains nothing. Omitting the edge loses no meaning —
	// on a single-store city the edge exists and the readiness reader ignores it
	// for exactly the same reason.
	drainBlockerSatisfied

	// drainBlockerUnprojectable: the blocker is not co-resident and is still an
	// open constraint. Omitting the edge does lose meaning, so it is recorded.
	drainBlockerUnprojectable
)

// classifyDrainBlocker decides whether an out-of-manifest blocker can be
// projected onto an item workflow that lives in store.
//
// A dependency row lives in one store's dep table and can only reference an id
// that store resolves — the same rule convoy.ErrMemberNotCoResident enforces for
// membership edges. Writing one anyway does not degrade to "unenforced": the
// readiness reader LEFT JOINs the blocker row, so an absent target yields a NULL
// status that is never 'closed', and the dependent bead drops out of Ready
// permanently. Closing the real blocker in its own store cannot release it,
// because the store holding the edge can never see that row. So the choice is
// not between a strict edge and a loose one, it is between omitting the edge and
// wedging the workflow forever.
//
// Single-store callers skip the check entirely: there is one store, so every id
// it resolves is co-resident and the probe would be pure overhead.
//
// The "is it resolvable at all" half goes through resolveDrainMember, the same
// ByID plan the manifest and the reservation use. It used to be storeref.Resolve
// over the member-store tail, whose PrefixOwner fast path routes on the id's own
// prefix: a blocker whose class was relocated but whose id the migration
// preserved matched the WORK store's prefix and was read from the frozen
// retained copy, so a blocker CLOSED after the cutover still looked open and the
// edge was dropped as unprojectable (ga-cu12x). The ambient store is asked
// first, above, and that read is the co-residency question — a different one
// from residency, and the only reason it stays hand-written here.
func classifyDrainBlocker(store beads.Store, blockerID string, opts ProcessOptions) (drainBlockerResidence, error) {
	if len(opts.MemberStores) == 0 {
		return drainBlockerProjectable, nil
	}
	if _, err := store.Get(blockerID); err == nil {
		return drainBlockerProjectable, nil
	} else if !errors.Is(err, beads.ErrNotFound) {
		return drainBlockerUnclassified, err
	}
	owner, found, err := resolveDrainMember(store, blockerID, opts)
	if err != nil {
		return drainBlockerUnclassified, err
	}
	if !found {
		// Resolvable nowhere: the source member's own edge already dangles.
		// Report it rather than copying the dangle into another store.
		return drainBlockerUnprojectable, nil
	}
	if convoycore.IsTerminalStatus(owner.Bead.Status) {
		return drainBlockerSatisfied, nil
	}
	return drainBlockerUnprojectable, nil
}

func drainProjectedBlockerIDs(store beads.Store, memberID string, manifest drainManifest, opts ProcessOptions) (drainBlockerProjection, error) {
	rootByMember := drainRootByMember(manifest)
	manifestMembers := drainManifestMemberIDs(manifest)
	// A member work bead's dependency edges are co-resident with it and may live
	// in a different per-class store than this drain control's ambient graph
	// store; read them from the member's owning store (probe-free identity to the
	// ambient store for single-store callers — see drainMemberDepStore).
	memberStore, err := drainMemberDepStore(store, memberID, opts)
	if err != nil {
		return drainBlockerProjection{}, err
	}
	deps, err := memberStore.DepList(memberID, "down")
	if err != nil {
		return drainBlockerProjection{}, err
	}
	seen := make(map[string]bool, len(deps))
	projection := drainBlockerProjection{BlockerIDs: make([]string, 0, len(deps))}
	for _, dep := range deps {
		if !beads.IsReadyBlockingDependencyType(dep.Type) {
			continue
		}
		dependsOnID := strings.TrimSpace(dep.DependsOnID)
		if dependsOnID == "" || dependsOnID == memberID {
			continue
		}
		blockerID := dependsOnID
		onItemRoot := false
		if projectedRootID := strings.TrimSpace(rootByMember[dependsOnID]); projectedRootID != "" {
			blockerID = projectedRootID
			onItemRoot = true
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
		if onItemRoot {
			// Item roots are minted in store, so this one is co-resident by
			// construction and needs no probe.
			projection.BlockerIDs = append(projection.BlockerIDs, blockerID)
			continue
		}
		residence, err := classifyDrainBlocker(store, blockerID, opts)
		if err != nil {
			return drainBlockerProjection{}, err
		}
		switch residence {
		case drainBlockerProjectable:
			projection.BlockerIDs = append(projection.BlockerIDs, blockerID)
		case drainBlockerSatisfied:
			opts.tracef("drain: member %s blocker %s is already terminal and lives outside the item workflow's store; not projecting an edge that would constrain nothing", memberID, blockerID)
		case drainBlockerUnprojectable:
			projection.Unprojectable = append(projection.Unprojectable, blockerID)
		default:
			return drainBlockerProjection{}, fmt.Errorf("classifying blocker %s for member %s: unclassified residence %d", blockerID, memberID, residence)
		}
	}
	slices.Sort(projection.Unprojectable)
	return projection, nil
}

// recordUnprojectableDrainBlockers stamps the blockers a drain could not express
// onto the item root, so an item workflow that runs without a constraint its
// source member had says so on its own bead instead of only in a log line.
//
// It is a durable record rather than a failure on purpose. Refusing the drain
// would turn the commonest backlog shape — a member with any out-of-convoy
// `blocks` edge — into a hard control failure on split cities, which is the
// terminal state this routing exists to remove; and writing the edge anyway
// wedges the item workflow permanently (see classifyDrainBlocker). Recording is
// the only option that neither loses the information nor stalls the work.
func recordUnprojectableDrainBlockers(store beads.Store, control beads.Bead, memberID, rootID string, blockerIDs []string, opts ProcessOptions) error {
	if len(blockerIDs) == 0 {
		return nil
	}
	value := strings.Join(blockerIDs, ",")
	root, err := store.Get(rootID)
	if err != nil {
		return fmt.Errorf("loading item root %s: %w", rootID, err)
	}
	if root.Metadata[beadmeta.DrainUnprojectedBlockersMetadataKey] == value {
		return nil
	}
	opts.tracef("drain %s: member %s is blocked by %s, which the item workflow's store cannot resolve; item root %s runs without that constraint (recorded as %s)",
		control.ID, memberID, value, rootID, beadmeta.DrainUnprojectedBlockersMetadataKey)
	if err := store.SetMetadata(rootID, beadmeta.DrainUnprojectedBlockersMetadataKey, value); err != nil {
		return fmt.Errorf("recording unprojectable blockers on item root %s: %w", rootID, err)
	}
	return nil
}

func ensureDrainWorkflowBlocksOn(store beads.Store, rootID, blockerID string) error {
	rootID = strings.TrimSpace(rootID)
	blockerID = strings.TrimSpace(blockerID)
	if rootID == "" || blockerID == "" || rootID == blockerID {
		return nil
	}
	workflowBeads, err := beads.DirectMembers(store, rootID)
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
	workflowBeads, err := beads.DirectMembers(store, rootID)
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
	if err := releaseDrainReservations(store, beadID, manifest, opts); err != nil {
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

func orderDrainMembersByDependencies(store beads.Store, members []beads.Bead, opts ProcessOptions) ([]beads.Bead, error) {
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
		// A member's dependency edges are co-resident with the member work bead,
		// which may live in a different per-class store than the ambient graph
		// store; resolve the member's owning store (probe-free identity to the
		// ambient store for single-store callers — see drainMemberDepStore)
		// before listing its edges.
		memberStore, err := drainMemberDepStore(store, memberID, opts)
		if err != nil {
			return nil, fmt.Errorf("resolving source dependency store for member %s: %w", memberID, err)
		}
		deps, err := memberStore.DepList(memberID, "down")
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

// walkDrainUnitConvoyLegs visits the stores a drain resolves its OWN unit
// convoys through, stopping at the first leg that answers.
//
// The intent is RoutedWork, not ByID, and that is the whole reason this
// resolution and drainMemberOwningStore's do not share an order. A unit convoy
// is a SYNTHETIC convoy and a synthetic convoy is a WORK bead
// (coordclass.Classify), so the work class is authoritative here and the ambient
// graph binding is only a fallback for rows predating that ruling. Asking the
// binding first gets the wrong answer on a real converged city: cities migrated
// under the previous classification carry an EDGELESS copy of every synthetic
// convoy in their binding (`gc storage migrate` copied the row;
// importInfraSnapshot re-added only the edges whose both endpoints were infra),
// so the binding can answer gc.drain_unit_key with a convoy that has no tracks
// edge and cannot grow one. A drain MEMBER is the opposite case — foreign, class
// unknown — which is why that one plans ByID and leads with the binding.
//
// With no member stores configured — every single-store caller — the plan is the
// one ambient store, so every read through it is the single read it is today.
func walkDrainUnitConvoyLegs(store beads.Store, opts ProcessOptions, visit func(beads.Store) (bool, error)) error {
	topo, err := drainResidency(store, opts)
	if err != nil {
		return err
	}
	plan, err := storeref.Plan(storeref.RoutedWork{}, topo)
	if err != nil {
		return err
	}
	_, err = storeref.Walk(plan, func(leg storeref.Leg) (bool, error) { return visit(leg.Store) })
	return err
}

// drainWorkClassStore returns the handle for the work class: the first member
// store the caller named, or the ambient store when it named none.
func drainWorkClassStore(store beads.Store, opts ProcessOptions) beads.Store {
	for _, probe := range opts.MemberStores {
		if probe == nil {
			continue
		}
		return probe
	}
	return store
}

// drainUnitConvoyStore returns the store a drain MINTS a unit convoy in: the
// store that owns the member the unit convoy exists to track.
//
// That is not a choice among stores, it is the only store that works. A drain
// unit convoy is a SYNTHETIC convoy and a synthetic convoy is a WORK bead
// (coordclass.Classify), which is the same thing the mechanics already require:
// the convoy's whole content is one `tracks` edge to one work member, and
// convoy.TrackItemIn refuses an edge whose member is owned by another class,
// because a dep row cannot reference an id its own store cannot resolve. Minted
// anywhere else the convoy can never track the member it was created for, and
// the drain fails on the very next call.
//
// A member no store can resolve pins the convoy to the WORK class rather than
// falling back to the ambient store. An unresolved member is a supported state,
// not an error — loadDrainManifestMembers synthesizes a placeholder for a member
// deleted mid-drain and trackDrainMember stamps gc.drain_member_unresolved
// instead of writing an edge — so the mint still has to happen, and it still has
// to happen in the class that owns synthetic convoys. Falling back to the ambient
// graph binding would write a work-class bead into the infra ledger, which the
// migration's own equality invariant says never happens.
//
// Only the mint is derived from the member. Lookup by gc.drain_unit_key and
// reload by id go through drainUnitConvoyProbeSet, because a member's
// resolvability can change between drain passes while the convoy it already
// minted does not move.
//
// With no member stores configured — every single-store caller — it returns the
// ambient store without any probe, so the create is the same single-store write
// it is today, with no extra round-trip.
func drainUnitConvoyStore(store beads.Store, member beads.Bead, opts ProcessOptions) (beads.Store, error) {
	if len(opts.MemberStores) == 0 {
		return store, nil
	}
	memberID := strings.TrimSpace(member.ID)
	if memberID == "" || convoycore.IsUnresolvedTrackedItem(member) {
		return drainWorkClassStore(store, opts), nil
	}
	var owner beads.Store
	err := walkDrainUnitConvoyLegs(store, opts, func(probe beads.Store) (bool, error) {
		if _, err := probe.Get(memberID); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				return false, nil
			}
			return false, err
		}
		owner = probe
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	if owner != nil {
		return owner, nil
	}
	return drainWorkClassStore(store, opts), nil
}

// drainUnitConvoyByKey finds the unit convoy a previous pass already minted for
// row.UnitKey, and the store that holds it, across every candidate store.
//
// The key is the idempotence token for the mint, so the lookup has to see every
// store the mint could have chosen. Running it against one store re-derived from
// the member is what mints a duplicate: a member that resolved on the pass that
// minted the convoy and does not resolve on the pass that resumes sends the
// lookup to a different store, it misses, and the drain mints a second unit
// convoy for a row that already has one.
func drainUnitConvoyByKey(store beads.Store, unitKey string, opts ProcessOptions) (beads.Bead, beads.Store, bool, error) {
	var (
		found beads.Bead
		owner beads.Store
	)
	err := walkDrainUnitConvoyLegs(store, opts, func(probe beads.Store) (bool, error) {
		existing, err := probe.ListByMetadata(map[string]string{beadmeta.DrainUnitKeyMetadataKey: unitKey}, 1, beads.WithBothTiers)
		if err != nil {
			return false, err
		}
		if len(existing) == 0 {
			return false, nil
		}
		found, owner = existing[0], probe
		return true, nil
	})
	if err != nil {
		return beads.Bead{}, nil, false, err
	}
	if owner == nil {
		return beads.Bead{}, nil, false, nil
	}
	return found, owner, true, nil
}

func ensureDrainUnitConvoy(store beads.Store, control beads.Bead, parentConvoyID string, count int, row drainManifestRow, member beads.Bead, opts ProcessOptions) (beads.Bead, bool, error) {
	unlock := graphv2.LockKey(row.UnitKey)
	defer unlock()
	existing, existingStore, found, err := drainUnitConvoyByKey(store, row.UnitKey, opts)
	if err != nil {
		return beads.Bead{}, false, fmt.Errorf("%s: looking up unit convoy for member %s: %w", control.ID, member.ID, err)
	}
	if found {
		if err := ensureDrainUnitTrack(existingStore, control.ID, existing.ID, member); err != nil {
			return beads.Bead{}, false, err
		}
		return existing, false, nil
	}
	unitStore, err := drainUnitConvoyStore(store, member, opts)
	if err != nil {
		return beads.Bead{}, false, fmt.Errorf("%s: resolving the store for member %s: %w", control.ID, member.ID, err)
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
	created, err := unitStore.Create(beads.Bead{
		Title:    fmt.Sprintf("drain unit %d for %s", row.Index, member.ID),
		Type:     "convoy",
		Priority: member.Priority,
		Metadata: metadata,
	})
	if err != nil {
		return beads.Bead{}, false, fmt.Errorf("%s: creating unit convoy for member %s: %w", control.ID, member.ID, err)
	}
	if err := trackDrainMember(unitStore, created.ID, member); err != nil {
		return beads.Bead{}, false, fmt.Errorf("%s: tracking member %s from unit convoy %s: %w", control.ID, member.ID, created.ID, err)
	}
	return created, true, nil
}

// reloadDrainUnitConvoy re-reads a unit convoy a previous pass already minted,
// by the convoy's OWN id, exactly as loadDrainManifestMembers re-reads a member
// by its own id.
//
// Re-deriving the store from the member instead is unstable across passes, and
// that instability is a permanent quarantine rather than a retry. The manifest
// persists row.UnitConvoyID but not the store that answered when it was minted,
// so a member that resolves on the minting pass and not on the resuming pass —
// a hard delete between passes, which the drain otherwise tolerates by design —
// sends the reload to a different store, Get reports a convoy that exists as
// missing, and a not-found is not a transient controller error, so the caller
// quarantines the control instead of retrying it.
//
// With no member stores configured this is the same single store.Get it has
// always been, including its error text.
func reloadDrainUnitConvoy(store beads.Store, unitConvoyID string, opts ProcessOptions) (beads.Bead, error) {
	if len(opts.MemberStores) == 0 {
		return store.Get(unitConvoyID)
	}
	var reloaded beads.Bead
	found := false
	err := walkDrainUnitConvoyLegs(store, opts, func(probe beads.Store) (bool, error) {
		bead, err := probe.Get(unitConvoyID)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				return false, nil
			}
			return false, err
		}
		reloaded, found = bead, true
		return true, nil
	})
	if err != nil {
		return beads.Bead{}, err
	}
	if !found {
		return beads.Bead{}, fmt.Errorf("reloading drain unit convoy %s: %w", unitConvoyID, beads.ErrNotFound)
	}
	return reloaded, nil
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
		if candidate.Metadata[beadmeta.MoleculeFailedMetadataKey] == "true" {
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
		return "", false, fmt.Errorf("%w: %s: drain item formula %q must declare the formulas v2 contract ([requires] formula_compiler = \">=2.0.0\")", errDrainInvalidItemFormula, control.ID, itemFormula)
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
		if root.Status == "closed" || root.Metadata[beadmeta.MoleculeFailedMetadataKey] != "true" {
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
	workDir := strings.TrimSpace(member.Metadata[beadmeta.WorkDirMetadataKey])
	if workDir == "" {
		workDir = strings.TrimSpace(member.Metadata[beadmeta.LegacyWorkDirMetadataKey])
	}
	if workDir != "" {
		for i := range recipe.Steps {
			step := &recipe.Steps[i]
			if step.Metadata == nil {
				step.Metadata = make(map[string]string)
			}
			step.Metadata[beadmeta.WorkDirMetadataKey] = workDir
			step.Metadata[beadmeta.LegacyWorkDirMetadataKey] = workDir
		}
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

// reserveDrainMember claims a member for exclusive drain access by stamping the
// reservation metadata on the member bead. The member is a work bead that may
// live in the work-class store rather than the primary graph store, so both the
// reservation read and write route to the member's owning store
// (drainMemberOwningStore). On origin/main the owning store is the single store,
// matching the pre-seam store.Get/store.SetMetadata behavior exactly.
func reserveDrainMember(store beads.Store, control, member beads.Bead, opts ProcessOptions) error {
	if drainMemberAccess(control) != beadmeta.DrainMemberAccessExclusive {
		return nil
	}
	memberStore, err := drainMemberOwningStore(store, member.ID, opts)
	if err != nil {
		return fmt.Errorf("%s: resolving exclusive drain member store for %s: %w", control.ID, member.ID, err)
	}
	current, err := memberStore.Get(member.ID)
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
	return claimDrainReservation(memberStore, control, member)
}

// claimDrainReservation claims the empty reservation slot. When the member's
// owning store resolves a conditional writer (beads.conditional_writes auto or
// require on a capable store), the claim is a value-CAS so two racing drains
// cannot both observe an empty owner and both stamp; otherwise it is the
// byte-identical legacy write. A require-mode refusal surfaces as-is — the
// drain fails closed rather than issuing an unconditional claim.
func claimDrainReservation(memberStore beads.Store, control, member beads.Bead) error {
	writer, _, err := beads.ResolveConditionalWriter(memberStore)
	if err != nil {
		return fmt.Errorf("%s: reserving drain member %s: %w", control.ID, member.ID, err)
	}
	if writer == nil {
		return memberStore.SetMetadata(member.ID, beadmeta.ExclusiveDrainReservationMetadataKey, control.ID)
	}
	return claimDrainReservationCAS(memberStore, writer, control, member)
}

// claimDrainReservationCAS fences the claim. A failed CAS is an observation,
// never a loss verdict by itself: the reservation value identifies its writer
// (control.ID), so the claim re-reads and re-decides — our own value means
// self-win (idempotent re-entry, or our own committed-but-unacknowledged
// write on an ambiguous transport error); a still-empty owner means a
// spurious conflict (a raced release, or cross-key revision interference on
// stores that emulate value-CAS over a whole-bead fence), re-issued once
// before surfacing; anything else is a genuine competing reservation.
func claimDrainReservationCAS(memberStore beads.Store, writer beads.ConditionalWriter, control, member beads.Bead) error {
	const claimAttempts = 2
	var lastErr error
	for attempt := 1; attempt <= claimAttempts; attempt++ {
		ok, casErr := writer.CompareAndSetMetadataKey(member.ID, beadmeta.ExclusiveDrainReservationMetadataKey, "", control.ID)
		if ok {
			return nil
		}
		lastErr = casErr
		current, getErr := memberStore.Get(member.ID)
		if getErr != nil {
			if casErr != nil {
				return fmt.Errorf("%s: reserving drain member %s: %w", control.ID, member.ID, casErr)
			}
			return fmt.Errorf("%s: re-reading drain member %s after conditional claim: %w", control.ID, member.ID, getErr)
		}
		switch owner := strings.TrimSpace(current.Metadata[beadmeta.ExclusiveDrainReservationMetadataKey]); {
		case owner == control.ID:
			// Self-win: the value is ours — an ambiguous transport error whose
			// write committed, or a concurrent re-entry of this same drain.
			return nil
		case owner != "":
			return drainReservationError{ControlID: control.ID, MemberID: member.ID, Owner: owner}
		}
		// Owner still empty: spurious conflict. A non-precondition error is
		// surfaced (transport/exhaustion — the level-triggered pass retries);
		// a precondition/value-loss gets one bounded re-issue.
		if casErr != nil && !beads.IsPreconditionFailed(casErr) {
			return fmt.Errorf("%s: reserving drain member %s: %w", control.ID, member.ID, casErr)
		}
	}
	if lastErr == nil {
		lastErr = errors.New("conditional claim kept losing with an empty owner")
	}
	return fmt.Errorf("%s: reserving drain member %s: %w", control.ID, member.ID, lastErr)
}

func reserveDrainMembers(store beads.Store, control beads.Bead, members []beads.Bead, opts ProcessOptions) error {
	for _, member := range members {
		if err := reserveDrainMember(store, control, member, opts); err != nil {
			return err
		}
	}
	return nil
}

func releaseDrainReservations(store beads.Store, controlID string, manifest drainManifest, opts ProcessOptions) error {
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
		memberStore, err := drainMemberOwningStore(store, memberID, opts)
		if err != nil {
			return fmt.Errorf("%s: resolving drain member store for %s: %w", controlID, memberID, err)
		}
		if err := releaseDrainReservation(memberStore, controlID, memberID); err != nil {
			return err
		}
	}
	return nil
}

// releaseDrainReservation clears this control's reservation on one member.
// The fenced form is symmetric with the claim: CAS(controlID → ""), and
// LOSING that CAS is the correct outcome — the member was already re-claimed
// by a successor drain, which is precisely the case where clearing it would
// clobber; the loss is never retried. The legacy form preserves the original
// read-verify-clear byte-for-byte.
func releaseDrainReservation(memberStore beads.Store, controlID, memberID string) error {
	writer, _, err := beads.ResolveConditionalWriter(memberStore)
	if err != nil {
		return err
	}
	if writer == nil {
		member, err := memberStore.Get(memberID)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				return nil
			}
			return fmt.Errorf("%s: loading drain member %s for reservation release: %w", controlID, memberID, err)
		}
		if strings.TrimSpace(member.Metadata[beadmeta.ExclusiveDrainReservationMetadataKey]) != controlID {
			return nil
		}
		if err := memberStore.SetMetadata(memberID, beadmeta.ExclusiveDrainReservationMetadataKey, ""); err != nil {
			return fmt.Errorf("%s: releasing drain reservation on %s: %w", controlID, memberID, err)
		}
		return nil
	}
	ok, casErr := writer.CompareAndSetMetadataKey(memberID, beadmeta.ExclusiveDrainReservationMetadataKey, controlID, "")
	if ok {
		return nil
	}
	if casErr == nil || beads.IsPreconditionFailed(casErr) {
		// Value loss or revision conflict: we no longer own the slot (already
		// cleared, or a successor re-claimed it). Clearing now would clobber —
		// the loss IS the release goal being moot.
		return nil
	}
	if errors.Is(casErr, beads.ErrNotFound) {
		return nil
	}
	// Ambiguous transport errors may have committed our clear: verify before
	// surfacing (§9.3 — never conclude from the error alone).
	if member, getErr := memberStore.Get(memberID); getErr == nil {
		if strings.TrimSpace(member.Metadata[beadmeta.ExclusiveDrainReservationMetadataKey]) != controlID {
			return nil
		}
	} else if errors.Is(getErr, beads.ErrNotFound) {
		return nil
	}
	return fmt.Errorf("%s: releasing drain reservation on %s: %w", controlID, memberID, casErr)
}

// retryableDrainReservationError reports whether a reservation failure is a
// level-triggered re-entry class rather than a terminal drain disposition.
// Conditional-write contention (bounded-CAS exhaustion), a runtime capability
// latch (the next resolve degrades under auto), and transport-transient store
// errors all heal on a later pass. A genuine competing owner
// (drainReservationError) and a require-mode policy refusal stay terminal —
// the first is the drain's designed skip/fail outcome, the second is
// fail-closed by contract.
func retryableDrainReservationError(err error) bool {
	var re drainReservationError
	if errors.As(err, &re) {
		return false
	}
	if beads.IsConditionalWritesRequired(err) {
		return false
	}
	return beads.IsCASRetriesExhausted(err) || beads.IsConditionalWriteUnsupported(err) || IsTransientControllerError(err)
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
	if releaseErr := releaseDrainReservations(store, bead.ID, manifest, opts); releaseErr != nil {
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
	if releaseErr := releaseDrainReservations(store, bead.ID, manifest, opts); releaseErr != nil {
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

// reloadDrain re-reads the drain control bead so completeDrain sees the freshly
// persisted post-expansion state. On a read error it returns the error rather
// than the stale pre-transition bead, so the caller can retry next tick instead
// of completing the drain against a stale snapshot.
func reloadDrain(store beads.Store, bead beads.Bead) (beads.Bead, error) {
	reloaded, err := store.Get(bead.ID)
	if err != nil {
		return beads.Bead{}, fmt.Errorf("%s: reloading drain before completion: %w", bead.ID, err)
	}
	return reloaded, nil
}
