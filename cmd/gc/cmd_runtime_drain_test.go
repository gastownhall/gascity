package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
	sessiontmux "github.com/gastownhall/gascity/internal/runtime/tmux"
	"github.com/gastownhall/gascity/internal/session"
)

// drainOpsWithCountdown wraps fakeDrainOps and returns false for isRestartRequested
// after N calls, simulating the reconciler clearing the flag without concurrent map access.
type drainOpsWithCountdown struct {
	*fakeDrainOps
	remaining int
	cleared   bool
}

func (c *drainOpsWithCountdown) isRestartRequested(sessionName string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return false, c.err
	}
	if !c.restartRequested[sessionName] {
		if c.cleared {
			return false, nil
		}
		return false, errors.New("restart flag was not set before polling")
	}
	if c.remaining <= 0 {
		delete(c.restartRequested, sessionName)
		c.cleared = true
		return false, nil
	}
	c.remaining--
	return true, nil
}

// fakeDrainOps is a test double for drainOps.
type fakeDrainOps struct {
	mu               sync.Mutex
	draining         map[string]bool
	drainTimes       map[string]time.Time // when drain was set
	acked            map[string]bool
	restartRequested map[string]bool
	driftRestart     map[string]bool
	err              error // injected error for all ops
	restartReadErr   error
	setDrainCalls    []string
	clearDrainCalls  []string
}

func newFakeDrainOps() *fakeDrainOps {
	return &fakeDrainOps{
		draining:         make(map[string]bool),
		drainTimes:       make(map[string]time.Time),
		acked:            make(map[string]bool),
		restartRequested: make(map[string]bool),
		driftRestart:     make(map[string]bool),
	}
}

func TestProviderDrainOpsClearRestartRequestedUsesCityTmuxSocket(t *testing.T) {
	const socket = "bright-lights"
	argsFile := filepath.Join(t.TempDir(), "args")
	installFakeTmux(t, `printf '%s\n' "$@" >> "$FAKE_TMUX_ARGS"`)
	t.Setenv("FAKE_TMUX_ARGS", argsFile)

	sp, err := newSessionProviderForCityByName(
		nil,
		"tmux",
		config.SessionConfig{Socket: socket},
		"city",
		t.TempDir(),
	)
	if err != nil {
		t.Fatalf("newSessionProviderForCityByName: %v", err)
	}
	if err := newDrainOps(sp).clearRestartRequested("worker"); err != nil {
		t.Fatalf("clearRestartRequested: %v", err)
	}

	got, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read fake tmux args: %v", err)
	}
	// The environment, then the marker's user-option mirror (C7a-2b).
	want := "-u\n-L\n" + socket + "\nset-environment\n-t\n=worker\n-u\nGC_RESTART_REQUESTED\n" +
		"-u\n-L\n" + socket + "\nset-option\n-t\n=worker:\n-u\n@GC_RESTART_REQUESTED\n"
	if string(got) != want {
		t.Fatalf("tmux args = %q, want %q", got, want)
	}
}

func TestE2b2ProviderConstructionFailuresReturnThroughRun(t *testing.T) {
	if cityPath, sessionID, markerPath, ok := e2b2ProviderFailureHelperArgs(os.Args); ok {
		runE2b2ProviderFailureHelper(t, cityPath, sessionID, markerPath)
		return
	}

	cityPath, sessionID := writeE2b2ProviderFailureCity(t)
	markerPath := filepath.Join(t.TempDir(), "returned-through-run")
	cmd := exec.Command(
		os.Args[0],
		"-test.run=^TestE2b2ProviderConstructionFailuresReturnThroughRun$",
		"--",
		"e2b2-provider-failure-helper",
		cityPath,
		sessionID,
		markerPath,
	)
	cmd.Dir = cityPath
	cmd.Env = e2b2ProviderFailureChildEnv(
		"GC_BEADS=file",
		"GC_BEADS_SCOPE_ROOT=",
		"GC_CITY="+cityPath,
		"GC_CITY_PATH="+cityPath,
		"GC_CEILING_DIRECTORIES="+filepath.Dir(cityPath),
		"GC_HOME="+filepath.Join(filepath.Dir(cityPath), "gc-home"),
		"GC_SESSION=broken",
		"GC_ALIAS=worker",
		"GC_AGENT=worker",
		"GC_SESSION_ID="+sessionID,
		"GC_SESSION_NAME=test-city--frontend--worker",
		"GC_TMUX_SESSION=test-city--frontend--worker",
		"GC_INSTANCE_TOKEN=tok-provider-failure",
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

func e2b2ProviderFailureHelperArgs(args []string) (string, string, string, bool) {
	for index, arg := range args {
		if arg == "--" && index+5 == len(args) && args[index+1] == "e2b2-provider-failure-helper" {
			return args[index+2], args[index+3], args[index+4], true
		}
	}
	return "", "", "", false
}

func e2b2ProviderFailureChildEnv(extra ...string) []string {
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

func writeE2b2ProviderFailureCity(t *testing.T) (string, string) {
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

	store, err := openScopeLocalFileStore(cityPath)
	if err != nil {
		t.Fatalf("open city store: %v", err)
	}
	created, err := store.Create(beads.Bead{
		Title:  "runtime provider failure target",
		Type:   sessionBeadType,
		Labels: []string{"gc:session"},
		Metadata: map[string]string{
			"alias":        "worker",
			"agent_name":   "frontend/worker",
			"template":     "frontend/worker",
			"session_name": "test-city--frontend--worker",
			"state":        "awake",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	return cityPath, created.ID
}

func runE2b2ProviderFailureHelper(t *testing.T, cityPath, sessionID, markerPath string) {
	t.Helper()
	defer func() {
		if err := os.WriteFile(markerPath, []byte("returned\n"), 0o600); err != nil {
			t.Errorf("write run-return marker: %v", err)
		}
	}()
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_CITY", cityPath)
	t.Setenv("GC_CITY_PATH", cityPath)
	t.Setenv("GC_CEILING_DIRECTORIES", filepath.Dir(cityPath))
	t.Setenv("GC_SESSION", "broken")
	t.Setenv("GC_ALIAS", "worker")
	t.Setenv("GC_AGENT", "worker")
	t.Setenv("GC_SESSION_ID", sessionID)
	t.Setenv("GC_SESSION_NAME", "test-city--frontend--worker")
	t.Setenv("GC_TMUX_SESSION", "test-city--frontend--worker")
	t.Setenv("GC_INSTANCE_TOKEN", "tok-provider-failure")

	oldBuild := buildSessionProviderByName
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return nil, errors.New("injected provider failure")
	}
	defer func() { buildSessionProviderByName = oldBuild }()

	const constructionFailure = "constructing session provider: injected provider failure"
	// drain-check answers unknown (exit 3) rather than failing: the provider is
	// one of its two sources, and the session row still answered.
	const drainCheckUnknown = `gc runtime drain-check: drain state of "worker" unknown: runtime_meta: opening the session provider: ` + constructionFailure
	tests := []struct {
		name             string
		args             []string
		wantCommand      string
		wantJSONCode     string
		wantJSONMessage  string
		wantJSONDiagCode string
		wantExit         int    // 0 means 1
		wantMessage      string // "" means wantCommand + ": " + constructionFailure
	}{
		{name: "runtime drain text", args: []string{"--city", cityPath, "runtime", "drain", sessionID}, wantCommand: "gc runtime drain"},
		{name: "runtime drain json", args: []string{"--city", cityPath, "runtime", "drain", sessionID, "--json"}, wantCommand: "gc runtime drain", wantJSONCode: "command_failed", wantJSONMessage: "command failed; see stderr for diagnostics"},
		{name: "runtime undrain text", args: []string{"--city", cityPath, "runtime", "undrain", sessionID}, wantCommand: "gc runtime undrain"},
		{name: "runtime undrain json", args: []string{"--city", cityPath, "runtime", "undrain", sessionID, "--json"}, wantCommand: "gc runtime undrain", wantJSONCode: "command_failed", wantJSONMessage: "command failed; see stderr for diagnostics"},
		{name: "runtime drain-check explicit text", args: []string{"--city", cityPath, "runtime", "drain-check", sessionID}, wantCommand: "gc runtime drain-check", wantExit: drainCheckUnknownExit, wantMessage: drainCheckUnknown},
		{name: "runtime drain-check explicit json", args: []string{"--city", cityPath, "runtime", "drain-check", sessionID, "--json"}, wantCommand: "gc runtime drain-check", wantJSONCode: drainCheckUnknownCode, wantJSONMessage: drainCheckUnknown, wantJSONDiagCode: drainCheckUnknownCode, wantExit: drainCheckUnknownExit},
		{name: "runtime drain-check context text", args: []string{"--city", cityPath, "runtime", "drain-check"}, wantCommand: "gc runtime drain-check", wantExit: drainCheckUnknownExit, wantMessage: drainCheckUnknown},
		{name: "runtime drain-check context json", args: []string{"--city", cityPath, "runtime", "drain-check", "--json"}, wantCommand: "gc runtime drain-check", wantJSONCode: drainCheckUnknownCode, wantJSONMessage: drainCheckUnknown, wantJSONDiagCode: drainCheckUnknownCode, wantExit: drainCheckUnknownExit},
		{name: "runtime drain-ack explicit text", args: []string{"--city", cityPath, "runtime", "drain-ack", sessionID}, wantCommand: "gc runtime drain-ack"},
		{name: "runtime drain-ack explicit json", args: []string{"--city", cityPath, "runtime", "drain-ack", sessionID, "--json"}, wantCommand: "gc runtime drain-ack", wantJSONCode: "command_failed", wantJSONMessage: "command failed; see stderr for diagnostics"},
		{name: "runtime drain-ack context text", args: []string{"--city", cityPath, "runtime", "drain-ack"}, wantCommand: "gc runtime drain-ack"},
		{name: "runtime drain-ack context json", args: []string{"--city", cityPath, "runtime", "drain-ack", "--json"}, wantCommand: "gc runtime drain-ack", wantJSONCode: "command_failed", wantJSONMessage: "command failed; see stderr for diagnostics"},
		{name: "runtime request-restart text", args: []string{"--city", cityPath, "runtime", "request-restart"}, wantCommand: "gc runtime request-restart", wantMessage: "gc runtime request-restart: restart not recorded: " + constructionFailure},
		{name: "rig status text", args: []string{"--city", cityPath, "rig", "status", "frontend"}, wantCommand: "gc rig status"},
		{name: "rig status json", args: []string{"--city", cityPath, "rig", "status", "frontend", "--json"}, wantCommand: "gc rig status", wantJSONCode: "command_failed", wantJSONMessage: "command failed; see stderr for diagnostics"},
		{name: "city status text", args: []string{"--city", cityPath, "status"}, wantCommand: "gc status"},
		{name: "city status json flag", args: []string{"--city", cityPath, "status", "--json"}, wantCommand: "gc status", wantJSONCode: "session_provider_failed", wantJSONMessage: "gc status: " + constructionFailure, wantJSONDiagCode: "session_provider_failed"},
		{name: "city status json format", args: []string{"--city", cityPath, "status", "--format=json"}, wantCommand: "gc status", wantJSONCode: "session_provider_failed", wantJSONMessage: "gc status: " + constructionFailure, wantJSONDiagCode: "session_provider_failed"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wantExit := tc.wantExit
			if wantExit == 0 {
				wantExit = 1
			}
			var stdout, stderr bytes.Buffer
			if code := run(tc.args, &stdout, &stderr); code != wantExit {
				t.Fatalf("run(%v) = %d, want %d; stdout=%q stderr=%q", tc.args, code, wantExit, stdout.String(), stderr.String())
			}

			wantMessage := tc.wantMessage
			if wantMessage == "" {
				wantMessage = tc.wantCommand + ": " + constructionFailure
			}
			if tc.wantJSONCode == "" {
				if got := stdout.String(); got != "" {
					t.Fatalf("stdout = %q, want empty", got)
				}
				if got, want := stderr.String(), wantMessage+"\n"; got != want {
					t.Fatalf("stderr = %q, want %q", got, want)
				}
				return
			}

			var output cliJSONErrorOutput
			if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
				t.Fatalf("stdout is not a structured JSON failure: %v; stdout=%q", err, stdout.String())
			}
			if output.OK || output.Error.Code != tc.wantJSONCode || output.Error.ExitCode != wantExit || output.Error.Message != tc.wantJSONMessage {
				t.Fatalf("JSON failure = %#v, want code=%q message=%q exit_code=%d", output, tc.wantJSONCode, tc.wantJSONMessage, wantExit)
			}
			if tc.wantJSONDiagCode == "" {
				if got, want := stderr.String(), wantMessage+"\n"; got != want {
					t.Fatalf("stderr = %q, want %q", got, want)
				}
				return
			}
			var diagnostic cliJSONDiagnostic
			if err := json.Unmarshal(stderr.Bytes(), &diagnostic); err != nil {
				t.Fatalf("stderr is not a structured JSON diagnostic: %v; stderr=%q", err, stderr.String())
			}
			if diagnostic.Code != tc.wantJSONDiagCode || diagnostic.Message != tc.wantJSONMessage || diagnostic.ExitCode != wantExit {
				t.Fatalf("JSON diagnostic = %#v, want code=%q message=%q exit_code=%d", diagnostic, tc.wantJSONDiagCode, tc.wantJSONMessage, wantExit)
			}
		})
	}
}

func (f *fakeDrainOps) setDrain(sessionName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setDrainCalls = append(f.setDrainCalls, sessionName)
	if f.err != nil {
		return f.err
	}
	f.draining[sessionName] = true
	f.drainTimes[sessionName] = time.Now()
	return nil
}

func (f *fakeDrainOps) clearDrain(sessionName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clearDrainCalls = append(f.clearDrainCalls, sessionName)
	if f.err != nil {
		return f.err
	}
	delete(f.draining, sessionName)
	delete(f.drainTimes, sessionName)
	return nil
}

func (f *fakeDrainOps) isDraining(sessionName string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	return f.draining[sessionName], nil
}

func (f *fakeDrainOps) drainStartTime(sessionName string) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return time.Time{}, f.err
	}
	t, ok := f.drainTimes[sessionName]
	if !ok {
		return time.Time{}, fmt.Errorf("no drain time for %s", sessionName)
	}
	return t, nil
}

func (f *fakeDrainOps) setDrainAck(sessionName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.acked[sessionName] = true
	return nil
}

func (f *fakeDrainOps) isDrainAcked(sessionName string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	return f.acked[sessionName], nil
}

func (f *fakeDrainOps) setRestartRequested(sessionName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.restartRequested[sessionName] = true
	return nil
}

func (f *fakeDrainOps) isRestartRequested(sessionName string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	if f.restartReadErr != nil {
		return false, f.restartReadErr
	}
	return f.restartRequested[sessionName], nil
}

func (f *fakeDrainOps) clearRestartRequested(sessionName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	delete(f.restartRequested, sessionName)
	return nil
}

func (f *fakeDrainOps) setDriftRestart(sessionName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.driftRestart[sessionName] = true
	return nil
}

func (f *fakeDrainOps) isDriftRestart(sessionName string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	return f.driftRestart[sessionName], nil
}

func (f *fakeDrainOps) clearDriftRestart(sessionName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	delete(f.driftRestart, sessionName)
	return nil
}

// ---------------------------------------------------------------------------
// doRuntimeDrain tests
// ---------------------------------------------------------------------------

func TestDoRuntimeDrain(t *testing.T) {
	dops := newFakeDrainOps()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}

	rec := events.NewFake()
	var stdout, stderr bytes.Buffer
	code := doRuntimeDrain(dops, sp, rec, "worker", "worker", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !dops.draining["worker"] {
		t.Error("drain flag not set")
	}
	if got := stdout.String(); got != "Draining session 'worker'\n" {
		t.Errorf("stdout = %q, want %q", got, "Draining session 'worker'\n")
	}
	if len(rec.Events) != 1 || rec.Events[0].Type != events.SessionDraining {
		t.Errorf("events = %v, want one SessionDraining event", rec.Events)
	}
	if rec.Events[0].Subject != "worker" {
		t.Errorf("event subject = %q, want %q", rec.Events[0].Subject, "worker")
	}
}

func TestDoRuntimeDrainNotRunning(t *testing.T) {
	dops := newFakeDrainOps()
	sp := runtime.NewFake() // no sessions started

	var stdout, stderr bytes.Buffer
	code := doRuntimeDrain(dops, sp, events.Discard, "worker", "worker", false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if got := stderr.String(); got != "gc runtime drain: session \"worker\" is not running\n" {
		t.Errorf("stderr = %q", got)
	}
}

func TestDoRuntimeDrainSetError(t *testing.T) {
	dops := newFakeDrainOps()
	dops.err = errors.New("tmux borked")
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRuntimeDrain(dops, sp, events.Discard, "worker", "worker", false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if got := stderr.String(); got != "gc runtime drain: tmux borked\n" {
		t.Errorf("stderr = %q", got)
	}
}

func TestDoRuntimeDrainJSON(t *testing.T) {
	dops := newFakeDrainOps()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRuntimeDrain(dops, sp, events.Discard, "worker", "worker", true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	var result runtimeActionJSON
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.SchemaVersion != "1" || result.Command != "runtime drain" || result.Session != "worker" || result.Status != "draining" {
		t.Fatalf("unexpected JSON result: %+v", result)
	}
}

// ---------------------------------------------------------------------------
// doRuntimeUndrain tests
// ---------------------------------------------------------------------------

func TestDoRuntimeUndrain(t *testing.T) {
	dops := newFakeDrainOps()
	dops.draining["worker"] = true
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}

	rec := events.NewFake()
	var stdout, stderr bytes.Buffer
	code := doRuntimeUndrain(dops, sp, rec, "worker", "worker", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if dops.draining["worker"] {
		t.Error("drain flag still set after undrain")
	}
	if got := stdout.String(); got != "Undrained session 'worker'\n" {
		t.Errorf("stdout = %q, want %q", got, "Undrained session 'worker'\n")
	}
	if len(rec.Events) != 1 || rec.Events[0].Type != events.SessionUndrained {
		t.Errorf("events = %v, want one SessionUndrained event", rec.Events)
	}
}

func TestDoRuntimeUndrainNotRunning(t *testing.T) {
	dops := newFakeDrainOps()
	sp := runtime.NewFake()

	var stdout, stderr bytes.Buffer
	code := doRuntimeUndrain(dops, sp, events.Discard, "worker", "worker", false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if got := stderr.String(); got != "gc runtime undrain: session \"worker\" is not running\n" {
		t.Errorf("stderr = %q", got)
	}
}

func TestDoRuntimeUndrainJSON(t *testing.T) {
	dops := newFakeDrainOps()
	dops.draining["worker"] = true
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRuntimeUndrain(dops, sp, events.Discard, "worker", "worker", true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	var result runtimeActionJSON
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.SchemaVersion != "1" || result.Command != "runtime undrain" || result.Session != "worker" || result.Status != "undrained" {
		t.Fatalf("unexpected JSON result: %+v", result)
	}
}

// ---------------------------------------------------------------------------
// doRuntimeDrainCheck tests
// ---------------------------------------------------------------------------

// drainCheckRow returns a row-reader seam answering draining or err.
func drainCheckRow(draining bool, err error) func() (bool, error) {
	return func() (bool, error) { return draining, err }
}

// drainCheckRowMustNotRun fails the test if the row is read.
func drainCheckRowMustNotRun(t *testing.T) func() (bool, error) {
	t.Helper()
	return func() (bool, error) {
		t.Fatal("session row read after the runtime already answered draining")
		return false, nil
	}
}

func TestDoRuntimeDrainCheckMetaDraining(t *testing.T) {
	dops := newFakeDrainOps()
	dops.draining["worker"] = true

	code := doRuntimeDrainCheck(dops, drainCheckRowMustNotRun(t), "worker", "worker", false, &bytes.Buffer{}, &bytes.Buffer{})
	if code != 0 {
		t.Errorf("code = %d, want 0 (draining)", code)
	}
}

// The reconciler drains through the session row and never sets GC_DRAIN; a
// drain-check that reads only provider meta misses every such drain.
func TestDoRuntimeDrainCheckRowDrainingWithMetaClear(t *testing.T) {
	dops := newFakeDrainOps()

	var stderr bytes.Buffer
	code := doRuntimeDrainCheck(dops, drainCheckRow(true, nil), "worker", "worker", false, &bytes.Buffer{}, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0 (row draining); stderr: %s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestDoRuntimeDrainCheckMetaDrainingDespiteRowError(t *testing.T) {
	dops := newFakeDrainOps()
	dops.draining["worker"] = true

	code := doRuntimeDrainCheck(dops, drainCheckRow(false, errors.New("ledger unreachable")), "worker", "worker", false, &bytes.Buffer{}, &bytes.Buffer{})
	if code != 0 {
		t.Errorf("code = %d, want 0 (meta draining)", code)
	}
}

func TestDoRuntimeDrainCheckRowDrainingDespiteMetaError(t *testing.T) {
	dops := newFakeDrainOps()
	dops.err = errors.New("provider transport: connection refused")

	code := doRuntimeDrainCheck(dops, drainCheckRow(true, nil), "worker", "worker", false, &bytes.Buffer{}, &bytes.Buffer{})
	if code != 0 {
		t.Errorf("code = %d, want 0 (row draining)", code)
	}
}

func TestDoRuntimeDrainCheckNotDraining(t *testing.T) {
	dops := newFakeDrainOps()

	var stderr bytes.Buffer
	code := doRuntimeDrainCheck(dops, drainCheckRow(false, nil), "worker", "worker", false, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("code = %d, want 1 (not draining)", code)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestDoRuntimeDrainCheckRowErrorIsUnknown(t *testing.T) {
	dops := newFakeDrainOps()

	var stdout, stderr bytes.Buffer
	code := doRuntimeDrainCheck(dops, drainCheckRow(false, errors.New("ledger unreachable")), "worker", "worker", false, &stdout, &stderr)
	if code != drainCheckUnknownExit {
		t.Fatalf("code = %d, want %d (unknown)", code, drainCheckUnknownExit)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], drainCheckSourceRow) || !strings.Contains(lines[0], "ledger unreachable") {
		t.Errorf("stderr = %q, want one line naming %s and its error", stderr.String(), drainCheckSourceRow)
	}
}

func TestDoRuntimeDrainCheckMetaTransportErrorIsUnknown(t *testing.T) {
	dops := newFakeDrainOps()
	dops.err = errors.New("provider transport: connection refused")

	var stderr bytes.Buffer
	code := doRuntimeDrainCheck(dops, drainCheckRow(false, nil), "worker", "worker", false, &bytes.Buffer{}, &stderr)
	if code != drainCheckUnknownExit {
		t.Fatalf("code = %d, want %d (unknown)", code, drainCheckUnknownExit)
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], drainCheckSourceRuntime) || strings.Contains(lines[0], drainCheckSourceRow+":") {
		t.Errorf("stderr = %q, want one line naming only %s", stderr.String(), drainCheckSourceRuntime)
	}
}

func TestDoRuntimeDrainCheckBothErrorsIsUnknown(t *testing.T) {
	dops := newFakeDrainOps()
	dops.err = errors.New("provider transport: connection refused")

	var stderr bytes.Buffer
	code := doRuntimeDrainCheck(dops, drainCheckRow(false, errors.New("ledger unreachable")), "worker", "worker", false, &bytes.Buffer{}, &stderr)
	if code != drainCheckUnknownExit {
		t.Fatalf("code = %d, want %d (unknown)", code, drainCheckUnknownExit)
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], drainCheckSourceRow) || !strings.Contains(lines[0], drainCheckSourceRuntime) {
		t.Errorf("stderr = %q, want one line naming both sources", stderr.String())
	}
}

// A runtime that is absent, or that keeps no metadata, has definitively no
// GC_DRAIN: that is an answer, not an error.
func TestDoRuntimeDrainCheckRuntimeAbsentIsAnAnswer(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "session not found", err: fmt.Errorf("reading GC_DRAIN: %w", runtime.ErrSessionNotFound)},
		// The tmux adapter's GetMeta shape for a missing session.
		{name: "tmux session gone", err: fmt.Errorf("reading GC_DRAIN: %w", fmt.Errorf("reading %s from session %q: %w: %w", "GC_DRAIN", "worker", runtime.ErrSessionNotFound, sessiontmux.ErrSessionNotFound))},
		{name: "meta unsupported", err: fmt.Errorf("reading GC_DRAIN: %w", runtime.ErrMetaUnsupported)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dops := newFakeDrainOps()
			dops.err = tc.err

			var stderr bytes.Buffer
			code := doRuntimeDrainCheck(dops, drainCheckRow(false, nil), "worker", "worker", false, &bytes.Buffer{}, &stderr)
			if code != 1 {
				t.Fatalf("code = %d, want 1 (not draining); stderr: %s", code, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

// Only typed absence is an answer. An error that merely reads like absence —
// an exec adapter's stderr — or a runtime that could not be observed at all is
// a failure to answer, whatever its text says.
func TestDoRuntimeDrainCheckUntypedRuntimeErrorsAreUnknown(t *testing.T) {
	writeAdapter := func(t *testing.T, stderrLine string) runtime.Provider {
		t.Helper()
		script := filepath.Join(t.TempDir(), "adapter")
		body := fmt.Sprintf("#!/bin/sh\necho %q >&2\necho 'second line' >&2\nexit 1\n", stderrLine)
		if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
			t.Fatalf("write adapter: %v", err)
		}
		return sessionexec.NewProvider(script)
	}
	for _, tc := range []struct {
		name string
		dops func(t *testing.T) drainOps
	}{
		{name: "exec command not found", dops: func(t *testing.T) drainOps {
			return newDrainOps(writeAdapter(t, "boxctl: command not found"))
		}},
		{name: "exec endpoint not found", dops: func(t *testing.T) drainOps {
			return newDrainOps(writeAdapter(t, "HTTP 404: endpoint not found"))
		}},
		{name: "exec daemon not running", dops: func(t *testing.T) drainOps {
			return newDrainOps(writeAdapter(t, "box daemon not running"))
		}},
		{name: "tmux no server", dops: func(*testing.T) drainOps {
			dops := newFakeDrainOps()
			// The tmux adapter's GetMeta shape when no server is running.
			dops.err = fmt.Errorf("reading GC_DRAIN: %w", fmt.Errorf("reading %s from session %q: %w: %w", "GC_DRAIN", "worker", runtime.ErrRuntimeUnavailable, sessiontmux.ErrNoServer))
			return dops
		}},
		// A session-not-found that also says the runtime could not be
		// observed is not an answer of absence.
		{name: "not found joined with unavailable", dops: func(*testing.T) drainOps {
			dops := newFakeDrainOps()
			dops.err = fmt.Errorf("reading GC_DRAIN: %w", fmt.Errorf("reading %s from session %q: %w: %w", "GC_DRAIN", "worker", runtime.ErrSessionNotFound, runtime.ErrRuntimeUnavailable))
			return dops
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := doRuntimeDrainCheck(tc.dops(t), drainCheckRow(false, nil), "worker", "worker", false, &bytes.Buffer{}, &stderr)
			if code != drainCheckUnknownExit {
				t.Fatalf("code = %d, want %d (unknown); stderr: %s", code, drainCheckUnknownExit, stderr.String())
			}
			if got := stderr.String(); strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, "\n") || !strings.Contains(got, drainCheckSourceRuntime+":") {
				t.Fatalf("stderr = %q, want exactly one line naming %s", got, drainCheckSourceRuntime)
			}
		})
	}
}

func TestDoRuntimeDrainCheckRuntimeAbsentRowDraining(t *testing.T) {
	dops := newFakeDrainOps()
	dops.err = runtime.ErrSessionNotFound

	code := doRuntimeDrainCheck(dops, drainCheckRow(true, nil), "worker", "worker", false, &bytes.Buffer{}, &bytes.Buffer{})
	if code != 0 {
		t.Errorf("code = %d, want 0 (row draining)", code)
	}
}

func decodeDrainCheckJSON(t *testing.T, data []byte) runtimeDrainCheckJSON {
	t.Helper()
	var result runtimeDrainCheckJSON
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, string(data))
	}
	validateJSONAgainstResultSchema(t, []string{"runtime", "drain-check"}, data)
	return result
}

func TestDoRuntimeDrainCheckJSON(t *testing.T) {
	for _, tc := range []struct {
		name       string
		meta, row  bool
		wantSource string
	}{
		{name: "meta", meta: true, wantSource: drainCheckSourceRuntime},
		{name: "row", row: true, wantSource: drainCheckSourceRow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dops := newFakeDrainOps()
			dops.draining["worker"] = tc.meta

			var stdout, stderr bytes.Buffer
			code := doRuntimeDrainCheck(dops, drainCheckRow(tc.row, nil), "worker-alias", "worker", true, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
			result := decodeDrainCheckJSON(t, stdout.Bytes())
			if result.SchemaVersion != "1" ||
				!result.OK ||
				result.Command != "runtime drain-check" ||
				!result.Draining ||
				result.Session != "worker" ||
				result.Target != "worker-alias" ||
				result.Source != tc.wantSource {
				t.Fatalf("unexpected JSON result: %+v", result)
			}
		})
	}
}

func TestDoRuntimeDrainCheckJSONNotDrainingWritesFalseResult(t *testing.T) {
	dops := newFakeDrainOps()

	var stdout, stderr bytes.Buffer
	code := doRuntimeDrainCheck(dops, drainCheckRow(false, nil), "worker", "worker", true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1 for shell-condition false; stderr: %s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	result := decodeDrainCheckJSON(t, stdout.Bytes())
	if result.SchemaVersion != "1" ||
		!result.OK ||
		result.Command != "runtime drain-check" ||
		result.Draining ||
		result.Session != "worker" ||
		result.Source != drainCheckSourceBoth {
		t.Fatalf("unexpected JSON result: %+v", result)
	}
}

func TestDoRuntimeDrainCheckJSONUnknownWritesErrorEnvelope(t *testing.T) {
	dops := newFakeDrainOps()

	var stdout, stderr bytes.Buffer
	code := doRuntimeDrainCheck(dops, drainCheckRow(false, errors.New("ledger unreachable")), "worker", "worker", true, &stdout, &stderr)
	if code != drainCheckUnknownExit {
		t.Fatalf("code = %d, want %d", code, drainCheckUnknownExit)
	}
	var result cliJSONErrorOutput
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.SchemaVersion != "1" ||
		result.OK ||
		result.Error.Code != drainCheckUnknownCode ||
		result.Error.ExitCode != drainCheckUnknownExit ||
		!strings.Contains(result.Error.Message, drainCheckSourceRow) {
		t.Fatalf("unexpected JSON error: %+v", result)
	}
}

// newDrainCheckRowStore returns an in-memory store holding one session row
// in state, and the row's id.
func newDrainCheckRowStore(t *testing.T, sessionName string, state session.State) (beads.Store, string) {
	t.Helper()
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{
		Title:  "session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": sessionName,
			"template":     "worker",
			"state":        string(state),
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	return store, b.ID
}

func TestDrainCheckRowDrainingReadsRowState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state session.State
		want  bool
	}{
		{name: "draining", state: session.StateDraining, want: true},
		{name: "active", state: session.StateActive, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, id := newDrainCheckRowStore(t, "worker", tc.state)
			front := sessionFrontDoor(store)
			for _, form := range []struct {
				name, sessionID string
			}{
				{name: "by id", sessionID: id},
				{name: "by session name", sessionID: ""},
			} {
				got, err := drainCheckRowDraining(front, "worker", form.sessionID)
				if err != nil {
					t.Fatalf("%s: drainCheckRowDraining: %v", form.name, err)
				}
				if got != tc.want {
					t.Fatalf("%s: draining = %v, want %v", form.name, got, tc.want)
				}
			}
		})
	}
}

// A closed row is not draining, read by id or by name, even when its last
// state said draining.
func TestDrainCheckRowDrainingClosedRowIsNotDraining(t *testing.T) {
	store, id := newDrainCheckRowStore(t, "worker", session.StateDraining)
	if err := store.Close(id); err != nil {
		t.Fatalf("close row: %v", err)
	}
	front := sessionFrontDoor(store)
	for _, sessionID := range []string{id, ""} {
		got, err := drainCheckRowDraining(front, "worker", sessionID)
		if err != nil {
			t.Fatalf("sessionID %q: drainCheckRowDraining: %v", sessionID, err)
		}
		if got {
			t.Fatalf("sessionID %q: closed row reads as draining", sessionID)
		}
	}
}

func TestDrainCheckRowDrainingMissingRowIsAnError(t *testing.T) {
	store, _ := newDrainCheckRowStore(t, "worker", session.StateActive)
	front := sessionFrontDoor(store)
	if _, err := drainCheckRowDraining(front, "someone-else", ""); err == nil {
		t.Fatal("drainCheckRowDraining for an unknown session returned no error")
	}
	if _, err := drainCheckRowDraining(front, "worker", "gc-missing"); err == nil {
		t.Fatal("drainCheckRowDraining for an unknown id returned no error")
	}
}

// A city config that will not load leaves the session row unread: the row
// source reports the failure, so drain-check answers unknown instead of
// reading the row without the config's session-class routing.
func TestDrainCheckRowReaderConfigLoadFailureIsUnknown(t *testing.T) {
	store, _ := newDrainCheckRowStore(t, "worker", session.StateActive)
	swapDrainCheckSessionLeg(t, store)
	original := drainCheckOpenSessionLeg
	legOpened := false
	drainCheckOpenSessionLeg = func(path string, cfg *config.City) (beads.Store, error) {
		legOpened = true
		return original(path, cfg)
	}
	readRow := drainCheckRowReader(t.TempDir(), func() (*config.City, error) {
		return nil, errors.New("city.toml: parse error")
	}, "worker", "")

	var stdout, stderr bytes.Buffer
	code := doRuntimeDrainCheck(newFakeDrainOps(), readRow, "worker", "worker", false, &stdout, &stderr)
	if code != drainCheckUnknownExit {
		t.Fatalf("code = %d, want %d (unknown); stderr: %s", code, drainCheckUnknownExit, stderr.String())
	}
	if legOpened {
		t.Error("session leg opened although the city config did not load")
	}
	if got := stderr.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, drainCheckSourceRow+":") || !strings.Contains(got, "city.toml: parse error") {
		t.Errorf("stderr = %q, want one line naming %s and the config error", got, drainCheckSourceRow)
	}
}

func TestCmdRuntimeDrainCheckSelfReadsRowByEnvSessionID(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"drain-check\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	front, id := newDrainCheckRowStore(t, "worker-pane", session.StateDraining)
	gotCity := swapDrainCheckSessionLeg(t, front)
	t.Setenv("GC_CITY", cityPath)
	t.Setenv("GC_ALIAS", "worker")
	t.Setenv("GC_SESSION_ID", id)
	t.Setenv("GC_SESSION_NAME", "not-the-row-name")
	t.Setenv("GC_TMUX_SESSION", "")

	target, rowName, err := currentDrainCheckTarget()
	if err != nil {
		t.Fatalf("currentDrainCheckTarget: %v", err)
	}
	if target.sessionID != id {
		t.Fatalf("target.sessionID = %q, want %q", target.sessionID, id)
	}
	draining, err := drainCheckRowReader(target.cityPath, func() (*config.City, error) { return nil, nil }, rowName, target.sessionID)()
	if err != nil {
		t.Fatalf("row reader: %v", err)
	}
	if !draining {
		t.Fatal("row reader = not draining, want draining")
	}
	wantCity, err := filepath.EvalSymlinks(cityPath)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(*gotCity); err != nil || resolved != wantCity {
		t.Fatalf("session leg opened at %q, want %q", *gotCity, cityPath)
	}
}

// In a container GC_TMUX_SESSION names the tmux session inside it ("main"),
// not the row; without GC_SESSION_ID the row is found by GC_SESSION_NAME.
func TestCurrentDrainCheckTargetRowNameIgnoresTmuxSession(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"drain-check\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_CITY", cityPath)
	t.Setenv("GC_ALIAS", "worker")
	t.Setenv("GC_SESSION_ID", "")
	t.Setenv("GC_TMUX_SESSION", "main")

	t.Setenv("GC_SESSION_NAME", "city--worker")
	target, rowName, err := currentDrainCheckTarget()
	if err != nil {
		t.Fatalf("currentDrainCheckTarget: %v", err)
	}
	if target.sessionName != "main" || rowName != "city--worker" {
		t.Fatalf("meta name = %q, row name = %q; want meta %q, row %q", target.sessionName, rowName, "main", "city--worker")
	}

	t.Setenv("GC_SESSION_NAME", "")
	if _, rowName, err = currentDrainCheckTarget(); err != nil || rowName != "worker" {
		t.Fatalf("row name = %q, err = %v; want the display identity %q", rowName, err, "worker")
	}
}

func TestDrainCheckNoContextExitsOne(t *testing.T) {
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_SESSION_ID", "")
	t.Setenv("GC_AGENT", "")
	t.Setenv("GC_CITY", "")
	for _, jsonOutput := range []bool{false, true} {
		var stdout, stderr bytes.Buffer
		if code := cmdRuntimeDrainCheck(nil, jsonOutput, &stdout, &stderr); code != 1 {
			t.Fatalf("json=%v: code = %d, want 1 (no session context)", jsonOutput, code)
		}
		if stdout.Len() != 0 || stderr.Len() != 0 {
			t.Fatalf("json=%v: stdout=%q stderr=%q, want both empty", jsonOutput, stdout.String(), stderr.String())
		}
	}
}

// swapDrainCheckSessionLeg makes drain-check's session leg store and returns
// where the city path it was opened at is recorded.
func swapDrainCheckSessionLeg(t *testing.T, store beads.Store) *string {
	t.Helper()
	original := drainCheckOpenSessionLeg
	t.Cleanup(func() { drainCheckOpenSessionLeg = original })
	var gotCity string
	drainCheckOpenSessionLeg = func(path string, _ *config.City) (beads.Store, error) {
		gotCity = path
		return store, nil
	}
	return &gotCity
}

// drainCheckNamedCity writes a file-store city holding the E2b2 session row
// "worker" and points the process at it with no session context and the
// fake session provider.
func drainCheckNamedCity(t *testing.T) (string, string) {
	t.Helper()
	cityPath, sessionID := writeE2b2ProviderFailureCity(t)
	t.Setenv("GC_CITY", cityPath)
	t.Setenv("GC_CITY_PATH", cityPath)
	t.Setenv("GC_CEILING_DIRECTORIES", filepath.Dir(cityPath))
	t.Setenv("GC_SESSION", "fake")
	for _, key := range []string{"GC_ALIAS", "GC_AGENT", "GC_SESSION_ID", "GC_SESSION_NAME", "GC_TMUX_SESSION"} {
		t.Setenv(key, "")
	}
	return cityPath, sessionID
}

// listFailingStore fails every List, as a ledger that drops mid-query does.
type listFailingStore struct{ beads.Store }

func (s listFailingStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("ledger unreachable: connection refused")
}

// A [name] drain-check that cannot resolve its target because the ledger
// failed has not learned the session is absent: that is unknown, not 1.
func TestCmdRuntimeDrainCheckNamedResolutionFailureIsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(t *testing.T, original func(string, string) (beads.Store, error)) func(string, string) (beads.Store, error)
		want string
	}{
		{
			name: "store open failure",
			open: func(*testing.T, func(string, string) (beads.Store, error)) func(string, string) (beads.Store, error) {
				return func(string, string) (beads.Store, error) {
					return nil, errors.New("ledger unreachable: connection refused")
				}
			},
			want: "ledger unreachable",
		},
		{
			name: "list failure",
			open: func(t *testing.T, original func(string, string) (beads.Store, error)) func(string, string) (beads.Store, error) {
				return func(scope, city string) (beads.Store, error) {
					store, err := original(scope, city)
					if err != nil {
						t.Fatalf("open city store: %v", err)
					}
					return listFailingStore{Store: store}, nil
				}
			},
			want: "ledger unreachable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			drainCheckNamedCity(t)
			original := openNudgeWorkStore
			t.Cleanup(func() { openNudgeWorkStore = original })
			openNudgeWorkStore = tc.open(t, original)

			var stdout, stderr bytes.Buffer
			if code := cmdRuntimeDrainCheck([]string{"worker"}, false, &stdout, &stderr); code != drainCheckUnknownExit {
				t.Fatalf("text: code = %d, want %d; stderr=%q", code, drainCheckUnknownExit, stderr.String())
			}
			lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
			if len(lines) != 1 || !strings.Contains(lines[0], drainCheckSourceRow+":") || !strings.Contains(lines[0], tc.want) {
				t.Fatalf("text: stderr = %q, want one line naming %s and %q", stderr.String(), drainCheckSourceRow, tc.want)
			}

			stdout.Reset()
			stderr.Reset()
			code := cmdRuntimeDrainCheck([]string{"worker"}, true, &stdout, &stderr)
			if code != drainCheckUnknownExit {
				t.Fatalf("json: code = %d, want %d; stderr=%q", code, drainCheckUnknownExit, stderr.String())
			}
			var result cliJSONErrorOutput
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("json: stdout is not JSON: %v\nraw: %s", err, stdout.String())
			}
			if result.OK || result.Error.Code != drainCheckUnknownCode || result.Error.ExitCode != drainCheckUnknownExit ||
				!strings.Contains(result.Error.Message, drainCheckSourceRow+":") {
				t.Fatalf("json: unexpected error envelope: %+v", result)
			}
		})
	}
}

func TestCmdRuntimeDrainCheckNamedUnknownSessionExitsOne(t *testing.T) {
	drainCheckNamedCity(t)

	var stdout, stderr bytes.Buffer
	if code := cmdRuntimeDrainCheck([]string{"nobody-by-this-name"}, false, &stdout, &stderr); code != 1 {
		t.Fatalf("code = %d, want 1 (no such session); stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Fatalf("stderr = %q, want a not-found diagnostic", stderr.String())
	}
}

// A [name] drain-check whose name matches two open sessions has not learned
// which row to read: that is unknown, not 1.
func TestCmdRuntimeDrainCheckNamedAmbiguousTargetIsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name       string
		identifier string
		metadata   map[string]string
	}{
		{
			name:       "shared alias",
			identifier: "worker",
			metadata: map[string]string{
				"alias":        "worker",
				"agent_name":   "frontend/worker",
				"template":     "frontend/worker",
				"session_name": "test-city--frontend--worker-2",
				"state":        "awake",
			},
		},
		{
			name:       "shared session_name",
			identifier: "test-city--frontend--worker",
			metadata: map[string]string{
				"agent_name":   "frontend/worker",
				"template":     "frontend/worker",
				"session_name": "test-city--frontend--worker",
				"state":        "awake",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, _ := drainCheckNamedCity(t)
			store, err := openScopeLocalFileStore(cityPath)
			if err != nil {
				t.Fatalf("open city store: %v", err)
			}
			if _, err := store.Create(beads.Bead{
				Title:    "second row",
				Type:     sessionBeadType,
				Labels:   []string{sessionBeadLabel},
				Metadata: tc.metadata,
			}); err != nil {
				t.Fatalf("create second row: %v", err)
			}

			var stdout, stderr bytes.Buffer
			if code := cmdRuntimeDrainCheck([]string{tc.identifier}, false, &stdout, &stderr); code != drainCheckUnknownExit {
				t.Fatalf("text: code = %d, want %d; stderr=%q", code, drainCheckUnknownExit, stderr.String())
			}
			lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
			if len(lines) != 1 || !strings.Contains(lines[0], drainCheckSourceRow+":") || !strings.Contains(lines[0], session.ErrAmbiguous.Error()) {
				t.Fatalf("text: stderr = %q, want one line naming %s and the ambiguity", stderr.String(), drainCheckSourceRow)
			}

			stdout.Reset()
			stderr.Reset()
			code := cmdRuntimeDrainCheck([]string{tc.identifier}, true, &stdout, &stderr)
			if code != drainCheckUnknownExit {
				t.Fatalf("json: code = %d, want %d; stderr=%q", code, drainCheckUnknownExit, stderr.String())
			}
			var result cliJSONErrorOutput
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("json: stdout is not JSON: %v\nraw: %s", err, stdout.String())
			}
			if result.OK || result.Error.Code != drainCheckUnknownCode || result.Error.ExitCode != drainCheckUnknownExit ||
				!strings.Contains(result.Error.Message, session.ErrAmbiguous.Error()) {
				t.Fatalf("json: unexpected error envelope: %+v", result)
			}
		})
	}
}

// The [name] form reads the row the target resolved to, by its id: a by-name
// lookup of the shared session_name would land on the other row.
func TestCmdRuntimeDrainCheckNamedReadsRowByResolvedID(t *testing.T) {
	cityPath, workerID := drainCheckNamedCity(t)
	store, err := openScopeLocalFileStore(cityPath)
	if err != nil {
		t.Fatalf("open city store: %v", err)
	}
	draining, err := store.Create(beads.Bead{
		Title:  "draining row",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"agent_name":   "frontend/worker",
			"template":     "frontend/worker",
			"session_name": "test-city--frontend--worker",
			"state":        string(session.StateDraining),
		},
	})
	if err != nil {
		t.Fatalf("create draining row: %v", err)
	}
	if draining.ID == workerID {
		t.Fatal("rows share an id")
	}

	for _, tc := range []struct {
		id   string
		want int
	}{
		{id: draining.ID, want: 0},
		{id: workerID, want: 1},
	} {
		var stdout, stderr bytes.Buffer
		if code := cmdRuntimeDrainCheck([]string{tc.id}, true, &stdout, &stderr); code != tc.want {
			t.Fatalf("drain-check %s: code = %d, want %d; stdout=%q stderr=%q", tc.id, code, tc.want, stdout.String(), stderr.String())
		}
		result := decodeDrainCheckJSON(t, stdout.Bytes())
		if tc.want == 0 && result.Source != drainCheckSourceRow {
			t.Fatalf("drain-check %s: source = %q, want %q", tc.id, result.Source, drainCheckSourceRow)
		}
	}
}

// ---------------------------------------------------------------------------
// doRuntimeDrainAck tests
// ---------------------------------------------------------------------------

func TestDoRuntimeDrainAck(t *testing.T) {
	testCity := t.TempDir()
	old := drainAckPokeController
	drainAckPokeController = func(string, reconcilekey.Key) error { return nil }
	t.Cleanup(func() { drainAckPokeController = old })

	dops := newFakeDrainOps()
	var stdout, stderr bytes.Buffer
	code := doRuntimeDrainAck(dops, testCity, "worker", "worker", "", drainAckCaller{}, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !dops.acked["worker"] {
		t.Error("drain ack flag not set")
	}
	if got := stdout.String(); got != "Drain acknowledged. Controller poked for immediate stop.\n" {
		t.Errorf("stdout = %q", got)
	}
}

func TestDoRuntimeDrainAckError(t *testing.T) {
	testCity := t.TempDir()
	dops := newFakeDrainOps()
	dops.err = errors.New("tmux borked")
	var stdout, stderr bytes.Buffer
	code := doRuntimeDrainAck(dops, testCity, "worker", "worker", "", drainAckCaller{}, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if got := stderr.String(); got != "gc runtime drain-ack: tmux borked\n" {
		t.Errorf("stderr = %q", got)
	}
}

func TestJoinDrainAckMutationErrorsMissingSessionBeadIsIdempotent(t *testing.T) {
	err := joinDrainAckMutationErrors(
		fmt.Errorf("setting metadata on %q: %w", "gc-missing", beads.ErrNotFound),
		fmt.Errorf("removing metadata on %q: %w", "gc-missing", beads.ErrNotFound),
	)
	if err != nil {
		t.Fatalf("joinDrainAckMutationErrors(...) = %v, want nil for missing session bead", err)
	}
}

func TestDoRuntimeDrainAckJSON(t *testing.T) {
	testCity := t.TempDir()
	old := drainAckPokeController
	drainAckPokeController = func(string, reconcilekey.Key) error { return nil }
	t.Cleanup(func() { drainAckPokeController = old })

	dops := newFakeDrainOps()
	var stdout, stderr bytes.Buffer
	code := doRuntimeDrainAck(dops, testCity, "worker", "worker", "", drainAckCaller{}, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	var result runtimeActionJSON
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.SchemaVersion != "1" || result.Command != "runtime drain-ack" || result.Session != "worker" || result.Status != "acknowledged" {
		t.Fatalf("unexpected JSON result: %+v", result)
	}
}

func TestDoRuntimeDrainAckPokesController(t *testing.T) {
	var gotCityPath string
	var gotKey reconcilekey.Key
	calls := 0
	old := drainAckPokeController
	drainAckPokeController = func(cityPath string, key reconcilekey.Key) error {
		calls++
		gotCityPath = cityPath
		gotKey = key
		return nil
	}
	t.Cleanup(func() { drainAckPokeController = old })

	// Distinct, non-overlapping cityPath/targetName/sn catch a transposition
	// of the three adjacent string params in the new signature.
	dops := newFakeDrainOps()
	var stdout, stderr bytes.Buffer
	code := doRuntimeDrainAck(dops, "/city/path", "display-name", "session-name", "gc-42", drainAckCaller{}, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if calls != 1 {
		t.Fatalf("poke called %d times, want 1", calls)
	}
	if gotCityPath != "/city/path" {
		t.Errorf("poke cityPath = %q, want %q", gotCityPath, "/city/path")
	}
	if want := reconcilekey.SessionRef("gc-42", "session-name"); gotKey != want {
		t.Errorf("poke key = %v, want %v", gotKey, want)
	}
	if !dops.acked["session-name"] {
		t.Error("drain ack flag not set")
	}
}

func TestDoRuntimeDrainAckErrorDoesNotPoke(t *testing.T) {
	calls := 0
	old := drainAckPokeController
	drainAckPokeController = func(string, reconcilekey.Key) error {
		calls++
		return nil
	}
	t.Cleanup(func() { drainAckPokeController = old })

	dops := newFakeDrainOps()
	dops.err = errors.New("tmux borked")
	var stdout, stderr bytes.Buffer
	code := doRuntimeDrainAck(dops, "/city/path", "worker", "worker", "", drainAckCaller{}, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if calls != 0 {
		t.Fatalf("poke called %d times, want 0 (setDrainAck failed)", calls)
	}
}

func TestDoRuntimeDrainAckPokeFailureWarns(t *testing.T) {
	old := drainAckPokeController
	drainAckPokeController = func(string, reconcilekey.Key) error { return errors.New("dial failed") }
	t.Cleanup(func() { drainAckPokeController = old })

	dops := newFakeDrainOps()
	var stdout, stderr bytes.Buffer
	code := doRuntimeDrainAck(dops, "/city/path", "worker", "worker", "", drainAckCaller{}, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0 (poke failure is best-effort)", code)
	}
	if got, want := stderr.String(), "gc runtime drain-ack: warning: poke failed: dial failed\n"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
	if !strings.Contains(stdout.String(), "Drain acknowledged.") {
		t.Errorf("stdout = %q, want it to contain %q", stdout.String(), "Drain acknowledged.")
	}
}

// ---------------------------------------------------------------------------
// newDrainOps factory tests
// ---------------------------------------------------------------------------

func TestNewDrainOpsAlwaysReturnsNonNil(t *testing.T) {
	// newDrainOps works with any Provider — no type assertions.
	fp := runtime.NewFake()
	dops := newDrainOps(fp)
	if dops == nil {
		t.Fatal("newDrainOps(Fake) = nil, want non-nil")
	}
	if _, ok := dops.(*providerDrainOps); !ok {
		t.Errorf("newDrainOps returned %T, want *providerDrainOps", dops)
	}
}

func TestProviderDrainOpsRoundTrip(t *testing.T) {
	// Verify drain ops work through Provider meta interface.
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "worker", runtime.Config{})
	dops := newDrainOps(sp)

	// Not draining initially.
	draining, _ := dops.isDraining("worker")
	if draining {
		t.Error("should not be draining initially")
	}

	// Set drain.
	if err := dops.setDrain("worker"); err != nil {
		t.Fatalf("setDrain: %v", err)
	}
	draining, _ = dops.isDraining("worker")
	if !draining {
		t.Error("should be draining after setDrain")
	}

	// Drain start time should be parseable.
	ts, err := dops.drainStartTime("worker")
	if err != nil {
		t.Fatalf("drainStartTime: %v", err)
	}
	if ts.IsZero() {
		t.Error("drain start time should not be zero")
	}

	// Set and check ack.
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}
	acked, _ := dops.isDrainAcked("worker")
	if !acked {
		t.Error("should be acked after setDrainAck")
	}

	// Clear drain (also clears ack).
	if err := dops.clearDrain("worker"); err != nil {
		t.Fatalf("clearDrain: %v", err)
	}
	draining, _ = dops.isDraining("worker")
	if draining {
		t.Error("should not be draining after clearDrain")
	}
	acked, _ = dops.isDrainAcked("worker")
	if acked {
		t.Error("ack should be cleared after clearDrain")
	}
}

func TestProviderDrainOpsReportsMetadataErrors(t *testing.T) {
	sp := runtime.NewFailFake()
	dops := newDrainOps(sp)

	if _, err := dops.isDraining("worker"); err == nil {
		t.Fatal("isDraining should return an error when metadata lookup fails")
	}
	if _, err := dops.isDrainAcked("worker"); err == nil {
		t.Fatal("isDrainAcked should return an error when metadata lookup fails")
	}
	if _, err := dops.isRestartRequested("worker"); err == nil {
		t.Fatal("isRestartRequested should return an error when metadata lookup fails")
	}
	if _, err := dops.isDriftRestart("worker"); err == nil {
		t.Fatal("isDriftRestart should return an error when metadata lookup fails")
	}
	if err := dops.clearDrain("worker"); err == nil {
		t.Fatal("clearDrain should return an error when metadata removal fails")
	}
	if err := dops.setDrainAck("worker"); err == nil {
		t.Fatal("setDrainAck should return an error when metadata removal fails")
	}
}

type recordingMetaProvider struct {
	*runtime.Fake
	removeErr  error
	removeKeys []string
	setKeys    []string
}

func (p *recordingMetaProvider) RemoveMeta(name, key string) error {
	p.removeKeys = append(p.removeKeys, key)
	if p.removeErr != nil {
		return p.removeErr
	}
	return p.Fake.RemoveMeta(name, key)
}

func (p *recordingMetaProvider) SetMeta(name, key, value string) error {
	p.setKeys = append(p.setKeys, key)
	return p.Fake.SetMeta(name, key, value)
}

func TestProviderDrainOpsClearDrainAttemptsAllMetadataRemovals(t *testing.T) {
	sp := &recordingMetaProvider{
		Fake:      runtime.NewFake(),
		removeErr: errors.New("metadata removal unavailable"),
	}
	dops := newDrainOps(sp)

	if err := dops.clearDrain("worker"); err == nil {
		t.Fatal("clearDrain should report metadata removal errors")
	}
	want := []string{
		"GC_DRAIN_ACK",
		reconcilerDrainAckSourceKey,
		drainAckRequesterInstanceTokenKey,
		reconcilerDrainAckReasonKey,
		reconcilerDrainAckGenerationKey,
		"GC_DRAIN",
	}
	if !slices.Equal(sp.removeKeys, want) {
		t.Fatalf("removed keys = %v, want %v", sp.removeKeys, want)
	}
}

func TestProviderDrainOpsSetDrainAckAttemptsAckAfterCleanupErrors(t *testing.T) {
	sp := &recordingMetaProvider{
		Fake:      runtime.NewFake(),
		removeErr: errors.New("metadata removal unavailable"),
	}
	dops := newDrainOps(sp)

	if err := dops.setDrainAck("worker"); err == nil {
		t.Fatal("setDrainAck should report metadata cleanup errors")
	}
	wantRemove := []string{
		reconcilerDrainAckReasonKey,
		reconcilerDrainAckGenerationKey,
	}
	if !slices.Equal(sp.removeKeys, wantRemove) {
		t.Fatalf("removed keys = %v, want %v", sp.removeKeys, wantRemove)
	}
	// The stamp lands BEFORE the source, so the source is never admissible ahead
	// of its own binding: drainReminderAckPin admits an acknowledgement on the
	// source alone and reads the stamp in a separate round-trip, so written the
	// other way round it would pair this ack's fresh source with the previous
	// occupant's stamp and mint "proven stale" about an ack microseconds old.
	// GC_DRAIN_ACK still lands last, which is what the effect boundary gates on.
	wantSet := []string{
		drainAckRequesterInstanceTokenKey,
		reconcilerDrainAckSourceKey,
		"GC_DRAIN_ACK",
	}
	if !slices.Equal(sp.setKeys, wantSet) {
		t.Fatalf("set keys = %v, want %v", sp.setKeys, wantSet)
	}
}

// ---------------------------------------------------------------------------
// doRuntimeRequestRestart tests
// ---------------------------------------------------------------------------

// TestDoRuntimeRequestRestartError pins that a failed runtime hint, which
// follows the durable row write, is only a warning (SG-3).
func TestDoRuntimeRequestRestartError(t *testing.T) {
	dops := newFakeDrainOps()
	dops.err = errors.New("tmux borked")
	var stdout, stderr bytes.Buffer
	code := doRuntimeRequestRestart(testRowFirstRestart("gc runtime request-restart", "worker", dops, &stderr), events.Discard, &stdout)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if got, want := stderr.String(), "gc runtime request-restart: warning: restart recorded; the runtime restart hint was not set: tmux borked\n"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}

// TestDoRuntimeRequestRestart_RowWriteFailureRecordsNothing pins that the row
// write is mandatory for every session (the pinned special case collapsed
// into it): unavailable or failing, the command exits 1 with no hint, no
// event and no poke.
func TestDoRuntimeRequestRestart_RowWriteFailureRecordsNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record func(string) error
	}{
		{name: "no session store"},
		{name: "row write error", record: func(string) error { return errors.New("worker boundary unavailable") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dops := newFakeDrainOps()
			rec := events.NewFake()
			var stdout, stderr bytes.Buffer
			restart := testRowFirstRestart("gc runtime request-restart", "mayor", dops, &stderr)
			restart.record = tc.record
			poked := false
			restart.poke = func(string, reconcilekey.Key) error { poked = true; return nil }
			code := doRuntimeRequestRestart(restart, rec, &stdout)
			if code != 1 {
				t.Fatalf("code = %d, want 1; stderr: %s", code, stderr.String())
			}
			if got := stdout.String(); got != "" {
				t.Errorf("stdout = %q, want nothing", got)
			}
			if dops.restartRequested["mayor"] {
				t.Error("runtime restart hint set although the row write failed")
			}
			if poked {
				t.Error("controller poked although nothing was recorded")
			}
			if len(rec.Events) != 0 {
				t.Fatalf("events = %v, want none", rec.Events)
			}
		})
	}
}

func TestControllerRestartTimeout(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.City
		want time.Duration
	}{
		{name: "nil config uses floor", cfg: nil, want: 5 * time.Minute},
		{name: "empty interval uses floor", cfg: &config.City{}, want: 5 * time.Minute},
		{name: "below floor clamps up", cfg: &config.City{Daemon: config.DaemonConfig{PatrolInterval: "15s"}}, want: 5 * time.Minute},
		{name: "middle range uses multiplier", cfg: &config.City{Daemon: config.DaemonConfig{PatrolInterval: "2m"}}, want: 10 * time.Minute},
		{name: "ceiling edge", cfg: &config.City{Daemon: config.DaemonConfig{PatrolInterval: "6m"}}, want: 30 * time.Minute},
		{name: "above ceiling clamps down", cfg: &config.City{Daemon: config.DaemonConfig{PatrolInterval: "10m"}}, want: 30 * time.Minute},
		{name: "invalid duration uses default floor", cfg: &config.City{Daemon: config.DaemonConfig{PatrolInterval: "later"}}, want: 5 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := controllerRestartTimeout(tt.cfg); got != tt.want {
				t.Fatalf("controllerRestartTimeout() = %s, want %s", got, tt.want)
			}
		})
	}
}

type getMetaErrorProvider struct {
	*runtime.Fake
	err error
}

func (p *getMetaErrorProvider) GetMeta(_, _ string) (string, error) {
	return "", p.err
}

func TestProviderDrainOpsIsRestartRequestedTreatsGoneSessionAsCleared(t *testing.T) {
	dops := newDrainOps(&getMetaErrorProvider{
		Fake: runtime.NewFake(),
		err:  runtime.ErrSessionNotFound,
	})
	requested, err := dops.isRestartRequested("worker")
	if err != nil {
		t.Fatalf("isRestartRequested returned gone-session error: %v", err)
	}
	if requested {
		t.Fatal("isRestartRequested = true, want false for gone session")
	}
}

func TestRequestRestartAcceptsNoArgs(t *testing.T) {
	// Verify the cobra command accepts no args.
	var stdout, stderr bytes.Buffer
	cmd := newRuntimeRequestRestartCmd(&stdout, &stderr)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_SESSION_ID", "")
	t.Setenv("GC_CITY", "")
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err == nil {
		t.Error("request-restart with no env should return non-zero")
	}
	if !strings.Contains(stderr.String(), "not in session context") {
		t.Errorf("stderr = %q, want 'not in session context' error", stderr.String())
	}
}

func TestProviderDrainOpsRestartRequestedRoundTrip(t *testing.T) {
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "worker", runtime.Config{})
	dops := newDrainOps(sp)

	// Not requested initially.
	requested, _ := dops.isRestartRequested("worker")
	if requested {
		t.Error("should not be restart-requested initially")
	}

	// Set restart requested.
	if err := dops.setRestartRequested("worker"); err != nil {
		t.Fatalf("setRestartRequested: %v", err)
	}
	requested, _ = dops.isRestartRequested("worker")
	if !requested {
		t.Error("should be restart-requested after set")
	}

	// Clear restart requested.
	if err := dops.clearRestartRequested("worker"); err != nil {
		t.Fatalf("clearRestartRequested: %v", err)
	}
	requested, _ = dops.isRestartRequested("worker")
	if requested {
		t.Error("should not be restart-requested after clear")
	}
}

type removeMetaErrorProvider struct {
	*runtime.Fake
	err error
}

func (p *removeMetaErrorProvider) RemoveMeta(_, _ string) error {
	return p.err
}

func TestProviderDrainOpsClearRestartRequestedTreatsSessionGoneAsBenign(t *testing.T) {
	dops := newDrainOps(&removeMetaErrorProvider{
		Fake: runtime.NewFake(),
		err:  errors.New("no tmux server running"),
	})
	if err := dops.clearRestartRequested("worker"); err != nil {
		t.Fatalf("clearRestartRequested returned gone-session error: %v", err)
	}
}

func TestProviderDrainOpsClearRestartRequestedReturnsCleanupErrors(t *testing.T) {
	dops := newDrainOps(&removeMetaErrorProvider{
		Fake: runtime.NewFake(),
		err:  errors.New("permission denied"),
	})
	if err := dops.clearRestartRequested("worker"); err == nil {
		t.Fatal("clearRestartRequested should return non-gone cleanup errors")
	}
}

func TestProviderDrainOpsDriftRestartRoundTrip(t *testing.T) {
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "worker", runtime.Config{})
	dops := newDrainOps(sp)

	// Not drift-restart initially.
	isDrift, _ := dops.isDriftRestart("worker")
	if isDrift {
		t.Error("should not be drift-restart initially")
	}

	// Set drift restart.
	if err := dops.setDriftRestart("worker"); err != nil {
		t.Fatalf("setDriftRestart: %v", err)
	}
	isDrift, _ = dops.isDriftRestart("worker")
	if !isDrift {
		t.Error("should be drift-restart after set")
	}

	// Clear drift restart.
	if err := dops.clearDriftRestart("worker"); err != nil {
		t.Fatalf("clearDriftRestart: %v", err)
	}
	isDrift, _ = dops.isDriftRestart("worker")
	if isDrift {
		t.Error("should not be drift-restart after clear")
	}
}

// ---------------------------------------------------------------------------
// newRuntimeDrainCheckCmd / newRuntimeDrainAckCmd arg acceptance tests
// ---------------------------------------------------------------------------

func TestDrainCheckAcceptsPositionalArg(t *testing.T) {
	// Verify cobra allows 0 or 1 positional arg (no longer NoArgs).
	// The command will fail at runtime (no city), but it should NOT fail
	// with "unknown command" or "accepts 0 arg(s)" errors.
	var stdout, stderr bytes.Buffer
	cmd := newRuntimeDrainCheckCmd(&stdout, &stderr)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"polecat"})
	err := cmd.Execute()
	// Expect a runtime error (no city dir), NOT an arg-count error.
	if err != nil {
		// errExit is expected — the command runs but fails to find a city.
		// What we're testing is that cobra didn't reject the arg.
		if stderr.String() != "" && strings.Contains(stderr.String(), "accepts 0 arg") {
			t.Errorf("drain-check should accept positional arg, got: %s", stderr.String())
		}
	}
}

func TestDrainCheckNoArgsStillWorks(t *testing.T) {
	// Without args and without env vars, drain-check returns exit 1 silently.
	var stdout, stderr bytes.Buffer
	cmd := newRuntimeDrainCheckCmd(&stdout, &stderr)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	// Ensure env vars are not set (in case test environment has them).
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_SESSION_ID", "")
	t.Setenv("GC_CITY", "")
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if code := commandExitCode(err); code != 1 {
		t.Errorf("drain-check with no args and no env exit = %d, want 1 (not draining)", code)
	}
	// Should be silent — no error message on stderr.
	if stderr.Len() > 0 {
		t.Errorf("drain-check should be silent without env vars, got stderr: %q", stderr.String())
	}
}

func TestDrainAckAcceptsPositionalArg(t *testing.T) {
	// Same pattern as drain-check: verify cobra allows positional arg.
	var stdout, stderr bytes.Buffer
	cmd := newRuntimeDrainAckCmd(&stdout, &stderr)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"polecat"})
	err := cmd.Execute()
	if err != nil {
		if stderr.String() != "" && strings.Contains(stderr.String(), "accepts 0 arg") {
			t.Errorf("drain-ack should accept positional arg, got: %s", stderr.String())
		}
	}
}

func TestDrainAckNoArgsErrorMessage(t *testing.T) {
	// Without args and without env vars, drain-ack prints an error message.
	var stdout, stderr bytes.Buffer
	cmd := newRuntimeDrainAckCmd(&stdout, &stderr)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_SESSION_ID", "")
	t.Setenv("GC_CITY", "")
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err == nil {
		t.Error("drain-ack with no args and no env should return non-zero")
	}
	if !strings.Contains(stderr.String(), "not in session context") {
		t.Errorf("stderr = %q, want 'not in session context' error", stderr.String())
	}
}

func TestDrainAckNoArgsFallsBackToCityPathEnv(t *testing.T) {
	old := drainAckPokeController
	drainAckPokeController = func(string, reconcilekey.Key) error { return nil }
	t.Cleanup(func() { drainAckPokeController = old })

	// drain-ack now reads the city store to release any in_progress claim the
	// session is still holding, so this bare temp city needs the same store
	// guards its siblings in this file use — otherwise the read provisions a
	// managed Dolt server the test never tears down.
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	cmd := newRuntimeDrainAckCmd(&stdout, &stderr)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_ALIAS", "mayor")
	t.Setenv("GC_SESSION_ID", "gc-42")
	t.Setenv("GC_SESSION_NAME", "s-gc-42")
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", cityDir)

	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err != nil {
		t.Fatalf("drain-ack should succeed with GC_CITY_PATH fallback: %v; stderr=%q", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Drain acknowledged") {
		t.Fatalf("stdout = %q, want drain acknowledgement", stdout.String())
	}
}

// ---------------------------------------------------------------------------
// resolveAgentIdentity / findAgentByQualified unit tests
// ---------------------------------------------------------------------------

func TestResolveAgentIdentity(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
			{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5), ScaleCheck: "echo 3"},
			{Name: "singleton", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1), ScaleCheck: "echo 1"},
		},
	}

	tests := []struct {
		name      string
		lookup    string
		wantFound bool
		wantName  string
		wantPool  bool
		wantLabel string
	}{
		// Exact match on non-pool agent.
		{"exact match", "mayor", true, "mayor", false, ""},

		// Exact match on pool agent (base name).
		{"pool base name", "worker", true, "worker", true, ""},

		// Pool instance matches.
		{"pool instance worker-1", "worker-1", true, "worker-1", true, "worker"},
		{"pool instance worker-5", "worker-5", true, "worker-5", true, "worker"},

		// Pool instance out of range (too high).
		{"pool instance worker-6", "worker-6", false, "", false, ""},

		// Pool instance out of range (zero).
		{"pool instance worker-0", "worker-0", false, "", false, ""},

		// Pool instance non-numeric suffix.
		{"pool instance worker-abc", "worker-abc", false, "", false, ""},

		// Pool instance negative (parsed as non-numeric due to dash).
		{"pool instance worker--1", "worker--1", false, "", false, ""},

		// Max=1 pool-shaped agents still run through pool reconciliation, but
		// non-namepool singleton pools use the canonical configured identity.
		// A numeric suffix would materialize a phantom concrete agent.
		{"singleton-1 rejected for canonical max=1 pool", "singleton-1", false, "", false, ""},

		// Nonexistent agent.
		{"nonexistent", "nobody", false, "", false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := resolveAgentIdentity(cfg, tt.lookup, "")
			if found != tt.wantFound {
				t.Fatalf("resolveAgentIdentity(%q) found = %v, want %v", tt.lookup, found, tt.wantFound)
			}
			if !found {
				return
			}
			if got.Name != tt.wantName {
				t.Errorf("agent.Name = %q, want %q", got.Name, tt.wantName)
			}
			gotIsMulti := got.SupportsInstanceExpansion()
			if gotIsMulti != tt.wantPool {
				t.Errorf("supportsInstanceExpansion = %v, want %v", gotIsMulti, tt.wantPool)
			}
			if got.PoolName != tt.wantLabel {
				t.Errorf("agent.PoolName = %q, want %q", got.PoolName, tt.wantLabel)
			}
		})
	}
}

func TestResolveAgentIdentityQualified(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
			{Name: "polecat", Dir: "frontend"},
			{Name: "polecat", Dir: "backend"},
			{Name: "worker", Dir: "frontend", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "echo 2"},
			{Name: "coder", Dir: "backend", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(-1)},
		},
	}

	tests := []struct {
		name       string
		input      string
		rigContext string
		wantFound  bool
		wantDir    string
		wantName   string
	}{
		// City-wide literal.
		{"city-wide literal", "mayor", "", true, "", "mayor"},
		// Qualified literal.
		{"qualified frontend/polecat", "frontend/polecat", "", true, "frontend", "polecat"},
		{"qualified backend/polecat", "backend/polecat", "", true, "backend", "polecat"},
		// Bare name with rig context.
		{"bare polecat + frontend context", "polecat", "frontend", true, "frontend", "polecat"},
		{"bare polecat + backend context", "polecat", "backend", true, "backend", "polecat"},
		// Bare name with no matching context — ambiguous (2 polecats), not found.
		{"bare polecat no context", "polecat", "", false, "", ""},
		// Pool instance with qualified name.
		{"qualified pool frontend/worker-2", "frontend/worker-2", "", true, "frontend", "worker-2"},
		// Pool instance with rig context.
		{"bare pool worker-1 + frontend context", "worker-1", "frontend", true, "frontend", "worker-1"},
		// Step 3: unambiguous bare name — worker is unique across all agents.
		{"unambiguous bare worker", "worker", "", true, "frontend", "worker"},
		// Step 3: unambiguous pool instance via bare name.
		{"unambiguous bare worker-2", "worker-2", "", true, "frontend", "worker-2"},
		// Unlimited pool: qualified lookup.
		{"qualified unlimited backend/coder-5", "backend/coder-5", "", true, "backend", "coder-5"},
		// Unlimited pool: unambiguous bare name.
		{"unambiguous bare coder-42", "coder-42", "", true, "backend", "coder-42"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := resolveAgentIdentity(cfg, tt.input, tt.rigContext)
			if found != tt.wantFound {
				t.Fatalf("resolveAgentIdentity(%q, %q) found = %v, want %v", tt.input, tt.rigContext, found, tt.wantFound)
			}
			if !found {
				return
			}
			if got.Dir != tt.wantDir {
				t.Errorf("Dir = %q, want %q", got.Dir, tt.wantDir)
			}
			if got.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", got.Name, tt.wantName)
			}
			if strings.Contains(tt.wantName, "-") && got.PoolName == "" {
				t.Errorf("PoolName = %q, want template label for pool instance", got.PoolName)
			}
		})
	}
}

func TestResolveAgentIdentityUnambiguous(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
			{Name: "polecat", Dir: "myrig"},
			{Name: "builder", Dir: "frontend", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "echo 2"},
			{Name: "coder", Dir: "backend", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(-1)},
		},
	}

	tests := []struct {
		name      string
		input     string
		wantFound bool
		wantDir   string
		wantName  string
	}{
		// Unique bare name resolves via step 3.
		{"unambiguous polecat", "polecat", true, "myrig", "polecat"},
		// Unique pool template resolves via step 3.
		{"unambiguous builder", "builder", true, "frontend", "builder"},
		// Pool instance, unambiguous.
		{"unambiguous builder-2", "builder-2", true, "frontend", "builder-2"},
		// Pool instance out of range.
		{"builder-4 out of range", "builder-4", false, "", ""},
		// City-wide agent resolves via step 1, before step 3.
		{"mayor step 1", "mayor", true, "", "mayor"},
		// Unlimited pool: bare name resolves.
		{"unlimited pool bare", "coder", true, "backend", "coder"},
		// Unlimited pool: any instance number resolves.
		{"unlimited pool instance", "coder-99", true, "backend", "coder-99"},
		// Qualified pool instance: "frontend/builder-2" resolves via step 1b.
		{"qualified pool instance", "frontend/builder-2", true, "frontend", "builder-2"},
		// Qualified pool instance out of range.
		{"qualified pool instance out of range", "frontend/builder-4", false, "", ""},
		// Qualified unlimited pool instance.
		{"qualified unlimited pool instance", "backend/coder-42", true, "backend", "coder-42"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := resolveAgentIdentity(cfg, tt.input, "")
			if found != tt.wantFound {
				t.Fatalf("resolveAgentIdentity(%q) found = %v, want %v", tt.input, found, tt.wantFound)
			}
			if !found {
				return
			}
			if got.Dir != tt.wantDir {
				t.Errorf("Dir = %q, want %q", got.Dir, tt.wantDir)
			}
			if got.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", got.Name, tt.wantName)
			}
			if strings.Contains(tt.wantName, "-") && got.PoolName == "" {
				t.Errorf("PoolName = %q, want template label for pool instance", got.PoolName)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// findAgentByName unit tests (pool suffix stripping for gc prime)
// ---------------------------------------------------------------------------

func TestFindAgentByNameExact(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
			{Name: "polecat", Dir: "frontend"},
		},
	}
	a, ok := findAgentByName(cfg, "mayor")
	if !ok {
		t.Fatal("expected to find mayor")
	}
	if a.Name != "mayor" {
		t.Errorf("Name = %q, want %q", a.Name, "mayor")
	}
}

func TestFindAgentByNamePoolInstance(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "polecat", Dir: "frontend", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5), ScaleCheck: "echo 3"},
		},
	}
	// "polecat-3" should strip suffix and match "polecat" pool.
	a, ok := findAgentByName(cfg, "polecat-3")
	if !ok {
		t.Fatal("expected to find polecat via pool suffix stripping")
	}
	if a.Name != "polecat" {
		t.Errorf("Name = %q, want %q", a.Name, "polecat")
	}
}

func TestFindAgentByNamePoolOutOfRange(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "polecat", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "echo 2"},
		},
	}
	// "polecat-4" is out of range (max=3).
	_, ok := findAgentByName(cfg, "polecat-4")
	if ok {
		t.Error("polecat-4 should not match pool with max=3")
	}
}

func TestFindAgentByNameSingletonPoolRejectsSuffix(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "singleton", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1), ScaleCheck: "echo 1"},
		},
	}
	if _, ok := findAgentByName(cfg, "singleton-1"); ok {
		t.Fatal("singleton-1 should not match a canonical singleton pool agent")
	}
}

func TestFindAgentByNameUnlimitedPool(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "polecat", Dir: "myrig", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(-1)},
		},
	}
	// Any instance number should match an unlimited pool.
	a, ok := findAgentByName(cfg, "polecat-99")
	if !ok {
		t.Fatal("expected to find polecat-99 in unlimited pool")
	}
	if a.Name != "polecat" {
		t.Errorf("Name = %q, want %q", a.Name, "polecat")
	}
}

func TestFindAgentByNameNoMatch(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
		},
	}
	_, ok := findAgentByName(cfg, "nobody")
	if ok {
		t.Error("expected no match for nonexistent agent")
	}
}

// TestOperatorDrainRow (PAR-FLOORACK, CONTRACT v5.9p): in a v2 city `gc
// runtime drain` marks the row with its generation in its own key
// (operator_drain), whatever controller request the row carries, which it
// never touches; `gc runtime undrain` clears the mark and leaves the
// request.
func TestOperatorDrainRow(t *testing.T) {
	for _, c := range []struct {
		name    string
		meta    map[string]string
		request string // the controller request, before and after
	}{
		{"a bare row", map[string]string{"generation": "3"}, ""},
		{"a row a controller request holds", map[string]string{
			"generation": "3", session.DrainIntentReasonKey: drainConfigDrift, session.DrainIntentAtKey: "2026-10-12T12:00:00Z", session.DrainIntentIncarnationKey: "3",
		}, drainConfigDrift},
		{"a mark of an older generation", map[string]string{"generation": "3", session.OperatorDrainKey: "2"}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			store, id := stampedMem(t, gate.Require)
			if err := store.SetMetadataBatch(id, c.meta); err != nil {
				t.Fatal(err)
			}
			if err := writeOperatorDrainRow(store, id); err != nil {
				t.Fatal(err)
			}
			b, _ := store.Get(id)
			if b.Metadata[session.OperatorDrainKey] != "3" || b.Metadata[session.DrainIntentReasonKey] != c.request {
				t.Fatalf("after drain: mark %q, request %q; want the mark at 3 and the request %q untouched", b.Metadata[session.OperatorDrainKey], b.Metadata[session.DrainIntentReasonKey], c.request)
			}
			if err := clearOperatorDrainRow(store, id); err != nil {
				t.Fatal(err)
			}
			if b, _ := store.Get(id); b.Metadata[session.OperatorDrainKey] != "" || b.Metadata[session.DrainIntentReasonKey] != c.request {
				t.Fatalf("after undrain: mark %q, request %q; want no mark and the request %q", b.Metadata[session.OperatorDrainKey], b.Metadata[session.DrainIntentReasonKey], c.request)
			}
		})
	}
}

// TestDrainRuntimeRowWriteFailure (O9): a failed row mark in a v2 city
// sets no env drain flag, records nothing and exits 1.
func TestDrainRuntimeRowWriteFailure(t *testing.T) {
	dops := newFakeDrainOps()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	rec := events.NewFake()
	var stdout, stderr bytes.Buffer
	code := drainRuntime(dops, func() error { return errors.New("store down") }, sp, rec, "worker", "worker", false, &stdout, &stderr)
	if code != 1 || dops.draining["worker"] || len(rec.Events) != 0 || !strings.Contains(stderr.String(), "store down") {
		t.Fatalf("drain = %d (draining %v, events %v, stderr %q), want 1 with nothing set", code, dops.draining["worker"], rec.Events, stderr.String())
	}
}
