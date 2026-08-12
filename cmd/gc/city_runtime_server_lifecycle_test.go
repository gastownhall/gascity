package main

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// serverLifecycleCityRuntime builds a minimal city runtime over the given
// provider, mirroring the shape the supervisor's managed-stop paths drive:
// stopManagedCity / the run loop's deferred cr.shutdown() are the only ways a
// supervisor-managed city ever stops, so CityRuntime.shutdown() is where the
// managed path either tears the provider server down or leaks it (#5175).
func serverLifecycleCityRuntime(t *testing.T, sp runtime.Provider) *CityRuntime {
	t.Helper()
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	return newTestCityRuntime(t, CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: &stdout,
		Stderr: &stderr,
	})
}

// TestCityRuntimeShutdownTearsDownProviderServer pins the managed-stop half
// of #5175: a full (non-preserving) CityRuntime.shutdown() must tear down the
// provider's shared server after stopping the sessions, exactly like the
// standalone stop path (cmdStopBody -> teardownServerForStop) already does.
// Without it, a supervisor-managed `gc stop` reports "City stopped." while
// the city's tmux server — mayor session included — keeps running until
// someone kills it by hand.
func TestCityRuntimeShutdownTearsDownProviderServer(t *testing.T) {
	sp := &lifecycleOrderProvider{Fake: runtime.NewFake()}
	cr := serverLifecycleCityRuntime(t, sp)

	cr.shutdown()

	sp.mu.Lock()
	eventsSnapshot := append([]string(nil), sp.events...)
	sp.mu.Unlock()

	teardowns := 0
	lastListRunning := -1
	firstTeardown := -1
	for i, e := range eventsSnapshot {
		switch e {
		case "TeardownServer":
			teardowns++
			if firstTeardown == -1 {
				firstTeardown = i
			}
		case "ListRunning":
			lastListRunning = i
		}
	}
	if teardowns != 1 {
		t.Fatalf("shutdown called TeardownServer %d time(s), want exactly 1 (events: %v): the managed stop path must not leak the provider server (#5175)", teardowns, eventsSnapshot)
	}
	if lastListRunning == -1 || lastListRunning > firstTeardown {
		t.Fatalf("TeardownServer must come after the session-stop sweep's ListRunning (events: %v)", eventsSnapshot)
	}

	// The Once makes "exactly once" a property of the process: a second
	// shutdown call must not tear the (next supervisor's) server down again.
	cr.shutdown()
	sp.mu.Lock()
	total := 0
	for _, e := range sp.events {
		if e == "TeardownServer" {
			total++
		}
	}
	sp.mu.Unlock()
	if total != 1 {
		t.Fatalf("a second shutdown called TeardownServer again (%d calls total)", total)
	}
}

// TestCityRuntimeShutdownPreservingSessionsKeepsServer pins the deliberate
// asymmetry: preserve-mode shutdown hands live sessions to the next
// supervisor for re-adoption, and those sessions live inside the provider
// server — tearing it down would kill exactly what preserve mode exists to
// keep. The server must survive a preserving shutdown.
func TestCityRuntimeShutdownPreservingSessionsKeepsServer(t *testing.T) {
	sp := &lifecycleOrderProvider{Fake: runtime.NewFake()}
	cr := serverLifecycleCityRuntime(t, sp)

	cr.preserveSessionsOnShutdown()
	cr.shutdown()

	sp.mu.Lock()
	defer sp.mu.Unlock()
	for _, e := range sp.events {
		if e == "TeardownServer" {
			t.Fatalf("preserve-mode shutdown tore down the provider server (events: %v); preserved sessions cannot outlive their server", sp.events)
		}
	}
}

// TestCityRuntimeShutdownWithoutServerLifecycleProviderIsANoOp pins that a
// provider without the optional ServerLifecycleProvider extension shuts down
// exactly as before — the teardown hook is a type-asserted no-op, not a new
// requirement on every provider.
func TestCityRuntimeShutdownWithoutServerLifecycleProviderIsANoOp(t *testing.T) {
	cr := serverLifecycleCityRuntime(t, runtime.NewFake())
	cr.shutdown() // must not panic
}
