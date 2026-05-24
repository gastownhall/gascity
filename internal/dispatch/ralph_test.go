package dispatch

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestFormatGateExitCode(t *testing.T) {
	t.Parallel()

	intPtr := func(v int) *int {
		return &v
	}

	tests := []struct {
		name string
		code *int
		want string
	}{
		{name: "nil", code: nil, want: "<nil>"},
		{name: "zero", code: intPtr(0), want: "0"},
		{name: "positive", code: intPtr(42), want: "42"},
		{name: "negative", code: intPtr(-7), want: "-7"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatGateExitCode(tt.code); got != tt.want {
				t.Fatalf("formatGateExitCode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTraceClipString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		limit int
		want  string
	}{
		{name: "empty", input: "", limit: 4, want: ""},
		{name: "below limit", input: "abc", limit: 4, want: "abc"},
		{name: "exact limit", input: "abcd", limit: 4, want: "abcd"},
		{name: "over limit", input: "abcde", limit: 4, want: "abcd...[clipped]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := traceClipString(tt.input, tt.limit); got != tt.want {
				t.Fatalf("traceClipString(%q, %d) = %q, want %q", tt.input, tt.limit, got, tt.want)
			}
		})
	}
}

// TestResolveRalphCheckMoleculePaths_MoleculeMember pins the
// gastownhall/gascity#2522 fix: a ralph parent bead carrying
// gc.root_bead_id metadata (stamped by molecule.Instantiate) must
// resolve both the molecule root directory and a per-bead artifact
// directory, so the ralph engine can inject GC_MOLECULE_DIR and
// GC_ARTIFACT_DIR into the check script's environment. Before the fix
// both env vars were absent and `set -eu` check scripts referencing
// them crashed with "unbound variable" on every attempt.
func TestResolveRalphCheckMoleculePaths_MoleculeMember(t *testing.T) {
	cityPath := t.TempDir()
	const rootID = "gc-mol-root"
	const beadID = "gc-ralph-parent"

	bead := beads.Bead{
		ID: beadID,
		Metadata: map[string]string{
			"gc.root_bead_id": rootID,
		},
	}

	moleculeDir, artifactDir := resolveRalphCheckMoleculePaths(bead, cityPath)

	wantMolDir := filepath.Join(cityPath, ".gc", "molecules", rootID)
	if moleculeDir != wantMolDir {
		t.Fatalf("moleculeDir = %q, want %q", moleculeDir, wantMolDir)
	}
	wantArtDir := filepath.Join(wantMolDir, "artifacts", beadID)
	if artifactDir != wantArtDir {
		t.Fatalf("artifactDir = %q, want %q", artifactDir, wantArtDir)
	}
}

// TestResolveRalphCheckMoleculePaths_NonMolecule pins the no-side-effect
// guard: a bead without gc.root_bead_id (not a molecule member) returns
// empty strings for both paths, and the caller treats empty as "omit the
// env var" so non-molecule ralph checks keep their pre-#2522 behavior.
func TestResolveRalphCheckMoleculePaths_NonMolecule(t *testing.T) {
	cityPath := t.TempDir()
	bead := beads.Bead{ID: "gc-loose-bead"}

	moleculeDir, artifactDir := resolveRalphCheckMoleculePaths(bead, cityPath)

	if moleculeDir != "" || artifactDir != "" {
		t.Fatalf("non-molecule bead got moleculeDir=%q artifactDir=%q; want both empty", moleculeDir, artifactDir)
	}
}

// TestResolveRalphCheckMoleculePaths_EmptyCityPath guards against
// resolving paths against the caller's cwd when the city path is
// missing (mirrors the empty-cityPath rejection in molecule.RemoveDir).
func TestResolveRalphCheckMoleculePaths_EmptyCityPath(t *testing.T) {
	bead := beads.Bead{
		ID:       "gc-ralph-parent",
		Metadata: map[string]string{"gc.root_bead_id": "gc-mol-root"},
	}

	moleculeDir, artifactDir := resolveRalphCheckMoleculePaths(bead, "")

	if moleculeDir != "" || artifactDir != "" {
		t.Fatalf("empty cityPath got moleculeDir=%q artifactDir=%q; want both empty", moleculeDir, artifactDir)
	}
	// Tiny sanity check that we did not accidentally return relative
	// strings either (which a future regression to filepath.Join("",..)
	// could produce).
	if strings.Contains(moleculeDir, ".gc") || strings.Contains(artifactDir, ".gc") {
		t.Fatalf("empty cityPath should not surface .gc-rooted relative path; got moleculeDir=%q artifactDir=%q", moleculeDir, artifactDir)
	}
}
