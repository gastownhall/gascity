package dispatch

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convergence"
)

// TestRunRalphCheckUnlaunchableScriptIsDriftPending pins that a check script
// the orchestrator cannot launch (missing, not a regular file, not executable)
// is reported as ErrControlDriftPending rather than a bare error. A bare error
// classifies TierNone and quarantine-closes the step, releasing its dependents
// (gastownhall/gascity#4239).
func TestRunRalphCheckUnlaunchableScriptIsDriftPending(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		setup    func(t *testing.T, cityPath string) (checkPath, workDir string)
		wantRoot error
	}{
		{
			name: "missing relative path",
			setup: func(_ *testing.T, _ string) (string, string) {
				return ".gc/scripts/checks/missing.sh", ""
			},
			wantRoot: fs.ErrNotExist,
		},
		{
			name: "missing absolute path inside the city root",
			setup: func(_ *testing.T, cityPath string) (string, string) {
				return filepath.Join(cityPath, ".gc", "scripts", "checks", "missing.sh"), ""
			},
			wantRoot: fs.ErrNotExist,
		},
		{
			name: "script without execute bit",
			setup: func(t *testing.T, cityPath string) (string, string) {
				t.Helper()
				dir := filepath.Join(cityPath, ".gc", "scripts")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(filepath.Join(dir, "check.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
					t.Fatalf("write: %v", err)
				}
				return ".gc/scripts/check.sh", ""
			},
			wantRoot: convergence.ErrConditionNotExecutable,
		},
		{
			name: "directory at the check path",
			setup: func(t *testing.T, cityPath string) (string, string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(cityPath, ".gc", "scripts", "check.sh"), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				return ".gc/scripts/check.sh", ""
			},
			wantRoot: convergence.ErrConditionNotRegular,
		},
		{
			// Both the worktree lookup and the #3008 store fallback miss: the
			// preserved original not-exist error must still pend.
			name: "work_dir and store fallback both miss",
			setup: func(t *testing.T, cityPath string) (string, string) {
				t.Helper()
				workDir := filepath.Join(cityPath, "worktrees", "w1")
				if err := os.MkdirAll(workDir, 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				return ".gc/scripts/checks/missing.sh", workDir
			},
			wantRoot: fs.ErrNotExist,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cityPath := t.TempDir()
			checkPath, workDir := tc.setup(t, cityPath)
			store := beads.NewMemStore()
			meta := map[string]string{beadmeta.CheckPathMetadataKey: checkPath}
			if workDir != "" {
				meta[beadmeta.WorkDirMetadataKey] = workDir
			}
			check := beads.Bead{ID: "check-unlaunchable", Type: "task", Metadata: meta}
			subject := beads.Bead{ID: "run-unlaunchable", Type: "task"}
			opts := ProcessOptions{CityPath: cityPath}

			_, err := runRalphCheck(store, check, subject, 1, opts)
			if !errors.Is(err, ErrControlDriftPending) {
				t.Fatalf("runRalphCheck error = %v, want ErrControlDriftPending", err)
			}
			if !errors.Is(err, ErrControlPending) {
				t.Fatalf("runRalphCheck error = %v, want it to also match ErrControlPending", err)
			}
			if !errors.Is(err, tc.wantRoot) {
				t.Fatalf("runRalphCheck error = %v, lost the underlying %v", err, tc.wantRoot)
			}
			msg := err.Error()
			if !strings.Contains(msg, check.ID) || !strings.Contains(msg, "resolving check path") {
				t.Fatalf("error %q must name the bead and \"resolving check path\"", msg)
			}
			// The cmd layer freezes bookkeeping on a verbatim repeat, so the
			// refusal text must be stable across sweeps.
			_, again := runRalphCheck(store, check, subject, 1, opts)
			if again == nil || again.Error() != msg {
				t.Fatalf("second runRalphCheck error = %v, want verbatim repeat %q", again, msg)
			}
		})
	}
}

// TestRunRalphCheckSecurityRefusalsStayTerminal pins that containment
// refusals keep their terminal disposition: they are refused, not held open.
func TestRunRalphCheckSecurityRefusalsStayTerminal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		setup func(t *testing.T, root, cityPath string) (checkPath, workDir string)
	}{
		{
			// Also missing: the trusted-roots refusal runs before resolution.
			name: "absolute path outside trusted roots that does not exist",
			setup: func(_ *testing.T, root, _ string) (string, string) {
				return filepath.Join(root, "elsewhere", "missing.sh"), ""
			},
		},
		{
			name: "absolute path outside trusted roots that exists",
			setup: func(t *testing.T, root, _ string) (string, string) {
				t.Helper()
				script := filepath.Join(root, "elsewhere", "check.sh")
				writeExecutableScript(t, script, "#!/bin/sh\nexit 0\n")
				return script, ""
			},
		},
		{
			name: "relative traversal",
			setup: func(_ *testing.T, _, _ string) (string, string) {
				return "../x.sh", ""
			},
		},
		{
			name: "work_dir escaping both roots",
			setup: func(t *testing.T, root, _ string) (string, string) {
				t.Helper()
				workDir := filepath.Join(root, "elsewhere")
				if err := os.MkdirAll(workDir, 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				return ".gc/scripts/checks/missing.sh", workDir
			},
		},
		{
			name: "relative symlink resolving outside containment",
			setup: func(t *testing.T, root, cityPath string) (string, string) {
				t.Helper()
				outside := filepath.Join(root, "outside.sh")
				writeExecutableScript(t, outside, "#!/bin/sh\nexit 0\n")
				if err := os.Symlink(outside, filepath.Join(cityPath, "escape.sh")); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				return "escape.sh", ""
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			cityPath := filepath.Join(root, "city")
			if err := os.MkdirAll(cityPath, 0o755); err != nil {
				t.Fatalf("mkdir city: %v", err)
			}
			checkPath, workDir := tc.setup(t, root, cityPath)
			meta := map[string]string{beadmeta.CheckPathMetadataKey: checkPath}
			if workDir != "" {
				meta[beadmeta.WorkDirMetadataKey] = workDir
			}
			check := beads.Bead{ID: "check-refused", Type: "task", Metadata: meta}
			subject := beads.Bead{ID: "run-refused", Type: "task"}

			_, err := runRalphCheck(beads.NewMemStore(), check, subject, 1, ProcessOptions{CityPath: cityPath})
			if err == nil {
				t.Fatal("runRalphCheck = nil error, want a terminal refusal")
			}
			if errors.Is(err, ErrControlPending) {
				t.Fatalf("runRalphCheck error = %v, a security refusal must not be pending", err)
			}
		})
	}
}

// newUnlaunchableCheckRalphControl builds a compiled kind=ralph control
// (max_attempts=1, exec check at checkPath) blocking on a closed first
// iteration whose check has not run yet.
func newUnlaunchableCheckRalphControl(t *testing.T, checkPath string) (*beads.MemStore, beads.Bead) {
	t.Helper()
	store := beads.NewMemStore()
	root := mustCreate(t, store, beads.Bead{
		Title:    "workflow",
		Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
	})
	control := mustCreate(t, store, beads.Bead{
		Title: "verified step",
		Metadata: map[string]string{
			"gc.kind":                     "ralph",
			"gc.root_bead_id":             root.ID,
			"gc.step_ref":                 "mol-test.verified",
			"gc.step_id":                  "verified",
			"gc.max_attempts":             "1",
			"gc.control_epoch":            "1",
			beadmeta.CheckModeMetadataKey: beadmeta.CheckModeExec,
			beadmeta.CheckPathMetadataKey: checkPath,
		},
	})
	iteration := mustCreate(t, store, beads.Bead{
		Title: "verified step iteration 1",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-test.verified.iteration.1",
			"gc.attempt":      "1",
		},
	})
	mustClose(t, store, iteration.ID)
	mustDep(t, store, control.ID, iteration.ID, "blocks")
	return store, mustGet(t, store, control.ID)
}

func countAllBeads(t *testing.T, store beads.Store) int {
	t.Helper()
	all, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("list beads: %v", err)
	}
	return len(all)
}

// assertUnlaunchableRalphControlHeldOpen asserts the compiled-lane control was
// held open without spending its attempt or spawning a successor iteration.
func assertUnlaunchableRalphControlHeldOpen(t *testing.T, store beads.Store, controlID string, wantBeads int) {
	t.Helper()
	got := mustGet(t, store, controlID)
	if got.Status == "closed" {
		t.Fatalf("control %s closed (outcome=%q), want it held open", controlID, got.Metadata[beadmeta.OutcomeMetadataKey])
	}
	for _, key := range []string{
		beadmeta.OutcomeMetadataKey,
		beadmeta.AttemptLogMetadataKey,
		beadmeta.FailedAttemptMetadataKey,
		beadmeta.CheckInfraRetryMetadataKey,
	} {
		if v := got.Metadata[key]; v != "" {
			t.Errorf("%s = %q, want empty (an unlaunchable check produces no verdict)", key, v)
		}
	}
	if n := countAllBeads(t, store); n != wantBeads {
		t.Errorf("bead count = %d, want %d (no successor iteration may be spawned)", n, wantBeads)
	}
}

// TestProcessRalphControlUnlaunchableCheckStaysOpen covers the compiled
// kind=ralph lane: a missing check script holds the control open, and shipping
// the script lets the next sweep pass it.
func TestProcessRalphControlUnlaunchableCheckStaysOpen(t *testing.T) {
	t.Parallel()
	cityPath := t.TempDir()
	checkPath := ".gc/scripts/checks/verify.sh"
	store, control := newUnlaunchableCheckRalphControl(t, checkPath)
	opts := ProcessOptions{CityPath: cityPath}
	beadsBefore := countAllBeads(t, store)

	_, err := ProcessControl(store, control, opts)
	if !errors.Is(err, ErrControlDriftPending) {
		t.Fatalf("ProcessControl error = %v, want ErrControlDriftPending", err)
	}
	assertUnlaunchableRalphControlHeldOpen(t, store, control.ID, beadsBefore)

	writeExecutableScript(t, filepath.Join(cityPath, checkPath), "#!/bin/sh\nexit 0\n")
	result, err := ProcessControl(store, mustGet(t, store, control.ID), opts)
	if err != nil {
		t.Fatalf("ProcessControl after shipping the script: %v", err)
	}
	if result.Action != "pass" {
		t.Fatalf("result = %+v, want pass", result)
	}
	got := mustGet(t, store, control.ID)
	if got.Status != "closed" || got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Fatalf("control status=%q outcome=%q, want closed pass", got.Status, got.Metadata[beadmeta.OutcomeMetadataKey])
	}
}

// TestProcessRalphControlNonExecutableCheckRecoversAfterChmod covers a script
// that ships without its execute bit: held open until chmod, then passes.
func TestProcessRalphControlNonExecutableCheckRecoversAfterChmod(t *testing.T) {
	t.Parallel()
	cityPath := t.TempDir()
	checkPath := ".gc/scripts/checks/verify.sh"
	scriptPath := filepath.Join(cityPath, checkPath)
	if err := os.MkdirAll(filepath.Dir(scriptPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	store, control := newUnlaunchableCheckRalphControl(t, checkPath)
	opts := ProcessOptions{CityPath: cityPath}
	beadsBefore := countAllBeads(t, store)

	_, err := ProcessControl(store, control, opts)
	if !errors.Is(err, ErrControlDriftPending) {
		t.Fatalf("ProcessControl error = %v, want ErrControlDriftPending", err)
	}
	if !errors.Is(err, convergence.ErrConditionNotExecutable) {
		t.Fatalf("ProcessControl error = %v, want ErrConditionNotExecutable preserved", err)
	}
	assertUnlaunchableRalphControlHeldOpen(t, store, control.ID, beadsBefore)

	if err := os.Chmod(scriptPath, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	result, err := ProcessControl(store, mustGet(t, store, control.ID), opts)
	if err != nil {
		t.Fatalf("ProcessControl after chmod: %v", err)
	}
	if result.Action != "pass" {
		t.Fatalf("result = %+v, want pass", result)
	}
	if got := mustGet(t, store, control.ID); got.Status != "closed" || got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Fatalf("control status=%q outcome=%q, want closed pass", got.Status, got.Metadata[beadmeta.OutcomeMetadataKey])
	}
}

// TestProcessRalphCheckUnlaunchableScriptStaysOpen covers the legacy
// kind=check lane: a missing script holds the check and its logical bead open,
// spends no infrastructure budget, and clones no next attempt.
func TestProcessRalphCheckUnlaunchableScriptStaysOpen(t *testing.T) {
	t.Parallel()
	cityPath := t.TempDir()
	store, logical, run1, check1 := newSimpleRalphLoop(t, "implement", ".gc/scripts/checks/missing.sh", 2)
	mustClose(t, store, run1.ID)
	beadsBefore := countAllBeads(t, store)

	_, err := ProcessControl(store, mustGet(t, store, check1.ID), ProcessOptions{CityPath: cityPath})
	if !errors.Is(err, ErrControlDriftPending) {
		t.Fatalf("ProcessControl error = %v, want ErrControlDriftPending", err)
	}
	for _, id := range []string{check1.ID, logical.ID} {
		if got := mustGet(t, store, id); got.Status == "closed" {
			t.Fatalf("bead %s closed (outcome=%q), want it held open", id, got.Metadata[beadmeta.OutcomeMetadataKey])
		}
	}
	if v := mustGet(t, store, check1.ID).Metadata[beadmeta.CheckInfraRetryMetadataKey]; v != "" {
		t.Errorf("gc.check_infra_retry = %q, want unset (unlaunchable is not an infrastructure blip)", v)
	}
	if n := countAllBeads(t, store); n != beadsBefore {
		t.Errorf("bead count = %d, want %d (no attempt-2 run or check may be cloned)", n, beadsBefore)
	}
}
