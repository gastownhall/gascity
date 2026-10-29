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
