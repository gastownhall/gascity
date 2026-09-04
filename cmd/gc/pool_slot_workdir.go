package main

import (
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
func isPoolSlotWorkDirRoot(_ string) bool {
	return false
}

// workDirStampWouldClobberEvidence reports whether stamping workDir onto a
// work (or molecule root) bead currently holding metadata would overwrite
// genuine worktree evidence with a pool-slot label. It keys off the SHAPE of
// the canonical value already on the bead and the shape of the incoming
// value, rather than the executing session's self-reported pool_managed
// status -- closing the gap where a session physically running from a pool
// slot but whose own session bead never carries pool_managed=true could
// otherwise bypass workDirStampHasOwnershipEvidence entirely.
func workDirStampWouldClobberEvidence(_ map[string]string, _ string) bool {
	return false
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
func poolSlotWorkDirRepairFor(_ beads.Bead) *poolSlotWorkDirRepair {
	return nil
}
