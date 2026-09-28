package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeLiveResolve returns a LiveResolve seam that resolves to the given port
// via the managed-handle source.
func fakeLiveResolve() func(string) (liveDoltPortResolution, error) {
	return func(string) (liveDoltPortResolution, error) {
		return liveDoltPortResolution{
			Port:   28231,
			Source: liveDoltHandleSource,
			Attempts: []PortResolutionAttempt{
				{Source: liveDoltHandleSource, Status: "found", Detail: "live"},
			},
		}, nil
	}
}

// fakeLiveResolveMiss returns a LiveResolve seam where neither live source
// finds an endpoint (clean not-found, no errors).
func fakeLiveResolveMiss() func(string) (liveDoltPortResolution, error) {
	return func(string) (liveDoltPortResolution, error) {
		return liveDoltPortResolution{
			Attempts: []PortResolutionAttempt{
				{Source: liveDoltHandleSource, Status: "not-found"},
				{Source: liveDoltProcessSource, Status: "not-found"},
			},
		}, errNoLiveDoltEndpoint
	}
}

// fakeLiveResolveError returns a LiveResolve seam whose process-table step
// fails hard (e.g. ambiguous listeners).
func fakeLiveResolveError(detail string) func(string) (liveDoltPortResolution, error) {
	return func(string) (liveDoltPortResolution, error) {
		return liveDoltPortResolution{
			Attempts: []PortResolutionAttempt{
				{Source: liveDoltHandleSource, Status: "not-found"},
				{Source: liveDoltProcessSource, Status: "error", Detail: detail},
			},
		}, errors.New(detail)
	}
}

func TestResolveDoltPort_FlagWins(t *testing.T) {
	in := PortResolverInput{
		Flag:        "9999",
		CityPort:    4242,
		CityPath:    "/city",
		LiveResolve: fakeLiveResolve(),
	}

	got := ResolveDoltPort(in)

	if got.Port != 9999 {
		t.Errorf("Port = %d, want 9999", got.Port)
	}
	if got.Fallback {
		t.Errorf("Fallback = true, want false")
	}
	if got.Source != "--port flag" {
		t.Errorf("Source = %q, want %q", got.Source, "--port flag")
	}
}

func TestResolveDoltPort_FlagInvalidFallsThrough(t *testing.T) {
	in := PortResolverInput{
		Flag:        "not-a-number",
		CityPort:    4242,
		CityPath:    "/city",
		LiveResolve: fakeLiveResolveMiss(),
	}

	got := ResolveDoltPort(in)

	if got.Port != 4242 {
		t.Errorf("Port = %d, want 4242 (city config fallback)", got.Port)
	}
	if got.Source != "city config dolt.port" {
		t.Errorf("Source = %q, want %q", got.Source, "city config dolt.port")
	}
	// First attempt should record the parse error.
	if len(got.Tried) == 0 || got.Tried[0].Status != "error" {
		t.Errorf("expected first attempt to record error, got %+v", got.Tried)
	}
}

func TestResolveDoltPort_CityConfigBeatsLiveResolution(t *testing.T) {
	in := PortResolverInput{
		CityPort:    4242,
		CityPath:    "/city",
		LiveResolve: fakeLiveResolve(),
	}

	got := ResolveDoltPort(in)

	if got.Port != 4242 {
		t.Errorf("Port = %d, want 4242", got.Port)
	}
	if got.Source != "city config dolt.port" {
		t.Errorf("Source = %q, want city config dolt.port", got.Source)
	}
}

func TestResolveDoltPort_LiveResolutionWins(t *testing.T) {
	in := PortResolverInput{
		CityPath:    "/city",
		LiveResolve: fakeLiveResolve(),
	}

	got := ResolveDoltPort(in)

	if got.Port != 28231 {
		t.Errorf("Port = %d, want 28231 (live managed dolt)", got.Port)
	}
	if got.Source != liveDoltHandleSource {
		t.Errorf("Source = %q, want %q", got.Source, liveDoltHandleSource)
	}
	if got.Fallback {
		t.Errorf("Fallback = true, want false")
	}
}

func TestResolveDoltPort_ProcessTableSourceThreadsThrough(t *testing.T) {
	in := PortResolverInput{
		CityPath: "/city",
		LiveResolve: func(string) (liveDoltPortResolution, error) {
			return liveDoltPortResolution{
				Port:   19999,
				Source: liveDoltProcessSource,
				Attempts: []PortResolutionAttempt{
					{Source: liveDoltHandleSource, Status: "not-found"},
					{Source: liveDoltProcessSource, Status: "found", Detail: "19999"},
				},
			}, nil
		},
	}

	got := ResolveDoltPort(in)

	if got.Port != 19999 {
		t.Errorf("Port = %d, want 19999", got.Port)
	}
	if got.Source != liveDoltProcessSource {
		t.Errorf("Source = %q, want %q", got.Source, liveDoltProcessSource)
	}
}

func TestResolveDoltPort_LegacyFallbackWhenNothingResolves(t *testing.T) {
	in := PortResolverInput{
		CityPath:    "/city",
		LiveResolve: fakeLiveResolveMiss(),
	}

	got := ResolveDoltPort(in)

	if got.Port != 3307 {
		t.Errorf("Port = %d, want 3307 (legacy default)", got.Port)
	}
	if !got.Fallback {
		t.Errorf("Fallback = false, want true")
	}
	if got.Source != "legacy default" {
		t.Errorf("Source = %q, want legacy default", got.Source)
	}
}

func TestResolveDoltPort_TriedRecordsAllSources(t *testing.T) {
	in := PortResolverInput{
		CityPath:    "/city",
		LiveResolve: fakeLiveResolveMiss(),
	}

	got := ResolveDoltPort(in)

	if len(got.Tried) < 5 {
		t.Fatalf("Tried = %d entries, want at least 5 (flag, config, live handle, process table, legacy)", len(got.Tried))
	}
	wantSources := []string{
		"--port flag",
		"city config dolt.port",
		liveDoltHandleSource,
		liveDoltProcessSource,
		"legacy default",
	}
	for i, want := range wantSources {
		if got.Tried[i].Source != want {
			t.Errorf("Tried[%d].Source = %q, want %q", i, got.Tried[i].Source, want)
		}
	}
}

func TestResolveDoltPort_NeverReadsPortFile(t *testing.T) {
	in := PortResolverInput{
		CityPath:    "/city",
		LiveResolve: fakeLiveResolveMiss(),
	}

	got := ResolveDoltPort(in)

	for _, attempt := range got.Tried {
		if strings.Contains(attempt.Source, "dolt-server.port") {
			t.Fatalf("resolver consulted the dolt-server.port status file: %+v", got.Tried)
		}
	}
}

func TestResolveDoltPort_LiveResolutionErrorStopsBeforeLegacyFallback(t *testing.T) {
	in := PortResolverInput{
		CityPath:    "/city",
		LiveResolve: fakeLiveResolveError("ambiguous live dolt listeners on ports [28231 29000]"),
	}

	got := ResolveDoltPort(in)

	if got.Port != 0 {
		t.Errorf("Port = %d, want unresolved zero port", got.Port)
	}
	if got.Fallback {
		t.Errorf("Fallback = true, want false for live resolution error")
	}
	if got.Source != liveDoltProcessSource {
		t.Errorf("Source = %q, want %q", got.Source, liveDoltProcessSource)
	}
	for _, attempt := range got.Tried {
		if attempt.Source == "legacy default" {
			t.Fatalf("legacy default was tried after live resolution error: %+v", got.Tried)
		}
		if attempt.Source == liveDoltProcessSource {
			if attempt.Status != "error" {
				t.Errorf("process-table attempt status = %q, want error", attempt.Status)
			}
			if !strings.Contains(attempt.Detail, "ambiguous") {
				t.Errorf("process-table detail = %q, want ambiguity detail", attempt.Detail)
			}
			return
		}
	}
	t.Errorf("did not find %s in Tried entries: %+v", liveDoltProcessSource, got.Tried)
}

func TestResolveDoltPort_NoCityPathFallsThroughDirectly(t *testing.T) {
	got := ResolveDoltPort(PortResolverInput{
		LiveResolve: func(cityPath string) (liveDoltPortResolution, error) {
			return newLiveDoltPortResolver().resolve(cityPath)
		},
	})

	if got.Port != 3307 || !got.Fallback {
		t.Errorf("expected legacy fallback with no city path, got %+v", got)
	}
}

func TestResolveDoltPort_FlagZeroRejected(t *testing.T) {
	in := PortResolverInput{
		Flag:        "0",
		CityPort:    4242,
		CityPath:    "/city",
		LiveResolve: fakeLiveResolveMiss(),
	}

	got := ResolveDoltPort(in)

	if got.Port == 0 {
		t.Errorf("Port = 0; resolver must reject a zero --port and fall through")
	}
	if got.Source != "city config dolt.port" {
		t.Errorf("Source = %q, want city-config fallback after zero flag", got.Source)
	}
}

// TestResolveDoltPort_StalePortFileCannotOverrideLiveState is the behavioral
// pin for the property this resolver exists to guarantee, asserted against a
// REAL on-disk .beads/dolt-server.port rather than by sniffing attempt names
// (TestResolveDoltPort_NeverReadsPortFile). The file records a port that is
// wrong — the clobber this chain was written for wrote a proxy's ephemeral
// port here — and live state reports the real listener. Resolution must return
// live state's answer, and the file's port must appear nowhere in the trail.
func TestResolveDoltPort_StalePortFileCannotOverrideLiveState(t *testing.T) {
	const stalePort = "31364"
	cityPath := t.TempDir()
	beadsDir := filepath.Join(cityPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "dolt-server.port"), []byte(stalePort+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := ResolveDoltPort(PortResolverInput{
		CityPath:    cityPath,
		LiveResolve: fakeLiveResolve(), // the real listener: 28231
	})

	if got.Port != 28231 {
		t.Fatalf("Port = %d, want the live-resolved 28231; a stale port file must not win", got.Port)
	}
	if got.Source != liveDoltHandleSource {
		t.Errorf("Source = %q, want %q", got.Source, liveDoltHandleSource)
	}
	if got.Fallback {
		t.Error("Fallback = true, want false when live state resolved")
	}
	for _, attempt := range got.Tried {
		if strings.Contains(attempt.Detail, stalePort) {
			t.Fatalf("stale port %s leaked into the resolution trail: %+v", stalePort, got.Tried)
		}
	}
}

// TestResolveDoltPort_PortFileCannotResurrectADeadEndpoint is the other half:
// the file names a plausible, parseable port but nothing is listening for the
// city. The chain must NOT adopt the recorded port — it falls through to the
// legacy default and flags Fallback, so destructive callers can refuse (see
// TestControllerDropManagedDoltDatabase_RefusesOnLegacyFallback) instead of
// connecting to whatever now owns the recorded port.
func TestResolveDoltPort_PortFileCannotResurrectADeadEndpoint(t *testing.T) {
	const recordedPort = "31364"
	cityPath := t.TempDir()
	beadsDir := filepath.Join(cityPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "dolt-server.port"), []byte(recordedPort+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := ResolveDoltPort(PortResolverInput{
		CityPath:    cityPath,
		LiveResolve: fakeLiveResolveMiss(),
	})

	if got.Port == 31364 {
		t.Fatalf("resolver adopted the recorded port %d from the status file; it must never be an endpoint source", got.Port)
	}
	if got.Port != LegacyDefaultDoltPort || !got.Fallback {
		t.Fatalf("got (port %d, fallback %v), want (%d, true) — a clean live miss falls through to the legacy default", got.Port, got.Fallback, LegacyDefaultDoltPort)
	}
}
