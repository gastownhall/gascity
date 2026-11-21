package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func fakeLayoutFn(layout managedDoltRuntimeLayout) func(string) (managedDoltRuntimeLayout, error) {
	return func(string) (managedDoltRuntimeLayout, error) { return layout, nil }
}

func testManagedLayout() managedDoltRuntimeLayout {
	return managedDoltRuntimeLayout{
		DataDir:    "/city/.beads/dolt",
		ConfigFile: "/city/.gc/runtime/packs/dolt/config.yaml",
	}
}

func attemptStatuses(attempts []PortResolutionAttempt) []string {
	out := make([]string, 0, len(attempts))
	for _, a := range attempts {
		out = append(out, a.Source+":"+a.Status)
	}
	return out
}

func TestLiveDoltPortResolver_ManagedHandleWins(t *testing.T) {
	r := liveDoltPortResolver{
		managedHandlePort: func(cityPath string) string {
			if cityPath != "/city" {
				t.Errorf("managedHandlePort cityPath = %q, want /city", cityPath)
			}
			return "28231"
		},
		runtimeLayout: fakeLayoutFn(testManagedLayout()),
		discoverProcesses: func() ([]DoltProcInfo, error) {
			t.Error("process table consulted although the managed handle resolved")
			return nil, nil
		},
	}

	got, err := r.resolve("/city")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Port != 28231 {
		t.Errorf("Port = %d, want 28231", got.Port)
	}
	if got.Source != liveDoltHandleSource {
		t.Errorf("Source = %q, want %q", got.Source, liveDoltHandleSource)
	}
	if len(got.Attempts) != 1 || got.Attempts[0].Status != "found" {
		t.Errorf("Attempts = %v, want single found entry", attemptStatuses(got.Attempts))
	}
}

func TestResolveManagedDoltRuntimeLayoutStrict_IgnoresEnvOverrides(t *testing.T) {
	t.Setenv("GC_DOLT_DATA_DIR", "/foreign/.beads/dolt")
	t.Setenv("GC_DOLT_CONFIG_FILE", "/foreign/dolt-config.yaml")
	t.Setenv("GC_PACK_STATE_DIR", "/foreign/packs/dolt")

	strict, err := resolveManagedDoltRuntimeLayoutStrict("/cityA")
	if err != nil {
		t.Fatalf("strict: %v", err)
	}
	if samePath(strict.DataDir, "/foreign/.beads/dolt") {
		t.Fatalf("strict DataDir = %q, must be cityPath-derived, not the GC_DOLT_DATA_DIR override", strict.DataDir)
	}
	if !samePath(strict.DataDir, "/cityA/.beads/dolt") {
		t.Fatalf("strict DataDir = %q, want /cityA/.beads/dolt", strict.DataDir)
	}
	if samePath(strict.ConfigFile, "/foreign/dolt-config.yaml") {
		t.Fatalf("strict ConfigFile = %q, must not honor GC_DOLT_CONFIG_FILE", strict.ConfigFile)
	}

	// Contrast: the env-honoring resolver DOES pick up the overrides.
	env, err := resolveManagedDoltRuntimeLayout("/cityA")
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	if !samePath(env.DataDir, "/foreign/.beads/dolt") {
		t.Fatalf("env-honoring DataDir = %q, want the GC_DOLT_DATA_DIR override", env.DataDir)
	}
}

// TestLiveDoltPortResolverForExplicitCity_PoisonedEnvDoesNotMatchForeignCity is
// the cleanup-safety regression: with GC_DOLT_* pointing at a foreign city's
// dolt sql-server, `gc dolt cleanup --city /cityA` must NOT resolve the foreign
// city's port (and so cannot connect to / reap the wrong DB). The strict match
// layout ignores the env poison.
func TestLiveDoltPortResolverForExplicitCity_PoisonedEnvDoesNotMatchForeignCity(t *testing.T) {
	const foreignConfig = "/cityB/.gc/runtime/packs/dolt/dolt-config.yaml"
	t.Setenv("GC_DOLT_CONFIG_FILE", foreignConfig)
	t.Setenv("GC_DOLT_DATA_DIR", "/cityB/.beads/dolt")

	foreignProcs := func() ([]DoltProcInfo, error) {
		return []DoltProcInfo{
			{PID: 99, Ports: []int{29999}, Argv: []string{"dolt", "sql-server", "--config", foreignConfig}},
		}, nil
	}

	r := newLiveDoltPortResolverForExplicitCity()
	r.managedHandlePort = func(string) string { return "" } // force the process-table path
	r.discoverProcesses = foreignProcs
	if _, err := r.resolve("/cityA"); !errors.Is(err, errNoLiveDoltEndpoint) {
		t.Fatalf("strict resolve(/cityA) err = %v, want errNoLiveDoltEndpoint — the foreign city's poisoned process must not match", err)
	}

	// Sanity: the env-honoring resolver WOULD match the foreign process,
	// confirming the poison vector the strict cleanup path closes.
	rEnv := newLiveDoltPortResolver()
	rEnv.managedHandlePort = func(string) string { return "" }
	rEnv.discoverProcesses = foreignProcs
	got, err := rEnv.resolve("/cityA")
	if err != nil || got.Port != 29999 {
		t.Fatalf("env-honoring resolve(/cityA) = (port %d, err %v), want a match on the foreign process via the GC_DOLT_CONFIG_FILE poison", got.Port, err)
	}
}

func TestLiveDoltPortResolver_ProcessTableByConfigPath(t *testing.T) {
	layout := testManagedLayout()
	r := liveDoltPortResolver{
		managedHandlePort: func(string) string { return "" },
		runtimeLayout:     fakeLayoutFn(layout),
		discoverProcesses: func() ([]DoltProcInfo, error) {
			return []DoltProcInfo{
				// Foreign server: different config, must be ignored.
				{PID: 1, Ports: []int{4000}, Argv: []string{"dolt", "sql-server", "--config", "/elsewhere/config.yaml"}},
				{PID: 2, Ports: []int{28231}, Argv: []string{"dolt", "sql-server", "--config", layout.ConfigFile}},
			}, nil
		},
	}

	got, err := r.resolve("/city")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Port != 28231 {
		t.Errorf("Port = %d, want 28231", got.Port)
	}
	if got.Source != liveDoltProcessSource {
		t.Errorf("Source = %q, want %q", got.Source, liveDoltProcessSource)
	}
	want := []string{
		liveDoltHandleSource + ":not-found",
		liveDoltProcessSource + ":found",
	}
	if fmt.Sprint(attemptStatuses(got.Attempts)) != fmt.Sprint(want) {
		t.Errorf("Attempts = %v, want %v", attemptStatuses(got.Attempts), want)
	}
}

func TestLiveDoltPortResolver_ProcessTableByDataDir(t *testing.T) {
	layout := testManagedLayout()
	r := liveDoltPortResolver{
		managedHandlePort: func(string) string { return "" },
		runtimeLayout:     fakeLayoutFn(layout),
		discoverProcesses: func() ([]DoltProcInfo, error) {
			return []DoltProcInfo{
				{PID: 2, Ports: []int{19999}, Argv: []string{"dolt", "sql-server", "--data-dir", layout.DataDir}},
			}, nil
		},
	}

	got, err := r.resolve("/city")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Port != 19999 || got.Source != liveDoltProcessSource {
		t.Errorf("got %+v, want port 19999 from %q", got, liveDoltProcessSource)
	}
}

func TestLiveDoltPortResolver_NoLiveEndpointErrors(t *testing.T) {
	r := liveDoltPortResolver{
		managedHandlePort: func(string) string { return "" },
		runtimeLayout:     fakeLayoutFn(testManagedLayout()),
		discoverProcesses: func() ([]DoltProcInfo, error) {
			return []DoltProcInfo{
				// Listener with a foreign data dir: not ours.
				{PID: 1, Ports: []int{4000}, Argv: []string{"dolt", "sql-server", "--data-dir", "/elsewhere/dolt"}},
			}, nil
		},
	}

	got, err := r.resolve("/city")
	if !errors.Is(err, errNoLiveDoltEndpoint) {
		t.Fatalf("err = %v, want errNoLiveDoltEndpoint", err)
	}
	if got.Port != 0 {
		t.Errorf("Port = %d, want 0", got.Port)
	}
	want := []string{
		liveDoltHandleSource + ":not-found",
		liveDoltProcessSource + ":not-found",
	}
	if fmt.Sprint(attemptStatuses(got.Attempts)) != fmt.Sprint(want) {
		t.Errorf("Attempts = %v, want %v", attemptStatuses(got.Attempts), want)
	}
}

func TestLiveDoltPortResolver_MatchingProcessWithoutPortsIsNotFound(t *testing.T) {
	layout := testManagedLayout()
	r := liveDoltPortResolver{
		managedHandlePort: func(string) string { return "" },
		runtimeLayout:     fakeLayoutFn(layout),
		discoverProcesses: func() ([]DoltProcInfo, error) {
			// Matching process that is not (yet) listening.
			return []DoltProcInfo{{PID: 2, Argv: []string{"dolt", "sql-server", "--data-dir", layout.DataDir}}}, nil
		},
	}

	_, err := r.resolve("/city")
	if !errors.Is(err, errNoLiveDoltEndpoint) {
		t.Fatalf("err = %v, want errNoLiveDoltEndpoint", err)
	}
}

func TestLiveDoltPortResolver_AmbiguousListenersError(t *testing.T) {
	layout := testManagedLayout()
	r := liveDoltPortResolver{
		managedHandlePort: func(string) string { return "" },
		runtimeLayout:     fakeLayoutFn(layout),
		discoverProcesses: func() ([]DoltProcInfo, error) {
			return []DoltProcInfo{
				{PID: 2, Ports: []int{28231}, Argv: []string{"dolt", "sql-server", "--data-dir", layout.DataDir}},
				{PID: 3, Ports: []int{29000}, Argv: []string{"dolt", "sql-server", "--config", layout.ConfigFile}},
			}, nil
		},
	}

	got, err := r.resolve("/city")
	if err == nil {
		t.Fatalf("resolve succeeded with ambiguous listeners: %+v", got)
	}
	if errors.Is(err, errNoLiveDoltEndpoint) {
		t.Fatalf("ambiguity reported as no-endpoint: %v", err)
	}
	if !strings.Contains(err.Error(), "28231") || !strings.Contains(err.Error(), "29000") {
		t.Errorf("error %q does not name the candidate ports", err)
	}
	last := got.Attempts[len(got.Attempts)-1]
	if last.Source != liveDoltProcessSource || last.Status != "error" {
		t.Errorf("last attempt = %+v, want process-table error", last)
	}
}

func TestLiveDoltPortResolver_DiscoveryFailureErrors(t *testing.T) {
	r := liveDoltPortResolver{
		managedHandlePort: func(string) string { return "" },
		runtimeLayout:     fakeLayoutFn(testManagedLayout()),
		discoverProcesses: func() ([]DoltProcInfo, error) {
			return nil, errors.New("ps exploded")
		},
	}

	got, err := r.resolve("/city")
	if err == nil {
		t.Fatal("resolve succeeded although discovery failed")
	}
	last := got.Attempts[len(got.Attempts)-1]
	if last.Source != liveDoltProcessSource || last.Status != "error" || !strings.Contains(last.Detail, "ps exploded") {
		t.Errorf("last attempt = %+v, want process-table discovery error", last)
	}
}

func TestLiveDoltPortResolver_InvalidHandleValueFallsThrough(t *testing.T) {
	layout := testManagedLayout()
	r := liveDoltPortResolver{
		managedHandlePort: func(string) string { return "not-a-port" },
		runtimeLayout:     fakeLayoutFn(layout),
		discoverProcesses: func() ([]DoltProcInfo, error) {
			return []DoltProcInfo{{PID: 2, Ports: []int{28231}, Argv: []string{"dolt", "sql-server", "--data-dir", layout.DataDir}}}, nil
		},
	}

	got, err := r.resolve("/city")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Port != 28231 || got.Source != liveDoltProcessSource {
		t.Errorf("got %+v, want process-table fallback after invalid handle value", got)
	}
	if got.Attempts[0].Status != "error" {
		t.Errorf("handle attempt = %+v, want recorded error", got.Attempts[0])
	}
}

// invalidHandleResolver returns a resolver whose managed handle is garbled
// (unparseable port) and whose process table is supplied by procs. This is
// the corrupt-managed-runtime-state shape that must suppress the legacy 3307
// default for the whole chain — see the resolve() doc comment.
func invalidHandleResolver(procs []DoltProcInfo) liveDoltPortResolver {
	return liveDoltPortResolver{
		managedHandlePort: func(string) string { return "not-a-port" },
		runtimeLayout:     fakeLayoutFn(testManagedLayout()),
		discoverProcesses: func() ([]DoltProcInfo, error) { return procs, nil },
	}
}

// assertNoLegacyFallbackAfterHandleError asserts the full-chain contract: an
// "error" attempt from the managed handle stops ResolveDoltPort at Port 0
// rather than falling through to LegacyDefaultDoltPort.
func assertNoLegacyFallbackAfterHandleError(t *testing.T, r liveDoltPortResolver) {
	t.Helper()

	got := ResolveDoltPort(PortResolverInput{
		CityPath:    "/city",
		LiveResolve: r.resolve,
	})

	if got.Port != 0 {
		t.Errorf("Port = %d, want 0 — a garbled managed handle must not resolve a port", got.Port)
	}
	if got.Port == LegacyDefaultDoltPort {
		t.Errorf("Port fell through to the legacy default %d after a garbled managed handle", LegacyDefaultDoltPort)
	}
	if got.Fallback {
		t.Error("Fallback = true, want false — the legacy default must be suppressed")
	}
	// The handle error is the FIRST error attempt, so it is the reported
	// source even when a later step also errors.
	if got.Source != liveDoltHandleSource {
		t.Errorf("Source = %q, want %q", got.Source, liveDoltHandleSource)
	}
	for _, attempt := range got.Tried {
		if attempt.Source == "legacy default" {
			t.Fatalf("legacy default was tried after a garbled managed handle: %+v", got.Tried)
		}
	}
	if len(got.Tried) == 0 {
		t.Fatal("no attempts recorded")
	}
	for _, attempt := range got.Tried {
		if attempt.Source == liveDoltHandleSource {
			if attempt.Status != "error" {
				t.Errorf("managed-handle attempt status = %q, want error", attempt.Status)
			}
			if !strings.Contains(attempt.Detail, "not-a-port") {
				t.Errorf("managed-handle detail = %q, want the offending value", attempt.Detail)
			}
			return
		}
	}
	t.Errorf("did not find %s in Tried entries: %+v", liveDoltHandleSource, got.Tried)
}

// TestLiveDoltPortResolver_InvalidHandleNoProcessSuppressesLegacyFallback
// pins the conservative hard stop: a garbled managed handle plus no matching
// live process yields Port 0, NOT legacy 3307. Contrast
// TestResolveDoltPort_NoCityPathFallsThroughDirectly, where the absence of a
// handle records "not-provided" and the legacy default stays reachable — it
// is corruption, not absence, that suppresses the fallback.
func TestLiveDoltPortResolver_InvalidHandleNoProcessSuppressesLegacyFallback(t *testing.T) {
	r := invalidHandleResolver([]DoltProcInfo{
		// Live listener belonging to a different city: must not match.
		{PID: 1, Ports: []int{4000}, Argv: []string{"dolt", "sql-server", "--data-dir", "/elsewhere/dolt"}},
	})

	res, err := r.resolve("/city")
	if !errors.Is(err, errNoLiveDoltEndpoint) {
		t.Fatalf("resolve err = %v, want errNoLiveDoltEndpoint", err)
	}
	want := []string{
		liveDoltHandleSource + ":error",
		liveDoltProcessSource + ":not-found",
	}
	if fmt.Sprint(attemptStatuses(res.Attempts)) != fmt.Sprint(want) {
		t.Errorf("Attempts = %v, want %v", attemptStatuses(res.Attempts), want)
	}

	assertNoLegacyFallbackAfterHandleError(t, r)
}

// TestLiveDoltPortResolver_InvalidHandleAmbiguousMatchSuppressesLegacyFallback
// pins the same hard stop for the other failure shape the reviewer named:
// a garbled managed handle plus multiple candidate listeners. Two independent
// "error" attempts are recorded and the chain still stops at Port 0.
func TestLiveDoltPortResolver_InvalidHandleAmbiguousMatchSuppressesLegacyFallback(t *testing.T) {
	layout := testManagedLayout()
	r := invalidHandleResolver([]DoltProcInfo{
		{PID: 2, Ports: []int{28231}, Argv: []string{"dolt", "sql-server", "--data-dir", layout.DataDir}},
		{PID: 3, Ports: []int{29000}, Argv: []string{"dolt", "sql-server", "--config", layout.ConfigFile}},
	})

	res, err := r.resolve("/city")
	if err == nil {
		t.Fatalf("resolve succeeded with a garbled handle and ambiguous listeners: %+v", res)
	}
	if errors.Is(err, errNoLiveDoltEndpoint) {
		t.Fatalf("ambiguity reported as no-endpoint: %v", err)
	}
	want := []string{
		liveDoltHandleSource + ":error",
		liveDoltProcessSource + ":error",
	}
	if fmt.Sprint(attemptStatuses(res.Attempts)) != fmt.Sprint(want) {
		t.Errorf("Attempts = %v, want %v", attemptStatuses(res.Attempts), want)
	}

	assertNoLegacyFallbackAfterHandleError(t, r)
}

func TestLiveDoltPortResolver_EmptyCityPathErrors(t *testing.T) {
	r := newLiveDoltPortResolver()

	got, err := r.resolve("")
	if !errors.Is(err, errNoLiveDoltEndpoint) {
		t.Fatalf("err = %v, want errNoLiveDoltEndpoint", err)
	}
	for _, a := range got.Attempts {
		if a.Status != "not-provided" {
			t.Errorf("attempt %+v, want not-provided", a)
		}
	}
}

func TestDoltProcMatchesManagedLayout(t *testing.T) {
	layout := testManagedLayout()
	cases := []struct {
		name string
		argv []string
		want bool
	}{
		{"config space form", []string{"dolt", "sql-server", "--config", layout.ConfigFile}, true},
		{"config equals form", []string{"dolt", "sql-server", "--config=" + layout.ConfigFile}, true},
		{"data-dir space form", []string{"dolt", "sql-server", "--data-dir", layout.DataDir}, true},
		{"data-dir equals form", []string{"dolt", "sql-server", "--data-dir=" + layout.DataDir}, true},
		{"foreign config", []string{"dolt", "sql-server", "--config", "/other/config.yaml"}, false},
		{"foreign data dir", []string{"dolt", "sql-server", "--data-dir", "/other/dolt"}, false},
		{"no flags", []string{"dolt", "sql-server"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := doltProcMatchesManagedLayout(DoltProcInfo{Argv: tc.argv}, layout); got != tc.want {
				t.Errorf("doltProcMatchesManagedLayout(%v) = %v, want %v", tc.argv, got, tc.want)
			}
		})
	}
}
