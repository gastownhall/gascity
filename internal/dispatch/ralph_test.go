package dispatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convergence"
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

// TestResolveRalphCheckMoleculePaths_UnsafeRootID pins the fail-closed guard
// against a path-traversing gc.root_bead_id: when the root ID is unsafe,
// EnsureArtifactDir rejects it, and the resolver must NOT surface a
// path-escaping GC_MOLECULE_DIR — both paths come back empty so the caller
// omits the env vars entirely.
func TestResolveRalphCheckMoleculePaths_UnsafeRootID(t *testing.T) {
	cityPath := t.TempDir()
	for _, rootID := range []string{"../escape", "/abs/root", "..", `a\b`} {
		bead := beads.Bead{
			ID:       "gc-ralph-parent",
			Metadata: map[string]string{"gc.root_bead_id": rootID},
		}
		moleculeDir, artifactDir := resolveRalphCheckMoleculePaths(bead, cityPath)
		if moleculeDir != "" || artifactDir != "" {
			t.Fatalf("unsafe rootID %q got moleculeDir=%q artifactDir=%q; want both empty", rootID, moleculeDir, artifactDir)
		}
	}
}

// TestRunRalphCheckEnvTracksSubject pins gastownhall/gascity#2558 review
// feedback: GC_BEAD_ID and the molecule/artifact dirs must describe the SAME
// bead. The per-attempt agent runs on the subject (attempt) bead and writes
// its verdict under that bead's artifact dir, so the check script's
// GC_ARTIFACT_DIR must key off the subject — not the control bead.
func TestRunRalphCheckEnvTracksSubject(t *testing.T) {
	cityPath := t.TempDir()
	script := filepath.Join(cityPath, "check.sh")
	// Echo the env the check subprocess actually receives so the test can
	// assert which bead the molecule/artifact dirs were derived from.
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"GC_BEAD_ID=$GC_BEAD_ID\"\necho \"GC_ARTIFACT_DIR=$GC_ARTIFACT_DIR\"\necho \"GC_MOLECULE_DIR=$GC_MOLECULE_DIR\"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	store := beads.NewMemStore()
	root := mustCreate(t, store, beads.Bead{Title: "workflow", Metadata: map[string]string{"gc.kind": "workflow"}})
	control := mustCreate(t, store, beads.Bead{
		Title: "review loop",
		Metadata: map[string]string{
			"gc.kind":         "ralph",
			"gc.root_bead_id": root.ID,
			"gc.check_path":   "check.sh",
			"gc.max_attempts": "3",
		},
	})
	subject := mustCreate(t, store, beads.Bead{
		Title:    "review loop iteration 1",
		Metadata: map[string]string{"gc.kind": "scope", "gc.root_bead_id": root.ID},
	})
	if subject.ID == control.ID {
		t.Fatalf("test setup: subject and control share ID %q", subject.ID)
	}

	result, err := runRalphCheck(store, control, subject, 1, ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("runRalphCheck: %v", err)
	}
	if result.Outcome != convergence.GatePass {
		t.Fatalf("Outcome = %q (stderr=%q), want pass", result.Outcome, result.Stderr)
	}

	wantBeadID := "GC_BEAD_ID=" + subject.ID
	if !strings.Contains(result.Stdout, wantBeadID) {
		t.Errorf("stdout missing %q; got %q", wantBeadID, result.Stdout)
	}
	wantArtifact := "GC_ARTIFACT_DIR=" + filepath.Join(cityPath, ".gc", "molecules", root.ID, "artifacts", subject.ID)
	if !strings.Contains(result.Stdout, wantArtifact) {
		t.Errorf("artifact dir not keyed by subject; want line %q; got %q", wantArtifact, result.Stdout)
	}
	if strings.Contains(result.Stdout, "artifacts/"+control.ID) {
		t.Errorf("artifact dir wrongly keyed by control bead %q; got %q", control.ID, result.Stdout)
	}
}
