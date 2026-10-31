package dispatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// homeDerivedLookupScript models how a tool finds a per-user install: from the
// override variable when set, else from HOME. It records what it saw so the
// test can assert the contract, not just the verdict.
const homeDerivedLookupScript = `#!/bin/sh
base="${PYTHONUSERBASE:-$HOME/Library/Python}"
printf 'HOME=%s PYTHONUSERBASE=%s\n' "$HOME" "${PYTHONUSERBASE:-}" >&2
[ -f "$base/site-packages/marker" ] || { echo "marker not under $base" >&2; exit 1; }
exit 0
`

func writeMarkerUnder(t *testing.T, userBase string) {
	t.Helper()
	dir := filepath.Join(userBase, "site-packages")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "marker"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestProcessRalphControlCheckReceivesCheckScriptEnv drives the compiled
// kind=ralph lane, the one [steps.check] materializes to: the operator's
// check_script_env reaches the script through ProcessOptions, HOME stays the
// city, and without the setting the same script fails its lookup.
func TestProcessRalphControlCheckReceivesCheckScriptEnv(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	realHome := filepath.Join(root, "home")
	cityPath := filepath.Join(root, "city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	userBase := filepath.Join(realHome, "Library", "Python")
	writeMarkerUnder(t, userBase)
	checkPath := ".gc/scripts/checks/build-artifact-valid.sh"
	writeExecutableScript(t, filepath.Join(cityPath, checkPath), homeDerivedLookupScript)

	// Red: no setting, so the lookup runs against the city HOME and the
	// verdict is a real fail that spends the single attempt.
	store, control := newUnlaunchableCheckRalphControl(t, checkPath)
	result, err := ProcessControl(store, control, ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("ProcessControl without check_script_env: %v", err)
	}
	if result.Action != "fail" {
		t.Fatalf("result = %+v, want fail (lookup under the city HOME misses)", result)
	}
	if log := mustGet(t, store, control.ID).Metadata[beadmeta.AttemptLogMetadataKey]; !strings.Contains(log, "HOME="+cityPath) {
		t.Fatalf("gc.attempt_log = %q, want the sandbox HOME recorded by the script", log)
	}

	// Green: the operator hands the script the one variable it needs.
	store, control = newUnlaunchableCheckRalphControl(t, checkPath)
	result, err = ProcessControl(store, control, ProcessOptions{
		CityPath:       cityPath,
		CheckScriptEnv: map[string]string{"PYTHONUSERBASE": userBase},
	})
	if err != nil {
		t.Fatalf("ProcessControl with check_script_env: %v", err)
	}
	if result.Action != "pass" {
		t.Fatalf("result = %+v, want pass", result)
	}
	got := mustGet(t, store, control.ID)
	if got.Status != "closed" || got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Fatalf("control status=%q outcome=%q, want closed pass", got.Status, got.Metadata[beadmeta.OutcomeMetadataKey])
	}
}

// TestProcessRalphCheckReceivesCheckScriptEnv covers the legacy kind=check
// lane with the same fixture.
func TestProcessRalphCheckReceivesCheckScriptEnv(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cityPath := filepath.Join(root, "city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	userBase := filepath.Join(root, "home", "Library", "Python")
	writeMarkerUnder(t, userBase)
	checkPath := writeCheckScript(t, cityPath, "lookup.sh", homeDerivedLookupScript)

	store, _, run1, check1 := newSimpleRalphLoop(t, "implement", checkPath, 1)
	mustClose(t, store, run1.ID)
	result, err := ProcessControl(store, mustGet(t, store, check1.ID), ProcessOptions{
		CityPath:       cityPath,
		CheckScriptEnv: map[string]string{"PYTHONUSERBASE": userBase},
	})
	if err != nil {
		t.Fatalf("ProcessControl: %v", err)
	}
	if result.Action != "pass" {
		t.Fatalf("result = %+v, want pass", result)
	}
	stderr := mustGet(t, store, check1.ID).Metadata[beadmeta.StderrMetadataKey]
	if !strings.Contains(stderr, "HOME="+cityPath) || !strings.Contains(stderr, "PYTHONUSERBASE="+userBase) {
		t.Fatalf("check stderr = %q, want HOME still the city and PYTHONUSERBASE from the setting", stderr)
	}
}

// TestRunRalphCheckCheckScriptEnvCannotOverrideIdentity pins that a reserved
// key handed through ProcessOptions (bypassing config validation) is dropped:
// the check still sees the city HOME and the store's BEADS_DIR.
func TestRunRalphCheckCheckScriptEnvCannotOverrideIdentity(t *testing.T) {
	t.Parallel()
	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "env.sh", "#!/bin/sh\nprintf 'HOME=%s BEADS_DIR=%s\\n' \"$HOME\" \"$BEADS_DIR\" >&2\nexit 1\n")
	check := beads.Bead{ID: "check-env", Type: "task", Metadata: map[string]string{beadmeta.CheckPathMetadataKey: checkPath}}

	result, err := runRalphCheck(beads.NewMemStore(), check, beads.Bead{ID: "run-env", Type: "task"}, 1, ProcessOptions{
		CityPath:       cityPath,
		CheckScriptEnv: map[string]string{"HOME": "/elsewhere", "BEADS_DIR": "/elsewhere/.beads"},
	})
	if err != nil {
		t.Fatalf("runRalphCheck: %v", err)
	}
	if !strings.Contains(result.Stderr, "HOME="+cityPath) || !strings.Contains(result.Stderr, "BEADS_DIR="+filepath.Join(cityPath, ".beads")) {
		t.Fatalf("stderr = %q, want the identity variables untouched", result.Stderr)
	}
	if strings.Contains(result.Stderr, "/elsewhere") {
		t.Fatalf("stderr = %q, an operator value overrode an identity variable", result.Stderr)
	}
}
