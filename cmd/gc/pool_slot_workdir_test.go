package main

import (
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestIsPoolSlotWorkDirRoot(t *testing.T) {
	cases := map[string]struct {
		path string
		want bool
	}{
		"relative exact pool slot root": {
			path: ".gc/worktrees/gascity/builder-1",
			want: true,
		},
		"absolute exact pool slot root": {
			path: "/home/jaword/projects/gc-management/.gc/worktrees/gascity/builder-1",
			want: true,
		},
		"slot name with no configured bead prefix stays a slot": {
			path: ".gc/worktrees/gascity/builder-feature-branch",
			want: true,
		},
		"missing role segment": {
			path: ".gc/worktrees/gascity",
			want: false,
		},
		// Per-bead worktrees are SIBLINGS of pool slots at this exact depth in
		// the default city layout, so shape alone cannot tell them apart. The
		// configured-prefix check must reject these as slots, or the repair
		// sweep overwrites their accurate canonical work_dir.
		"bare per-bead worktree at slot depth": {
			path: ".gc/worktrees/gascity/ga-ui3mbs",
			want: false,
		},
		"absolute bare per-bead worktree at slot depth": {
			path: "/home/jaword/projects/gc-management/.gc/worktrees/gascity/ga-ui3mbs",
			want: false,
		},
		"compound per-bead worktree at slot depth": {
			path: ".gc/worktrees/gascity/ga-klo4gz.11-measure",
			want: false,
		},
		"hierarchical per-bead worktree at slot depth": {
			path: ".gc/worktrees/gascity/ga-lvrcyp.3.1-post-init-retry",
			want: false,
		},
		"per-bead worktree nested inside a pool slot": {
			path: ".gc/worktrees/gascity/builder-1/ga-3c5isi",
			want: false,
		},
		// Deeper slot layouts do not match the last-four check; guard and
		// repair are both inert there, which fails safe.
		"deeper pool slot layout is inert": {
			path: ".gc/worktrees/frontend/polecats/polecat-1",
			want: false,
		},
		"legacy per-bead worktree path": {
			path: "worktrees/ga-3c5isi",
			want: false,
		},
		"claude worktrees shape (ga-45tz5p exclusion)": {
			path: ".claude/worktrees/ga-45tz5p",
			want: false,
		},
		"empty": {
			path: "",
			want: false,
		},
	}
	cfg := gaConfig()
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := isPoolSlotWorkDirRoot(cfg, tc.path); got != tc.want {
				t.Errorf("isPoolSlotWorkDirRoot(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestWorkDirStampWouldClobberEvidence(t *testing.T) {
	const (
		realEvidence      = "/home/ds/gascity-worktrees/ga-3c5isi"
		otherRealEvidence = "/home/ds/gascity-worktrees/ga-other"
		perBeadAtSlot     = ".gc/worktrees/gascity/ga-ui3mbs"
		poolSlot          = ".gc/worktrees/gascity/builder-1"
		otherPoolSlot     = ".gc/worktrees/gascity/builder-2"
	)
	cases := map[string]struct {
		metadata map[string]string
		workDir  string
		want     bool
	}{
		"real evidence to pool slot label clobbers": {
			metadata: map[string]string{beadmeta.WorkDirMetadataKey: realEvidence},
			workDir:  poolSlot,
			want:     true,
		},
		// A per-bead worktree that happens to sit at slot depth is real
		// evidence, not a slot label: it must be protected like any other.
		"per-bead canonical at slot depth to pool slot label clobbers": {
			metadata: map[string]string{beadmeta.WorkDirMetadataKey: perBeadAtSlot},
			workDir:  poolSlot,
			want:     true,
		},
		"absent to pool slot label is fine": {
			metadata: map[string]string{},
			workDir:  poolSlot,
			want:     false,
		},
		// Canonical absent but legacy holding real, differing evidence:
		// stamping a slot label here manufactures the canonical/legacy
		// disagreement worktreeSpecForBead fails closed on.
		"absent canonical with differing real legacy clobbers": {
			metadata: map[string]string{beadmeta.LegacyWorkDirMetadataKey: realEvidence},
			workDir:  poolSlot,
			want:     true,
		},
		"whitespace-only canonical is treated as absent": {
			metadata: map[string]string{
				beadmeta.WorkDirMetadataKey:       "   ",
				beadmeta.LegacyWorkDirMetadataKey: realEvidence,
			},
			workDir: poolSlot,
			want:    true,
		},
		"absent canonical with legacy mirroring the incoming value is fine": {
			metadata: map[string]string{beadmeta.LegacyWorkDirMetadataKey: poolSlot},
			workDir:  poolSlot,
			want:     false,
		},
		"absent canonical with real legacy but non-slot incoming is fine": {
			metadata: map[string]string{beadmeta.LegacyWorkDirMetadataKey: realEvidence},
			workDir:  otherRealEvidence,
			want:     false,
		},
		"pool slot to different pool slot is fine": {
			metadata: map[string]string{beadmeta.WorkDirMetadataKey: poolSlot},
			workDir:  otherPoolSlot,
			want:     false,
		},
		"pool slot to real evidence is fine": {
			metadata: map[string]string{beadmeta.WorkDirMetadataKey: poolSlot},
			workDir:  realEvidence,
			want:     false,
		},
		"real evidence to different real evidence is fine": {
			metadata: map[string]string{beadmeta.WorkDirMetadataKey: realEvidence},
			workDir:  otherRealEvidence,
			want:     false,
		},
	}
	cfg := gaConfig()
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := workDirStampWouldClobberEvidence(cfg, tc.metadata, tc.workDir); got != tc.want {
				t.Errorf("workDirStampWouldClobberEvidence(%v, %q) = %v, want %v", tc.metadata, tc.workDir, got, tc.want)
			}
		})
	}
}

func TestPoolSlotWorkDirRepairFor(t *testing.T) {
	const (
		poolSlot       = ".gc/worktrees/gascity/builder-1"
		otherPoolSlot  = ".gc/worktrees/gascity/builder-2"
		perBeadAtSlot  = ".gc/worktrees/gascity/ga-ui3mbs"
		compoundAtSlot = ".gc/worktrees/gascity/ga-klo4gz.11-measure"
		realEvidence   = "/home/ds/gascity-worktrees/ga-3c5isi"
		staleLegacy    = "/home/ds/gascity-worktrees/ga-stale"
		claudeWorktree = ".claude/worktrees/ga-45tz5p"
	)
	cases := map[string]struct {
		cfg  *config.City
		bead beads.Bead
		want *poolSlotWorkDirRepair
	}{
		"pool-slot canonical with differing legacy needs repair": {
			cfg: gaConfig(),
			bead: beads.Bead{Metadata: map[string]string{
				beadmeta.WorkDirMetadataKey:       poolSlot,
				beadmeta.LegacyWorkDirMetadataKey: realEvidence,
			}},
			want: &poolSlotWorkDirRepair{RestoreValue: realEvidence},
		},
		// The repair's premise is that legacy holds real per-bead evidence. A
		// slot-shaped legacy is not that, so promoting it would only swap one
		// slot label for another -- and on an open bead nothing follows the
		// sweep to correct it.
		"differing pool-slot legacy is left untouched": {
			cfg: gaConfig(),
			bead: beads.Bead{Metadata: map[string]string{
				beadmeta.WorkDirMetadataKey:       poolSlot,
				beadmeta.LegacyWorkDirMetadataKey: otherPoolSlot,
			}},
			want: nil,
		},
		// The accurate canonical of a per-bead worktree living at slot depth
		// must survive: repairing it would relocate a live bead to a stale
		// legacy path.
		"bare per-bead canonical at slot depth is left untouched": {
			cfg: gaConfig(),
			bead: beads.Bead{Metadata: map[string]string{
				beadmeta.WorkDirMetadataKey:       perBeadAtSlot,
				beadmeta.LegacyWorkDirMetadataKey: staleLegacy,
			}},
			want: nil,
		},
		"compound per-bead canonical at slot depth is left untouched": {
			cfg: gaConfig(),
			bead: beads.Bead{Metadata: map[string]string{
				beadmeta.WorkDirMetadataKey:       compoundAtSlot,
				beadmeta.LegacyWorkDirMetadataKey: staleLegacy,
			}},
			want: nil,
		},
		// Without config the classifier cannot tell a per-bead worktree from a
		// slot, and this direction WRITES -- so it must skip entirely rather
		// than fall back to shape-only matching.
		"nil cfg performs no repair": {
			cfg: nil,
			bead: beads.Bead{Metadata: map[string]string{
				beadmeta.WorkDirMetadataKey:       poolSlot,
				beadmeta.LegacyWorkDirMetadataKey: realEvidence,
			}},
			want: nil,
		},
		"already equal needs no repair": {
			cfg: gaConfig(),
			bead: beads.Bead{Metadata: map[string]string{
				beadmeta.WorkDirMetadataKey:       realEvidence,
				beadmeta.LegacyWorkDirMetadataKey: realEvidence,
			}},
			want: nil,
		},
		"equal after trimming needs no repair": {
			cfg: gaConfig(),
			bead: beads.Bead{Metadata: map[string]string{
				beadmeta.WorkDirMetadataKey:       realEvidence,
				beadmeta.LegacyWorkDirMetadataKey: "  " + realEvidence + "  ",
			}},
			want: nil,
		},
		"whitespace-only legacy is treated as absent": {
			cfg: gaConfig(),
			bead: beads.Bead{Metadata: map[string]string{
				beadmeta.WorkDirMetadataKey:       poolSlot,
				beadmeta.LegacyWorkDirMetadataKey: "   ",
			}},
			want: nil,
		},
		"whitespace-only canonical is treated as absent": {
			cfg: gaConfig(),
			bead: beads.Bead{Metadata: map[string]string{
				beadmeta.WorkDirMetadataKey:       " ",
				beadmeta.LegacyWorkDirMetadataKey: realEvidence,
			}},
			want: nil,
		},
		"canonical only needs no repair": {
			cfg: gaConfig(),
			bead: beads.Bead{Metadata: map[string]string{
				beadmeta.WorkDirMetadataKey: poolSlot,
			}},
			want: nil,
		},
		"legacy only needs no repair": {
			cfg: gaConfig(),
			bead: beads.Bead{Metadata: map[string]string{
				beadmeta.LegacyWorkDirMetadataKey: realEvidence,
			}},
			want: nil,
		},
		"canonical unequal but not pool-slot shaped (ga-45tz5p) is excluded": {
			cfg: gaConfig(),
			bead: beads.Bead{Metadata: map[string]string{
				beadmeta.WorkDirMetadataKey:       claudeWorktree,
				beadmeta.LegacyWorkDirMetadataKey: realEvidence,
			}},
			want: nil,
		},
		"nil metadata needs no repair": {
			cfg:  gaConfig(),
			bead: beads.Bead{},
			want: nil,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := poolSlotWorkDirRepairFor(tc.cfg, tc.bead)
			if (got == nil) != (tc.want == nil) {
				t.Fatalf("poolSlotWorkDirRepairFor() = %v, want %v", got, tc.want)
			}
			if got != nil && got.RestoreValue != tc.want.RestoreValue {
				t.Errorf("RestoreValue = %q, want %q", got.RestoreValue, tc.want.RestoreValue)
			}
		})
	}
}

// TestRepairPoolSlotWorkDirClobber exercises the one-shot repair sweep (the
// active driver for poolSlotWorkDirRepairFor) rather than the pure decision
// function alone: ga-3c5isi's exit_contract requires beads already clobbered
// -- by a prior reconciler tick, before workDirStampWouldClobberEvidence
// existed -- to be actively restored, not merely protected from future
// clobbers. Status-agnostic by design: mayor's manual sweep found latent
// victims on OPEN beads (released back to open by a drain) as well as
// in_progress ones, so this sweep must not gate on bead status.
func TestRepairPoolSlotWorkDirClobber(t *testing.T) {
	const (
		poolSlot      = ".gc/worktrees/gascity/builder-1"
		otherPoolSlot = ".gc/worktrees/gascity/builder-2"
		perBeadAtSlot = ".gc/worktrees/gascity/ga-ui3mbs"
		compoundSlot  = ".gc/worktrees/gascity/ga-klo4gz.11-measure"
		realEvidence  = "/home/ds/gascity-worktrees/ga-3c5isi"
		staleLegacy   = "/home/ds/gascity-worktrees/ga-stale"
	)
	clobbered := beads.Bead{
		ID: "ga-clobbered", Type: "task", Status: "open",
		Metadata: map[string]string{
			beadmeta.WorkDirMetadataKey:       poolSlot,
			beadmeta.LegacyWorkDirMetadataKey: realEvidence,
		},
	}
	clean := beads.Bead{
		ID: "ga-clean", Type: "task", Status: "in_progress",
		Metadata: map[string]string{
			beadmeta.WorkDirMetadataKey:       realEvidence,
			beadmeta.LegacyWorkDirMetadataKey: realEvidence,
		},
	}
	// A per-bead worktree sitting at the same depth as a pool slot: its
	// canonical value is accurate and must not be reverted to a stale legacy.
	perBead := beads.Bead{
		ID: "ga-ui3mbs", Type: "task", Status: "in_progress",
		Metadata: map[string]string{
			beadmeta.WorkDirMetadataKey:       perBeadAtSlot,
			beadmeta.LegacyWorkDirMetadataKey: staleLegacy,
		},
	}
	compound := beads.Bead{
		ID: "ga-klo4gz.11", Type: "task", Status: "open",
		Metadata: map[string]string{
			beadmeta.WorkDirMetadataKey:       compoundSlot,
			beadmeta.LegacyWorkDirMetadataKey: staleLegacy,
		},
	}
	excluded := beads.Bead{
		ID: "ga-45tz5p", Type: "task", Status: "open",
		Metadata: map[string]string{
			beadmeta.WorkDirMetadataKey:       ".claude/worktrees/ga-45tz5p",
			beadmeta.LegacyWorkDirMetadataKey: realEvidence,
		},
	}
	// Both halves slot-shaped: the sweep has no real evidence to restore, so
	// it must leave the bead alone rather than promote the other slot label.
	slotToSlot := beads.Bead{
		ID: "ga-slotslot", Type: "task", Status: "open",
		Metadata: map[string]string{
			beadmeta.WorkDirMetadataKey:       poolSlot,
			beadmeta.LegacyWorkDirMetadataKey: otherPoolSlot,
		},
	}
	all := []beads.Bead{clobbered, clean, perBead, compound, excluded, slotToSlot}
	mem := beads.NewMemStoreFrom(0, all, nil)
	store := &countingStore{Store: mem}
	stores := []beads.Store{store, store, store, store, store, store}
	cfg := gaConfig()

	repairPoolSlotWorkDirClobber(cfg, all, stores, io.Discard)

	got, err := mem.Get("ga-clobbered")
	if err != nil {
		t.Fatalf("Get(ga-clobbered): %v", err)
	}
	if got.Metadata[beadmeta.WorkDirMetadataKey] != realEvidence {
		t.Errorf("gc.work_dir = %q, want %q (repaired from legacy work_dir)", got.Metadata[beadmeta.WorkDirMetadataKey], realEvidence)
	}

	for _, tc := range []struct {
		id   string
		want string
	}{
		{"ga-ui3mbs", perBeadAtSlot},
		{"ga-klo4gz.11", compoundSlot},
		{"ga-45tz5p", ".claude/worktrees/ga-45tz5p"},
		{"ga-slotslot", poolSlot},
	} {
		gotUntouched, err := mem.Get(tc.id)
		if err != nil {
			t.Fatalf("Get(%s): %v", tc.id, err)
		}
		if gotUntouched.Metadata[beadmeta.WorkDirMetadataKey] != tc.want {
			t.Errorf("%s gc.work_dir = %q, want %q (accurate canonical must not be reverted to a stale legacy)",
				tc.id, gotUntouched.Metadata[beadmeta.WorkDirMetadataKey], tc.want)
		}
	}

	// Idempotent: a second pass over the now-repaired beads writes nothing --
	// the "one-shot" contract achieved via steady-state convergence rather
	// than tracked migration-run state.
	var second []beads.Bead
	for _, b := range all {
		refreshed, err := mem.Get(b.ID)
		if err != nil {
			t.Fatalf("Get(%s): %v", b.ID, err)
		}
		second = append(second, refreshed)
	}
	store.writes = 0
	repairPoolSlotWorkDirClobber(cfg, second, stores, io.Discard)
	if store.writes != 0 {
		t.Errorf("second pass wrote %d times, want 0 (repair must be one-shot/idempotent)", store.writes)
	}
}

// TestRepairPoolSlotWorkDirClobberNilConfigIsInert pins the asymmetry
// documented on poolSlotWorkDirRepairFor: the repair direction writes, so
// without config it must skip rather than fall back to shape-only matching
// and overwrite an accurate canonical.
func TestRepairPoolSlotWorkDirClobberNilConfigIsInert(t *testing.T) {
	const (
		perBeadAtSlot = ".gc/worktrees/gascity/ga-ui3mbs"
		staleLegacy   = "/home/ds/gascity-worktrees/ga-stale"
	)
	perBead := beads.Bead{
		ID: "ga-ui3mbs", Type: "task", Status: "in_progress",
		Metadata: map[string]string{
			beadmeta.WorkDirMetadataKey:       perBeadAtSlot,
			beadmeta.LegacyWorkDirMetadataKey: staleLegacy,
		},
	}
	mem := beads.NewMemStoreFrom(0, []beads.Bead{perBead}, nil)
	store := &countingStore{Store: mem}

	repairPoolSlotWorkDirClobber(nil, []beads.Bead{perBead}, []beads.Store{store}, io.Discard)

	if store.writes != 0 {
		t.Errorf("nil cfg wrote %d times, want 0", store.writes)
	}
	got, err := mem.Get(perBead.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", perBead.ID, err)
	}
	if got.Metadata[beadmeta.WorkDirMetadataKey] != perBeadAtSlot {
		t.Errorf("gc.work_dir = %q, want %q (nil cfg must not repair)", got.Metadata[beadmeta.WorkDirMetadataKey], perBeadAtSlot)
	}
}
