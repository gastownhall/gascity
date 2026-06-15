package main

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

func TestResolveManagedDoltStartAddressInUseRetryWindow_EmptyCityPathReturnsDefault(t *testing.T) {
	got := resolveManagedDoltStartAddressInUseRetryWindow("")
	if got != config.DefaultDoltStartAddressInUseRetryWindow {
		t.Fatalf("empty cityPath: got %s, want %s", got, config.DefaultDoltStartAddressInUseRetryWindow)
	}
}

// TestResolveManagedDoltStartAddressInUseRetryWindow_EmptyCityPathDoesNotReadStrayCityToml
// mirrors TestResolveManagedDoltStopTimeoutEmptyCityPathReturnsDefault: with
// an empty cityPath the resolver must NOT walk cwd, NOT materialize builtin
// packs, and NOT read a stray ./city.toml. We plant a stray config with a
// non-default value and assert (a) the default still wins and (b) no .gc/
// subtree was created.
func TestResolveManagedDoltStartAddressInUseRetryWindow_EmptyCityPathDoesNotReadStrayCityToml(t *testing.T) {
	dir := t.TempDir()
	stray := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(stray, []byte(`
[workspace]
name = "stray"

[daemon]
dolt_start_address_in_use_retry_window = "11s"
`), 0o644); err != nil {
		t.Fatalf("write stray city.toml: %v", err)
	}
	t.Chdir(dir)

	got := resolveManagedDoltStartAddressInUseRetryWindow("")
	if got != config.DefaultDoltStartAddressInUseRetryWindow {
		t.Fatalf("empty cityPath read the stray ./city.toml: got %s, want %s",
			got, config.DefaultDoltStartAddressInUseRetryWindow)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gc")); !os.IsNotExist(err) {
		t.Fatalf("empty cityPath materialized .gc subtree under cwd: %v", err)
	}
}

func TestResolveManagedDoltStartAddressInUseRetryWindow_UnloadableConfigReturnsDefault(t *testing.T) {
	// Point at a directory with no city.toml; loadCityConfig returns an error
	// and we expect the default.
	dir := t.TempDir()
	got := resolveManagedDoltStartAddressInUseRetryWindow(dir)
	if got != config.DefaultDoltStartAddressInUseRetryWindow {
		t.Fatalf("missing city.toml: got %s, want %s", got, config.DefaultDoltStartAddressInUseRetryWindow)
	}
}

func TestResolveManagedDoltStartAddressInUseRetryWindow_FromCityToml(t *testing.T) {
	dir := t.TempDir()
	cityToml := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(cityToml, []byte(`
[workspace]
name = "test-city"

[daemon]
dolt_start_address_in_use_retry_window = "7s"
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	got := resolveManagedDoltStartAddressInUseRetryWindow(dir)
	if got != 7*time.Second {
		t.Fatalf("from city.toml: got %s, want 7s", got)
	}
}

func TestResolveManagedDoltStartAddressInUseRetryWindow_ZeroDisablesRetry(t *testing.T) {
	dir := t.TempDir()
	cityToml := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(cityToml, []byte(`
[workspace]
name = "test-city"

[daemon]
dolt_start_address_in_use_retry_window = "0s"
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	got := resolveManagedDoltStartAddressInUseRetryWindow(dir)
	if got != 0 {
		t.Fatalf("0s should disable retry: got %s, want 0", got)
	}
}

// TestResolveManagedDoltStartAddressInUseRetryWindow_NegativeReturnsDefault
// verifies the load-path behavior: a negative value in city.toml is rejected
// by ValidateNonNegativeDurations during loadCityConfig, the load fails, and
// the resolver falls back to the package default.
func TestResolveManagedDoltStartAddressInUseRetryWindow_NegativeReturnsDefault(t *testing.T) {
	dir := t.TempDir()
	cityToml := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(cityToml, []byte(`
[workspace]
name = "test-city"

[daemon]
dolt_start_address_in_use_retry_window = "-5s"
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	got := resolveManagedDoltStartAddressInUseRetryWindow(dir)
	if got != config.DefaultDoltStartAddressInUseRetryWindow {
		t.Fatalf("negative in city.toml: validation should reject → default; got %s, want %s",
			got, config.DefaultDoltStartAddressInUseRetryWindow)
	}
}

func TestResolveManagedDoltStartAddressInUseRetryWindow_UnparseableReturnsDefault(t *testing.T) {
	dir := t.TempDir()
	cityToml := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(cityToml, []byte(`
[workspace]
name = "test-city"

[daemon]
dolt_start_address_in_use_retry_window = "not-a-duration"
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	got := resolveManagedDoltStartAddressInUseRetryWindow(dir)
	if got != config.DefaultDoltStartAddressInUseRetryWindow {
		t.Fatalf("unparseable: got %s, want %s", got, config.DefaultDoltStartAddressInUseRetryWindow)
	}
}

func TestManagedDoltStartAddressInUsePollInterval(t *testing.T) {
	cases := []struct {
		name        string
		retryWindow time.Duration
		want        time.Duration
	}{
		{"default-2s", 30 * time.Second, 2 * time.Second},
		{"window-exactly-2s", 2 * time.Second, 2 * time.Second},
		{"sub-2s-window-shrinks-poll", 500 * time.Millisecond, 500 * time.Millisecond},
		{"zero-window-uses-default", 0, 2 * time.Second},
		{"negative-window-uses-default", -1 * time.Second, 2 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := managedDoltStartAddressInUsePollInterval(tc.retryWindow)
			if got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// TestManagedDoltStartWaitForPortFree_WakesMidWindowOnInLoopCheck pins the
// IN-LOOP wake path: the loop's `if managedDoltPortAvailableFn(...) { return
// true }` check at the top of each iteration fires mid-window (NOT the
// post-deadline final check at the end).
//
// We need the iteration check to actually fire AFTER the sleep but BEFORE
// the deadline. That requires poll < window so the loop iterates more than
// once, AND the flag flip to happen between the first sleep and the second
// poll. Setup:
//
//   - window = 5s, so poll = default 2s (managedDoltStartAddressInUsePollInterval)
//   - flag flips to "free" at 1s (during the first sleep)
//   - first poll at t=0 returns busy, then sleep(2s)
//   - second poll at t=2s returns free → in-loop return true
//
// We assert elapsed roughly matches that second-poll wake (~2s), well below
// the 5s deadline; if the post-deadline check were the only path, elapsed
// would be ≥ 5s. We also assert at least one sleep happened (elapsed > 1.5s),
// so a degenerate "first poll already true" wouldn't accidentally pass.
func TestManagedDoltStartWaitForPortFree_WakesMidWindowOnInLoopCheck(t *testing.T) {
	orig := managedDoltPortAvailableFn
	defer func() { managedDoltPortAvailableFn = orig }()

	var free int32
	go func() {
		time.Sleep(1 * time.Second)
		atomic.StoreInt32(&free, 1)
	}()
	managedDoltPortAvailableFn = func(_ string, _ int) bool {
		return atomic.LoadInt32(&free) == 1
	}

	start := time.Now()
	got := managedDoltStartWaitForPortFree("0.0.0.0", 17360, 5*time.Second)
	elapsed := time.Since(start)
	if !got {
		t.Fatalf("expected port to become free within window, got false (elapsed=%s)", elapsed)
	}
	if elapsed < 1500*time.Millisecond {
		t.Fatalf("expected at least one sleep before wake (~2s); only waited %s — degenerate first-poll-true path?", elapsed)
	}
	if elapsed > 4*time.Second {
		t.Fatalf("wake should fire on second poll (~2s), but waited %s; in-loop wake path may be broken (post-deadline final check would be ~5s)", elapsed)
	}
}

func TestManagedDoltStartWaitForPortFree_ReturnsFalseWhenWindowExpires(t *testing.T) {
	orig := managedDoltPortAvailableFn
	defer func() { managedDoltPortAvailableFn = orig }()

	managedDoltPortAvailableFn = func(_ string, _ int) bool { return false }

	start := time.Now()
	got := managedDoltStartWaitForPortFree("0.0.0.0", 17360, 300*time.Millisecond)
	elapsed := time.Since(start)
	if got {
		t.Fatalf("expected false (port never free), got true")
	}
	if elapsed < 300*time.Millisecond {
		t.Fatalf("expected to wait the full window, only waited %s", elapsed)
	}
	if elapsed > 1*time.Second {
		t.Fatalf("waited too long (%s); poll-shrink may be broken", elapsed)
	}
}

func TestManagedDoltStartWaitForPortFree_NonPositiveWindowReturnsImmediately(t *testing.T) {
	orig := managedDoltPortAvailableFn
	defer func() { managedDoltPortAvailableFn = orig }()
	// Even if the port IS free, retry-disabled (0) should return false to
	// preserve the legacy fall-through-immediately behavior.
	managedDoltPortAvailableFn = func(_ string, _ int) bool { return true }

	start := time.Now()
	got := managedDoltStartWaitForPortFree("0.0.0.0", 17360, 0)
	if got {
		t.Fatalf("retry disabled (window=0): want false, got true")
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatalf("disabled retry should return immediately; slept %s", time.Since(start))
	}

	start = time.Now()
	got = managedDoltStartWaitForPortFree("0.0.0.0", 17360, -1*time.Second)
	if got {
		t.Fatalf("retry disabled (window=-1s): want false, got true")
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatalf("negative retry should return immediately; slept %s", time.Since(start))
	}
}

// TestManagedDoltStartWaitForPortFree_FullChainBindsHostPortViaProductionProbe
// exercises the FULL production chain (wait helper → managedDoltPortAvailableFn
// → managedDoltPortAvailableForHost → net.Listen) against a real bound socket.
// If a future refactor accidentally bypasses the indirection (e.g. by calling
// net.Listen directly), this test catches it because the wait helper would
// then read from an unstubbed probe.
func TestManagedDoltStartWaitForPortFree_FullChainBindsHostPortViaProductionProbe(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind listener: %v", err)
	}
	defer listener.Close() //nolint:errcheck
	addr := listener.Addr().(*net.TCPAddr)

	// Production indirection in place (no stub). Wait briefly; the port is
	// actively bound so the wait must return false.
	start := time.Now()
	got := managedDoltStartWaitForPortFree("127.0.0.1", addr.Port, 100*time.Millisecond)
	if got {
		t.Fatalf("wait reported port %d free while real net.Listen still holds it", addr.Port)
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Fatalf("wait should have honored the window; returned in %s", time.Since(start))
	}
}

// TestManagedDoltPortAvailableForHost_NormalizesEmptyHost confirms the host
// normalization shim ("" / "*" → "0.0.0.0") is wired and does not panic.
// Picks a fresh unused random port via net.Listen("tcp", ":0") (Linux/macOS
// will not put a never-bound port in TIME_WAIT), so the probe must report
// available for at least the wildcard / unspecified forms. The interface-
// specific 127.0.0.1 form is covered separately by the FullChain test.
func TestManagedDoltPortAvailableForHost_NormalizesEmptyHost(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind picker: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close() //nolint:errcheck

	// The wildcard host forms ("" / "*" / "0.0.0.0") all normalize to
	// 0.0.0.0; on Linux a 127.0.0.1 listener Close()'d immediately ago can
	// leave a SYN_RECV / TIME_WAIT slot, but a fresh 0.0.0.0 bind should
	// succeed since wildcard bind doesn't conflict with a closed 127.0.0.1
	// listener that was never connected to. Assert at least one wildcard
	// form reports available — otherwise the normalization is broken.
	wildcardAvailable := false
	for _, host := range []string{"", "*", "0.0.0.0"} {
		if managedDoltPortAvailableForHost(host, port) {
			wildcardAvailable = true
			break
		}
	}
	if !wildcardAvailable {
		t.Errorf("none of {\"\", \"*\", \"0.0.0.0\"} reported port %d available after Close(); host normalization may be broken", port)
	}
}

// startManagedDoltLoopTestHarness installs a complete stub set for
// startManagedDoltProcessWithOptions so the inner address-in-use branch can
// be driven without spawning a real dolt subprocess. The harness returns a
// cleanup func and a per-test cityPath wired into a real tmpdir so layout
// resolution, config-file writes, and state-file writes succeed against the
// filesystem.
//
// Stubs installed (each restored by the returned cleanup):
//
//   - managedDoltPreflightCleanupFn → no-op (skip stop-managed-dolt + lock dance)
//   - managedDoltStartSQLServerFn   → returns the configured fake-started process
//   - managedDoltWaitForReadyFn     → returns whatever waitReadyFn yields
//   - managedDoltLogSuffixFn        → returns whatever logSuffixFn yields
//   - managedDoltPortAvailableFn    → returns whatever portAvailableFn yields
//   - managedDoltStartAddressInUseRetryWindowFn → returns retryWindow constant
type startManagedDoltLoopStubs struct {
	startFn          func(cityPath, configFile, logFilePath string, logFile *os.File) (managedDoltStartedProcess, error)
	waitReadyFn      func(cityPath, host, port, user string, pid int, timeout time.Duration, checkDeleted bool) (managedDoltWaitReadyReport, error)
	logSuffixFn      func(path string, offset int64) (string, error)
	portAvailableFn  func(host string, port int) bool
	retryWindow      time.Duration
	preflightCleanup func(cityPath string) error
}

func installStartManagedDoltLoopStubs(t *testing.T, stubs startManagedDoltLoopStubs) string {
	t.Helper()
	cityPath := t.TempDir()
	// Carve a deterministic, isolated layout so resolveManagedDoltRuntimeLayout
	// can synthesize valid paths under tmpdir without touching the operator's
	// real city layout. GC_PACK_STATE_DIR is the simplest knob — it overrides
	// the per-pack path that every other layout entry derives from.
	packStateDir := filepath.Join(cityPath, "pack-state")
	if err := os.MkdirAll(packStateDir, 0o755); err != nil {
		t.Fatalf("mkdir pack state dir: %v", err)
	}
	t.Setenv("GC_PACK_STATE_DIR", packStateDir)

	origPreflight := managedDoltPreflightCleanupFn
	origStart := managedDoltStartSQLServerFn
	origWait := managedDoltWaitForReadyFn
	origLog := managedDoltLogSuffixFn
	origProbe := managedDoltPortAvailableFn
	origRetryWindow := managedDoltStartAddressInUseRetryWindowFn
	t.Cleanup(func() {
		managedDoltPreflightCleanupFn = origPreflight
		managedDoltStartSQLServerFn = origStart
		managedDoltWaitForReadyFn = origWait
		managedDoltLogSuffixFn = origLog
		managedDoltPortAvailableFn = origProbe
		managedDoltStartAddressInUseRetryWindowFn = origRetryWindow
	})

	if stubs.preflightCleanup != nil {
		managedDoltPreflightCleanupFn = stubs.preflightCleanup
	} else {
		managedDoltPreflightCleanupFn = func(string) error { return nil }
	}
	managedDoltStartSQLServerFn = stubs.startFn
	managedDoltWaitForReadyFn = stubs.waitReadyFn
	managedDoltLogSuffixFn = stubs.logSuffixFn
	managedDoltPortAvailableFn = stubs.portAvailableFn
	retryWindow := stubs.retryWindow
	managedDoltStartAddressInUseRetryWindowFn = func(string) time.Duration { return retryWindow }

	return cityPath
}

// TestStartManagedDoltProcessWithOptions_AddressInUseSamePortRetrySucceeds
// drives the full loop body through the address-in-use branch. The first
// attempt simulates dolt failing to bind (log returns the address-in-use
// substring); the wait helper returns available (probe shim) so the loop
// retries the SAME port; the second attempt succeeds (waitReady returns
// Ready=true). PR contract:
//
//   - err == nil (final attempt succeeded)
//   - report.Ready == true
//   - report.Port == originalPort (NO bump — same-port retry path taken)
//   - report.Attempts == 2 (one retry happened)
//   - startCalls == 2 (start invoked once per attempt)
//
// Note on AddressInUse: the field is reset at the top of every loop iteration
// (cmd/gc/dolt_start_managed.go:156) so the final report reflects only the
// LAST attempt. Asserting Port-unchanged + Attempts==2 is the load-bearing
// check that the retry branch fired.
func TestStartManagedDoltProcessWithOptions_AddressInUseSamePortRetrySucceeds(t *testing.T) {
	const originalPort = 17777
	var startCalls int32
	var logCalls int32
	cityPath := installStartManagedDoltLoopStubs(t, startManagedDoltLoopStubs{
		startFn: func(cityPath, _, _ string, _ *os.File) (managedDoltStartedProcess, error) {
			atomic.AddInt32(&startCalls, 1)
			return managedDoltStartedProcess{CityPath: cityPath, PID: 0}, nil
		},
		waitReadyFn: func(_, _, _, _ string, _ int, _ time.Duration, _ bool) (managedDoltWaitReadyReport, error) {
			// Attempt 1: not ready, falls through to log-read.
			// Attempt 2: ready → success exit.
			if atomic.LoadInt32(&startCalls) >= 2 {
				return managedDoltWaitReadyReport{Ready: true, PIDAlive: true}, nil
			}
			return managedDoltWaitReadyReport{Ready: false, PIDAlive: false}, nil
		},
		logSuffixFn: func(_ string, _ int64) (string, error) {
			atomic.AddInt32(&logCalls, 1)
			return "panic: listen tcp 0.0.0.0:17777: bind: address already in use\n", nil
		},
		portAvailableFn: func(_ string, _ int) bool { return true }, // wait succeeds → retry same port
		retryWindow:     50 * time.Millisecond,
	})

	report, err := startManagedDoltProcessWithOptions(cityPath, "0.0.0.0", strconv.Itoa(originalPort), "root", "warning", -1, 1*time.Second, false)
	if err != nil {
		t.Fatalf("expected success on attempt 2; got %v", err)
	}
	if !report.Ready {
		t.Errorf("report.Ready=false; expected true (attempt 2 returned Ready)")
	}
	if report.Port != originalPort {
		t.Errorf("report.Port=%d; expected %d (same-port retry must NOT bump)", report.Port, originalPort)
	}
	if report.Attempts != 2 {
		t.Errorf("report.Attempts=%d; expected 2 (one retry happened)", report.Attempts)
	}
	if startCalls != 2 {
		t.Errorf("startCalls=%d; expected 2 (one retry after wait succeeded)", startCalls)
	}
}

// TestStartManagedDoltProcessWithOptions_AddressInUseBumpsPortWhenWaitTimesOut
// drives the alternate branch: wait helper times out (probe stays busy for
// its 2 calls), so the loop bumps to the next port. Attempt 2 on the new
// port returns Ready=true. PR contract:
//
//   - err == nil
//   - report.Ready == true
//   - report.Port != originalPort (bumped via nextAvailableManagedDoltPortForHost)
//   - report.Attempts == 2 (one bump happened)
func TestStartManagedDoltProcessWithOptions_AddressInUseBumpsPortWhenWaitTimesOut(t *testing.T) {
	const originalPort = 17778
	var startCalls int32
	cityPath := installStartManagedDoltLoopStubs(t, startManagedDoltLoopStubs{
		startFn: func(cityPath, _, _ string, _ *os.File) (managedDoltStartedProcess, error) {
			atomic.AddInt32(&startCalls, 1)
			return managedDoltStartedProcess{CityPath: cityPath, PID: 0}, nil
		},
		waitReadyFn: func(_, _, _, _ string, _ int, _ time.Duration, _ bool) (managedDoltWaitReadyReport, error) {
			if atomic.LoadInt32(&startCalls) >= 2 {
				return managedDoltWaitReadyReport{Ready: true, PIDAlive: true}, nil
			}
			return managedDoltWaitReadyReport{Ready: false, PIDAlive: false}, nil
		},
		logSuffixFn: func(_ string, _ int64) (string, error) {
			// Only attempt 1 should reach the log-read; attempt 2 returns Ready.
			return "address already in use", nil
		},
		// Probe BUSY for first 2 calls (the wait helper's in-loop + post-deadline
		// checks), then AVAILABLE for the port-bump probe.
		portAvailableFn: portBusyForFirstNCallsThenAvailable(2),
		retryWindow:     50 * time.Millisecond,
	})

	report, err := startManagedDoltProcessWithOptions(cityPath, "0.0.0.0", strconv.Itoa(originalPort), "root", "warning", -1, 1*time.Second, false)
	if err != nil {
		t.Fatalf("expected success on attempt 2 (bumped port); got %v", err)
	}
	if !report.Ready {
		t.Errorf("report.Ready=false; expected true")
	}
	if report.Port == originalPort {
		t.Errorf("report.Port=%d; expected bump away from original (%d)", report.Port, originalPort)
	}
	if report.Attempts != 2 {
		t.Errorf("report.Attempts=%d; expected 2", report.Attempts)
	}
	if startCalls != 2 {
		t.Errorf("startCalls=%d; expected 2 (one start per attempt)", startCalls)
	}
}

// TestStartManagedDoltProcessWithOptions_WaitedPortsBoundsBumpsAfterRetry
// pins the `waitedPorts` budget: a port that has ALREADY been waited on in
// the current invocation must NOT be waited on again. Same-port-busy twice
// means: attempt 1 waits + retries same port; attempt 2 hits address-in-use
// again on the same port, waitedPorts says "already touched" → skip wait,
// bump. Attempt 3 (new port) succeeds.
//
// We pin the budget via a probe call counter discriminated by port (the
// wait helper probes originalPort; the port-bump probes originalPort+N).
// A working budget calls the probe on originalPort exactly TWICE: once
// in the wait helper's in-loop check (BUSY), once at the wait helper's
// post-deadline final check (AVAILABLE, after the deliberate flip). A
// broken budget (e.g. dropping the `!waitedPorts[currentPort]` guard at
// dolt_start_managed.go:246) would re-enter the wait helper on attempt 2
// and call the probe on originalPort a third (and fourth) time.
//
// Elapsed-time bound: 1× retryWindow + scheduling slack. With a broken
// budget, attempt 2 would also burn ~retryWindow before bumping.
func TestStartManagedDoltProcessWithOptions_WaitedPortsBoundsBumpsAfterRetry(t *testing.T) {
	const originalPort = 17779
	const retryWindow = 300 * time.Millisecond
	const waitDelay = 200 * time.Millisecond
	var startCalls int32
	var origPortProbeCalls int32
	flippedAt := time.Now().Add(waitDelay)

	cityPath := installStartManagedDoltLoopStubs(t, startManagedDoltLoopStubs{
		startFn: func(cityPath, _, _ string, _ *os.File) (managedDoltStartedProcess, error) {
			atomic.AddInt32(&startCalls, 1)
			return managedDoltStartedProcess{CityPath: cityPath, PID: 0}, nil
		},
		waitReadyFn: func(_, _, _, _ string, _ int, _ time.Duration, _ bool) (managedDoltWaitReadyReport, error) {
			if atomic.LoadInt32(&startCalls) >= 3 {
				return managedDoltWaitReadyReport{Ready: true, PIDAlive: true}, nil
			}
			return managedDoltWaitReadyReport{Ready: false, PIDAlive: false}, nil
		},
		logSuffixFn: func(_ string, _ int64) (string, error) {
			return "address already in use", nil
		},
		portAvailableFn: func(_ string, port int) bool {
			// Only the wait helper probes the originalPort; port-bump probes
			// originalPort+N. Tracking originalPort calls isolates the
			// wait-helper invocation count from port-bump noise.
			if port == originalPort {
				atomic.AddInt32(&origPortProbeCalls, 1)
				return time.Now().After(flippedAt)
			}
			return true // port-bump always finds something free
		},
		retryWindow: retryWindow,
	})

	report, err := startManagedDoltProcessWithOptions(cityPath, "0.0.0.0", strconv.Itoa(originalPort), "root", "warning", -1, 1*time.Second, false)
	if err != nil {
		t.Fatalf("expected success on attempt 3; got %v", err)
	}
	if !report.Ready {
		t.Errorf("report.Ready=false; expected true")
	}
	if report.Attempts != 3 {
		t.Errorf("report.Attempts=%d; expected 3 (retry + bump + success)", report.Attempts)
	}
	if startCalls != 3 {
		t.Errorf("startCalls=%d; expected 3", startCalls)
	}
	if report.Port == originalPort {
		t.Errorf("report.Port=%d; expected bump (attempt 2 should bump because waitedPorts blocks a second wait)", report.Port)
	}

	// Load-bearing budget assertion: probe on originalPort must be called by
	// ONLY attempt 1's wait helper. Two calls = (in-loop BUSY) + (post-deadline
	// AVAILABLE). A broken budget would re-enter the wait helper on attempt 2,
	// adding 2 more originalPort probes (3-4 total).
	got := atomic.LoadInt32(&origPortProbeCalls)
	if got > 2 {
		t.Errorf("origPortProbeCalls=%d; expected ≤2 (only attempt 1's wait should probe originalPort). >2 implies attempt 2 re-waited, budget broken", got)
	}
	if got < 1 {
		t.Errorf("origPortProbeCalls=%d; expected ≥1 (attempt 1's wait must have probed originalPort)", got)
	}
}

// TestStartManagedDoltProcessWithOptions_HappyPathReturnsReadyOnFirstAttempt
// is the no-address-in-use regression check: dolt starts cleanly, becomes
// ready, no retry branch entered. A refactor that broke the early-return at
// dolt_start_managed.go:205-214 (e.g., always entering the AddressInUse
// branch) would be caught here. Asserts:
//
//   - err == nil
//   - report.Ready == true
//   - report.Attempts == 1
//   - report.AddressInUse == false
//   - startCalls == 1
//   - logSuffix NEVER called (early-return must happen before log-read)
func TestStartManagedDoltProcessWithOptions_HappyPathReturnsReadyOnFirstAttempt(t *testing.T) {
	const originalPort = 17780
	var startCalls int32
	var logCalls int32
	cityPath := installStartManagedDoltLoopStubs(t, startManagedDoltLoopStubs{
		startFn: func(cityPath, _, _ string, _ *os.File) (managedDoltStartedProcess, error) {
			atomic.AddInt32(&startCalls, 1)
			return managedDoltStartedProcess{CityPath: cityPath, PID: 0}, nil
		},
		waitReadyFn: func(_, _, _, _ string, _ int, _ time.Duration, _ bool) (managedDoltWaitReadyReport, error) {
			return managedDoltWaitReadyReport{Ready: true, PIDAlive: true}, nil
		},
		logSuffixFn: func(_ string, _ int64) (string, error) {
			atomic.AddInt32(&logCalls, 1)
			return "", nil // should never be reached on happy path
		},
		portAvailableFn: func(_ string, _ int) bool { return true },
		retryWindow:     30 * time.Second, // default; should be irrelevant
	})

	report, err := startManagedDoltProcessWithOptions(cityPath, "0.0.0.0", strconv.Itoa(originalPort), "root", "warning", -1, 1*time.Second, false)
	if err != nil {
		t.Fatalf("happy path: got err %v", err)
	}
	if !report.Ready {
		t.Errorf("report.Ready=false; expected true")
	}
	if report.Attempts != 1 {
		t.Errorf("report.Attempts=%d; expected 1 (single successful attempt)", report.Attempts)
	}
	if report.AddressInUse {
		t.Errorf("report.AddressInUse=true; expected false (no address-in-use on happy path)")
	}
	if report.Port != originalPort {
		t.Errorf("report.Port=%d; expected %d (no bump on happy path)", report.Port, originalPort)
	}
	if startCalls != 1 {
		t.Errorf("startCalls=%d; expected 1", startCalls)
	}
	if logCalls != 0 {
		t.Errorf("logCalls=%d; expected 0 (early-return must skip log-read on success)", logCalls)
	}
}

// TestStartManagedDoltProcessWithOptions_RetryWindowZeroBumpsImmediately
// pins the legacy fall-back-immediately behavior: retryWindow=0 means the
// wait helper at line 246 is gated out (`retryWindow > 0` is false), so
// the loop bumps the port on the very first address-in-use without
// entering the port-wait helper. A regression dropping the `retryWindow > 0`
// guard would call the wait helper at least once.
func TestStartManagedDoltProcessWithOptions_RetryWindowZeroBumpsImmediately(t *testing.T) {
	const originalPort = 17781
	var startCalls int32
	var origPortProbeCalls int32
	cityPath := installStartManagedDoltLoopStubs(t, startManagedDoltLoopStubs{
		startFn: func(cityPath, _, _ string, _ *os.File) (managedDoltStartedProcess, error) {
			atomic.AddInt32(&startCalls, 1)
			return managedDoltStartedProcess{CityPath: cityPath, PID: 0}, nil
		},
		waitReadyFn: func(_, _, _, _ string, _ int, _ time.Duration, _ bool) (managedDoltWaitReadyReport, error) {
			if atomic.LoadInt32(&startCalls) >= 2 {
				return managedDoltWaitReadyReport{Ready: true, PIDAlive: true}, nil
			}
			return managedDoltWaitReadyReport{Ready: false, PIDAlive: false}, nil
		},
		logSuffixFn: func(_ string, _ int64) (string, error) {
			return "address already in use", nil
		},
		portAvailableFn: func(_ string, port int) bool {
			if port == originalPort {
				atomic.AddInt32(&origPortProbeCalls, 1)
				return false // would force a real wait if helper ran
			}
			return true
		},
		retryWindow: 0, // legacy fall-back-immediately
	})

	report, err := startManagedDoltProcessWithOptions(cityPath, "0.0.0.0", strconv.Itoa(originalPort), "root", "warning", -1, 1*time.Second, false)
	if err != nil {
		t.Fatalf("expected success on attempt 2 (bump); got %v", err)
	}
	if report.Port == originalPort {
		t.Errorf("report.Port=%d; expected bump on first address-in-use (retryWindow=0 means no wait)", report.Port)
	}
	if report.Attempts != 2 {
		t.Errorf("report.Attempts=%d; expected 2", report.Attempts)
	}
	if atomic.LoadInt32(&origPortProbeCalls) != 0 {
		t.Errorf("origPortProbeCalls=%d; expected 0 (retryWindow=0 must NOT call wait helper, which probes originalPort)", atomic.LoadInt32(&origPortProbeCalls))
	}
}

// TestStartManagedDoltProcessWithOptions_FiveAttemptCapExhausts pins the
// 5-attempt loop bound at dolt_start_managed.go:154 (`attempt <= 5`). All
// attempts fail with address-in-use; the loop must give up after 5 attempts
// and surface the "could not find a free port after repeated address-in-use
// failures" error. A regression with `attempt < 5` (off-by-one) or an
// unbounded loop would fail this test by attempt count or by timing out.
func TestStartManagedDoltProcessWithOptions_FiveAttemptCapExhausts(t *testing.T) {
	const originalPort = 17782
	var startCalls int32
	cityPath := installStartManagedDoltLoopStubs(t, startManagedDoltLoopStubs{
		startFn: func(cityPath, _, _ string, _ *os.File) (managedDoltStartedProcess, error) {
			atomic.AddInt32(&startCalls, 1)
			return managedDoltStartedProcess{CityPath: cityPath, PID: 0}, nil
		},
		waitReadyFn: func(_, _, _, _ string, _ int, _ time.Duration, _ bool) (managedDoltWaitReadyReport, error) {
			return managedDoltWaitReadyReport{Ready: false, PIDAlive: false}, nil
		},
		logSuffixFn: func(_ string, _ int64) (string, error) {
			return "address already in use", nil
		},
		portAvailableFn: func(_ string, _ int) bool { return true }, // wait succeeds every time
		retryWindow:     10 * time.Millisecond,                      // tiny window so test runs fast
	})

	report, err := startManagedDoltProcessWithOptions(cityPath, "0.0.0.0", strconv.Itoa(originalPort), "root", "warning", -1, 1*time.Second, false)

	if err == nil {
		t.Fatalf("expected 5-attempt cap exhaustion error; got nil")
	}
	if !strings.Contains(err.Error(), "after repeated address-in-use failures") {
		t.Errorf("expected error to mention 'after repeated address-in-use failures'; got %q", err.Error())
	}
	if report.Attempts != 5 {
		t.Errorf("report.Attempts=%d; expected 5 (cap exhaustion). Off-by-one in `for attempt := 1; attempt <= 5; attempt++`?", report.Attempts)
	}
	if startCalls != 5 {
		t.Errorf("startCalls=%d; expected 5 (one start per attempt)", startCalls)
	}
}

// portBusyForFirstNCallsThenAvailable returns a port-availability stub that
// answers "busy" for the first n probe calls, then "available" thereafter.
// Used to simulate "wait window expires busy, but next port is free".
func portBusyForFirstNCallsThenAvailable(n int32) func(host string, port int) bool {
	var calls int32
	return func(_ string, _ int) bool {
		c := atomic.AddInt32(&calls, 1)
		return c > n
	}
}

// TestNextAvailableManagedDoltPortForHost_UsesHostAwareProbe asserts the
// host-aware port-bump consults managedDoltPortAvailableFn (the host-aware
// indirection) rather than the legacy localhost-only managedDoltPortAvailable.
// We shim the indirection to be host-discriminating: ports < stopPort on
// host "0.0.0.0" are busy, all others free. The bump from seed 17800 must
// walk to stopPort instead of returning the seed. stopPort is within the
// 100-attempt budget (seed..seed+99 inclusive).
func TestNextAvailableManagedDoltPortForHost_UsesHostAwareProbe(t *testing.T) {
	origProbe := managedDoltPortAvailableFn
	defer func() { managedDoltPortAvailableFn = origProbe }()

	const seed = 17800
	const stopPort = 17850 // 50 hops from seed, well within the 100-iter budget
	var probeCalls int32
	managedDoltPortAvailableFn = func(host string, port int) bool {
		atomic.AddInt32(&probeCalls, 1)
		if host == "0.0.0.0" {
			return port >= stopPort
		}
		return true // localhost: everything free (would short-circuit if probe were localhost-only)
	}

	got := nextAvailableManagedDoltPortForHost("0.0.0.0", seed)
	if got != stopPort {
		t.Errorf("expected host-aware probe to walk to %d; got %d (host-aware probe not wired? legacy 127.0.0.1 probe would have returned %d)", stopPort, got, seed)
	}
	if probeCalls < int32(stopPort-seed+1) {
		t.Errorf("expected at least %d probe calls to reach %d; got %d", stopPort-seed+1, stopPort, probeCalls)
	}
}
