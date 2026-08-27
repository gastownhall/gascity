package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
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
		runController(dir, tomlPath, cfg, "", buildFn, nil, sp, nil, nil, nil, nil, events.Discard, nil, &controllerStdout, &controllerStderr)
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
		runController(dir, tomlPath, cfg, "", buildFn, nil, sp, nil, nil, nil, nil, events.Discard, nil, &controllerStdout, &controllerStderr)
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
		runController(dir, tomlPath, cfg, "", buildFn, nil, sp, nil, nil, nil, nil, events.Discard, nil, &controllerStdout, &controllerStderr)
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
}

func TestCmdStopInvalidConfigManagedRuntimeStopsAfterVerifiedShutdown(t *testing.T) {
	resetFlags(t)
	cityDir := setupInvalidConfigManagedRuntime(t)
	var shutdowns int
	overrideShutdownBeadsProviderForStop(t, func(path string) error {
		shutdowns++
		assertSameTestPath(t, path, cityDir)
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
}

func TestCmdStopInvalidConfigManagedRuntimeStopsStandaloneController(t *testing.T) {
	resetFlags(t)
	cityDir := setupInvalidConfigManagedRuntime(t)
	stopCommands := startAcknowledgingStandaloneController(t, cityDir)
	var shutdowns int
	overrideShutdownBeadsProviderForStop(t, func(path string) error {
		shutdowns++
		assertSameTestPath(t, path, cityDir)
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
		runController(dir, filepath.Join(dir, "city.toml"), cfg, "", buildFn, nil, sp, nil, nil, nil, nil, events.Discard, nil, &controllerStdout, &controllerStderr)
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
