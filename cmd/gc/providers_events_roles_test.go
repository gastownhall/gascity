package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// eventsRole is the part a recorder plays on a city's events.jsonl.
type eventsRole string

const (
	// roleOwner rotates the log and sweeps rotation leftovers on open. Exactly
	// one process per log may hold it.
	roleOwner eventsRole = "owner"
	// roleSecondary appends but never rotates or sweeps.
	roleSecondary eventsRole = "secondary"
	// roleReader holds no write handle and creates nothing.
	roleReader eventsRole = "reader"
)

// TestEventsOpenerRoles pins the role every opener of a city's events.jsonl
// takes. Only the supervisor (its per-city recorder and its own log) or a
// standalone controller holding controller.lock may own rotation; a second
// owner is how a stale CLI rotated the controller's fresh log and orphaned it
// (mc-zndi7.58). Watchers must hold no write handle at all.
//
// Each case plants an in-flight rotating file first: an owner's open sweeps it
// into an archive, and nothing else may touch it.
func TestEventsOpenerRoles(t *testing.T) {
	for _, tc := range []struct {
		name string
		want eventsRole
		open func(t *testing.T, cityDir string) (any, error)
	}{
		{name: "supervisor per-city recorder", want: roleOwner, open: func(_ *testing.T, cityDir string) (any, error) {
			return openSupervisorCityEventsRecorder(cityDir, config.EventsConfig{}, io.Discard)
		}},
		{name: "supervisor log", want: roleOwner, open: func(_ *testing.T, cityDir string) (any, error) {
			return openSupervisorEventsRecorder(filepath.Join(cityDir, ".gc"), io.Discard)
		}},
		{name: "gc start holding controller.lock", want: roleOwner, open: func(_ *testing.T, cityDir string) (any, error) {
			return openStandaloneCityEventsRecorder(cityDir, config.EventsConfig{}, true, io.Discard)
		}},
		{name: "one-shot gc start", want: roleSecondary, open: func(_ *testing.T, cityDir string) (any, error) {
			return openStandaloneCityEventsRecorder(cityDir, config.EventsConfig{}, false, io.Discard)
		}},
		{name: "CLI recorder (gc stop, gc sling, gc mail, ...)", want: roleSecondary, open: func(_ *testing.T, cityDir string) (any, error) {
			return openCityRecorderAt(cityDir, io.Discard), nil
		}},
		{name: "city events log (reemit, city init)", want: roleSecondary, open: func(_ *testing.T, cityDir string) (any, error) {
			return openCityEventsLog(cityDir, io.Discard)
		}},
		{name: "CLI events provider", want: roleSecondary, open: func(t *testing.T, cityDir string) (any, error) {
			useCityForEventsTest(t, cityDir)
			ep, code := openCityEventsProvider(io.Discard, "test")
			if code != 0 {
				t.Fatalf("openCityEventsProvider exit %d", code)
			}
			return ep, nil
		}},
		{name: "provider by name", want: roleSecondary, open: func(_ *testing.T, cityDir string) (any, error) {
			return newEventsProviderForName("", cityEventsPath(cityDir), io.Discard)
		}},
		{name: "worker factory recorder", want: roleSecondary, open: func(_ *testing.T, cityDir string) (any, error) {
			return newCLIFactoryRecorder(config.EventsConfig{}, cityEventsPath(cityDir))
		}},
		{name: "class-store emitter", want: roleSecondary, open: func(_ *testing.T, cityDir string) (any, error) {
			return newClassStoreEmitRecorder(cityDir)
		}},
		{name: "dolt project-id", want: roleSecondary, open: func(t *testing.T, cityDir string) (any, error) {
			rec, closeRec := openProjectIdentityEventRecorder(cityDir, io.Discard)
			t.Cleanup(closeRec)
			return rec, nil
		}},
		{name: "gc convoy control --serve", want: roleReader, open: func(t *testing.T, cityDir string) (any, error) {
			useCityForEventsTest(t, cityDir)
			return workflowServeOpenEventsProvider(io.Discard)
		}},
		{name: "gc status store health", want: roleReader, open: func(_ *testing.T, cityDir string) (any, error) {
			return defaultOpenStoreHealthEvents(cityDir, io.Discard), nil
		}},
		{name: "reader by name", want: roleReader, open: func(_ *testing.T, cityDir string) (any, error) {
			return newEventsReaderForName("", cityEventsPath(cityDir), io.Discard)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_EVENTS", "")
			t.Setenv("GC_EVENTS_ROTATION_ENABLED", "")
			cityDir := t.TempDir()
			inFlight := plantInFlightRotation(t, cityDir)

			got, err := tc.open(t, cityDir)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			rec, ok := got.(*events.FileRecorder)
			if !ok {
				t.Fatalf("opener returned %T, want *events.FileRecorder", got)
			}
			t.Cleanup(func() { _ = rec.Close() })

			if rec.RotationEnabled() != (tc.want == roleOwner) {
				t.Errorf("RotationEnabled = %t, want %t for a %s", rec.RotationEnabled(), tc.want == roleOwner, tc.want)
			}
			if rec.ReadOnly() != (tc.want == roleReader) {
				t.Errorf("ReadOnly = %t, want %t for a %s", rec.ReadOnly(), tc.want == roleReader, tc.want)
			}
			_, statErr := os.Stat(inFlight)
			if swept := os.IsNotExist(statErr); swept != (tc.want == roleOwner) {
				t.Errorf("in-flight rotating file swept = %t (stat err %v), want %t for a %s", swept, statErr, tc.want == roleOwner, tc.want)
			}
			if tc.want == roleReader {
				assertNoEventsFilesCreated(t, cityDir)
			}
		})
	}
}

// TestTransientCityEventProviderWatchIsReadOnly covers the registry's
// per-city provider for cities the supervisor does not run: its Watch must
// not open a writer, which would create the log, sweep it, and pin it.
func TestTransientCityEventProviderWatchIsReadOnly(t *testing.T) {
	cityDir := t.TempDir()
	inFlight := plantInFlightRotation(t, cityDir)

	w, err := transientCityEventProvider{path: cityEventsPath(cityDir)}.Watch(t.Context(), 0)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(inFlight); err != nil {
		t.Errorf("in-flight rotating file was swept: %v", err)
	}
	assertNoEventsFilesCreated(t, cityDir)
}

func cityEventsPath(cityDir string) string {
	return filepath.Join(cityDir, ".gc", "events.jsonl")
}

// plantInFlightRotation leaves a rotating file in the city's runtime dir, as a
// rotation still compressing (or one a crash stranded) does, and returns it.
func plantInFlightRotation(t *testing.T, cityDir string) string {
	t.Helper()
	dir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "events.jsonl.rotating-20261001T125436Z-seq-1-1")
	if err := os.WriteFile(path, []byte(`{"seq":1,"type":"bead.created","ts":"2026-10-01T12:54:36Z","actor":"seed"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// assertNoEventsFilesCreated fails if events.jsonl or its sidecar lock exists.
func assertNoEventsFilesCreated(t *testing.T, cityDir string) {
	t.Helper()
	for _, name := range []string{"events.jsonl", "events.jsonl.lock"} {
		if _, err := os.Stat(filepath.Join(cityDir, ".gc", name)); !os.IsNotExist(err) {
			t.Errorf("read-only opener created %s (stat err %v)", name, err)
		}
	}
}

// useCityForEventsTest points city resolution at cityDir for openers that
// resolve the current city themselves.
func useCityForEventsTest(t *testing.T, cityDir string) {
	t.Helper()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_RIG", "")
	oldCityFlag, oldRigFlag := cityFlag, rigFlag
	cityFlag, rigFlag = "", ""
	t.Cleanup(func() {
		cityFlag = oldCityFlag
		rigFlag = oldRigFlag
	})
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
