package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// governorClock is a settable stand-in for providerRecoverNow.
type governorClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *governorClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *governorClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// stubGovernorClock pins providerRecoverNow and a 30 s providerRecoverCooldown.
func stubGovernorClock(t *testing.T) *governorClock {
	t.Helper()
	clock := &governorClock{now: time.Unix(1_700_000_000, 0).UTC()}
	prevNow, prevCD := providerRecoverNow, providerRecoverCooldown
	providerRecoverNow = clock.Now
	providerRecoverCooldown = func() time.Duration { return 30 * time.Second }
	t.Cleanup(func() { providerRecoverNow, providerRecoverCooldown = prevNow, prevCD })
	return clock
}

// resetManagedDoltGovernor clears every piece of per-city governor state so a
// test starts and leaves the process clean.
func resetManagedDoltGovernor(t *testing.T, cityPath string) {
	t.Helper()
	key := normalizePathForCompare(cityPath)
	reset := func() {
		managedDoltFlights.Delete(key)
		managedDoltHolds.Delete(key)
		managedDoltControllerCities.Delete(key)
		lastBeadsProviderRecover.Delete(key)
	}
	reset()
	t.Cleanup(reset)
}

// governorFakes writes a gc-beads-bd script whose health op fails and whose
// recover op fails after logging, so every flight ends in an error. A
// non-empty healthGate or recoverGate makes that op wait until the named file
// exists. The ops log gets "health", "recover" and "recover-done" lines.
func governorFakes(t *testing.T, cityPath, healthGate, recoverGate string) string {
	t.Helper()
	opsFile := writeBreakerAwarePreflightFakes(t, cityPath, "unhealthy")
	wait := func(gate string) string {
		if gate == "" {
			return ":"
		}
		return fmt.Sprintf("while [ ! -e %q ]; do sleep 0.02; done", gate)
	}
	body := fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s\n' "$1" >> %q
case "$1" in
  health)
    %s
    printf 'unhealthy\n' >&2
    exit 1
    ;;
  recover)
    %s
    printf 'recover-done\n' >> %q
    exit 1
    ;;
  *)
    exit 2
    ;;
esac
`, opsFile, wait(healthGate), wait(recoverGate), opsFile)
	if err := os.WriteFile(gcBeadsBdScriptPath(cityPath), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return opsFile
}

// governorCity returns a legacy-managed city whose provider ops fail.
func governorCity(t *testing.T, healthGate, recoverGate string) (cityPath, opsFile string) {
	t.Helper()
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "")
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")
	cityPath = t.TempDir()
	writeMinimalCityToml(t, cityPath)
	opsFile = governorFakes(t, cityPath, healthGate, recoverGate)
	resetManagedDoltGovernor(t, cityPath)
	return cityPath, opsFile
}

func readGovernorOps(t *testing.T, opsFile string) []string {
	t.Helper()
	data, err := os.ReadFile(opsFile)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read provider ops: %v", err)
	}
	return strings.Fields(string(data))
}

func waitForGovernorOp(t *testing.T, opsFile, op string) {
	t.Helper()
	ok := pollGovernor(func() bool {
		for _, got := range readGovernorOps(t, opsFile) {
			if got == op {
				return true
			}
		}
		return false
	})
	if !ok {
		t.Fatalf("provider op %q never ran; ops = %v", op, readGovernorOps(t, opsFile))
	}
}

// pollGovernor reports whether cond became true within ten seconds.
func pollGovernor(cond func() bool) bool {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(10 * time.Second)
	for !cond() {
		select {
		case <-ticker.C:
		case <-deadline:
			return cond()
		}
	}
	return true
}

// waitForGovernorWaiters blocks until n goroutines are parked waiting on the
// city's flight, so a test knows its callers have all reached it.
func waitForGovernorWaiters(t *testing.T, n int) {
	t.Helper()
	count := func() int {
		buf := make([]byte, 1<<20)
		return strings.Count(string(buf[:runtime.Stack(buf, true)]), "(*managedDoltFlight).wait(")
	}
	if !pollGovernor(func() bool { return count() >= n }) {
		t.Fatalf("%d goroutines waiting on the flight, want %d", count(), n)
	}
}

func openGate(t *testing.T, gate string) {
	t.Helper()
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// requireFailOpenEnv checks the env a failed ladder returns: Dolt keys cleared
// and a nil error.
func requireFailOpenEnv(t *testing.T, env map[string]string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("env build error = %v, want nil (fail-open)", err)
	}
	if port := env["GC_DOLT_PORT"]; port != "" {
		t.Fatalf("GC_DOLT_PORT = %q, want cleared", port)
	}
}

// 1. Concurrent recovering env builds share one flight.
func TestManagedDoltGovernorSharesOneFlightAcrossConcurrentEnvBuilds(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "health-gate")
	cityPath, opsFile := governorCity(t, gate, "")
	stubGovernorClock(t)

	const builds = 8
	var wg sync.WaitGroup
	for i := 0; i < builds; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = bdRuntimeEnvWithErrorRecoveryContext(context.Background(), cityPath, true)
		}()
	}
	waitForGovernorOp(t, opsFile, "health")
	waitForGovernorWaiters(t, builds)
	openGate(t, gate)
	wg.Wait()

	health, recoverOps := countOps(readGovernorOps(t, opsFile), "health", "recover")
	if health != 1 || recoverOps > 1 {
		t.Fatalf("provider ops = %v, want exactly 1 health and at most 1 recover for %d concurrent builds",
			readGovernorOps(t, opsFile), builds)
	}
}

// 2. A build inside the hold of a failed flight runs no provider op.
func TestManagedDoltGovernorHoldsAfterFailedFlight(t *testing.T) {
	cityPath, opsFile := governorCity(t, "", "")
	clock := stubGovernorClock(t)

	_, _ = bdRuntimeEnvWithErrorRecoveryContext(context.Background(), cityPath, true)
	first := readGovernorOps(t, opsFile)
	if h, _ := countOps(first, "health", "recover"); h != 1 {
		t.Fatalf("first build ops = %v, want 1 health", first)
	}

	clock.Advance(10 * time.Second)
	env, err := bdRuntimeEnvWithErrorRecoveryContext(context.Background(), cityPath, true)
	requireFailOpenEnv(t, env, err)
	if got := readGovernorOps(t, opsFile); len(got) != len(first) {
		t.Fatalf("ops grew inside the hold: before %v, after %v", first, got)
	}

	clock.Advance(30 * time.Second)
	_, _ = bdRuntimeEnvWithErrorRecoveryContext(context.Background(), cityPath, true)
	if h, _ := countOps(readGovernorOps(t, opsFile), "health", "recover"); h != 2 {
		t.Fatalf("ops after the hold expired = %v, want exactly one new flight (2 health)", readGovernorOps(t, opsFile))
	}
}

// 3. A live port neither clears nor is gated by the hold, and the hold still
// refuses a bd retry-path recover until it expires.
func TestManagedDoltGovernorHoldSurvivesLivePort(t *testing.T) {
	cityPath, opsFile := governorCity(t, "", "")
	clock := stubGovernorClock(t)

	_, _ = bdRuntimeEnvWithErrorRecoveryContext(context.Background(), cityPath, true)
	before := readGovernorOps(t, opsFile)

	port := writeReachableManagedDoltState(t, cityPath)
	env, err := bdRuntimeEnvWithErrorRecoveryContext(context.Background(), cityPath, true)
	if err != nil {
		t.Fatalf("env build with a live port: %v", err)
	}
	if env["GC_DOLT_PORT"] != fmt.Sprint(port) {
		t.Fatalf("GC_DOLT_PORT = %q, want live port %d", env["GC_DOLT_PORT"], port)
	}
	if got := readGovernorOps(t, opsFile); len(got) != len(before) {
		t.Fatalf("live-port build ran provider ops: before %v, after %v", before, got)
	}

	calls := 0
	prev := recoverManagedBDCommand
	recoverManagedBDCommand = func(string) error { calls++; return nil }
	t.Cleanup(func() { recoverManagedBDCommand = prev })

	// Only the hold may refuse the recover: forget the ladder's cooldown stamp.
	lastBeadsProviderRecover.Delete(normalizePathForCompare(cityPath))
	clock.Advance(10 * time.Second)
	if err := governedManagedRecover(cityPath); err != nil || calls != 0 {
		t.Fatalf("recover inside the hold: err=%v calls=%d, want nil and 0 stub calls", err, calls)
	}
	clock.Advance(30 * time.Second)
	if err := governedManagedRecover(cityPath); err != nil || calls != 1 {
		t.Fatalf("recover after the hold: err=%v calls=%d, want nil and 1 stub call", err, calls)
	}
}

// 4. A joiner whose context ends returns promptly and leaves the flight running.
func TestManagedDoltGovernorJoinerCancelLeavesFlightRunning(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "recover-gate")
	cityPath, opsFile := governorCity(t, "", gate)
	stubGovernorClock(t)

	starterDone := make(chan struct{})
	go func() {
		defer close(starterDone)
		_, _ = bdRuntimeEnvWithErrorRecoveryContext(context.Background(), cityPath, true)
	}()
	waitForGovernorOp(t, opsFile, "recover")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := bdRuntimeEnvWithErrorRecoveryContext(ctx, cityPath, true)
		done <- err
	}()
	waitForGovernorWaiters(t, 2) // the starter and the joiner
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled joiner error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled joiner did not return while the flight was still running")
	}
	select {
	case <-starterDone:
		t.Fatal("a joiner's cancellation ended the flight")
	default:
	}
	openGate(t, gate)
	waitForGovernorOp(t, opsFile, "recover-done")
	<-starterDone
}

func TestManagedDoltGovernorShutdownEndedFlightSetsNoHold(t *testing.T) {
	cityPath, _ := governorCity(t, "", "")
	stubGovernorClock(t)

	ctx, cancel := context.WithCancel(context.Background())
	unregister := registerControllerCity(ctx, cityPath)
	t.Cleanup(unregister)
	cancel() // controller shutdown: the flight's own context is already done
	flight := startGovernedManagedDoltFlight(ctx, cityPath, func(context.Context) error {
		return errors.New("recover interrupted by shutdown")
	})
	<-flight.done
	if _, held := managedDoltHolds.Load(normalizePathForCompare(cityPath)); held {
		t.Fatal("a flight ended by controller shutdown set a hold")
	}
}

// stubBlockingRecover swaps recoverManagedBDCommand for a stub that blocks until
// release is closed, logging every call and return to events.
type governorEvents struct {
	mu  sync.Mutex
	log []string
}

func (e *governorEvents) add(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.log = append(e.log, s)
}

func (e *governorEvents) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.log...)
}

// 5. Two concurrent bd calls that hit transport errors share one recover.
func TestManagedDoltGovernorDedupesAndJoinsRetryPathRecover(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	stubGovernorClock(t)
	cityPath := t.TempDir()
	resetManagedDoltGovernor(t, cityPath)

	events := &governorEvents{}
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	origRunner, origRecover := beadsExecCommandRunnerWithEnv, recoverManagedBDCommand
	t.Cleanup(func() { beadsExecCommandRunnerWithEnv, recoverManagedBDCommand = origRunner, origRecover })

	var attemptsMu sync.Mutex
	attempts := map[string]int{}
	beadsExecCommandRunnerWithEnv = func(map[string]string) beads.CommandRunner {
		return func(dir, _ string, _ ...string) ([]byte, error) {
			attemptsMu.Lock()
			attempts[dir]++
			n := attempts[dir]
			attemptsMu.Unlock()
			events.add(fmt.Sprintf("run %s #%d", filepath.Base(dir), n))
			if n == 1 {
				started <- struct{}{}
				return nil, fmt.Errorf("server unreachable at 127.0.0.1:3307")
			}
			return []byte("ok"), nil
		}
	}
	recoverManagedBDCommand = func(string) error {
		events.add("recover start")
		<-release
		events.add("recover end")
		return nil
	}

	runner := bdCommandRunnerWithManagedRetry(cityPath, func(string) map[string]string {
		return map[string]string{"GC_DOLT_PORT": "3307"}
	})
	var wg sync.WaitGroup
	for _, dir := range []string{t.TempDir(), t.TempDir()} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := runner(dir, "bd", "list", "--json"); err != nil {
				t.Errorf("runner error = %v, want nil after retry", err)
			}
		}()
	}
	<-started
	<-started
	waitForGovernorWaiters(t, 2) // both callers are now at the recover
	close(release)
	wg.Wait()

	log := events.snapshot()
	starts, endIdx, firstRetry := 0, -1, -1
	for i, e := range log {
		switch {
		case e == "recover start":
			starts++
		case e == "recover end":
			endIdx = i
		case strings.HasSuffix(e, "#2") && firstRetry < 0:
			firstRetry = i
		}
	}
	if starts != 1 {
		t.Fatalf("recover stub calls = %d, want 1; events = %v", starts, log)
	}
	if firstRetry < endIdx || endIdx < 0 {
		t.Fatalf("a retry ran before the shared recover returned; events = %v", log)
	}
	retries := 0
	for _, e := range log {
		if strings.HasSuffix(e, "#2") {
			retries++
		}
	}
	if retries != 2 {
		t.Fatalf("retries = %d, want both callers to retry once; events = %v", retries, log)
	}
}

// 6. The retry-path recover shares the legacy branch's cooldown clock.
func TestManagedDoltGovernorRetryRecoverHonorsLegacyCooldown(t *testing.T) {
	cityPath := t.TempDir()
	resetManagedDoltGovernor(t, cityPath)
	clock := stubGovernorClock(t)

	calls := 0
	prev := recoverManagedBDCommand
	recoverManagedBDCommand = func(string) error { calls++; return nil }
	t.Cleanup(func() { recoverManagedBDCommand = prev })

	// An E2 legacy recover started now.
	lastBeadsProviderRecover.Store(normalizePathForCompare(cityPath), clock.Now())

	clock.Advance(10 * time.Second)
	if err := governedManagedRecover(cityPath); err != nil || calls != 0 {
		t.Fatalf("recover at t0+10s: err=%v calls=%d, want nil and 0", err, calls)
	}
	clock.Advance(30 * time.Second)
	if err := governedManagedRecover(cityPath); err != nil || calls != 1 {
		t.Fatalf("recover at t0+40s: err=%v calls=%d, want nil and 1", err, calls)
	}
	if _, ok := lastBeadsProviderRecover.Load(normalizePathForCompare(cityPath)); !ok {
		t.Fatal("the retry-path recover must record lastBeadsProviderRecover")
	}
}

// 7. The retry-path recover takes the per-city provider slot.
func TestManagedDoltGovernorRetryRecoverTakesProviderSlot(t *testing.T) {
	cityPath := t.TempDir()
	resetManagedDoltGovernor(t, cityPath)
	stubGovernorClock(t)

	ran := make(chan struct{})
	prev := recoverManagedBDCommand
	recoverManagedBDCommand = func(string) error { close(ran); return nil }
	t.Cleanup(func() { recoverManagedBDCommand = prev })

	release, err := acquireProviderSemaphoreForOp(cityPath, "health")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- governedManagedRecover(cityPath) }()
	select {
	case <-ran:
		t.Fatal("the recover ran while another provider op held the city slot")
	case <-time.After(300 * time.Millisecond):
	}
	release()
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("the recover never ran after the slot was freed")
	}
	if err := <-done; err != nil {
		t.Fatalf("governedManagedRecover: %v", err)
	}
}

// 8. One logical bd call with everything failing costs at most one ladder and
// one recover.
func TestManagedDoltGovernorBoundsOneLogicalBdCall(t *testing.T) {
	cityPath, opsFile := governorCity(t, "", "")
	stubGovernorClock(t)

	recoverCalls := 0
	origRunner, origRecover, origSleep := beadsExecCommandRunnerWithEnv, recoverManagedBDCommand, bdCommandRetrySleep
	t.Cleanup(func() {
		beadsExecCommandRunnerWithEnv, recoverManagedBDCommand, bdCommandRetrySleep = origRunner, origRecover, origSleep
	})
	bdCommandRetrySleep = func(time.Duration) {}
	beadsExecCommandRunnerWithEnv = func(map[string]string) beads.CommandRunner {
		return func(string, string, ...string) ([]byte, error) {
			return nil, fmt.Errorf("server unreachable at 127.0.0.1:3307")
		}
	}
	recoverManagedBDCommand = func(string) error { recoverCalls++; return errors.New("recover failed") }

	runner := bdCommandRunnerWithManagedRetryErr(cityPath, func(string) (map[string]string, error) {
		return bdRuntimeEnvWithErrorRecoveryContext(context.Background(), cityPath, true)
	})
	_, _ = runner(cityPath, "bd", "list", "--json")

	if h, _ := countOps(readGovernorOps(t, opsFile), "health", "recover"); h > 1 {
		t.Fatalf("provider ops = %v, want at most one health ladder", readGovernorOps(t, opsFile))
	}
	if recoverCalls > 1 {
		t.Fatalf("recover stub calls = %d, want at most 1", recoverCalls)
	}
}

// 9. A controller-class caller waits for a flight only until flight.start + the
// shared window; any other caller waits for the flight's end.
func TestManagedDoltGovernorControllerWaitWindow(t *testing.T) {
	cityPath := t.TempDir()
	resetManagedDoltGovernor(t, cityPath)
	stubGovernorClock(t)

	release := make(chan struct{})
	var flightRuns int
	var mu sync.Mutex
	run := func(context.Context) error {
		mu.Lock()
		flightRuns++
		mu.Unlock()
		<-release
		return errors.New("flight failed")
	}

	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	unregister := registerControllerCity(runCtx, cityPath)
	defer unregister()

	// First caller starts the flight and waits out the window.
	start := time.Now()
	flight := startGovernedManagedDoltFlight(runCtx, cityPath, run)
	if !flight.started.Before(start.Add(time.Second)) {
		t.Fatalf("flight start %v not recent", flight.started)
	}

	// A joiner arriving after flight.start + W returns at once with the
	// in-flight error, while the flight is still running.
	managedDoltFlights.Range(func(_, v any) bool {
		v.(*managedDoltFlight).started = time.Now().Add(-managedDoltFlightWindow - time.Second)
		return true
	})
	begin := time.Now()
	err := awaitGovernedManagedDoltFlight(context.Background(), cityPath, flight)
	if !errors.Is(err, errManagedDoltFlightWindow) {
		t.Fatalf("controller joiner past the window: err = %v, want errManagedDoltFlightWindow", err)
	}
	if time.Since(begin) > time.Second {
		t.Fatal("controller joiner past the window did not return at once")
	}
	if _, running := managedDoltFlightStartedAt(cityPath); !running {
		t.Fatal("the flight must keep running after a window expiry")
	}

	// An unregistered caller waits for the flight to end.
	other := t.TempDir()
	resetManagedDoltGovernor(t, other)
	oneShot := startGovernedManagedDoltFlight(context.Background(), other, func(context.Context) error {
		<-release
		return errors.New("one-shot flight failed")
	})
	waited := make(chan error, 1)
	go func() { waited <- awaitGovernedManagedDoltFlight(context.Background(), other, oneShot) }()
	select {
	case err := <-waited:
		t.Fatalf("one-shot caller returned before the flight ended: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-waited; err == nil || errors.Is(err, errManagedDoltFlightWindow) {
		t.Fatalf("one-shot caller error = %v, want the flight's own error", err)
	}
	<-flight.done
}

// 10. A window expiry is not the caller's ctx error: the build is fail-open.
func TestManagedDoltGovernorWindowExpiryIsNotContextError(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "health-gate")
	cityPath, opsFile := governorCity(t, gate, "")
	stubGovernorClock(t)

	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	t.Cleanup(registerControllerCity(runCtx, cityPath))

	// A flight that started long ago and is still running.
	flight := startGovernedManagedDoltFlight(runCtx, cityPath, func(context.Context) error {
		<-runCtx.Done()
		return nil
	})
	flight.started = time.Now().Add(-managedDoltFlightWindow - time.Second)

	callerCtx := context.Background()
	env, err := bdRuntimeEnvWithErrorRecoveryContext(callerCtx, cityPath, true)
	requireFailOpenEnv(t, env, err)
	if callerCtx.Err() != nil {
		t.Fatal("caller ctx must stay live")
	}
	if got := readGovernorOps(t, opsFile); len(got) != 0 {
		t.Fatalf("a window-expired build ran provider ops: %v", got)
	}
}

// 11. An entry called under the flight's own marker runs directly.
func TestManagedDoltGovernorReentrantEntryRunsDirectly(t *testing.T) {
	cityPath := t.TempDir()
	resetManagedDoltGovernor(t, cityPath)
	stubGovernorClock(t)

	ran := make(chan struct{})
	flight := startGovernedManagedDoltFlight(context.Background(), cityPath, func(flightCtx context.Context) error {
		done := make(chan error, 1)
		go func() {
			// Would deadlock if it joined the flight it is running inside.
			done <- runGovernedManagedDoltEntry(flightCtx, cityPath, func(context.Context) error {
				close(ran)
				return nil
			})
		}()
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			return errors.New("reentrant entry waited on its own flight")
		}
	})
	<-flight.done
	select {
	case <-ran:
	default:
		t.Fatal("reentrant entry did not run directly")
	}
	if flight.err != nil {
		t.Fatalf("flight error = %v", flight.err)
	}
}

// 13. The retry-path recover never waits on a flight past the window, whatever
// the caller's class: it cannot tell it runs inside that flight.
func TestManagedDoltGovernorRetryRecoverJoinIsWindowed(t *testing.T) {
	cityPath := t.TempDir()
	resetManagedDoltGovernor(t, cityPath)
	stubGovernorClock(t)

	calls := 0
	prev := recoverManagedBDCommand
	recoverManagedBDCommand = func(string) error { calls++; return nil }
	t.Cleanup(func() { recoverManagedBDCommand = prev })

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	flight := startGovernedManagedDoltFlight(context.Background(), cityPath, func(context.Context) error {
		<-release
		return nil
	})
	flight.started = time.Now().Add(-managedDoltFlightWindow - time.Second)

	done := make(chan error, 1)
	go func() { done <- governedManagedRecover(cityPath) }()
	select {
	case err := <-done:
		if err != nil || calls != 0 {
			t.Fatalf("governedManagedRecover past the window: err=%v calls=%d, want nil and 0", err, calls)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the retry-path recover waited on a flight past the window")
	}
}
