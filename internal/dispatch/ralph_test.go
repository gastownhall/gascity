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

// TestRunRalphCheckPackRelativeCheckPathWorkDirFallback covers
// gastownhall/gascity#3008: a pack-relative gc.check_path
// (e.g. assets/<pack>/scripts/check.sh) names a pack-shipped script that lives
// under the store/city root, not the per-task gc.work_dir worktree. When the
// control bead carries a work_dir pointing at a worktree that lacks the pack
// tree, the relative join <work_dir>/assets/... does not exist and the check
// was control-quarantined. The fallback resolves the relative path against the
// store root instead, so the gate is evaluated.
func TestRunRalphCheckPackRelativeCheckPathWorkDirFallback(t *testing.T) {
	cityPath := t.TempDir()
	// Pack-shipped check script lives under the city/store root.
	checkRel := filepath.Join("assets", "demo-pack", "scripts", "check.sh")
	storeScript := filepath.Join(cityPath, checkRel)
	if err := os.MkdirAll(filepath.Dir(storeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(storeScript, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Per-task worktree under the city root that does NOT contain the pack tree.
	workDir := filepath.Join(cityPath, "worktrees", "task1")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	store := beads.NewMemStore()
	root := mustCreate(t, store, beads.Bead{Title: "workflow", Metadata: map[string]string{"gc.kind": "workflow"}})
	control := mustCreate(t, store, beads.Bead{
		Title: "review loop",
		Metadata: map[string]string{
			"gc.kind":         "ralph",
			"gc.root_bead_id": root.ID,
			"gc.check_path":   filepath.ToSlash(checkRel),
			"gc.work_dir":     workDir,
			"gc.max_attempts": "3",
		},
	})
	subject := mustCreate(t, store, beads.Bead{
		Title:    "review loop iteration 1",
		Metadata: map[string]string{"gc.kind": "scope", "gc.root_bead_id": root.ID},
	})

	result, err := runRalphCheck(store, control, subject, 1, ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("runRalphCheck: %v (the pack-relative check_path should fall back to the store root)", err)
	}
	if result.Outcome != convergence.GatePass {
		t.Fatalf("Outcome = %q (stderr=%q), want pass via store-root fallback", result.Outcome, result.Stderr)
	}
}

// TestRunRalphCheckWorkDirRelativeCheckPathKeepsPrecedence guards that the
// #3008 fallback only fires when the work_dir join is missing: a check_path
// that DOES exist under the worktree must still resolve against the worktree,
// not the store root.
func TestRunRalphCheckWorkDirRelativeCheckPathKeepsPrecedence(t *testing.T) {
	cityPath := t.TempDir()
	checkRel := "check.sh"
	// Same relative name exists in both the store root and the worktree; the
	// worktree copy must win. Distinguish them by exit code.
	if err := os.WriteFile(filepath.Join(cityPath, checkRel), []byte("#!/bin/sh\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(cityPath, "worktrees", "task1")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, checkRel), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	store := beads.NewMemStore()
	root := mustCreate(t, store, beads.Bead{Title: "workflow", Metadata: map[string]string{"gc.kind": "workflow"}})
	control := mustCreate(t, store, beads.Bead{
		Title: "review loop",
		Metadata: map[string]string{
			"gc.kind":         "ralph",
			"gc.root_bead_id": root.ID,
			"gc.check_path":   checkRel,
			"gc.work_dir":     workDir,
			"gc.max_attempts": "3",
		},
	})
	subject := mustCreate(t, store, beads.Bead{
		Title:    "review loop iteration 1",
		Metadata: map[string]string{"gc.kind": "scope", "gc.root_bead_id": root.ID},
	})

	result, err := runRalphCheck(store, control, subject, 1, ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("runRalphCheck: %v", err)
	}
	// The worktree copy exits 0 (pass); the store copy exits 7 (fail). A pass
	// proves the worktree script ran, i.e. the fallback did not shadow it.
	if result.Outcome != convergence.GatePass {
		t.Fatalf("Outcome = %q (stderr=%q), want pass from the worktree-relative script", result.Outcome, result.Stderr)
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

// TestProcessRalphCheckHardSubjectFailureTerminatesWithoutRetry proves FIX 1:
// when the ralph subject closed with gc.failure_class=hard, the loop stops in a
// single attempt (Action "hard-fail") instead of cloning attempts up to
// gc.max_attempts (the treadmill that abort_scope-killed molecules).
func TestProcessRalphCheckHardSubjectFailureTerminatesWithoutRetry(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	// A passing check script proves termination is driven by the subject's
	// hard-class failure alone; the check never gets a chance to pass.
	checkPath := writeCheckScript(t, cityPath, "check.sh", "#!/bin/bash\nexit 0\n")
	store, logical, run1, check1 := newSimpleRalphLoop(t, "implement", checkPath, 5)

	if err := store.SetMetadataBatch(run1.ID, map[string]string{
		"gc.outcome":        "fail",
		"gc.failure_class":  "hard",
		"gc.failure_reason": "external_live_head_changed",
	}); err != nil {
		t.Fatalf("stamp hard subject failure: %v", err)
	}
	if err := store.Close(run1.ID); err != nil {
		t.Fatalf("close run1: %v", err)
	}

	result, err := ProcessControl(store, check1, ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("ProcessControl(check1): %v", err)
	}
	if !result.Processed || result.Action != "hard-fail" {
		t.Fatalf("result = %+v, want processed hard-fail", result)
	}

	logicalAfter := mustGetBead(t, store, logical.ID)
	if logicalAfter.Status != "closed" || logicalAfter.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("logical = status %q outcome %q, want closed/fail", logicalAfter.Status, logicalAfter.Metadata["gc.outcome"])
	}
	if logicalAfter.Metadata["gc.failure_class"] != "hard" {
		t.Fatalf("logical gc.failure_class = %q, want hard", logicalAfter.Metadata["gc.failure_class"])
	}
	if logicalAfter.Metadata["gc.failure_reason"] != "external_live_head_changed" {
		t.Fatalf("logical gc.failure_reason = %q, want external_live_head_changed", logicalAfter.Metadata["gc.failure_reason"])
	}

	checkAfter := mustGetBead(t, store, check1.ID)
	if checkAfter.Status != "closed" || checkAfter.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("check = status %q outcome %q, want closed/fail", checkAfter.Status, checkAfter.Metadata["gc.outcome"])
	}

	rootID := run1.Metadata["gc.root_bead_id"]
	all, err := listByWorkflowRoot(store, rootID)
	if err != nil {
		t.Fatalf("listByWorkflowRoot: %v", err)
	}
	for _, bead := range all {
		if bead.Metadata["gc.attempt"] == "2" {
			t.Fatalf("hard-fail must not clone another attempt; found %s (kind %q)", bead.ID, bead.Metadata["gc.kind"])
		}
	}
}

// TestProcessRalphCheckSoftSubjectFailureStillRetries is the FIX 1 regression
// guard: a non-hard (repairable) subject failure must still clone up to
// gc.max_attempts. Crucially an empty gc.failure_class stays repairable here,
// unlike retry-eval which maps empty to hard.
func TestProcessRalphCheckSoftSubjectFailureStillRetries(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "check.sh", "#!/bin/bash\nexit 1\n")
	store, logical, run1, check1 := newSimpleRalphLoop(t, "implement", checkPath, 5)

	// gc.outcome=fail with no gc.failure_class is the ordinary repairable case.
	if err := store.SetMetadata(run1.ID, "gc.outcome", "fail"); err != nil {
		t.Fatalf("stamp soft subject failure: %v", err)
	}
	if err := store.Close(run1.ID); err != nil {
		t.Fatalf("close run1: %v", err)
	}

	result, err := ProcessControl(store, check1, ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("ProcessControl(check1): %v", err)
	}
	if !result.Processed || result.Action != "retry" {
		t.Fatalf("result = %+v, want processed retry", result)
	}

	logicalAfter := mustGetBead(t, store, logical.ID)
	if logicalAfter.Status != "open" {
		t.Fatalf("logical status = %q, want open (loop continues)", logicalAfter.Status)
	}

	rootID := run1.Metadata["gc.root_bead_id"]
	all, err := listByWorkflowRoot(store, rootID)
	if err != nil {
		t.Fatalf("listByWorkflowRoot: %v", err)
	}
	sawAttempt2 := false
	for _, bead := range all {
		if bead.Metadata["gc.attempt"] == "2" {
			sawAttempt2 = true
			break
		}
	}
	if !sawAttempt2 {
		t.Fatalf("soft failure must clone attempt 2; none found under root %s", rootID)
	}
}
