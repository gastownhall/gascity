package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionhybrid "github.com/gastownhall/gascity/internal/runtime/hybrid"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

type recordingStopProvider struct {
	*runtime.Fake
	stops      chan string
	interrupts chan string
}

func newRecordingStopProvider() *recordingStopProvider {
	return &recordingStopProvider{
		Fake:       runtime.NewFake(),
		stops:      make(chan string, 8),
		interrupts: make(chan string, 8),
	}
}

func (p *recordingStopProvider) Stop(name string) error {
	p.stops <- name
	return p.Fake.Stop(name)
}

func (p *recordingStopProvider) Interrupt(name string) error {
	p.interrupts <- name
	return p.Fake.Interrupt(name)
}

func TestCmdStopWaitsForStandaloneControllerExit(t *testing.T) {
	t.Setenv("GC_HOME", shortSocketTempDir(t, "gc-home-"))

	dir := shortSocketTempDir(t, "gc-stop-")
	for legacyLen := len(filepath.Join(dir, ".gc", "controller.sock")); legacyLen <= 120; legacyLen = len(filepath.Join(dir, ".gc", "controller.sock")) {
		dir = filepath.Join(dir, "very-long-controller-path-segment")
	}
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "0s"},
	}
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	tomlPath := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(tomlPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	writeBuiltinImportsFixture(t, dir, "core")
	if got := controllerSocketPath(dir); got == filepath.Join(dir, ".gc", "controller.sock") {
		t.Fatalf("controllerSocketPath(%q) = legacy path %q, want short fallback", dir, got)
	}
	if got, want := controllerSocketPath(dir), controllerSocketPath(canonicalTestPath(dir)); got != want {
		t.Fatalf("controllerSocketPath fallback mismatch across equivalent paths: %q vs %q", got, want)
	}

	sp := newGatedStopProvider()
	buildFn := func(_ *config.City, _ runtime.Provider, _ beads.Store) DesiredStateResult {
		return DesiredStateResult{State: map[string]TemplateParams{}}
	}
	const seededSession = "seeded-session"

	var controllerStdout, controllerStderr lockedBuffer
	done := make(chan struct{})
	go func() {
		runController(dir, nil, tomlPath, cfg, "", buildFn, nil, sp, nil, nil, nil, nil, 0, events.Discard, nil, &controllerStdout, &controllerStderr)
		close(done)
	}()
	t.Cleanup(func() {
		running, _ := sp.ListRunning("")
		for _, name := range running {
			sp.release(name)
		}
		tryStopController(dir, &bytes.Buffer{})
		// Best-effort cleanup wait, not a hang detector; bumped to hangBudget
		// to avoid spurious CPU-starvation failures.
		select {
		case <-done:
		case <-time.After(hangBudget):
		}
	})

	waitForControllerAvailable(t, dir)
	if err := sp.Start(context.Background(), seededSession, runtime.Config{}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr lockedBuffer
	stopDone := make(chan int, 1)
	go func() {
		stopDone <- cmdStop([]string{dir}, &stdout, &stderr, 0, false)
	}()

	stopped := sp.waitForStops(t, 1)
	if len(stopped) != 1 || stopped[0] != seededSession {
		t.Fatalf("stop targets = %v, want [%s]", stopped, seededSession)
	}

	select {
	case code := <-stopDone:
		t.Fatalf("cmdStop returned early with code %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	case <-time.After(200 * time.Millisecond):
	}

	sp.release(stopped[0])

	var code int
	awaitCond(t, func() bool {
		select {
		case code = <-stopDone:
			return true
		default:
			return false
		}
	}, "cmdStop to finish after releasing controller shutdown")
	if code != 0 {
		t.Fatalf("cmdStop = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	awaitClose(t, done, "controller to exit after cmdStop")

	if pid := controllerAlive(dir); pid != 0 {
		t.Fatalf("controllerAlive after cmdStop = %d, want 0", pid)
	}
	if !strings.Contains(stdout.String(), "Controller stopping...") {
		t.Fatalf("stdout missing controller stop message: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "City stopped.") {
		t.Fatalf("stdout missing city stopped message: %q", stdout.String())
	}
	if stderr.String() != "" {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
}

func TestCmdStopWallClockTimeoutBoundsDirectStop(t *testing.T) {
	t.Setenv("GC_HOME", shortSocketTempDir(t, "gc-home-"))

	cityDir := shortSocketTempDir(t, "gc-stop-timeout-")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "timeout-city"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "0s"},
		Agents: []config.Agent{
			{Name: "worker", StartCommand: "sleep 1"},
		},
	}
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	sp := newHangingProvider()
	sessionName := lookupSessionNameOrLegacy(nil, loadedCityName(cfg, cityDir), cfg.Agents[0].QualifiedName(), cfg.Workspace.SessionTemplate)
	if err := sp.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
		t.Fatal(err)
	}

	// cmdStop's wall-clock cap returns 1 while its worker is still blocked in
	// hangingProvider.Stop. The worker eventually calls back into
	// shutdownBeadsProviderForStop; if it does so after another test has
	// installed its own override, the global state races. Capture the worker's
	// done channel via stopBodyLifecycleHook and wait for it to close in
	// teardown so the leaked goroutine cannot outlive this test.
	oldFactory := sessionProviderForStopCity
	oldHook := stopBodyLifecycleHook
	var bodyDone <-chan struct{}
	stopBodyLifecycleHook = func(done <-chan struct{}) { bodyDone = done }
	sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) {
		return sp, nil
	}
	t.Cleanup(func() {
		sp.release()
		if bodyDone != nil {
			// Reports via Errorf (not Fatal) so a stuck goroutine doesn't skip
			// the global-state restore below; bumped to hangBudget.
			select {
			case <-bodyDone:
			case <-time.After(hangBudget):
				t.Errorf("gc stop worker did not exit after hangingProvider release")
			}
		}
		sessionProviderForStopCity = oldFactory
		stopBodyLifecycleHook = oldHook
	})

	var stdout, stderr lockedBuffer
	const testWallClockCap = 100 * time.Millisecond
	started := time.Now()
	code := cmdStop([]string{cityDir}, &stdout, &stderr, testWallClockCap, false)
	if code != 1 {
		t.Fatalf("cmdStop() = %d, want timeout code 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	// This is the "subject under test" exception in TESTING.md's Test deadline
	// rule, not a hang budget: it asserts cmdStop actually honors
	// testWallClockCap instead of blocking indefinitely on the still-hung
	// provider, so it must stay well below hangBudget. The multiplier is
	// evidence-based rather than arbitrary -- the previous 10x bound (1s) was
	// still too tight under real make test-fast-parallel shard contention
	// (observed 1.230021478s).
	if elapsed := time.Since(started); elapsed > 50*testWallClockCap {
		t.Fatalf("cmdStop returned after %s, want wall-clock cap near %s", elapsed, testWallClockCap)
	}
	if !strings.Contains(stderr.String(), fmt.Sprintf("timed out after %s", testWallClockCap)) {
		t.Fatalf("stderr = %q, want wall-clock timeout message", stderr.String())
	}
}

func TestCmdStopForceDelegatesImmediateControllerStop(t *testing.T) {
	t.Setenv("GC_HOME", shortSocketTempDir(t, "gc-home-"))

	dir := shortSocketTempDir(t, "gc-force-stop-")
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "force-stop-city"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "250ms"},
	}
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	tomlPath := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(tomlPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	sp := newRecordingStopProvider()
	buildFn := func(_ *config.City, _ runtime.Provider, _ beads.Store) DesiredStateResult {
		return DesiredStateResult{State: map[string]TemplateParams{}}
	}

	var controllerStdout, controllerStderr lockedBuffer
	done := make(chan struct{})
	go func() {
		runController(dir, nil, tomlPath, cfg, "", buildFn, nil, sp, nil, nil, nil, nil, 0, events.Discard, nil, &controllerStdout, &controllerStderr)
		close(done)
	}()
	t.Cleanup(func() {
		tryStopController(dir, &bytes.Buffer{})
		// Best-effort cleanup wait, not a hang detector; bumped to hangBudget
		// to avoid spurious CPU-starvation failures.
		select {
		case <-done:
		case <-time.After(hangBudget):
		}
	})

	waitForControllerAvailable(t, dir)
	const sess = "force-stop-session"
	if err := sp.Start(context.Background(), sess, runtime.Config{}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr lockedBuffer
	stopDone := make(chan int, 1)
	go func() {
		stopDone <- cmdStop([]string{dir}, &stdout, &stderr, 5*time.Second, true)
	}()

	select {
	case interrupted := <-sp.interrupts:
		t.Fatalf("gc stop --force delegated interrupt for %q; want immediate stop", interrupted)
	case stopped := <-sp.stops:
		if stopped != sess {
			t.Fatalf("stopped = %q, want %q", stopped, sess)
		}
	case <-time.After(hangBudget):
		t.Fatal("timed out waiting for delegated force stop")
	}

	select {
	case code := <-stopDone:
		if code != 0 {
			t.Fatalf("cmdStop = %d, want 0; stdout=%q stderr=%q controller stderr=%q", code, stdout.String(), stderr.String(), controllerStderr.String())
		}
	case <-time.After(hangBudget):
		t.Fatal("cmdStop did not finish after delegated force stop")
	}
}

func TestCmdStopForceEscalatesInProgressControllerStop(t *testing.T) {
	t.Setenv("GC_HOME", shortSocketTempDir(t, "gc-home-"))

	dir := shortSocketTempDir(t, "gc-force-escalate-")
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "force-escalate-city"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "5s"},
	}
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	tomlPath := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(tomlPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	sp := newGatedStopProvider()
	buildFn := func(_ *config.City, _ runtime.Provider, _ beads.Store) DesiredStateResult {
		return DesiredStateResult{State: map[string]TemplateParams{}}
	}

	var controllerStdout, controllerStderr lockedBuffer
	done := make(chan struct{})
	go func() {
		runController(dir, nil, tomlPath, cfg, "", buildFn, nil, sp, nil, nil, nil, nil, 0, events.Discard, nil, &controllerStdout, &controllerStderr)
		close(done)
	}()
	t.Cleanup(func() {
		tryStopControllerWithForce(dir, io.Discard, true)
		// Best-effort cleanup wait, not a hang detector; bumped to hangBudget
		// to avoid spurious CPU-starvation failures.
		select {
		case <-done:
		case <-time.After(hangBudget):
		}
	})

	waitForControllerAvailable(t, dir)
	const sess = "force-escalate-session"
	if err := sp.Start(context.Background(), sess, runtime.Config{}); err != nil {
		t.Fatal(err)
	}

	var normalStdout, normalStderr lockedBuffer
	normalDone := make(chan int, 1)
	go func() {
		normalDone <- cmdStop([]string{dir}, &normalStdout, &normalStderr, 0, false)
	}()

	interrupted := sp.waitForInterrupts(t, 1)
	if interrupted[0] != sess {
		t.Fatalf("interrupted = %q, want %q", interrupted[0], sess)
	}

	var forceStdout, forceStderr lockedBuffer
	forceDone := make(chan int, 1)
	go func() {
		forceDone <- cmdStop([]string{dir}, &forceStdout, &forceStderr, 0, true)
	}()

	stopped := sp.waitForStops(t, 1)
	if stopped[0] != sess {
		t.Fatalf("stopped = %q, want %q", stopped[0], sess)
	}
	sp.release(stopped[0])
	sp.releaseInterrupt(interrupted[0])

	for _, result := range []struct {
		name string
		ch   <-chan int
		out  *lockedBuffer
		err  *lockedBuffer
	}{
		{name: "normal stop", ch: normalDone, out: &normalStdout, err: &normalStderr},
		{name: "force stop", ch: forceDone, out: &forceStdout, err: &forceStderr},
	} {
		var code int
		awaitCond(t, func() bool {
			select {
			case code = <-result.ch:
				return true
			default:
				return false
			}
		}, fmt.Sprintf("%s to finish after force escalation", result.name))
		if code != 0 {
			t.Fatalf("%s code = %d, want 0; stdout=%q stderr=%q controller stderr=%q",
				result.name, code, result.out.String(), result.err.String(), controllerStderr.String())
		}
	}
}

func TestCmdStopExplicitRegisteredRigPathUsesSharedResolver(t *testing.T) {
	resetFlags(t)
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_DIR", "")
	t.Setenv("GC_BEADS", "")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := setupCity(t, "stop-registered-rig")
	rigDir := filepath.Join(t.TempDir(), "registered-rig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	toml := fmt.Sprintf("[workspace]\nname = \"stop-registered-rig\"\n\n[beads]\nprovider = \"file\"\n\n[[agent]]\nname = \"worker\"\n\n[[rigs]]\nname = \"registered-rig\"\npath = %q\n", rigDir)
	writeRigAnywhereCityToml(t, cityDir, toml)
	registerCityForRigResolution(t, gcHome, cityDir, "stop-registered-rig")

	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int { return 0 },
		func() int { return 0 },
		func(string) (bool, string, bool) { return false, "", false },
		20*time.Millisecond,
		time.Millisecond,
	)

	oldFactory := sessionProviderForStopCity
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
	var gotCityPath string
	sessionProviderForStopCity = func(_ *config.City, cityPath string) (runtime.Provider, error) {
		gotCityPath = cityPath
		return runtime.NewFake(), nil
	}

	var stdout, stderr lockedBuffer
	code := cmdStop([]string{rigDir}, &stdout, &stderr, 0, false)
	if code != 0 {
		t.Fatalf("cmdStop() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	assertSameTestPath(t, gotCityPath, cityDir)
}

func TestCmdStopExplicitCityPathIgnoresUnrelatedRegisteredCityLoadErrors(t *testing.T) {
	for _, tt := range []struct {
		name string
		arg  func(string) string
	}{
		{
			name: "city_root",
			arg:  func(cityDir string) string { return cityDir },
		},
		{
			name: "inside_city",
			arg: func(cityDir string) string {
				return filepath.Join(cityDir, "nested", "work")
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resetFlags(t)
			clearInheritedBeadsEnv(t)
			gcHome := t.TempDir()
			t.Setenv("GC_HOME", gcHome)
			t.Setenv("GC_CITY", "")
			t.Setenv("GC_CITY_PATH", "")
			t.Setenv("GC_CITY_ROOT", "")
			t.Setenv("GC_DIR", "")
			t.Setenv("GC_BEADS_SCOPE_ROOT", "")

			cityDir := setupCity(t, "stop-explicit-city")
			writeRigAnywhereCityToml(t, cityDir, "[workspace]\nname = \"stop-explicit-city\"\n\n[beads]\nprovider = \"file\"\n\n[[agent]]\nname = \"worker\"\n")
			arg := tt.arg(cityDir)
			if err := os.MkdirAll(arg, 0o755); err != nil {
				t.Fatal(err)
			}

			badCity := setupCity(t, "broken-registered-city")
			registerCityForRigResolution(t, gcHome, badCity, "broken-registered-city")
			if err := os.WriteFile(filepath.Join(badCity, "city.toml"), []byte("[workspace\nname = \"broken\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			withSupervisorTestHooks(
				t,
				func(_, _ io.Writer) int { return 0 },
				func(_, _ io.Writer) int { return 0 },
				func() int { return 0 },
				func(string) (bool, string, bool) { return false, "", false },
				20*time.Millisecond,
				time.Millisecond,
			)

			oldFactory := sessionProviderForStopCity
			t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
			var gotCityPath string
			sessionProviderForStopCity = func(_ *config.City, cityPath string) (runtime.Provider, error) {
				gotCityPath = cityPath
				return runtime.NewFake(), nil
			}

			var stdout, stderr lockedBuffer
			code := cmdStop([]string{arg}, &stdout, &stderr, 0, false)
			if code != 0 {
				t.Fatalf("cmdStop() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			assertSameTestPath(t, gotCityPath, cityDir)
			if strings.Contains(stderr.String(), "loading registered city rig bindings") {
				t.Fatalf("stderr = %q, want unrelated registered-city load error ignored for explicit city path", stderr.String())
			}
		})
	}
}

func TestCmdStopSupervisorManagedInvalidCityTomlWaitsForControllerStop(t *testing.T) {
	cityDir := setupSupervisorManagedInvalidCity(t)
	var waitedPath string
	waitForSupervisorControllerStopHook = func(path string, _ time.Duration) error {
		waitedPath = path
		return nil
	}

	var stdout, stderr lockedBuffer
	code := cmdStop([]string{cityDir}, &stdout, &stderr, 0, false)
	if code != 0 {
		t.Fatalf("cmdStop() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	assertSameTestPath(t, waitedPath, cityDir)
	if !strings.Contains(stdout.String(), "City stopped.") {
		t.Fatalf("stdout missing city stopped message: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "invalid config") {
		t.Fatalf("stderr = %q, want invalid config warning", stderr.String())
	}
}

func setupSupervisorManagedInvalidCity(t *testing.T) string {
	t.Helper()
	resetFlags(t)
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := filepath.Join(t.TempDir(), "invalid-supervisor-city")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace\nname = \"broken\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := registryAt(t, gcHome)
	if err := reg.Register(cityDir, "registered-invalid-city"); err != nil {
		t.Fatal(err)
	}

	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int { return 0 },
		func() int { return 4242 },
		func(string) (bool, string, bool) { return false, "", true },
		20*time.Millisecond,
		time.Millisecond,
	)
	return cityDir
}

func TestCmdStopWallClockTimeoutBoundsSupervisorManagedInvalidConfigStop(t *testing.T) {
	cityDir := setupSupervisorManagedInvalidCity(t)
	reg := registryAt(t, os.Getenv("GC_HOME"))
	assertOriginalRegistration := func(when string) {
		t.Helper()
		entries, err := reg.List()
		if err != nil {
			t.Fatalf("list registry %s: %v", when, err)
		}
		if len(entries) != 1 {
			t.Fatalf("registry %s = %v, want the original city entry", when, entries)
		}
		if got, want := canonicalTestPath(entries[0].Path), canonicalTestPath(cityDir); got != want {
			t.Fatalf("registry path %s = %q, want %q", when, got, want)
		}
		if got, want := entries[0].EffectiveName(), "registered-invalid-city"; got != want {
			t.Fatalf("registry name %s = %q, want %q", when, got, want)
		}
	}
	waitEntered := make(chan struct{})
	releaseWait := make(chan struct{})
	waitExited := make(chan struct{})
	waitForSupervisorControllerStopHook = func(string, time.Duration) error {
		close(waitEntered)
		<-releaseWait
		close(waitExited)
		return nil
	}

	oldHook := stopBodyLifecycleHook
	var bodyDone <-chan struct{}
	stopBodyLifecycleHook = func(done <-chan struct{}) { bodyDone = done }

	var stdout, stderr lockedBuffer
	stopDone := make(chan int, 1)
	commandExited := make(chan struct{})
	released := false
	workerDrained := false
	releaseAndDrainWorker := func() {
		if !released {
			close(releaseWait)
			released = true
		}
		select {
		case <-waitExited:
		case <-time.After(hangBudget):
			t.Errorf("supervisor controller wait did not exit after release")
		}
		select {
		case <-commandExited:
		case <-time.After(hangBudget):
			t.Errorf("gc stop command did not exit after supervisor wait release")
		}
		if bodyDone != nil {
			select {
			case <-bodyDone:
			case <-time.After(hangBudget):
				t.Errorf("gc stop worker did not exit after supervisor wait release")
			}
		}
		workerDrained = true
	}
	const testWallClockCap = 100 * time.Millisecond
	started := time.Now()
	go func() {
		defer close(commandExited)
		stopDone <- cmdStopJSON([]string{cityDir}, &stdout, &stderr, testWallClockCap, false, true)
	}()
	t.Cleanup(func() {
		if !workerDrained {
			releaseAndDrainWorker()
		}
		stopBodyLifecycleHook = oldHook
	})

	select {
	case <-waitEntered:
	case <-time.After(hangBudget):
		t.Fatal("gc stop did not enter the supervisor controller wait")
	}

	var code int
	select {
	case code = <-stopDone:
	case <-time.After(50 * testWallClockCap):
		t.Fatalf("cmdStop did not honor wall-clock cap %s while unregistering invalid-config city", testWallClockCap)
	}
	if code != 1 {
		t.Fatalf("cmdStop() = %d, want timeout code 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if elapsed := time.Since(started); elapsed > 50*testWallClockCap {
		t.Fatalf("cmdStop returned after %s, want wall-clock cap near %s", elapsed, testWallClockCap)
	}
	if !strings.Contains(stderr.String(), fmt.Sprintf("timed out after %s", testWallClockCap)) {
		t.Fatalf("stderr = %q, want wall-clock timeout message", stderr.String())
	}
	assertOriginalRegistration("when the timeout returns")
	if !strings.Contains(stderr.String(), "restored registration for 'registered-invalid-city'") {
		t.Fatalf("stderr = %q, want timeout rollback message", stderr.String())
	}
	releaseAndDrainWorker()
	assertOriginalRegistration("after the late worker exits")
	if stdout.String() != "" {
		t.Fatalf("stdout = %q after timed-out worker exited, want no late success JSON", stdout.String())
	}
	if !strings.Contains(stderr.String(), "invalid config") {
		t.Fatalf("stderr = %q after timed-out worker exited, want invalid-config diagnostic", stderr.String())
	}
}

func TestControllerStopTimeoutUsesHostingMode(t *testing.T) {
	tests := []struct {
		name string
		mode controllerHostingMode
		want string
	}{
		{name: "supervisor", mode: controllerHostingSupervisor, want: "supervisor-hosted controller"},
		{name: "standalone", mode: controllerHostingStandalone, want: "standalone controller"},
		{name: "legacy unknown", mode: controllerHostingUnknown, want: "waiting for controller (PID 4242)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := controllerStopTimeoutError(controllerIdentityReply{PID: 4242, HostingMode: tt.mode}, false)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("controllerStopTimeoutError = %v, want %q", err, tt.want)
			}
			if tt.mode == controllerHostingUnknown && strings.Contains(err.Error(), "standalone") {
				t.Fatalf("controllerStopTimeoutError = %v, legacy unknown must not be labeled standalone", err)
			}
		})
	}
}

// TestCmdStopJSONReportsUnregisteredTrueForSupervisorManagedCity pins the
// #4366 fix: gc stop --json must report that a supervisor-managed city was
// unregistered from the registry as part of the stop, not just that
// sessions stopped. Reuses the invalid-city-toml scaffolding from
// TestCmdStopSupervisorManagedInvalidCityTomlWaitsForControllerStop, the
// simplest existing setup that reaches the supervisor-managed success path.
func TestCmdStopJSONReportsUnregisteredTrueForSupervisorManagedCity(t *testing.T) {
	resetFlags(t)
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := filepath.Join(t.TempDir(), "invalid-supervisor-city")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace\nname = \"broken\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := registryAt(t, gcHome)
	if err := reg.Register(cityDir, "invalid-supervisor-city"); err != nil {
		t.Fatal(err)
	}

	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int { return 0 },
		func() int { return 4242 },
		func(string) (bool, string, bool) { return false, "", true },
		20*time.Millisecond,
		time.Millisecond,
	)
	waitForSupervisorControllerStopHook = func(string, time.Duration) error { return nil }

	var stdout, stderr lockedBuffer
	code := cmdStopJSON([]string{cityDir}, &stdout, &stderr, 5*time.Second, false, true)
	if code != 0 {
		t.Fatalf("cmdStopJSON() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var got lifecycleActionJSON
	if err := json.Unmarshal([]byte(stdout.String()), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if got.Unregistered == nil || !*got.Unregistered {
		t.Fatalf("payload.Unregistered = %v, want pointer to true; payload=%+v", got.Unregistered, got)
	}
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("registry after successful JSON stop = %v, want committed removal", entries)
	}
}

// TestCmdStopJSONReportsUnregisteredTrueWhenSupervisorNotRunning closes the
// remaining #4366 branch: a registered city whose supervisor is not alive
// falls through to the ordinary loaded-config stop, so the unregister the
// command just performed must still be reported. The sibling
// TestCmdStopJSONReportsUnregisteredTrueForSupervisorManagedCity only covers
// the alive-supervisor early return and never reaches this path, because it
// stubs the alive hook to a live PID and writes an invalid city.toml. Here
// the alive hook returns 0 and city.toml is valid, so cmdStopJSONSequence
// runs stopLoadedCity.
func TestCmdStopJSONReportsUnregisteredTrueWhenSupervisorNotRunning(t *testing.T) {
	resetFlags(t)
	gcHome := shortSocketTempDir(t, "gc-home-")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := shortSocketTempDir(t, "gc-stop-city-")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "unregistered-on-stop"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Session:   config.SessionConfig{Provider: "subprocess"},
	}
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	reg := registryAt(t, gcHome)
	if err := reg.Register(cityDir, "unregistered-on-stop"); err != nil {
		t.Fatal(err)
	}

	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int { return 0 },
		func() int { return 0 },
		func(string) (bool, string, bool) { return false, "", true },
		20*time.Millisecond,
		time.Millisecond,
	)

	oldFactory := sessionProviderForStopCity
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
	sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) {
		return runtime.NewFake(), nil
	}

	var stdout, stderr lockedBuffer
	code := cmdStopJSON([]string{cityDir}, &stdout, &stderr, 0, false, true)
	if code != 0 {
		t.Fatalf("cmdStopJSON() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var got lifecycleActionJSON
	if err := json.Unmarshal([]byte(stdout.String()), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if got.Unregistered == nil || !*got.Unregistered {
		t.Fatalf("payload.Unregistered = %v, want pointer to true; payload=%+v", got.Unregistered, got)
	}
}

func TestCmdStopSupervisorManagedInvalidCityTomlFailsWhenShutdownFails(t *testing.T) {
	resetFlags(t)
	cityDir := setupInvalidConfigManagedRuntime(t)
	gcHome := os.Getenv("GC_HOME")
	reg := registryAt(t, gcHome)
	if err := reg.Register(cityDir, "invalid-supervisor-city"); err != nil {
		t.Fatal(err)
	}

	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int { return 0 },
		func() int { return 4242 },
		func(string) (bool, string, bool) { return false, "", true },
		20*time.Millisecond,
		time.Millisecond,
	)
	waitForSupervisorControllerStopHook = func(string, time.Duration) error {
		return nil
	}
	overrideShutdownBeadsProviderForStop(t, func(path string) error {
		assertSameTestPath(t, path, cityDir)
		return fmt.Errorf("provider-stop-failed")
	})

	var stdout, stderr lockedBuffer
	// Input 0 selects production's normal config-derived timeout path instead
	// of an arbitrary test override; completion is observed via the package
	// hang detector below rather than via this input, so a scheduler-starved
	// run reports a hang budget failure instead of racing a tight deadline.
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- cmdStop([]string{cityDir}, &stdout, &stderr, 0, false)
	}()

	var code int
	awaitCond(t, func() bool {
		select {
		case code = <-codeCh:
			return true
		default:
			return false
		}
	}, "cmdStop to finish for invalid-config shutdown failure")
	if code != 1 {
		t.Fatalf("cmdStop() = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "City stopped.") {
		t.Fatalf("stdout = %q, did not want success message", stdout.String())
	}
	if !strings.Contains(stderr.String(), "bead store") || !strings.Contains(stderr.String(), "provider-stop-failed") {
		t.Fatalf("stderr = %q, want bead-store shutdown error", stderr.String())
	}
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !samePath(entries[0].Path, cityDir) || entries[0].EffectiveName() != "invalid-supervisor-city" {
		t.Fatalf("registry after managed-provider stop failure = %v, want exact original entry", entries)
	}
	if !strings.Contains(stderr.String(), "restored registration for 'invalid-supervisor-city'") {
		t.Fatalf("stderr = %q, want registration rollback after managed-provider stop failure", stderr.String())
	}
	if probeControllerLockHeld(t, cityDir) {
		t.Fatal("controller lock still held after failed gc stop restored the registration")
	}
}

// After the supervisor stops a registered city's controller, gc stop must keep
// the controller lock until the city's bead store is retired, so the
// supervisor (or anything else) cannot bring a controller back up against it.
func TestCmdStopSupervisorManagedCityHoldsControllerLockThroughProviderShutdown(t *testing.T) {
	for _, tc := range []struct {
		name string
		// aliveCalls is how many supervisorAliveHook calls see a live
		// supervisor; later calls see it gone.
		aliveCalls int
	}{
		{name: "supervisor stays up", aliveCalls: -1},
		// The supervisor exiting after the unregister must not send gc stop
		// down the standalone path, which would wait on the lock it holds.
		{name: "supervisor exits after unregister", aliveCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetFlags(t)
			cityDir := setupInvalidConfigManagedRuntime(t)
			reg := registryAt(t, os.Getenv("GC_HOME"))
			if err := reg.Register(cityDir, "invalid-supervisor-city"); err != nil {
				t.Fatal(err)
			}

			var aliveCalls atomic.Int32
			withSupervisorTestHooks(
				t,
				func(_, _ io.Writer) int { return 0 },
				func(_, _ io.Writer) int { return 0 },
				func() int {
					n := aliveCalls.Add(1)
					if tc.aliveCalls >= 0 && int(n) > tc.aliveCalls {
						return 0
					}
					return 4242
				},
				func(string) (bool, string, bool) { return false, "", true },
				20*time.Millisecond,
				time.Millisecond,
			)
			var waited int
			waitForSupervisorControllerStopHook = func(path string, _ time.Duration) error {
				waited++
				assertSameTestPath(t, path, cityDir)
				return nil
			}
			var shutdowns int
			heldDuringShutdown := false
			overrideShutdownBeadsProviderForStop(t, func(path string) error {
				shutdowns++
				assertSameTestPath(t, path, cityDir)
				heldDuringShutdown = probeControllerLockHeld(t, path)
				return nil
			})

			var stdout, stderr lockedBuffer
			code := cmdStop([]string{cityDir}, &stdout, &stderr, 0, false)
			if code != 0 {
				t.Fatalf("cmdStop() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if waited != 1 {
				t.Fatalf("supervisor controller-stop waits = %d, want 1", waited)
			}
			if shutdowns != 1 {
				t.Fatalf("provider shutdown calls = %d, want 1", shutdowns)
			}
			if !heldDuringShutdown {
				t.Fatal("controller lock was released before the supervisor-managed city's provider shutdown")
			}
			if probeControllerLockHeld(t, cityDir) {
				t.Fatal("controller lock still held after gc stop returned")
			}
			entries, err := reg.List()
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("registry after successful stop = %v, want committed removal", entries)
			}
		})
	}
}

// If a controller grabs the lock between the supervisor-side stop wait and gc
// stop claiming it, gc stop must not retire the bead store under that
// controller, and must hand the registration back.
func TestCmdStopSupervisorManagedCityFailsClosedWhenControllerRestartsBeforeClaim(t *testing.T) {
	resetFlags(t)
	cityDir := setupInvalidConfigManagedRuntime(t)
	reg := registryAt(t, os.Getenv("GC_HOME"))
	if err := reg.Register(cityDir, "invalid-supervisor-city"); err != nil {
		t.Fatal(err)
	}
	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int { return 0 },
		func() int { return 4242 },
		func(string) (bool, string, bool) { return false, "", true },
		20*time.Millisecond,
		time.Millisecond,
	)
	var restarted *os.File
	t.Cleanup(func() {
		if restarted != nil {
			_ = restarted.Close()
		}
	})
	waitForSupervisorControllerStopHook = func(path string, _ time.Duration) error {
		lock, err := acquireControllerLock(path)
		if err != nil {
			return err
		}
		restarted = lock
		return nil
	}
	var shutdowns int
	overrideShutdownBeadsProviderForStop(t, func(string) error {
		shutdowns++
		return nil
	})

	var stdout, stderr lockedBuffer
	code := cmdStop([]string{cityDir}, &stdout, &stderr, 0, false)
	if code != 1 {
		t.Fatalf("cmdStop() = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if shutdowns != 0 {
		t.Fatalf("provider shutdown calls = %d, want 0 while another controller holds the lock", shutdowns)
	}
	if !strings.Contains(stderr.String(), "a controller started for the city") {
		t.Fatalf("stderr = %q, want controller-restart error", stderr.String())
	}
	if !strings.Contains(stderr.String(), "restored registration for 'invalid-supervisor-city'") {
		t.Fatalf("stderr = %q, want registration rollback", stderr.String())
	}
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("registry after fail-closed stop = %v, want original entry restored", entries)
	}
}

func TestCmdStopInvalidConfigManagedRuntimeStopsAfterVerifiedShutdown(t *testing.T) {
	resetFlags(t)
	cityDir := setupInvalidConfigManagedRuntime(t)
	var shutdowns int
	heldDuringShutdown := false
	overrideShutdownBeadsProviderForStop(t, func(path string) error {
		shutdowns++
		assertSameTestPath(t, path, cityDir)
		heldDuringShutdown = probeControllerLockHeld(t, path)
		return nil
	})

	var stdout, stderr lockedBuffer
	code := cmdStop([]string{cityDir}, &stdout, &stderr, 0, false)
	if code != 0 {
		t.Fatalf("cmdStop() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "City stopped.") {
		t.Fatalf("stdout missing city stopped message: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "invalid config") {
		t.Fatalf("stderr = %q, want invalid config warning", stderr.String())
	}
	if shutdowns != 1 {
		t.Fatalf("shutdown calls = %d, want 1", shutdowns)
	}
	if !heldDuringShutdown {
		t.Fatal("controller lock was released between the absence probe and managed provider shutdown")
	}
	if probeControllerLockHeld(t, cityDir) {
		t.Fatal("controller lock still held after gc stop returned")
	}
}

func TestCmdStopInvalidConfigManagedRuntimeStopsStandaloneController(t *testing.T) {
	resetFlags(t)
	cityDir := setupInvalidConfigManagedRuntime(t)
	stopCommands := startAcknowledgingStandaloneController(t, cityDir)
	var shutdowns int
	heldDuringShutdown := false
	overrideShutdownBeadsProviderForStop(t, func(path string) error {
		shutdowns++
		assertSameTestPath(t, path, cityDir)
		heldDuringShutdown = probeControllerLockHeld(t, path)
		return nil
	})

	var stdout, stderr lockedBuffer
	code := cmdStop([]string{cityDir}, &stdout, &stderr, 0, false)
	if code != 0 {
		t.Fatalf("cmdStop() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var cmd string
	awaitCond(t, func() bool {
		select {
		case cmd = <-stopCommands:
			return true
		default:
			return false
		}
	}, fmt.Sprintf("controller to receive stop command; stdout=%q stderr=%q", stdout.String(), stderr.String()))
	if cmd != "stop" {
		t.Fatalf("controller command = %q, want stop", cmd)
	}
	if shutdowns != 1 {
		t.Fatalf("shutdown calls = %d, want 1", shutdowns)
	}
	if !heldDuringShutdown {
		t.Fatal("controller lock was released before managed provider shutdown")
	}
	if probeControllerLockHeld(t, cityDir) {
		t.Fatal("controller lock still held after gc stop returned")
	}
	if !strings.Contains(stdout.String(), "Controller stopping...") {
		t.Fatalf("stdout missing controller stop message: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "City stopped.") {
		t.Fatalf("stdout missing city stopped message: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "invalid config") {
		t.Fatalf("stderr = %q, want invalid config warning", stderr.String())
	}
}

func TestCmdStopBodyDoesNotTakeOverAfterAmbiguousControllerRequest(t *testing.T) {
	cityDir := setupCity(t, "ambiguous-controller-stop")
	writeRigAnywhereCityToml(t, cityDir, `
[workspace]
name = "ambiguous-controller-stop"

[beads]
provider = "file"
`)
	stopCommands := startStandaloneControllerWithReply(t, cityDir, nil)

	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "orphan", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	oldFactory := sessionProviderForStopCity
	sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) {
		return sp, nil
	}
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })

	cfg := &config.City{
		Workspace: config.Workspace{Name: "ambiguous-controller-stop"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "0s"},
	}
	var stdout, stderr lockedBuffer
	code := cmdStopBody(cityDir, cfg, false, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("cmdStopBody() = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if calls := sp.CountCalls("Stop", "orphan"); calls != 0 {
		t.Fatalf("direct orphan stop calls = %d, want 0 after ambiguous controller request", calls)
	}
	select {
	case command := <-stopCommands:
		if command != "stop" {
			t.Fatalf("controller command = %q, want stop", command)
		}
	case <-time.After(time.Second):
		t.Fatal("controller did not receive stop command")
	}
}

func TestCmdStopInvalidConfigManagedRuntimeFailsWhenShutdownFails(t *testing.T) {
	resetFlags(t)
	cityDir := setupInvalidConfigManagedRuntime(t)
	var shutdowns int
	overrideShutdownBeadsProviderForStop(t, func(path string) error {
		shutdowns++
		assertSameTestPath(t, path, cityDir)
		return fmt.Errorf("provider-stop-failed")
	})

	var stdout, stderr lockedBuffer
	code := cmdStop([]string{cityDir}, &stdout, &stderr, 0, false)
	if code != 1 {
		t.Fatalf("cmdStop() = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "City stopped.") {
		t.Fatalf("stdout = %q, did not want success message", stdout.String())
	}
	if !strings.Contains(stderr.String(), "bead store") || !strings.Contains(stderr.String(), "provider-stop-failed") {
		t.Fatalf("stderr = %q, want bead-store shutdown error", stderr.String())
	}
	if shutdowns != 1 {
		t.Fatalf("shutdown calls = %d, want 1", shutdowns)
	}
}

func TestStopCityManagedBeadsProviderUsesProviderStateWhenPublishedStateIsMissing(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := t.TempDir()
	_ = writeReachableProviderManagedDoltState(t, cityDir)

	var shutdowns int
	overrideShutdownBeadsProviderForStop(t, func(path string) error {
		shutdowns++
		assertSameTestPath(t, path, cityDir)
		return nil
	})

	stopped, err := stopCityManagedBeadsProvider(cityDir)
	if err != nil {
		t.Fatalf("stopCityManagedBeadsProvider() error = %v", err)
	}
	if !stopped {
		t.Fatal("stopCityManagedBeadsProvider() stopped = false, want true")
	}
	if shutdowns != 1 {
		t.Fatalf("shutdown calls = %d, want 1", shutdowns)
	}
	if _, err := os.Stat(managedDoltStatePath(cityDir)); !os.IsNotExist(err) {
		t.Fatalf("stop detection should not publish runtime state, stat err = %v", err)
	}
}

func setupInvalidConfigManagedRuntime(t *testing.T) string {
	t.Helper()

	t.Setenv("GC_HOME", shortSocketTempDir(t, "gc-home-"))
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_DOLT", "")

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace\nname = \"broken\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	state := doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityDir, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeDoltState(cityDir, state); err != nil {
		t.Fatalf("writeDoltState: %v", err)
	}

	return cityDir
}

func overrideShutdownBeadsProviderForStop(t *testing.T, fn func(string) error) {
	t.Helper()
	old := shutdownBeadsProviderForStop
	shutdownBeadsProviderForStop = fn
	t.Cleanup(func() { shutdownBeadsProviderForStop = old })
}

func startAcknowledgingStandaloneController(t *testing.T, cityDir string) <-chan string {
	t.Helper()
	return startStandaloneControllerWithReply(t, cityDir, []byte("ok\n"))
}

func startStandaloneControllerWithReply(t *testing.T, cityDir string, reply []byte) <-chan string {
	t.Helper()

	lock, err := acquireControllerLock(cityDir)
	if err != nil {
		t.Fatalf("acquireControllerLock: %v", err)
	}
	sockPath := controllerSocketPath(cityDir)
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(sockPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen controller socket: %v", err)
	}
	commands := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := lis.Accept()
		if err != nil {
			return
		}
		defer conn.Close() //nolint:errcheck // best-effort test cleanup
		buf := make([]byte, 64)
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		commands <- strings.TrimSpace(string(buf[:n]))
		if len(reply) > 0 {
			_, _ = conn.Write(reply)
		}
		_ = lis.Close()
		_ = os.Remove(sockPath)
		_ = lock.Close()
	}()
	t.Cleanup(func() {
		_ = lis.Close()
		_ = os.Remove(sockPath)
		_ = lock.Close()
		// Best-effort cleanup wait, not a hang detector; bumped to hangBudget
		// to avoid spurious CPU-starvation failures.
		select {
		case <-done:
		case <-time.After(hangBudget):
		}
	})
	return commands
}

func TestDefaultStopWallClockTimeoutScalesWithConfiguredStopTargets(t *testing.T) {
	origStop := stopPerTargetTimeoutDefault
	origMargin := interruptPerTargetTimeoutMargin
	stopPerTargetTimeoutDefault = 10 * time.Second
	interruptPerTargetTimeoutMargin = time.Second
	t.Cleanup(func() {
		stopPerTargetTimeoutDefault = origStop
		interruptPerTargetTimeoutMargin = origMargin
	})

	cfg := &config.City{
		Daemon: config.DaemonConfig{ShutdownTimeout: "2s"},
	}
	for i := 0; i < 7; i++ {
		cfg.Agents = append(cfg.Agents, config.Agent{Name: fmt.Sprintf("worker-%d", i+1)})
	}

	got := defaultStopWallClockTimeout(cfg)
	// One stop pass budgets a 3s interrupt-dispatch cap, 2s graceful-exit
	// wait, and three 10s stop waves. The default cap allows two passes plus
	// one extra orphan-cleanup stop wave: 2*(3s+2s+30s)+10s.
	want := 80 * time.Second
	if got != want {
		t.Fatalf("defaultStopWallClockTimeout() = %s, want %s", got, want)
	}
}

func TestStopCityManagedBeadsProviderAfterSuccessfulStopStopsDefaultBD(t *testing.T) {
	skipSlowCmdGCTest(t, "exercises managed bd provider shutdown; run make test-cmd-gc-process for full coverage")
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := gcBeadsBdScriptPath(cityDir)
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}

	logFile := filepath.Join(t.TempDir(), "ops.log")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" >> \""+logFile+"\"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close() //nolint:errcheck

	state := doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityDir, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	stateData, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt-state.json"), stateData, 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr lockedBuffer
	if !stopCityManagedBeadsProviderAfterSuccessfulStop(cityDir, &stderr) {
		t.Fatalf("stopCityManagedBeadsProviderAfterSuccessfulStop returned false; stderr=%q", stderr.String())
	}
	if stderr.String() != "" {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
	ops := readOpLog(t, logFile)
	if len(ops) != 1 || ops[0] != "stop" {
		t.Fatalf("provider ops = %v, want [stop]", ops)
	}
}

func TestMarkCityStopSessionSleepReasonSkipsCreatingSessions(t *testing.T) {
	store := beads.NewMemStore()
	active, err := store.Create(beads.Bead{
		Title:  "active",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"state":        "active",
			"session_name": "active",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	creating, err := store.Create(beads.Bead{
		Title:  "creating",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"state":                "creating",
			"session_name":         "creating",
			"pending_create_claim": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	markCityStopSessionSleepReason(sessionFrontDoor(store), ioDiscard{})

	activeUpdated, err := store.Get(active.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := activeUpdated.Metadata["sleep_reason"]; got != string(sessionpkg.SleepReasonCityStop) {
		t.Fatalf("active sleep_reason = %q, want %q", got, string(sessionpkg.SleepReasonCityStop))
	}
	creatingUpdated, err := store.Get(creating.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := creatingUpdated.Metadata["sleep_reason"]; got != "" {
		t.Fatalf("creating sleep_reason = %q, want empty because create rollback owns this state", got)
	}
}

func TestCmdStopUsesTargetCitySessionProviderOutsideCityDir(t *testing.T) {
	t.Setenv("GC_HOME", shortSocketTempDir(t, "gc-home-"))

	cityDir := shortSocketTempDir(t, "gc-stop-city-")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "bright-lights"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Session:   config.SessionConfig{Provider: "subprocess"},
		Agents: []config.Agent{
			{Name: "mayor", StartCommand: "sleep 1"},
		},
	}
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	otherDir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(otherDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(cwd)
	})

	oldFactory := sessionProviderForStopCity
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })

	var gotPath, gotName, gotProvider string
	sessionProviderForStopCity = func(cfg *config.City, cityPath string) (runtime.Provider, error) {
		gotPath = cityPath
		if cfg != nil {
			gotName = cfg.Workspace.Name
			gotProvider = cfg.Session.Provider
		}
		return runtime.NewFake(), nil
	}

	var stdout, stderr lockedBuffer
	code := cmdStop([]string{cityDir}, &stdout, &stderr, 0, false)
	if code != 0 {
		t.Fatalf("cmdStop() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	assertSameTestPath(t, gotPath, cityDir)
	if gotName != "bright-lights" {
		t.Fatalf("session provider cityName = %q, want %q", gotName, "bright-lights")
	}
	if gotProvider != "subprocess" {
		t.Fatalf("session provider provider = %q, want %q", gotProvider, "subprocess")
	}
}

// TestCmdStopMarginExhaustion verifies that cmdStop tolerates slow controller
// shutdowns without timing out. With a non-zero ShutdownTimeout and a provider
// whose Stop blocks briefly (simulating CI scheduling delays or an in-flight
// tick), the increased wait margin must absorb the overhead.
//
// Regression test for gastownhall/gascity#572.
func TestCmdStopMarginExhaustion(t *testing.T) {
	t.Setenv("GC_HOME", shortSocketTempDir(t, "gc-home-"))

	dir := shortSocketTempDir(t, "gc-margin-")
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-margin"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "1s"},
	}
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	sp := newGatedStopProvider()
	buildFn := func(_ *config.City, _ runtime.Provider, _ beads.Store) DesiredStateResult {
		return DesiredStateResult{State: map[string]TemplateParams{}}
	}

	var controllerStdout, controllerStderr lockedBuffer
	done := make(chan struct{})
	go func() {
		runController(dir, nil, filepath.Join(dir, "city.toml"), cfg, "", buildFn, nil, sp, nil, nil, nil, nil, 0, events.Discard, nil, &controllerStdout, &controllerStderr)
		close(done)
	}()
	t.Cleanup(func() {
		running, _ := sp.ListRunning("")
		for _, name := range running {
			sp.release(name)
		}
		tryStopController(dir, &bytes.Buffer{})
		// Best-effort cleanup wait, not a hang detector; bumped to hangBudget
		// to avoid spurious CPU-starvation failures.
		select {
		case <-done:
		case <-time.After(hangBudget):
		}
	})

	waitForControllerAvailable(t, dir)

	const sess = "margin-session"
	if err := sp.Start(context.Background(), sess, runtime.Config{}); err != nil {
		t.Fatal(err)
	}

	go func() {
		sp.waitForInterrupts(t, 1)
		sp.releaseInterrupt(sess)
	}()

	var stdout, stderr lockedBuffer
	stopDone := make(chan int, 1)
	go func() {
		stopDone <- cmdStop([]string{dir}, &stdout, &stderr, 0, false)
	}()

	stopped := sp.waitForStops(t, 1)
	if len(stopped) != 1 || stopped[0] != sess {
		t.Fatalf("stop targets = %v, want [%s]", stopped, sess)
	}

	time.AfterFunc(500*time.Millisecond, func() {
		sp.release(sess)
	})

	var code int
	awaitCond(t, func() bool {
		select {
		case code = <-stopDone:
			return true
		default:
			return false
		}
	}, "cmdStop to finish within margin budget")
	if code != 0 {
		t.Fatalf("cmdStop = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	awaitClose(t, done, "controller to exit after cmdStop")

	if !strings.Contains(stdout.String(), "Controller stopping...") {
		t.Fatalf("stdout missing controller stop message: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "City stopped.") {
		t.Fatalf("stdout missing city stopped message: %q", stdout.String())
	}
}

func waitForControllerAvailable(t *testing.T, dir string) {
	t.Helper()
	awaitCond(t, func() bool { return controllerAcceptsPing(dir, 100*time.Millisecond) },
		"controller socket accepting pings")
}

func controllerAcceptsPing(dir string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("unix", controllerSocketPath(dir), timeout)
	if err != nil {
		return false
	}
	defer conn.Close() //nolint:errcheck // best-effort cleanup
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return false
	}
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		return false
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	return err == nil && strings.TrimSpace(string(buf[:n])) != ""
}

// TestWriteCityStopSuccessReportsUnregisteredFlag pins the #4366 JSON
// envelope contract at the unit level: the unregistered bool passed to
// writeCityStopSuccess must come through verbatim (not omitted, not
// defaulted), so gc stop --json can distinguish an unmanaged-city stop from
// one that also removed a supervisor registration.
func TestWriteCityStopSuccessReportsUnregisteredFlag(t *testing.T) {
	for _, unregistered := range []bool{true, false} {
		t.Run(fmt.Sprintf("unregistered=%v", unregistered), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := writeCityStopSuccess(&stdout, &stderr, "/city", false, unregistered)
			if code != 0 {
				t.Fatalf("writeCityStopSuccess() = %d, want 0; stderr=%q", code, stderr.String())
			}
			var got lifecycleActionJSON
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
			}
			if got.Unregistered == nil || *got.Unregistered != unregistered {
				t.Fatalf("payload.Unregistered = %v, want pointer to %v; payload=%+v", got.Unregistered, unregistered, got)
			}
		})
	}
}

// TestStopHelpDocumentsSupervisorUnregisterBehavior pins the #4366
// help/behavior parity fix: gc stop's long help must state that it
// unregisters a supervisor-managed city, since cmdStopJSON actually does
// that (via unregisterCityFromSupervisorWithForce) before help readers would
// otherwise expect from "Stop all agent sessions in the city".
func TestStopHelpDocumentsSupervisorUnregisterBehavior(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newStopCmd(&stdout, &stderr)
	long := strings.ToLower(cmd.Long)
	if !strings.Contains(long, "unregister") {
		t.Fatalf("gc stop --help does not mention unregistering a supervisor-managed city; Long=%q", cmd.Long)
	}
}

// probeControllerLockHeld reports whether some other owner holds the city's
// controller lock right now, releasing the probe when it was free.
func probeControllerLockHeld(t *testing.T, cityPath string) bool {
	t.Helper()
	lock, err := acquireControllerLock(cityPath)
	switch {
	case err == nil:
		_ = lock.Close()
		return false
	case errors.Is(err, errControllerAlreadyRunning):
		return true
	default:
		t.Errorf("probing controller lock: %v", err)
		return false
	}
}

func acknowledgedControllerStopCity(t *testing.T, name string) (string, *config.City) {
	t.Helper()
	cityDir := setupCity(t, name)
	cfg := &config.City{
		Workspace: config.Workspace{Name: name},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "0s"},
	}
	return cityDir, cfg
}

// A controller that acknowledged "stop" and exited leaves gc stop as the only
// party entitled to the city until the bead store is retired. gc stop must
// hold the controller lock through that shutdown and release it afterwards,
// including when the shutdown fails.
func TestCmdStopBodyHoldsControllerLockThroughProviderShutdown(t *testing.T) {
	for _, tc := range []struct {
		name        string
		shutdownErr error
	}{
		{name: "shutdown succeeds"},
		{name: "shutdown fails", shutdownErr: errors.New("provider-stop-failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityDir, cfg := acknowledgedControllerStopCity(t, "ack-stop-ownership")
			startAcknowledgingStandaloneController(t, cityDir)

			var shutdowns int
			heldDuringShutdown := false
			overrideShutdownBeadsProviderForStop(t, func(path string) error {
				shutdowns++
				assertSameTestPath(t, path, cityDir)
				heldDuringShutdown = probeControllerLockHeld(t, path)
				return tc.shutdownErr
			})

			var stdout, stderr lockedBuffer
			code := cmdStopBody(cityDir, cfg, false, &stdout, &stderr)

			if code != 0 {
				t.Fatalf("cmdStopBody() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if shutdowns != 1 {
				t.Fatalf("provider shutdown calls = %d, want 1", shutdowns)
			}
			if !heldDuringShutdown {
				t.Fatal("controller lock was released before bead-store provider shutdown")
			}
			if probeControllerLockHeld(t, cityDir) {
				t.Fatal("controller lock still held after gc stop returned")
			}
			if tc.shutdownErr != nil && !strings.Contains(stderr.String(), "provider-stop-failed") {
				t.Fatalf("stderr = %q, want provider shutdown warning", stderr.String())
			}
		})
	}
}

// Regression for the stop/restart race: a replacement controller (supervisor
// restart, second gc start) spinning on the controller lock must not win it
// while gc stop is retiring the bead store, and must win it once gc stop is
// done.
func TestCmdStopBodyReplacementControllerCannotAcquireLockDuringProviderShutdown(t *testing.T) {
	cityDir, cfg := acknowledgedControllerStopCity(t, "ack-stop-race")
	startAcknowledgingStandaloneController(t, cityDir)

	shutdownEntered := make(chan struct{})
	replacementAcquired := make(chan struct{})
	var shutdownDone atomic.Bool
	var acquiredBeforeShutdownDone atomic.Bool
	stopReplacement := make(chan struct{})
	replacementDone := make(chan struct{})
	go func() {
		defer close(replacementDone)
		select {
		case <-shutdownEntered:
		case <-stopReplacement:
			return
		}
		for {
			lock, err := acquireControllerLock(cityDir)
			if err == nil {
				acquiredBeforeShutdownDone.Store(!shutdownDone.Load())
				_ = lock.Close()
				close(replacementAcquired)
				return
			}
			if !errors.Is(err, errControllerAlreadyRunning) {
				t.Errorf("replacement controller lock: %v", err)
				return
			}
			select {
			case <-stopReplacement:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	t.Cleanup(func() {
		close(stopReplacement)
		<-replacementDone
	})

	overrideShutdownBeadsProviderForStop(t, func(string) error {
		close(shutdownEntered)
		// Give the replacement a real window to race the shutdown; on the
		// unfixed stop path it wins the lock here immediately.
		select {
		case <-replacementAcquired:
		case <-time.After(200 * time.Millisecond):
		}
		shutdownDone.Store(true)
		return nil
	})

	var stdout, stderr lockedBuffer
	code := cmdStopBody(cityDir, cfg, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdStopBody() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	awaitClose(t, replacementAcquired, "replacement controller to acquire the lock after gc stop returned")
	if acquiredBeforeShutdownDone.Load() {
		t.Fatal("replacement controller acquired the controller lock while gc stop was retiring the bead store")
	}
}

// Ownership is only handed out once no other owner holds the lock; a
// controller that still (or again) holds it makes the wait fail closed with
// nothing held, and the returned lock excludes every other acquirer until it
// is closed.
func TestAcquireStoppedControllerOwnership(t *testing.T) {
	cityDir := setupCity(t, "stopped-controller-ownership")

	other, err := acquireControllerLock(cityDir)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := acquireStoppedControllerOwnership(cityDir, 100*time.Millisecond)
	if err == nil {
		_ = lock.Close()
		t.Fatal("acquireStoppedControllerOwnership succeeded while another owner held the controller lock")
	}
	if !strings.Contains(err.Error(), "timed out waiting") {
		t.Fatalf("error = %v, want controller-stop timeout", err)
	}
	if lock != nil {
		t.Fatal("acquireStoppedControllerOwnership returned a lock alongside an error")
	}
	_ = other.Close()

	lock, err = acquireStoppedControllerOwnership(cityDir, time.Second)
	if err != nil {
		t.Fatalf("acquireStoppedControllerOwnership() error = %v", err)
	}
	if !probeControllerLockHeld(t, cityDir) {
		_ = lock.Close()
		t.Fatal("acquireStoppedControllerOwnership returned without holding the controller lock")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if probeControllerLockHeld(t, cityDir) {
		t.Fatal("controller lock still held after closing ownership")
	}
}

// inventoryStopProvider is a runtime.Fake whose ListRunning answer and
// per-session liveness observation can be made to fail, modeling a runtime
// that gc stop cannot fully see.
type inventoryStopProvider struct {
	*runtime.Fake
	mu sync.Mutex
	// listErrs is consumed one entry per ListRunning call; a nil entry (or an
	// exhausted slice) answers from the Fake.
	listErrs    []error
	listCalls   int
	livenessErr map[string]error
}

func newInventoryStopProvider() *inventoryStopProvider {
	return &inventoryStopProvider{Fake: runtime.NewFake(), livenessErr: map[string]error{}}
}

func (p *inventoryStopProvider) ListRunning(prefix string) ([]string, error) {
	p.mu.Lock()
	call := p.listCalls
	p.listCalls++
	var err error
	if call < len(p.listErrs) {
		err = p.listErrs[call]
	}
	p.mu.Unlock()
	names, listErr := p.Fake.ListRunning(prefix)
	if err == nil {
		return names, listErr
	}
	if runtime.IsPartialListError(err) && !runtime.IsRuntimeServerAbsent(err) {
		return names, err
	}
	return nil, err
}

func (p *inventoryStopProvider) ObserveLivenessWithError(name string, processNames []string) (runtime.Liveness, error) {
	p.mu.Lock()
	err := p.livenessErr[name]
	p.mu.Unlock()
	if err != nil {
		return runtime.Liveness{}, err
	}
	return runtime.ObserveLiveness(p.Fake, name, processNames), nil
}

var _ runtime.LivenessObserverWithError = (*inventoryStopProvider)(nil)

func startStopTestSessions(t *testing.T, sp runtime.Provider, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatalf("Start(%s): %v", name, err)
		}
	}
}

func assertStopNotConfirmed(t *testing.T, code int, stdout, stderr string) {
	t.Helper()
	if code != 1 {
		t.Fatalf("stop code = %d, want fail-closed 1; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "City stopped.") {
		t.Fatalf("stdout reported terminal success for an unverified stop: %q", stdout)
	}
	if !strings.Contains(stderr, "stop not confirmed") {
		t.Fatalf("stderr = %q, want a stop-not-confirmed summary", stderr)
	}
}

// TestDoStopFailsClosedOnPartialRuntimeInventory proves gc stop withholds
// "City stopped." when a backend could not be listed, while still stopping
// every session it positively observed.
func TestDoStopFailsClosedOnPartialRuntimeInventory(t *testing.T) {
	const sessionName = "partial-stop-session"
	tests := []struct {
		name      string
		running   bool
		wantStops int
	}{
		{name: "no positively observed names"},
		{name: "positively observed names are still stopped", running: true, wantStops: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			healthy := runtime.NewFake()
			if tt.running {
				startStopTestSessions(t, healthy, sessionName)
			}
			sp := sessionhybrid.New(healthy, runtime.NewFailFake(), func(string) bool { return false })

			var stdout, stderr bytes.Buffer
			code := doStop([]string{sessionName}, sp, nil, nil, 0, events.Discard, &stdout, &stderr)

			assertStopNotConfirmed(t, code, stdout.String(), stderr.String())
			if !strings.Contains(stderr.String(), "listing sessions partially failed") {
				t.Fatalf("stderr = %q, want the partial-listing detail", stderr.String())
			}
			if got := healthy.CountCalls("Stop", sessionName); got != tt.wantStops {
				t.Fatalf("Stop calls = %d, want %d", got, tt.wantStops)
			}
			if healthy.IsRunning(sessionName) {
				t.Fatalf("positively observed session %q is still running", sessionName)
			}
		})
	}
}

// TestDoStopFailsClosedOnFailedRuntimeInventory proves a wholly failed
// ListRunning is not read as "nothing is running": the target that its own
// observation proved running is still stopped, but success is withheld.
func TestDoStopFailsClosedOnFailedRuntimeInventory(t *testing.T) {
	sp := newInventoryStopProvider()
	startStopTestSessions(t, sp, "observed-worker")
	sp.listErrs = []error{errors.New("runtime socket unreachable")}

	var stdout, stderr bytes.Buffer
	code := doStop([]string{"observed-worker"}, sp, nil, nil, 0, events.Discard, &stdout, &stderr)

	assertStopNotConfirmed(t, code, stdout.String(), stderr.String())
	if !strings.Contains(stderr.String(), "gc stop: listing sessions: runtime socket unreachable") {
		t.Fatalf("stderr = %q, want the listing failure", stderr.String())
	}
	if sp.IsRunning("observed-worker") {
		t.Fatal("session observed running by its own liveness check survived the stop")
	}
}

// TestDoStopFailsClosedWhenSessionLivenessCheckFails proves a per-session
// observation error is reported against that session and withholds success,
// without contaminating a cleanly observed sibling. An unverifiable session is
// still stopped when the runtime inventory positively witnessed it, and left
// untouched otherwise.
func TestDoStopFailsClosedWhenSessionLivenessCheckFails(t *testing.T) {
	tests := []struct {
		name      string
		witnessed bool
		wantStops int
	}{
		{name: "unwitnessed session is left untouched"},
		{name: "inventory-witnessed session is still stopped", witnessed: true, wantStops: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sp := newInventoryStopProvider()
			startStopTestSessions(t, sp, "healthy-worker")
			if tt.witnessed {
				startStopTestSessions(t, sp, "unverifiable-worker")
			}
			sp.livenessErr["unverifiable-worker"] = errors.New("liveness probe timed out")

			var stdout, stderr bytes.Buffer
			code := doStop([]string{"healthy-worker", "unverifiable-worker"}, sp, nil, nil, 0, events.Discard, &stdout, &stderr)

			assertStopNotConfirmed(t, code, stdout.String(), stderr.String())
			for _, want := range []string{"gc stop: observing session unverifiable-worker: liveness probe timed out", "could not verify that session(s) unverifiable-worker stopped"} {
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("stderr = %q, want %q", stderr.String(), want)
				}
			}
			if strings.Contains(stderr.String(), "observing session healthy-worker") {
				t.Fatalf("stderr = %q, attributed an observation failure to the cleanly observed sibling", stderr.String())
			}
			if sp.IsRunning("healthy-worker") {
				t.Fatal("cleanly observed sibling survived the stop")
			}
			if got := sp.CountCalls("Stop", "unverifiable-worker"); got != tt.wantStops {
				t.Fatalf("Stop calls for unverifiable-worker = %d, want %d", got, tt.wantStops)
			}
		})
	}
}

// TestDoStopTreatsAbsentRuntimeServerAsEmptyInventory keeps a repeated stop
// idempotent: a runtime server that is not running at all holds no sessions,
// so its absence confirms the stop instead of failing it. This holds through a
// composite provider too, as long as every failing backend is merely absent.
func TestDoStopTreatsAbsentRuntimeServerAsEmptyInventory(t *testing.T) {
	absent := func() error {
		return &runtime.PartialListError{Err: errors.New("tmux server unreachable: no tmux server running"), ServerAbsent: true}
	}
	absentProvider := func() *inventoryStopProvider {
		sp := newInventoryStopProvider()
		sp.listErrs = []error{absent(), absent()}
		return sp
	}
	tests := []struct {
		name        string
		provider    func() runtime.Provider
		wantSuccess bool
	}{
		{
			name:        "single absent server",
			provider:    func() runtime.Provider { return absentProvider() },
			wantSuccess: true,
		},
		{
			name: "absent server beside a healthy backend",
			provider: func() runtime.Provider {
				return sessionhybrid.New(absentProvider(), runtime.NewFake(), func(string) bool { return false })
			},
			wantSuccess: true,
		},
		{
			name: "absent server beside a failing backend",
			provider: func() runtime.Provider {
				return sessionhybrid.New(absentProvider(), runtime.NewFailFake(), func(string) bool { return false })
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := doStop([]string{"stopped-worker"}, tt.provider(), nil, nil, 0, events.Discard, &stdout, &stderr)
			if !tt.wantSuccess {
				assertStopNotConfirmed(t, code, stdout.String(), stderr.String())
				return
			}
			if code != 0 || !strings.Contains(stdout.String(), "City stopped.") {
				t.Fatalf("doStop = %d, want 0 with City stopped.; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if strings.Contains(stderr.String(), "listing sessions") {
				t.Fatalf("stderr = %q, want no listing warning for an absent server", stderr.String())
			}
		})
	}
}

// TestCmdStopBodyFailsClosedOnFailedOrphanInventory proves the orphan sweep's
// own inventory counts too: sessions outside the config may still be running,
// so success is withheld, while the remaining safe cleanup (server teardown,
// bead-store shutdown) still runs.
func TestCmdStopBodyFailsClosedOnFailedOrphanInventory(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "orphan-inventory-city"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "0s"},
	}
	writeStopLifecycleCityConfig(t, cityDir, cfg)

	sp := &lifecycleOrderProvider{Fake: runtime.NewFake()}
	var listCalls atomic.Int32
	failingList := &orphanListFailingProvider{lifecycleOrderProvider: sp, calls: &listCalls}

	shutdownCalled := false
	overrideShutdownBeadsProviderForStop(t, func(string) error {
		shutdownCalled = true
		return nil
	})
	oldFactory := sessionProviderForStopCity
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
	sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return failingList, nil }

	var stdout, stderr lockedBuffer
	code := cmdStopBody(cityDir, cfg, false, &stdout, &stderr)

	assertStopNotConfirmed(t, code, stdout.String(), stderr.String())
	if !strings.Contains(stderr.String(), "gc stop: listing sessions: orphan listing failed") {
		t.Fatalf("stderr = %q, want the orphan listing failure", stderr.String())
	}
	if !strings.Contains(stderr.String(), "outside the configuration") {
		t.Fatalf("stderr = %q, want the orphan-inventory summary", stderr.String())
	}
	sp.mu.Lock()
	providerEvents := append([]string(nil), sp.events...)
	sp.mu.Unlock()
	if !containsString(providerEvents, "TeardownServer") {
		t.Fatalf("provider events = %v, want server teardown to still run", providerEvents)
	}
	if !shutdownCalled {
		t.Fatal("bead-store shutdown did not run after an unconfirmed stop")
	}
}

// orphanListFailingProvider answers the first (target) ListRunning cleanly and
// fails every later one, so only the orphan sweep's inventory fails.
type orphanListFailingProvider struct {
	*lifecycleOrderProvider
	calls *atomic.Int32
}

func (p *orphanListFailingProvider) ListRunning(prefix string) ([]string, error) {
	if p.calls.Add(1) > 1 {
		p.mu.Lock()
		p.events = append(p.events, "ListRunning")
		p.mu.Unlock()
		return nil, errors.New("orphan listing failed")
	}
	return p.lifecycleOrderProvider.ListRunning(prefix)
}

// TestCmdStopFailsClosedOnPartialRuntimeInventory is the command-boundary
// proof: an unverified stop exits non-zero and never prints "City stopped.".
func TestCmdStopFailsClosedOnPartialRuntimeInventory(t *testing.T) {
	t.Setenv("GC_HOME", shortSocketTempDir(t, "gc-home-"))

	cityDir := shortSocketTempDir(t, "gc-stop-city-")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "partial-stop-city"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "0s"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "sleep 1"}},
	}
	writeStopLifecycleCityConfig(t, cityDir, cfg)
	overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })

	healthy := runtime.NewFake()
	startStopTestSessions(t, healthy, "orphan-worker")
	sp := sessionhybrid.New(healthy, runtime.NewFailFake(), func(string) bool { return false })
	oldFactory := sessionProviderForStopCity
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
	sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return sp, nil }

	var stdout, stderr lockedBuffer
	code := cmdStop([]string{cityDir}, &stdout, &stderr, 0, false)

	assertStopNotConfirmed(t, code, stdout.String(), stderr.String())
	if healthy.IsRunning("orphan-worker") {
		t.Fatal("positively observed orphan survived the stop")
	}
}

// stubAlreadyStoppedSeams installs the store-retired answer and records every
// store open and provider stop the stop body attempts.
func stubAlreadyStoppedSeams(t *testing.T, retired bool) (storeOpens, providerStops *int) {
	t.Helper()
	opens, stops := 0, 0
	oldRetired, oldOpen, oldShutdown, oldProbe := stopCityStoreRetired, openCityStoreForStop, shutdownBeadsProviderForStop, sessionProviderForStopProbe
	stopCityStoreRetired = func(string) bool { return retired }
	openCityStoreForStop = func(cityPath string) (beads.Store, error) {
		opens++
		return oldOpen(cityPath)
	}
	shutdownBeadsProviderForStop = func(string) error {
		stops++
		return nil
	}
	// The probe lists through the same runtime the full flow stops through.
	sessionProviderForStopProbe = func(cfg *config.City, cityPath string) (runtime.Provider, error) {
		return sessionProviderForStopCity(cfg, cityPath)
	}
	t.Cleanup(func() {
		stopCityStoreRetired, openCityStoreForStop, shutdownBeadsProviderForStop, sessionProviderForStopProbe = oldRetired, oldOpen, oldShutdown, oldProbe
	})
	return &opens, &stops
}

func alreadyStoppedTestCity(t *testing.T) (string, *config.City) {
	t.Helper()
	t.Setenv("GC_HOME", shortSocketTempDir(t, "gc-home-"))
	cityDir := shortSocketTempDir(t, "gc-stopped-city-")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "stopped-city"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "0s"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "sleep 1"}},
	}
	// The full flow opens the store from disk, so the file provider has to be
	// on disk too: without a city.toml the store defaults to bd.
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return cityDir, cfg
}

// A stop of a city that is already stopped must not read the bead store: on a
// provider-owned proxied store every read restarts the proxy and its Dolt
// child, which is what made a repeated `gc stop` cost seconds and dozens of bd
// forks.
func TestCmdStopBodyAlreadyStoppedCityTouchesNoStore(t *testing.T) {
	cityDir, cfg := alreadyStoppedTestCity(t)
	storeOpens, providerStops := stubAlreadyStoppedSeams(t, true)

	// The full session provider loads a session-bead snapshot from the store,
	// so the already-stopped path must list through the probe alone.
	fullProviders := 0
	oldFactory := sessionProviderForStopCity
	sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) {
		fullProviders++
		return runtime.NewFake(), nil
	}
	sessionProviderForStopProbe = func(*config.City, string) (runtime.Provider, error) { return runtime.NewFake(), nil }
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })

	var stdout, stderr bytes.Buffer
	if code := cmdStopBodyWithoutSuccess(cityDir, cfg, false, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdStopBodyWithoutSuccess = %d, want 0; stderr=%q", code, stderr.String())
	}
	if fullProviders != 0 {
		t.Errorf("stop of a stopped city built the snapshot-loading session provider %d time(s), want 0", fullProviders)
	}
	if *storeOpens != 0 {
		t.Errorf("stop of a stopped city opened the bead store %d time(s), want 0", *storeOpens)
	}
	if *providerStops != 0 {
		t.Errorf("stop of a stopped city stopped the bead store provider %d time(s), want 0", *providerStops)
	}
}

// A partially stopped city — a session still running while the proxy has
// retired — still gets the full stop: the session is stopped and the store is
// read and then retired.
func TestCmdStopBodyRunningSessionWithRetiredStoreStillStops(t *testing.T) {
	cityDir, cfg := alreadyStoppedTestCity(t)
	storeOpens, providerStops := stubAlreadyStoppedSeams(t, true)

	sp := runtime.NewFake()
	sessionName := lookupSessionNameOrLegacy(nil, loadedCityName(cfg, cityDir), cfg.Agents[0].QualifiedName(), cfg.Workspace.SessionTemplate)
	if err := sp.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	oldFactory := sessionProviderForStopCity
	sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return sp, nil }
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })

	var stdout, stderr bytes.Buffer
	if code := cmdStopBodyWithoutSuccess(cityDir, cfg, false, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdStopBodyWithoutSuccess = %d, want 0; stderr=%q", code, stderr.String())
	}
	if sp.IsRunning(sessionName) {
		t.Errorf("session %s still running after stop", sessionName)
	}
	if *storeOpens != 1 || *providerStops != 1 {
		t.Errorf("store opens = %d, provider stops = %d; want 1 and 1", *storeOpens, *providerStops)
	}
}

// With nothing running but a store that is not provably retired (a live or
// killed proxy, a stray Dolt child), the full flow still retires the store.
func TestCmdStopBodyStoppedRuntimeWithLiveStoreRetiresStore(t *testing.T) {
	cityDir, cfg := alreadyStoppedTestCity(t)
	storeOpens, providerStops := stubAlreadyStoppedSeams(t, false)

	oldFactory := sessionProviderForStopCity
	sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return runtime.NewFake(), nil }
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })

	var stdout, stderr bytes.Buffer
	if code := cmdStopBodyWithoutSuccess(cityDir, cfg, false, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdStopBodyWithoutSuccess = %d, want 0; stderr=%q", code, stderr.String())
	}
	if *storeOpens != 1 || *providerStops != 1 {
		t.Errorf("store opens = %d, provider stops = %d; want 1 and 1", *storeOpens, *providerStops)
	}
}

func TestCityStoreRetiredForStop(t *testing.T) {
	const proxiedMetadata = `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"hq"}`
	newCity := func(t *testing.T, metadata string) string {
		t.Helper()
		city := t.TempDir()
		if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"t\"\n[beads]\nprovider = \"bd\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		writeScopeBeadsMetadata(t, city, metadata)
		if err := persistProviderScopeOwnership(city, city, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
			t.Fatal(err)
		}
		if err := markProviderScopeOwnershipReady(city, city); err != nil {
			t.Fatal(err)
		}
		return city
	}

	t.Run("provider-owned proxied scope with no proxy record", func(t *testing.T) {
		if !cityStoreRetiredForStop(newCity(t, proxiedMetadata)) {
			t.Fatal("a cleanly retired proxied scope was not reported retired")
		}
	})
	t.Run("proxy record left behind", func(t *testing.T) {
		city := newCity(t, proxiedMetadata)
		root := filepath.Join(city, ".beads", "dolt")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		// Any record at all — here one that does not decode — means the proxy
		// did not exit cleanly, so bd's own stop has to run.
		if err := os.WriteFile(filepath.Join(root, "proxy.pid"), []byte("not a record"), 0o644); err != nil {
			t.Fatal(err)
		}
		if cityStoreRetiredForStop(city) {
			t.Fatal("a scope with a proxy record was reported retired")
		}
	})
	t.Run("scope bound in direct mode", func(t *testing.T) {
		// A direct-mode scope has no proxy record to read, so "no record"
		// proves nothing about whether its server is down.
		if cityStoreRetiredForStop(newCity(t, `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`)) {
			t.Fatal("a direct-mode scope was reported to have a retired proxy")
		}
	})
	t.Run("file store", func(t *testing.T) {
		city := t.TempDir()
		if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"t\"\n[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if cityStoreRetiredForStop(city) {
			t.Fatal("a file store city was reported to have a retired proxied store")
		}
	})
}
