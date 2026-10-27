package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// Regression tests from the second review of #8332.

// "Transient" means store I/O that got no answer, nothing else.
func TestIsTransientStoreFaultIsStoreIOOnly(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"hosted dial timeout", errors.New(`Get "https://hosted-bd/v1/issues": dial tcp: i/o timeout`), true},
		{"context deadline", fmt.Errorf("listing: %w", context.DeadlineExceeded), true},
		{"store unavailable", fmt.Errorf("x: %w", beads.ErrStoreUnavailable), true},
		{"connection refused", fmt.Errorf("dial: %w", syscall.ECONNREFUSED), true},
		{"database is locked", errors.New("database is locked (SQLITE_BUSY)"), true},
		{"permission denied", fmt.Errorf("stat: %w", os.ErrPermission), false},
		{"EACCES", &os.PathError{Op: "stat", Path: "/x", Err: syscall.EACCES}, false},
		{"malformed annotation", errors.New(`reading the infra copy manifest: malformed "#gc:written x" line`), false},
		{"misconfigured store", errors.New("unknown beads provider \"nope\""), false},
		{"nil", nil, false},
	} {
		if got := isTransientStoreFault(tc.err); got != tc.want {
			t.Errorf("%s: isTransientStoreFault = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Probe P1: a malformed manifest annotation holds until edited, so it is a
// standing refusal and the drain fence fails CLOSED on it.
func TestMalformedManifestAnnotationFailsTheFenceClosed(t *testing.T) {
	cityPath, cfg := migratedOneShotCLICity(t)
	captureCLIStorageStderr(t)
	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatal(err)
	}
	target := mustResolveInfraTarget(t, cityPath, cfg)
	f, err := os.OpenFile(target.ManifestPath(), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("#gc:written not-a-time\n")
	_ = f.Close()
	_, err = hookSessionDrainPending(drainPendingTestSessionID)
	if err == nil || isTransientStorageGateFault(err) || !hookDrainProbeFailsClosed(err) {
		t.Errorf("malformed manifest: err=%v transient=%v; want a standing, fail-closed refusal", err, isTransientStorageGateFault(err))
	}
}

// A misconfigured work store (the census cannot open it, and not for want of
// an answer) is standing too.
func TestMisconfiguredWorkStoreIsStanding(t *testing.T) {
	migratedOneShotCLICity(t)
	captureCLIStorageStderr(t)
	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatal(err)
	}
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) {
		return nil, errors.New(`unknown beads provider "nope"`)
	}
	t.Cleanup(func() { openInfraMigrationSource = prev })
	_, err := hookSessionDrainPending(drainPendingTestSessionID)
	if err == nil || isTransientStorageGateFault(err) {
		t.Errorf("misconfigured work store: err=%v transient=%v; want standing", err, isTransientStorageGateFault(err))
	}
}

// unmarkSplitCity leaves a split city with no marker, manifest or database:
// the no-marker path (a census of the work store, then genesis or refusal).
func unmarkSplitCity(t *testing.T) string {
	t.Helper()
	cityPath, cfg := migratedOneShotCLICity(t)
	captureCLIStorageStderr(t)
	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatal(err)
	}
	target := mustResolveInfraTarget(t, cityPath, cfg)
	for _, p := range []string{target.MarkerPath(), target.ManifestPath()} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(filepath.Dir(target.Database)); err != nil {
		t.Fatal(err)
	}
	return cityPath
}

type listTimeoutStore struct{ beads.Store }

func (listTimeoutStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, fmt.Errorf("listing: %w", context.DeadlineExceeded)
}

// Probe P2 and mutant N11: on the no-marker path a census that times out
// (open or list) is transient, and its message does not claim the city "has
// not migrated onto it" — a census that could not run says nothing either way.
func TestNoMarkerCensusTimeoutsAreTransient(t *testing.T) {
	for name, tc := range map[string]struct {
		store   beads.Store
		openErr error
	}{
		"open": {openErr: errors.New("dial tcp: i/o timeout")},
		"list": {store: listTimeoutStore{beads.NewMemStore()}},
	} {
		t.Run(name, func(t *testing.T) {
			expireUnsettledCLIRoutesImmediately(t)
			unmarkSplitCity(t)
			prev := openInfraMigrationSource
			openInfraMigrationSource = func(string) (beads.Store, error) { return tc.store, tc.openErr }
			t.Cleanup(func() { openInfraMigrationSource = prev })
			_, err := hookSessionDrainPending(drainPendingTestSessionID)
			if err == nil || !isTransientStorageGateFault(err) || hookDrainProbeFailsClosed(err) {
				t.Errorf("err=%v transient=%v; want a transient, fail-open fault", err, isTransientStorageGateFault(err))
			}
			if err != nil && strings.Contains(err.Error(), "has not migrated onto it") {
				t.Errorf("a census that could not run claims the city has not migrated: %v", err)
			}
		})
	}
}

// The genesis race (another process created the binding at the same moment)
// clears on the next attempt: it is marked as the race.
func TestGenesisRaceIsRecognized(t *testing.T) {
	err := infraGenesisOpenFailure(infraBindingTarget{Binding: "infra", Database: "/x"}, errors.New("table issues already exists"))
	if !errors.Is(err, errInfraGenesisRace) {
		t.Errorf("genesis race not marked: %v", err)
	}
	if !strings.Contains(err.Error(), "lost the race") || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("genesis race message lost its explanation: %v", err)
	}
	if errors.Is(infraGenesisOpenFailure(infraBindingTarget{}, errors.New("disk full")), errInfraGenesisRace) {
		t.Error("an ordinary open failure was marked as the race")
	}
}

// The genesis race, reached through the gate on a split city with nothing to
// migrate, is a transient fault: the next attempt serves from the winner's
// binding.
func TestGenesisRaceThroughTheGateIsTransient(t *testing.T) {
	expireUnsettledCLIRoutesImmediately(t)
	unmarkSplitCity(t)
	prev := inspectInfraGenesis
	inspectInfraGenesis = func(target infraBindingTarget) error {
		return infraGenesisOpenFailure(target, errors.New("applying sqlite schema: table kv already exists (1)"))
	}
	t.Cleanup(func() { inspectInfraGenesis = prev })
	_, err := hookSessionDrainPending(drainPendingTestSessionID)
	if err == nil || !isTransientStorageGateFault(err) || hookDrainProbeFailsClosed(err) {
		t.Errorf("genesis race: err=%v transient=%v; want a transient, fail-open fault", err, isTransientStorageGateFault(err))
	}
}

// Mutant N14: a marker-state read that times out is transient; one refused
// for permission is standing.
func TestMarkerStateReadTransience(t *testing.T) {
	for name, tc := range map[string]struct {
		err       error
		transient bool
	}{
		"timeout":    {fmt.Errorf("reading the convergence marker: %w", syscall.ETIMEDOUT), true},
		"permission": {fmt.Errorf("reading the convergence marker: %w", &os.PathError{Op: "stat", Path: "/x", Err: syscall.EACCES}), false},
	} {
		t.Run(name, func(t *testing.T) {
			expireUnsettledCLIRoutesImmediately(t)
			migratedOneShotCLICity(t)
			captureCLIStorageStderr(t)
			if err := closeCLIStorageRoutes(); err != nil {
				t.Fatal(err)
			}
			prev := inspectInfraConvergenceState
			inspectInfraConvergenceState = func(infraBindingTarget) (infraConvergenceState, error) { return infraConvergenceAbsent, tc.err }
			t.Cleanup(func() { inspectInfraConvergenceState = prev })
			_, err := hookSessionDrainPending(drainPendingTestSessionID)
			if err == nil || isTransientStorageGateFault(err) != tc.transient {
				t.Errorf("err=%v transient=%v, want transient=%v", err, isTransientStorageGateFault(err), tc.transient)
			}
		})
	}
}

// Mutants N13 and N13b: the born-split census's open and list timeouts are
// transient; a misconfiguration is not.
func TestBornSplitCensusTransience(t *testing.T) {
	for name, tc := range map[string]struct {
		open      func(string) (beads.Store, error)
		transient bool
	}{
		"open timeout": {func(string) (beads.Store, error) { return nil, errors.New("dial tcp: i/o timeout") }, true},
		"list timeout": {func(string) (beads.Store, error) { return listTimeoutStore{beads.NewMemStore()}, nil }, true},
		"misconfig":    {func(string) (beads.Store, error) { return nil, errors.New(`unknown beads provider "nope"`) }, false},
	} {
		t.Run(name, func(t *testing.T) {
			prev := openInfraMigrationSource
			openInfraMigrationSource = tc.open
			t.Cleanup(func() { openInfraMigrationSource = prev })
			var stderr bytes.Buffer
			report := checkBornSplitDiscipline(t.TempDir(), "gc", &stderr)
			if report.Outcome != infraMigrationUncheckable || report.Transient != tc.transient {
				t.Errorf("report = %+v, want uncheckable with transient=%v", report, tc.transient)
			}
		})
	}
}

// Probe P3: a city migrated by `gc storage migrate` but not yet booted, whose
// city.toml is then truncated, must not route its classes to the work store:
// the migrate command now writes the served-binding note.
func TestMigratedUnbootedCityWithATruncatedConfigRefuses(t *testing.T) {
	cfg := infraSplitConfig(filepath.Join(t.TempDir(), "store"))
	request := storageTestRequest(t, cfg)
	stubStrandedRecoverySource(t)
	stubInfraControllerPing(t, 0)
	var stdout, stderr bytes.Buffer
	if code := doStorageMigrate(context.Background(), request, &stdout, &stderr); code != 0 {
		t.Fatalf("gc storage migrate = %d: %s %s", code, stdout.String(), stderr.String())
	}
	if _, present, err := readBornSplitServedNote(request.CityPath); err != nil || !present {
		t.Fatalf("gc storage migrate left no served-binding note (present=%v err=%v)", present, err)
	}
	if err := os.WriteFile(filepath.Join(request.CityPath, "city.toml"), []byte("[workspace]\nname = \"trunc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !cityMayRelocateClasses(request.CityPath) {
		t.Error("a migrated, unbooted city with a truncated city.toml is judged single-store")
	}
}
