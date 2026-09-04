package main

import (
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
