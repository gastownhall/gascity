package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/convergence"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestCheckScriptEnvFromConfigExpandsAgainstTheController pins where $VAR
// expansion happens: at the controller edge, against the controller's own
// environment. Inside the check HOME is the city, so expanding there would
// re-derive the path the setting exists to override. The controller-only
// keys read as empty, as they do for session env.
func TestCheckScriptEnvFromConfigExpandsAgainstTheController(t *testing.T) {
	t.Setenv("HOME", "/controller/home")
	t.Setenv("GC_CONTROLLER_TOKEN", "secret-token")

	if got := checkScriptEnvFromConfig(nil); got != nil {
		t.Fatalf("nil cfg = %v, want nil", got)
	}
	if got := checkScriptEnvFromConfig(&config.City{}); got != nil {
		t.Fatalf("empty table = %v, want nil", got)
	}

	cfg := &config.City{}
	cfg.CheckScriptEnv = map[string]string{
		"PYTHONUSERBASE": "$HOME/Library/Python/3.14",
		"LITERAL":        "as-is",
		"LEAK":           "$GC_CONTROLLER_TOKEN",
	}
	got := checkScriptEnvFromConfig(cfg)
	if got["PYTHONUSERBASE"] != "/controller/home/Library/Python/3.14" {
		t.Errorf("PYTHONUSERBASE = %q, want expansion against the controller HOME", got["PYTHONUSERBASE"])
	}
	if got["LITERAL"] != "as-is" {
		t.Errorf("LITERAL = %q", got["LITERAL"])
	}
	if got["LEAK"] != "" {
		t.Errorf("LEAK = %q, want the controller-only key masked to empty", got["LEAK"])
	}
	// The config value itself is left untouched: expansion returns a copy.
	if cfg.CheckScriptEnv["PYTHONUSERBASE"] != "$HOME/Library/Python/3.14" {
		t.Errorf("config mutated: %q", cfg.CheckScriptEnv["PYTHONUSERBASE"])
	}
	if keys := checkScriptEnvKeys(got); !slices.Equal(keys, []string{"LEAK", "LITERAL", "PYTHONUSERBASE"}) {
		t.Errorf("keys = %v, want sorted", keys)
	}
}

// TestControlDispatcherThreadsCheckScriptEnvFromConfig drives a [steps.check]
// control through the real dispatcher with a loaded city config, proving the
// configured variable (with $HOME expanded against the controller) reaches the
// check script while the script's own HOME stays the city.
func TestControlDispatcherThreadsCheckScriptEnvFromConfig(t *testing.T) {
	clearGCEnv(t)
	controllerHome := t.TempDir()
	t.Setenv("HOME", controllerHome)
	fakeProvider := runtime.NewFake()
	oldProvider := dispatchControlSessionProvider
	dispatchControlSessionProvider = func() (runtime.Provider, error) { return fakeProvider, nil }
	t.Cleanup(func() { dispatchControlSessionProvider = oldProvider })

	userBase := filepath.Join(controllerHome, "Library", "Python")
	if err := os.MkdirAll(filepath.Join(userBase, "site-packages"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userBase, "site-packages", "marker"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	cityDir := t.TempDir()
	const checkPath = ".gc/scripts/checks/verify.sh"
	scriptPath := filepath.Join(cityDir, checkPath)
	if err := os.MkdirAll(filepath.Dir(scriptPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scriptPath, []byte(`#!/bin/sh
base="${PYTHONUSERBASE:-$HOME/Library/Python}"
printf 'HOME=%s PYTHONUSERBASE=%s\n' "$HOME" "${PYTHONUSERBASE:-}" >&2
[ -f "$base/site-packages/marker" ] || { echo "marker not under $base" >&2; exit 1; }
exit 0
`), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	cfg.CheckScriptEnv = map[string]string{"PYTHONUSERBASE": "$HOME/Library/Python"}

	store := beads.NewMemStore()
	mustCreateBead := func(b beads.Bead) beads.Bead {
		t.Helper()
		created, err := store.Create(b)
		if err != nil {
			t.Fatalf("create %q: %v", b.Title, err)
		}
		return created
	}
	root := mustCreateBead(beads.Bead{Title: "workflow", Type: "task", Metadata: map[string]string{
		"gc.kind": "workflow", "gc.formula_contract": "graph.v2",
	}})
	control := mustCreateBead(beads.Bead{Title: "verified step", Type: "task", Metadata: map[string]string{
		"gc.kind":                     "ralph",
		"gc.root_bead_id":             root.ID,
		"gc.step_ref":                 "mol-test.verified",
		"gc.step_id":                  "verified",
		"gc.max_attempts":             "1",
		"gc.control_epoch":            "1",
		beadmeta.CheckModeMetadataKey: beadmeta.CheckModeExec,
		beadmeta.CheckPathMetadataKey: checkPath,
	}})
	iteration := mustCreateBead(beads.Bead{Title: "iteration 1", Type: "task", Metadata: map[string]string{
		"gc.kind": "scope", "gc.root_bead_id": root.ID, "gc.step_ref": "mol-test.verified.iteration.1", "gc.attempt": "1",
	}})
	if err := store.Close(iteration.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.DepAdd(control.ID, iteration.ID, "blocks"); err != nil {
		t.Fatal(err)
	}

	if err := runControlDispatcherWithStoreAndConfig(cityDir, cityDir, store, control.ID, cfg, io.Discard, io.Discard); err != nil {
		t.Fatalf("dispatcher: %v", err)
	}
	got, err := store.Get(control.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "closed" || got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Fatalf("control status=%q outcome=%q attempt_log=%q, want closed pass", got.Status, got.Metadata[beadmeta.OutcomeMetadataKey], got.Metadata[beadmeta.AttemptLogMetadataKey])
	}

	// Red half: the same workflow without the setting fails the lookup and
	// records the sandbox HOME, so the pass above is the setting's doing.
	store2 := beads.NewMemStore()
	mk := func(b beads.Bead) beads.Bead {
		t.Helper()
		created, err := store2.Create(b)
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	root2 := mk(beads.Bead{Title: "workflow", Type: "task", Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"}})
	control2 := mk(beads.Bead{Title: "verified step", Type: "task", Metadata: map[string]string{
		"gc.kind": "ralph", "gc.root_bead_id": root2.ID, "gc.step_ref": "mol-test.verified", "gc.step_id": "verified",
		"gc.max_attempts": "1", "gc.control_epoch": "1",
		beadmeta.CheckModeMetadataKey: beadmeta.CheckModeExec, beadmeta.CheckPathMetadataKey: checkPath,
	}})
	iteration2 := mk(beads.Bead{Title: "iteration 1", Type: "task", Metadata: map[string]string{
		"gc.kind": "scope", "gc.root_bead_id": root2.ID, "gc.step_ref": "mol-test.verified.iteration.1", "gc.attempt": "1",
	}})
	if err := store2.Close(iteration2.ID); err != nil {
		t.Fatal(err)
	}
	if err := store2.DepAdd(control2.ID, iteration2.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	plain := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	if err := runControlDispatcherWithStoreAndConfig(cityDir, cityDir, store2, control2.ID, plain, io.Discard, io.Discard); err != nil {
		t.Fatalf("dispatcher without the setting: %v", err)
	}
	got2, err := store2.Get(control2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got2.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail {
		t.Fatalf("without the setting outcome=%q, want fail", got2.Metadata[beadmeta.OutcomeMetadataKey])
	}
	if log := got2.Metadata[beadmeta.AttemptLogMetadataKey]; !strings.Contains(log, "HOME="+cityDir) {
		t.Fatalf("attempt_log = %q, want the sandbox HOME the script saw", log)
	}
}

// TestGateSandboxCheckRunsWithTheConfiguredEnvAndNamesKeysOnly pins that the
// doctor probe runs in the same environment the real gates do, and that its
// report names the configured variables by key only.
func TestGateSandboxCheckRunsWithTheConfiguredEnvAndNamesKeysOnly(t *testing.T) {
	cfg := &config.City{}
	cfg.CheckScriptEnv = map[string]string{"PYTHONUSERBASE": "/secret/value", "ALPHA": "1"}
	var seen []string
	check := newGateSandboxReadCheck(t.TempDir(), cfg)
	check.probe = func(_ string, env []string) error {
		seen = env
		return nil
	}

	res := check.Run(&doctor.CheckContext{CityPath: check.cityPath})
	if res.Status != doctor.StatusOK {
		t.Fatalf("status = %v (%s)", res.Status, res.Message)
	}
	if !slices.Contains(seen, "PYTHONUSERBASE=/secret/value") || !slices.Contains(seen, "ALPHA=1") {
		t.Fatalf("probe env %v lacks the configured variables", seen)
	}
	joined := strings.Join(res.Details, "\n")
	if !strings.Contains(joined, "check_script_env=ALPHA,PYTHONUSERBASE") {
		t.Fatalf("details %v should name the configured keys, sorted", res.Details)
	}
	if strings.Contains(joined, "/secret/value") {
		t.Fatalf("details %v leak a configured value", res.Details)
	}

	none := newGateSandboxReadCheck(t.TempDir(), nil)
	none.probe = func(string, []string) error { return nil }
	if d := strings.Join(none.Run(&doctor.CheckContext{CityPath: none.cityPath}).Details, "\n"); !strings.Contains(d, "check_script_env=(none configured)") {
		t.Fatalf("details without the setting = %q", d)
	}

	// The failing branch is where an operator goes looking, so it names the
	// configured keys too, and the fix hint points at the table.
	blind := newGateSandboxReadCheck(t.TempDir(), cfg)
	blind.probe = func(string, []string) error { return errors.New("access denied") }
	res = blind.Run(&doctor.CheckContext{CityPath: blind.cityPath})
	if res.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error", res.Status)
	}
	if d := strings.Join(res.Details, "\n"); !strings.Contains(d, "check_script_env=ALPHA,PYTHONUSERBASE") || strings.Contains(d, "/secret/value") {
		t.Fatalf("failing details = %q, want the keys named and no values", d)
	}
	if !strings.Contains(res.FixHint, "[check_script_env]") {
		t.Fatalf("fix hint = %q, want it to mention the table", res.FixHint)
	}
}

// TestConvergeTestCheckScriptEnvUsesTheCommandCityOnly pins two properties of
// the dry-run resolver: a city whose config does not load is an error rather
// than a silent run without the table, and the table comes from the city path
// the command runs in. The second matters for test-trigger, whose condition
// env is built from the bead's recorded city path; that path must not select
// whose [check_script_env] the dry run applies.
func TestConvergeTestCheckScriptEnvUsesTheCommandCityOnly(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("HOME", t.TempDir())
	city := t.TempDir()
	if _, err := convergeTestCheckScriptEnv(city); err == nil {
		t.Fatal("a city without city.toml must be an error, not a silent run without the table")
	}
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"c\"\n\n[check_script_env]\nPYTHONUSERBASE = \"$HOME/py\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := convergeTestCheckScriptEnv(city)
	if err != nil {
		t.Fatalf("convergeTestCheckScriptEnv: %v", err)
	}
	if want := filepath.Join(os.Getenv("HOME"), "py"); got["PYTHONUSERBASE"] != want {
		t.Fatalf("PYTHONUSERBASE = %q, want %q expanded against the controller HOME", got["PYTHONUSERBASE"], want)
	}
}

// TestConvergeDryRunsTakeCheckScriptEnvFromTheCommandCity drives the real
// `gc converge test-gate` and `test-trigger` commands. The bead's recorded city
// path points at a decoy city whose [check_script_env] sets MARKER=decoy; the
// city the command runs in sets MARKER=city. The condition scripts exit 0 only
// for MARKER=city, so a dry run that took the table from the bead's path (which
// test-trigger uses for the rest of its condition env) would fail. The live
// handler takes the table from the controller's own config, and the dry run
// must match it.
func TestConvergeDryRunsTakeCheckScriptEnvFromTheCommandCity(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	decoyDir := t.TempDir()
	cityToml := "[workspace]\nname = \"convtest\"\n\n" +
		"[beads]\nprovider = \"file\"\n\n" +
		"[session]\nprovider = \"fake\"\n\n" +
		"[check_script_env]\nMARKER = \"city\"\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(decoyDir, "city.toml"), []byte(
		"[workspace]\nname = \"decoy\"\n\n[check_script_env]\nMARKER = \"decoy\"\n"), 0o644); err != nil {
		t.Fatalf("write decoy city.toml: %v", err)
	}
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatalf("ensuring scoped file store layout: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatalf("ensuring city file store: %v", err)
	}
	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	scriptPath := filepath.Join(t.TempDir(), "marker.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\n[ \"${MARKER:-}\" = city ] || { echo \"MARKER=${MARKER:-}\" >&2; exit 1; }\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writing script: %v", err)
	}
	newLoop := func(state string) beads.Bead {
		t.Helper()
		bead, err := store.Create(beads.Bead{Title: "loop", Type: "convergence", Status: "in_progress"})
		if err != nil {
			t.Fatalf("creating convergence bead: %v", err)
		}
		for key, value := range map[string]string{
			convergence.FieldState:             state,
			convergence.FieldGateMode:          convergence.GateModeCondition,
			convergence.FieldGateCondition:     scriptPath,
			convergence.FieldTrigger:           convergence.TriggerEvent,
			convergence.FieldTriggerCondition:  scriptPath,
			convergence.FieldGateTimeout:       convergence.DefaultGateTimeout.String(),
			convergence.FieldGateTimeoutAction: convergence.TimeoutActionIterate,
			convergence.FieldIteration:         "1",
			convergence.FieldMaxIterations:     "3",
			convergence.FieldActiveWisp:        "wisp-1",
			convergence.FieldCityPath:          decoyDir,
		} {
			if err := store.SetMetadata(bead.ID, key, value); err != nil {
				t.Fatalf("setting %s: %v", key, err)
			}
		}
		return bead
	}

	prevCityFlag, prevRigFlag := cityFlag, rigFlag
	cityFlag, rigFlag = "", ""
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		rigFlag = prevRigFlag
	})
	t.Setenv("GC_CITY", cityDir)

	for _, tc := range []struct {
		name  string
		state string
		cmd   func(stdout, stderr io.Writer) *cobra.Command
	}{
		{"test-gate", convergence.StateActive, newConvergeTestGateCmd},
		{"test-trigger", convergence.StateWaitingTrigger, newConvergeTestTriggerCmd},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bead := newLoop(tc.state)
			var stdout, stderr bytes.Buffer
			cmd := tc.cmd(&stdout, &stderr)
			cmd.SetArgs([]string{bead.ID, "--json"})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("Execute: %v\nstderr:\n%s", err, stderr.String())
			}
			var result convergeTestGateJSONResult
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
				t.Fatalf("parsing %s JSON %q: %v", tc.name, stdout.String(), err)
			}
			if result.Outcome != convergence.GatePass {
				t.Fatalf("%s outcome = %q (stderr %q), want pass: the dry run must take [check_script_env] from the command's city, not the bead's recorded city path", tc.name, result.Outcome, result.Stderr)
			}
		})
	}
}
