package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// isPoolSlotWorkDirRoot reports whether path is shaped exactly like a pool
// slot's own worktree root (.gc/worktrees/<rig>/<slot>) -- a session-slot
// label, not evidence that a bead owns a real per-bead worktree. It matches
// both the city-relative form worktree-per-bead dispatch normally stores
// (see resolveWorkDirAgainstCity) and the equivalent absolute form (legacy
// convention).
//
// Shape alone is not sufficient: per-bead worktrees are SIBLINGS of pool
// slots under .gc/worktrees/<rig>/ (the same layout bead_worktree_reaper.go
// guards with sessionHomes), so .gc/worktrees/gascity/ga-ui3mbs sits at the
// exact depth a slot does. The final segment is therefore checked against the
// city's configured bead prefixes: a name that resolves to a bead ID
// ("ga-ui3mbs", "ga-klo4gz.11-measure") is a per-bead worktree, not a slot,
// while "builder-1" / "builder-feature-branch" have no configured prefix and
// stay slots.
//
// The match is on the LAST FOUR path segments only, so a deeper per-bead path
// nested under a pool slot (.gc/worktrees/<rig>/<slot>/<slug>) or a
// differently-rooted worktree (worktrees/<bead-id>, .claude/worktrees/...)
// correctly does not match. Deeper slot layouts
// (.gc/worktrees/<rig>/<pool>/<slot>) likewise do not match; both the guard
// and the repair are simply inert there, which fails safe.
func isPoolSlotWorkDirRoot(cfg *config.City, path string) bool {
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
	if last4[0] != ".gc" || last4[1] != "worktrees" {
		return false
	}
	return extractBeadIDFromWorktreeName(cfg, last4[3]) == ""
}

// workDirStampWouldClobberEvidence reports whether stamping workDir onto a
// work (or molecule root) bead currently holding metadata would overwrite
// genuine worktree evidence with a pool-slot label. It keys off the SHAPE of
// the canonical value already on the bead and the shape of the incoming
// value, rather than the executing session's self-reported pool_managed
// status -- closing the gap where a session physically running from a pool
// slot but whose own session bead never carries pool_managed=true could
// otherwise bypass workDirStampHasOwnershipEvidence entirely.
//
// When the canonical key is absent but the legacy key holds real, differing
// evidence, a slot-shaped stamp is refused too: writing it manufactures
// exactly the canonical/legacy disagreement worktreeSpecForBead fails closed
// on, starving the bead instead of leaving it resolvable from legacy alone.
//
// Deliberately asymmetric with poolSlotWorkDirRepairFor on a nil cfg: without
// config, isPoolSlotWorkDirRoot cannot recognize a per-bead name and errs
// toward "is a slot". Here that only SUPPRESSES a write, which is harmless.
// In the repair direction it would overwrite an accurate canonical, so
// poolSlotWorkDirRepairFor bails out instead. Do not "simplify" the two into
// a shared nil-cfg policy.
func workDirStampWouldClobberEvidence(cfg *config.City, metadata map[string]string, workDir string) bool {
	canonical := strings.TrimSpace(metadata[beadmeta.WorkDirMetadataKey])
	legacy := strings.TrimSpace(metadata[beadmeta.LegacyWorkDirMetadataKey])
	if canonical == "" {
		return legacy != "" && legacy != workDir && isPoolSlotWorkDirRoot(cfg, workDir)
	}
	if isPoolSlotWorkDirRoot(cfg, canonical) {
		return false
	}
	return isPoolSlotWorkDirRoot(cfg, workDir)
}

// poolSlotWorkDirRepair describes a one-shot repair for a bead whose
// canonical gc.work_dir was clobbered with a pool-slot label while its
// legacy work_dir still carries the real per-bead worktree evidence.
type poolSlotWorkDirRepair struct {
	// RestoreValue is the legacy work_dir value gc.work_dir should be reset to.
	RestoreValue string
}

// poolSlotWorkDirRepairFor reports the repair needed for bead b, or nil if
// none is needed: both gc.work_dir and work_dir must be non-blank and
// unequal, and gc.work_dir must match a pool-slot root exactly
// (isPoolSlotWorkDirRoot) -- other shapes (e.g. a legacy work_dir pointing
// into .claude/worktrees) are left untouched rather than blanket-copied.
//
// A nil cfg skips the repair entirely. This direction WRITES, and without
// config isPoolSlotWorkDirRoot cannot tell a per-bead worktree from a slot;
// proceeding on shape alone would overwrite an accurate canonical with a
// stale legacy path. See the note on workDirStampWouldClobberEvidence, which
// keeps shape-only behavior because erring there only suppresses a write.
func poolSlotWorkDirRepairFor(cfg *config.City, b beads.Bead) *poolSlotWorkDirRepair {
	if cfg == nil || b.Metadata == nil {
		return nil
	}
	canonical := strings.TrimSpace(b.Metadata[beadmeta.WorkDirMetadataKey])
	legacy := strings.TrimSpace(b.Metadata[beadmeta.LegacyWorkDirMetadataKey])
	if canonical == "" || legacy == "" || canonical == legacy {
		return nil
	}
	if !isPoolSlotWorkDirRoot(cfg, canonical) {
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
// Run it BEFORE stampRunSessionIdentity on the same snapshot: the stamp
// writes through the store without refreshing the in-memory slice, so a
// sweep placed after it reads pre-stamp metadata and can undo a legitimate
// fresh stamp. Repair first, live stamp last.
//
// Idempotent by design: once gc.work_dir is restored to the legacy value the
// two keys are equal and poolSlotWorkDirRepairFor returns nil, so
// steady-state reconciles after the first repair perform no writes. A write
// failure is logged and skipped -- recovery is best-effort and must never
// block reconciliation.
func repairPoolSlotWorkDirClobber(cfg *config.City, workBeads []beads.Bead, workStores []beads.Store, stderr io.Writer) {
	if len(workBeads) != len(workStores) {
		return
	}
	for i, wb := range workBeads {
		store := workStores[i]
		if store == nil {
			continue
		}
		repair := poolSlotWorkDirRepairFor(cfg, wb)
		if repair == nil {
			continue
		}
		patch := map[string]string{beadmeta.WorkDirMetadataKey: repair.RestoreValue}
		if err := store.SetMetadataBatch(wb.ID, patch); err != nil && stderr != nil {
			fmt.Fprintf(stderr, "repairPoolSlotWorkDirClobber: %s: %v\n", wb.ID, err) //nolint:errcheck
		}
	}
}
