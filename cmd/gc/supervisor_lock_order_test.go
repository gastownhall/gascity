package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// newLockOrderCity is a registered city with a loadable config, for driving
// the supervisor's per-city start up to its first init step.
func newLockOrderCity(t *testing.T) (string, *supervisor.Registry) {
	t.Helper()
	clearGCEnv(t)
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_BEADS", "file")
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"lock-city\"\n\n[daemon]\nsession_reconciler = \"legacy\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityDir, "lock-city"); err != nil {
		t.Fatal(err)
	}
	return cityDir, reg
}

// stubSupervisorPrepare replaces the supervisor's init step with one that
// records whether it ran and whether the controller lock was held by this
// process when it did, then stops the start there.
func stubSupervisorPrepare(t *testing.T) (ran *bool, lockHeld *bool) {
	t.Helper()
	ran, lockHeld = new(bool), new(bool)
	prev := supervisorPrepareCity
	t.Cleanup(func() { supervisorPrepareCity = prev })
	supervisorPrepareCity = func(cityPath, _ string, _ *config.City, _ io.Writer, _ func(string)) error {
		*ran = true
		// A second flock on a fresh descriptor conflicts even inside this
		// process, so a refusal here proves the start already holds the lock.
		probe, err := acquireControllerLock(cityPath)
		if err == nil {
			probe.Close() //nolint:errcheck // probe only
		}
		*lockHeld = errors.Is(err, errControllerAlreadyRunning)
		return errors.New("stop here: the test only observes the order")
	}
	return ran, lockHeld
}

// TestSupervisorCityStartTakesTheControllerLockBeforeInit pins the S5-A
// ordering fix: the supervisor's per-city start takes .gc/controller.lock
// before it starts the bead-store provider or runs the storage boot gate,
// as `gc start` does. A city whose lock another process holds (a running
// controller, `gc stop` retiring the provider, a storage migration) is never
// touched: its init step does not run at all.
func TestSupervisorCityStartTakesTheControllerLockBeforeInit(t *testing.T) {
	cityDir, reg := newLockOrderCity(t)
	ran, _ := stubSupervisorPrepare(t)

	held, err := acquireControllerLock(cityDir)
	if err != nil {
		t.Fatalf("taking the controller lock for the test: %v", err)
	}
	defer held.Close() //nolint:errcheck // test cleanup

	cr := newCityRegistry()
	var stdout, stderr bytes.Buffer
	startOneCity(supervisor.CityEntry{Path: cityDir, Name: "lock-city"}, reg, cr, supervisor.PublicationConfig{}, &stdout, &stderr)

	if *ran {
		t.Fatalf("the supervisor ran the city's init (bead store, boot gate) while another process held the controller lock; stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "controller lock: "+errControllerAlreadyRunning.Error()) {
		t.Fatalf("stderr = %q, want the controller-lock refusal (retrySupervisorCityStartAfterControllerLock matches its text)", stderr.String())
	}
	if cr.Has(cityDir) {
		t.Fatal("the supervisor published a city whose controller lock it does not hold")
	}
}

// TestSupervisorCityStartHoldsTheLockThroughInitAndReleasesItOnFailure is the
// other half: init runs with the lock already held, and a start that fails
// after taking it releases it, so the next attempt (or a migration) can.
func TestSupervisorCityStartHoldsTheLockThroughInitAndReleasesItOnFailure(t *testing.T) {
	cityDir, reg := newLockOrderCity(t)
	ran, lockHeld := stubSupervisorPrepare(t)

	cr := newCityRegistry()
	var stdout, stderr bytes.Buffer
	startOneCity(supervisor.CityEntry{Path: cityDir, Name: "lock-city"}, reg, cr, supervisor.PublicationConfig{}, &stdout, &stderr)

	if !*ran {
		t.Fatalf("init did not run on an unlocked city; stderr=%s", stderr.String())
	}
	if !*lockHeld {
		t.Fatal("init ran without the controller lock held")
	}
	after, err := acquireControllerLock(cityDir)
	if err != nil {
		t.Fatalf("a failed start left the controller lock held: %v", err)
	}
	after.Close() //nolint:errcheck // test cleanup
}

// TestSupervisorV2DefaultFallbackProbesUnderTheLockAfterInit pins where the
// supervisor runs the O6 probe: after init (the bead store is up, so the
// probe can read it) and with the controller lock held, before the socket or
// the runtime sees the latch.
func TestSupervisorV2DefaultFallbackProbesUnderTheLockAfterInit(t *testing.T) {
	cityDir, reg := newLockOrderCity(t)
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"lock-city\"\n\n[storage.classes]"+strings.SplitN(remoteStageSplitSource, "[storage.classes]", 2)[1]), 0o644); err != nil {
		t.Fatal(err)
	}
	prevEnv := reconcilerModeLookupEnv
	t.Cleanup(func() { reconcilerModeLookupEnv = prevEnv })
	reconcilerModeLookupEnv = envOf(nil)
	// The session provider this start builds next refuses to construct, so
	// the attempt ends right after the probe.
	t.Setenv("GC_SESSION", "ssh:")

	prepared := false
	prevPrepare := supervisorPrepareCity
	t.Cleanup(func() { supervisorPrepareCity = prevPrepare })
	supervisorPrepareCity = func(string, string, *config.City, io.Writer, func(string)) error {
		prepared = true
		return nil
	}
	var probedAfterInit, probedUnderLock bool
	prevProbe := v2DefaultBootProbe
	t.Cleanup(func() { v2DefaultBootProbe = prevProbe })
	v2DefaultBootProbe = func(cityPath, _ string, _ *config.City) error {
		probedAfterInit = prepared
		probe, err := acquireControllerLock(cityPath)
		if err == nil {
			probe.Close() //nolint:errcheck // probe only
		}
		probedUnderLock = errors.Is(err, errControllerAlreadyRunning)
		return errors.New("enterprise-era session rows: 1 open row(s)")
	}

	cr := newCityRegistry()
	var stdout, stderr bytes.Buffer
	startOneCity(supervisor.CityEntry{Path: cityDir, Name: "lock-city"}, reg, cr, supervisor.PublicationConfig{}, &stdout, &stderr)

	if !probedAfterInit || !probedUnderLock {
		t.Fatalf("probe after init=%v under lock=%v; stderr=%s", probedAfterInit, probedUnderLock, stderr.String())
	}
	if !strings.Contains(stderr.String(), "city 'lock-city': WARNING: session reconciler") || !strings.Contains(stderr.String(), "FALLING BACK TO LEGACY") {
		t.Errorf("stderr %q does not carry the city's loud fallback warning", stderr.String())
	}
}

// TestStartAfterDriftRestartAcceptsACityTheSupervisorIsInitializing: after a
// drift auto-restart the new supervisor re-inits the city. It holds the
// controller lock from the first init step and opens the socket only at the
// end, so in that window the lock is held with no identity to read. A city the
// supervisor lists as initializing is supervisor-managed: gc start and gc
// register proceed instead of failing "controller hosting mode unknown".
func TestStartAfterDriftRestartAcceptsACityTheSupervisorIsInitializing(t *testing.T) {
	cityPath, setCommit := driftCheckEnv(t, "old-build-id")
	setCommit("new-build-id")
	oldDry, oldNoAR := dryRunMode, noAutoRestartMode
	dryRunMode, noAutoRestartMode = false, false
	t.Cleanup(func() { dryRunMode, noAutoRestartMode = oldDry, oldNoAR })
	var stdout, stderr bytes.Buffer
	if code, cont := runStartDriftCheck(cityPath, &stdout, &stderr); code != 0 || !cont {
		t.Fatalf("drift auto-restart = (%d, %v); stderr=%s", code, cont, stderr.String())
	}

	// The restarted supervisor is initializing the city: lock held, no socket.
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"drift-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	held, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close() //nolint:errcheck // test cleanup
	prevIdentity := controllerIdentityHook
	controllerIdentityHook = func(string) controllerIdentityReply { return controllerIdentityReply{} }
	t.Cleanup(func() { controllerIdentityHook = prevIdentity })
	prevRunning := supervisorCityRunningHook
	t.Cleanup(func() { supervisorCityRunningHook = prevRunning })
	status := "starting_bead_store"
	supervisorCityRunningHook = func(string) (bool, string, bool) { return false, status, true }
	reached := false
	prevHook := registerCityWithSupervisorTestHook
	t.Cleanup(func() { registerCityWithSupervisorTestHook = prevHook })
	registerCityWithSupervisorTestHook = func(string, string, io.Writer, io.Writer) (bool, int) {
		reached = true
		return true, 0
	}

	for _, cmd := range []string{"gc start", "gc register"} {
		reached = false
		stderr.Reset()
		if code := registerCityWithSupervisorNamed(cityPath, "", io.Discard, &stderr, cmd, false); code != 0 || !reached {
			t.Fatalf("%s while the supervisor initializes the city = %d (reached=%v); stderr=%s", cmd, code, reached, stderr.String())
		}
	}

	// A failed init is not a managed city: the lock probe decides again.
	status = "init_failed"
	reached = false
	if code := registerCityWithSupervisorNamed(cityPath, "", io.Discard, &stderr, "gc start", false); code == 0 || reached {
		t.Fatal("a city whose supervisor init failed was treated as supervisor-managed while its lock is held")
	}
}
