package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

const e2c1ProviderConstructionFailure = "constructing session provider: injected provider failure"

func TestE2c1ProviderConstructionFailuresReturnThroughRun(t *testing.T) {
	if cityPath, markerPath, ok := e2c1ProviderFailureHelperArgs(os.Args); ok {
		runE2c1ProviderFailureHelper(t, cityPath, markerPath)
		return
	}

	cityPath := writeE2c1ProviderFailureCity(t)
	markerPath := filepath.Join(t.TempDir(), "returned-through-run")
	cmd := exec.Command(
		os.Args[0],
		"-test.run=^TestE2c1ProviderConstructionFailuresReturnThroughRun$",
		"--",
		"e2c1-provider-failure-helper",
		cityPath,
		markerPath,
	)
	cmd.Dir = cityPath
	cmd.Env = e2c1ProviderFailureChildEnv(
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
		t.Fatalf("provider failure helper did not return through run: %v; stdout=%q stderr=%q", err, processStdout.String(), processStderr.String())
	}
	marker, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("run-return marker missing: %v", err)
	}
	if got, want := string(marker), "returned\n"; got != want {
		t.Fatalf("run-return marker = %q, want %q", got, want)
	}
}

func e2c1ProviderFailureHelperArgs(args []string) (string, string, bool) {
	for index, arg := range args {
		if arg == "--" && index+4 == len(args) && args[index+1] == "e2c1-provider-failure-helper" {
			return args[index+2], args[index+3], true
		}
	}
	return "", "", false
}

func e2c1ProviderFailureChildEnv(extra ...string) []string {
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

func writeE2c1ProviderFailureCity(t *testing.T) string {
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
	writeCatalogFile(t, cityPath, "agents/worker/agent.toml", "dir = \"frontend\"\n")

	return cityPath
}

func runE2c1ProviderFailureHelper(t *testing.T, cityPath, markerPath string) {
	t.Helper()
	defer func() {
		if err := os.WriteFile(markerPath, []byte("returned\n"), 0o600); err != nil {
			t.Errorf("write run-return marker: %v", err)
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
		// The fail-open start warm-up still uses the legacy doctor caller owned
		// by E2c3. Let that distinct construction complete so this slice reaches
		// and characterizes cmd_start.go's own provider boundary.
		if providerBuilds == 1 {
			return runtime.NewFake(), nil
		}
		return nil, errors.New("injected provider failure")
	}
	defer func() { buildSessionProviderByName = oldBuild }()

	oldShutdown := shutdownBeadsProviderForStop
	shutdownCalls := 0
	shutdownBeadsProviderForStop = func(string) error {
		shutdownCalls++
		return nil
	}
	defer func() { shutdownBeadsProviderForStop = oldShutdown }()

	startStderr := "gc start: warmup: 1 check(s) failed (Warning); see mail to mayor and `gc doctor` for details\n" +
		"gc start: " + e2c1ProviderConstructionFailure + "\n" + startSummaryLine(startSummary{
		PID:      currentSupervisorPID(),
		Binary:   startSummaryBinaryPath(),
		Build:    shortBuildHash(),
		Drift:    "unknown",
		Warnings: 0,
		Fatal:    "",
	}) + "\n"
	assertE2c1RunFailure(t, []string{"start", cityPath, "--foreground"}, "", startStderr)
	if providerBuilds != 2 {
		t.Fatalf("start provider builds = %d, want 2 (warm-up plus start)", providerBuilds)
	}
	if shutdownCalls != 0 {
		t.Fatalf("start provider failure triggered %d stop cleanup calls, want 0", shutdownCalls)
	}
	createE2c1StopOrderingBead(t, cityPath)

	assertE2c1RunFailure(t, []string{"stop", cityPath}, "", "gc stop: "+e2c1ProviderConstructionFailure+"\n")
	if providerBuilds != 3 {
		t.Fatalf("stop provider builds = %d, want 3 cumulative", providerBuilds)
	}
	if shutdownCalls != 0 {
		t.Fatalf("stop provider failure triggered %d bead shutdown calls, want 0", shutdownCalls)
	}
	assertE2c1StopMarkedBeforeProviderFailure(t, cityPath)

	const genericJSONFailure = "{\"schema_version\":\"1\",\"ok\":false,\"error\":{\"code\":\"command_failed\",\"message\":\"command failed; see stderr for diagnostics\",\"exit_code\":1}}\n"
	assertE2c1RunFailure(t, []string{"stop", cityPath, "--json"}, genericJSONFailure, "gc stop: "+e2c1ProviderConstructionFailure+"\n")
	if providerBuilds != 4 || shutdownCalls != 0 {
		t.Fatalf("JSON stop ordering: provider builds=%d shutdown calls=%d, want 4 and 0", providerBuilds, shutdownCalls)
	}

	assertE2c1RunFailure(t, []string{"restart", cityPath}, "", "gc stop: "+e2c1ProviderConstructionFailure+"\n")
	if providerBuilds != 5 || shutdownCalls != 0 {
		t.Fatalf("restart stop-leg ordering: provider builds=%d shutdown calls=%d, want 5 and 0", providerBuilds, shutdownCalls)
	}

	assertE2c1RunFailure(t, []string{"restart", cityPath, "--json"}, genericJSONFailure, "gc stop: "+e2c1ProviderConstructionFailure+"\n")
	if providerBuilds != 6 || shutdownCalls != 0 {
		t.Fatalf("JSON restart stop-leg ordering: provider builds=%d shutdown calls=%d, want 6 and 0", providerBuilds, shutdownCalls)
	}

	assertE2c1RunFailure(t, []string{"--city", cityPath, "rig", "restart", "frontend"}, "", "gc rig restart: "+e2c1ProviderConstructionFailure+"\n")
	if providerBuilds != 7 || shutdownCalls != 0 {
		t.Fatalf("rig restart ordering: provider builds=%d shutdown calls=%d, want 7 and 0", providerBuilds, shutdownCalls)
	}
}

func createE2c1StopOrderingBead(t *testing.T, cityPath string) {
	t.Helper()
	store, err := openScopeLocalFileStore(cityPath)
	if err != nil {
		t.Fatalf("open city store: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  "provider failure ordering target",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"alias":        "worker",
			"agent_name":   "frontend/worker",
			"template":     "frontend/worker",
			"session_name": "test-city--frontend--worker",
			"state":        "active",
		},
	}); err != nil {
		t.Fatalf("create session bead: %v", err)
	}
}

func assertE2c1RunFailure(t *testing.T, args []string, wantStdout, wantStderr string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 1 {
		t.Fatalf("run(%v) = %d, want 1; stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
	}
	if got := stdout.String(); got != wantStdout {
		t.Fatalf("run(%v) stdout = %q, want %q", args, got, wantStdout)
	}
	if got := stderr.String(); got != wantStderr {
		t.Fatalf("run(%v) stderr = %q, want %q", args, got, wantStderr)
	}
}

func assertE2c1StopMarkedBeforeProviderFailure(t *testing.T, cityPath string) {
	t.Helper()
	store, err := openScopeLocalFileStore(cityPath)
	if err != nil {
		t.Fatalf("open city store after stop provider failure: %v", err)
	}
	sessions, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("list session beads after stop provider failure: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("session bead count after stop provider failure = %d, want 1", len(sessions))
	}
	if got, want := sessions[0].Metadata["sleep_reason"], "city-stop"; got != want {
		t.Fatalf("sleep_reason after stop provider failure = %q, want %q (mark must precede provider construction)", got, want)
	}
}
