package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// isPoolSlotWorkDirRoot reports whether path is shaped exactly like a pool
// slot's own worktree root (.gc/worktrees/<rig>/<role>) -- a session-slot
// label, not evidence that a bead owns a real per-bead worktree. It matches
// both the city-relative form worktree-per-bead dispatch normally stores
// (see resolveWorkDirAgainstCity) and the equivalent absolute form (legacy
// convention): the match is on the LAST FOUR path segments only, so a
// deeper per-bead path nested under a pool slot (.gc/worktrees/<rig>/<role>/<slug>)
// or a differently-rooted worktree (worktrees/<bead-id>, .claude/worktrees/...)
// correctly does not match.
func isPoolSlotWorkDirRoot(path string) bool {
	var segments []string
	for _, s := range strings.Split(path, "/") {
		if s != "" {
			segments = append(segments, s)
		}
	}
	if len(segments) < 4 {
		return false
	}
	last4 := segments[len(segments)-4:]
	return last4[0] == ".gc" && last4[1] == "worktrees"
}

// workDirStampWouldClobberEvidence reports whether stamping workDir onto a
// work (or molecule root) bead currently holding metadata would overwrite
// genuine worktree evidence with a pool-slot label. It keys off the SHAPE of
// the canonical value already on the bead and the shape of the incoming
// value, rather than the executing session's self-reported pool_managed
// status -- closing the gap where a session physically running from a pool
// slot but whose own session bead never carries pool_managed=true could
// otherwise bypass workDirStampHasOwnershipEvidence entirely.
func workDirStampWouldClobberEvidence(metadata map[string]string, workDir string) bool {
	existing := metadata[beadmeta.WorkDirMetadataKey]
	if existing == "" {
		return false
	}
	if isPoolSlotWorkDirRoot(existing) {
		return false
	}
	return isPoolSlotWorkDirRoot(workDir)
}

// poolSlotWorkDirRepair describes a one-shot repair for a bead whose
// canonical gc.work_dir was clobbered with a pool-slot label while its
// legacy work_dir still carries the real per-bead worktree evidence.
type poolSlotWorkDirRepair struct {
	// RestoreValue is the legacy work_dir value gc.work_dir should be reset to.
	RestoreValue string
}

// poolSlotWorkDirRepairFor reports the repair needed for bead b, or nil if
// none is needed: both gc.work_dir and work_dir must be set and unequal, and
// gc.work_dir must match a pool-slot root exactly (isPoolSlotWorkDirRoot) --
// other shapes (e.g. a legacy work_dir pointing into .claude/worktrees) are
// left untouched rather than blanket-copied.
func poolSlotWorkDirRepairFor(b beads.Bead) *poolSlotWorkDirRepair {
	if b.Metadata == nil {
		return nil
	}
	canonical, hasCanonical := b.Metadata[beadmeta.WorkDirMetadataKey]
	legacy, hasLegacy := b.Metadata[beadmeta.LegacyWorkDirMetadataKey]
	if !hasCanonical || !hasLegacy || canonical == legacy {
		return nil
	}
	if !isPoolSlotWorkDirRoot(canonical) {
		return nil
	}
	return &poolSlotWorkDirRepair{RestoreValue: legacy}
}

// repairPoolSlotWorkDirClobber is the one-shot repair sweep for beads whose
// canonical gc.work_dir was already clobbered with a pool-slot label by a
// reconciler tick that predates workDirStampWouldClobberEvidence. Callers
// pass it both the assigned (in_progress) and unassigned-routed (open) work
// collections: poolSlotWorkDirRepairFor does not consult Status, and this
// sweep deliberately does not add a status filter of its own, so a bead
// released back to open by a drain is repaired exactly like one still in
// progress -- gating on status here would leave one of the two shapes found
// in the wild across gascity/beads/BEADS permanently unrepaired.
//
// Idempotent by design: once gc.work_dir is restored to the legacy value the
// two keys are equal and poolSlotWorkDirRepairFor returns nil, so
// steady-state reconciles after the first repair perform no writes. A write
// failure is logged and skipped -- recovery is best-effort and must never
// block reconciliation.
func repairPoolSlotWorkDirClobber(workBeads []beads.Bead, workStores []beads.Store, stderr io.Writer) {
	if len(workBeads) != len(workStores) {
		return
	}
	for i, wb := range workBeads {
		store := workStores[i]
		if store == nil {
			continue
		}
		repair := poolSlotWorkDirRepairFor(wb)
		if repair == nil {
			continue
		}
		patch := map[string]string{beadmeta.WorkDirMetadataKey: repair.RestoreValue}
		if err := store.SetMetadataBatch(wb.ID, patch); err != nil && stderr != nil {
			fmt.Fprintf(stderr, "repairPoolSlotWorkDirClobber: %s: %v\n", wb.ID, err) //nolint:errcheck
		}
	}
}
