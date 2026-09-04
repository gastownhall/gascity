package main

import (
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
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
		"missing role segment": {
			path: ".gc/worktrees/gascity",
			want: false,
		},
		"per-bead worktree nested inside a pool slot": {
			path: ".gc/worktrees/gascity/builder-1/ga-3c5isi",
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
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := isPoolSlotWorkDirRoot(tc.path); got != tc.want {
				t.Errorf("isPoolSlotWorkDirRoot(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestWorkDirStampWouldClobberEvidence(t *testing.T) {
	const (
		realEvidence      = "/home/ds/gascity-worktrees/ga-3c5isi"
		otherRealEvidence = "/home/ds/gascity-worktrees/ga-other"
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
		"absent to pool slot label is fine": {
			metadata: map[string]string{},
			workDir:  poolSlot,
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
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := workDirStampWouldClobberEvidence(tc.metadata, tc.workDir); got != tc.want {
				t.Errorf("workDirStampWouldClobberEvidence(%v, %q) = %v, want %v", tc.metadata, tc.workDir, got, tc.want)
			}
		})
	}
}

func TestPoolSlotWorkDirRepairFor(t *testing.T) {
	const (
		poolSlot       = ".gc/worktrees/gascity/builder-1"
		realEvidence   = "/home/ds/gascity-worktrees/ga-3c5isi"
		claudeWorktree = ".claude/worktrees/ga-45tz5p"
	)
	cases := map[string]struct {
		bead beads.Bead
		want *poolSlotWorkDirRepair
	}{
		"pool-slot canonical with differing legacy needs repair": {
			bead: beads.Bead{Metadata: map[string]string{
				beadmeta.WorkDirMetadataKey:       poolSlot,
				beadmeta.LegacyWorkDirMetadataKey: realEvidence,
			}},
			want: &poolSlotWorkDirRepair{RestoreValue: realEvidence},
		},
		"already equal needs no repair": {
			bead: beads.Bead{Metadata: map[string]string{
				beadmeta.WorkDirMetadataKey:       realEvidence,
				beadmeta.LegacyWorkDirMetadataKey: realEvidence,
			}},
			want: nil,
		},
		"canonical only needs no repair": {
			bead: beads.Bead{Metadata: map[string]string{
				beadmeta.WorkDirMetadataKey: poolSlot,
			}},
			want: nil,
		},
		"legacy only needs no repair": {
			bead: beads.Bead{Metadata: map[string]string{
				beadmeta.LegacyWorkDirMetadataKey: realEvidence,
			}},
			want: nil,
		},
		"canonical unequal but not pool-slot shaped (ga-45tz5p) is excluded": {
			bead: beads.Bead{Metadata: map[string]string{
				beadmeta.WorkDirMetadataKey:       claudeWorktree,
				beadmeta.LegacyWorkDirMetadataKey: realEvidence,
			}},
			want: nil,
		},
		"nil metadata needs no repair": {
			bead: beads.Bead{},
			want: nil,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := poolSlotWorkDirRepairFor(tc.bead)
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
		poolSlot     = ".gc/worktrees/gascity/builder-1"
		realEvidence = "/home/ds/gascity-worktrees/ga-3c5isi"
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
	excluded := beads.Bead{
		ID: "ga-45tz5p", Type: "task", Status: "open",
		Metadata: map[string]string{
			beadmeta.WorkDirMetadataKey:       ".claude/worktrees/ga-45tz5p",
			beadmeta.LegacyWorkDirMetadataKey: realEvidence,
		},
	}
	mem := beads.NewMemStoreFrom(0, []beads.Bead{clobbered, clean, excluded}, nil)
	store := &countingStore{Store: mem}
	all := []beads.Bead{clobbered, clean, excluded}
	stores := []beads.Store{store, store, store}

	repairPoolSlotWorkDirClobber(all, stores, io.Discard)

	got, err := mem.Get("ga-clobbered")
	if err != nil {
		t.Fatalf("Get(ga-clobbered): %v", err)
	}
	if got.Metadata[beadmeta.WorkDirMetadataKey] != realEvidence {
		t.Errorf("gc.work_dir = %q, want %q (repaired from legacy work_dir)", got.Metadata[beadmeta.WorkDirMetadataKey], realEvidence)
	}

	gotExcluded, err := mem.Get("ga-45tz5p")
	if err != nil {
		t.Fatalf("Get(ga-45tz5p): %v", err)
	}
	if gotExcluded.Metadata[beadmeta.WorkDirMetadataKey] != ".claude/worktrees/ga-45tz5p" {
		t.Errorf("gc.work_dir = %q, want unchanged (non-pool-slot shape must not be blanket-copied)", gotExcluded.Metadata[beadmeta.WorkDirMetadataKey])
	}

	// Idempotent: a second pass over the now-repaired bead writes nothing --
	// the "one-shot" contract achieved via steady-state convergence rather
	// than tracked migration-run state.
	repaired, _ := mem.Get("ga-clobbered")
	store.writes = 0
	repairPoolSlotWorkDirClobber([]beads.Bead{repaired, clean, gotExcluded}, stores, io.Discard)
	if store.writes != 0 {
		t.Errorf("second pass wrote %d times, want 0 (repair must be one-shot/idempotent)", store.writes)
	}
}
