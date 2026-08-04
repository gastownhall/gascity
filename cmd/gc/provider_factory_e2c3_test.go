package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

const e2c3ProviderConstructionFailure = "constructing session provider: injected provider failure"

func TestE2c3ProviderConstructionFailuresReturnThroughCallers(t *testing.T) {
	if cityPath, markerPath, ok := e2c3ProviderFailureHelperArgs(os.Args); ok {
		runE2c3ProviderFailureHelper(t, cityPath, markerPath)
		return
	}

	cityPath := writeE2c3ProviderFailureCity(t)
	markerPath := filepath.Join(t.TempDir(), "returned-through-callers")
	cmd := exec.Command(
		os.Args[0],
		"-test.run=^TestE2c3ProviderConstructionFailuresReturnThroughCallers$",
		"--",
		"e2c3-provider-failure-helper",
		cityPath,
		markerPath,
	)
	cmd.Dir = cityPath
	cmd.Env = e2c3ProviderFailureChildEnv(
		"GC_BEADS=file",
		"GC_BEADS_SCOPE_ROOT=",
		"GC_BOOTSTRAP=skip",
		"GC_CITY="+cityPath,
		"GC_CITY_PATH="+cityPath,
		"GC_CEILING_DIRECTORIES="+filepath.Dir(cityPath),
		"GC_DOLT=skip",
		"GC_HOME="+filepath.Join(filepath.Dir(cityPath), "gc-home"),
		"GC_SESSION=broken",
	)
	var processStdout, processStderr bytes.Buffer
	cmd.Stdout = &processStdout
	cmd.Stderr = &processStderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("provider failure helper did not return through every caller: %v; stdout=%q stderr=%q", err, processStdout.String(), processStderr.String())
	}
	marker, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("caller-return marker missing: %v", err)
	}
	if got, want := string(marker), "returned\n"; got != want {
		t.Fatalf("caller-return marker = %q, want %q", got, want)
	}
}

func e2c3ProviderFailureHelperArgs(args []string) (string, string, bool) {
	for index, arg := range args {
		if arg == "--" && index+4 == len(args) && args[index+1] == "e2c3-provider-failure-helper" {
			return args[index+2], args[index+3], true
		}
	}
	return "", "", false
}

func e2c3ProviderFailureChildEnv(extra ...string) []string {
	base := sanitizedBaseEnv(extra...)
	env := make([]string, 0, len(base)+1)
	for _, entry := range base {
		if strings.HasPrefix(entry, "OTEL_") {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "OTEL_SDK_DISABLED=true")
}

func writeE2c3ProviderFailureCity(t *testing.T) string {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("create rig path: %v", err)
	}
	cityTOML := `[workspace]

[beads]
provider = "file"

[[rigs]]
name = "frontend"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	writeCatalogFile(t, cityPath, ".gc/site.toml", fmt.Sprintf(`workspace_name = "test-city"

[[rig]]
name = "frontend"
path = %q
`, rigPath))
	writeBuiltinImportsFixture(t, cityPath, "core")
	writeCatalogFile(t, cityPath, "agents/worker/agent.toml", "dir = \"frontend\"\nstart_command = \"true\"\n")
	return cityPath
}

func runE2c3ProviderFailureHelper(t *testing.T, cityPath, markerPath string) {
	t.Helper()
	defer func() {
		if err := os.WriteFile(markerPath, []byte("returned\n"), 0o600); err != nil {
			t.Errorf("write caller-return marker: %v", err)
		}
	}()

	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_BOOTSTRAP", "skip")
	t.Setenv("GC_CITY", cityPath)
	t.Setenv("GC_CITY_PATH", cityPath)
	t.Setenv("GC_CEILING_DIRECTORIES", filepath.Dir(cityPath))
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_SESSION", "broken")

	oldBuild := buildSessionProviderByName
	providerBuilds := 0
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		providerBuilds++
		return nil, errors.New("injected provider failure")
	}
	defer func() { buildSessionProviderByName = oldBuild }()

	assertE2c3SessionMaterializationFailures(t, cityPath)
	assertE2c3ProviderBuilds(t, providerBuilds, 2, "session materialization")
	assertE2c3ControlDispatchFailures(t, cityPath)
	assertE2c3ProviderBuilds(t, providerBuilds, 4, "control dispatch")
	assertE2c3RigListFailure(t, cityPath)
	assertE2c3ProviderBuilds(t, providerBuilds, 5, "JSON rig list")
	assertE2c3DoctorFailureCheck(t, cityPath)
	assertE2c3ProviderBuilds(t, providerBuilds, 6, "doctor checks")
}

func assertE2c3SessionMaterializationFailures(t *testing.T, cityPath string) {
	t.Helper()

	namedStore := beads.NewMemStore()
	namedCfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:         "mayor",
			StartCommand: "true",
		}},
		NamedSessions: []config.NamedSession{{Template: "mayor"}},
	}
	if _, err := materializeSessionForTemplateWithOptions(cityPath, namedCfg, namedStore, "mayor", io.Discard, ensureSessionForTemplateOptions{}); err == nil || err.Error() != e2c3ProviderConstructionFailure {
		t.Fatalf("named-session materialization error = %v, want %q", err, e2c3ProviderConstructionFailure)
	}
	assertE2c3NoSessionBeads(t, namedStore, "named-session provider failure")

	agentStore := beads.NewMemStore()
	agentCfg := &config.Agent{Name: "worker", StartCommand: "true"}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{*agentCfg},
	}
	if _, err := materializeSessionForAgentConfig(cityPath, cfg, agentStore, agentCfg); err == nil || err.Error() != e2c3ProviderConstructionFailure {
		t.Fatalf("agent-session materialization error = %v, want %q", err, e2c3ProviderConstructionFailure)
	}
	assertE2c3NoSessionBeads(t, agentStore, "agent-session provider failure")
}

func assertE2c3NoSessionBeads(t *testing.T, store beads.Store, operation string) {
	t.Helper()
	items, err := store.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("list session beads after %s: %v", operation, err)
	}
	if len(items) != 0 {
		t.Fatalf("session beads after %s = %#v, want none", operation, items)
	}
}

func assertE2c3ControlDispatchFailures(t *testing.T, cityPath string) {
	t.Helper()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	for _, kind := range []string{"retry-eval", "retry"} {
		store := beads.NewMemStore()
		control, err := store.Create(beads.Bead{
			Title: "provider failure " + kind,
			Type:  "task",
			Metadata: map[string]string{
				"gc.kind": kind,
			},
		})
		if err != nil {
			t.Fatalf("create %s control bead: %v", kind, err)
		}
		before := control
		err = runControlDispatcherWithStoreAndConfig(cityPath, cityPath, store, control, control.ID, cfg, io.Discard, io.Discard)
		if err == nil || err.Error() != e2c3ProviderConstructionFailure {
			t.Fatalf("%s control dispatch error = %v, want %q", kind, err, e2c3ProviderConstructionFailure)
		}
		after, getErr := store.Get(control.ID)
		if getErr != nil {
			t.Fatalf("get %s control bead after provider failure: %v", kind, getErr)
		}
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("%s control bead changed after provider failure\n got: %#v\nwant: %#v", kind, after, before)
		}
	}
}

func assertE2c3RigListFailure(t *testing.T, cityPath string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := doRigList(fsys.OSFS{}, cityPath, true, &stdout, &stderr); code != 1 {
		t.Fatalf("JSON rig list provider failure code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var payload cliJSONErrorOutput
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode JSON rig list provider failure: %v; stdout=%q", err, stdout.String())
	}
	wantMessage := "gc rig list: " + e2c3ProviderConstructionFailure
	if payload.SchemaVersion != "1" || payload.OK || payload.Error.Code != "session_provider_failed" || payload.Error.Message != wantMessage || payload.Error.ExitCode != 1 {
		t.Fatalf("JSON rig list provider failure = %#v, want session_provider_failed message %q", payload, wantMessage)
	}
	var diagnostic cliJSONDiagnostic
	if err := json.Unmarshal(stderr.Bytes(), &diagnostic); err != nil {
		t.Fatalf("decode JSON rig list provider diagnostic: %v; stderr=%q", err, stderr.String())
	}
	if diagnostic.SchemaVersion != "1" || diagnostic.Level != "error" || diagnostic.Code != "session_provider_failed" || diagnostic.Message != wantMessage || diagnostic.ExitCode != 1 {
		t.Fatalf("JSON rig list provider diagnostic = %#v, want session_provider_failed message %q", diagnostic, wantMessage)
	}
}

func assertE2c3DoctorFailureCheck(t *testing.T, cityPath string) {
	t.Helper()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	checks := buildDoctorChecks(cityPath, cfg, nil, buildDoctorChecksOpts{
		ControllerRunning:    false,
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
	})

	var providerCheck doctor.Check
	for _, check := range checks {
		switch check.Name() {
		case "session-provider":
			providerCheck = check
		case "agent-sessions", "zombie-sessions", "orphan-sessions":
			t.Fatalf("provider-backed doctor check %q registered after provider construction failed", check.Name())
		}
	}
	if providerCheck == nil {
		t.Fatal("session-provider error check not registered after provider construction failed")
	}
	if providerCheck.WarmupEligible() {
		t.Fatal("session-provider construction error check is warmup eligible, want fail-open warmup exclusion")
	}
	result := providerCheck.Run(&doctor.CheckContext{CityPath: cityPath})
	if result.Status != doctor.StatusError || result.Severity != doctor.SeverityBlocking || result.Message != e2c3ProviderConstructionFailure {
		t.Fatalf("session-provider doctor result = %#v, want blocking error %q", result, e2c3ProviderConstructionFailure)
	}

	d := &doctor.Doctor{}
	d.Register(providerCheck)
	report := d.RunCollect(&doctor.CheckContext{CityPath: cityPath}, false)
	var stdout bytes.Buffer
	if err := writeDoctorJSON(&stdout, report); err != nil {
		t.Fatalf("write doctor provider failure JSON: %v", err)
	}
	var payload doctorJSONReport
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode doctor provider failure JSON: %v; stdout=%q", err, stdout.String())
	}
	if payload.Failed != 1 || payload.BlockingFailed != 1 || len(payload.Results) != 1 {
		t.Fatalf("doctor provider failure JSON summary = %#v, want one blocking failure", payload)
	}
	got := payload.Results[0]
	if got.Name != "session-provider" || got.Status != "error" || got.Severity != "blocking" || got.Message != e2c3ProviderConstructionFailure {
		t.Fatalf("doctor provider failure JSON result = %#v, want blocking session-provider error %q", got, e2c3ProviderConstructionFailure)
	}
}

func assertE2c3ProviderBuilds(t *testing.T, got, want int, operation string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s provider builds = %d, want %d cumulative", operation, got, want)
	}
}
