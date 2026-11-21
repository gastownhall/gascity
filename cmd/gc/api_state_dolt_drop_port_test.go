package main

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// setControllerLiveDoltPortResolve installs a live-resolution seam for the
// duration of a test and restores the production nil afterwards.
func setControllerLiveDoltPortResolve(t *testing.T, fn func(cityPath string) (liveDoltPortResolution, error)) {
	t.Helper()
	orig := controllerLiveDoltPortResolve
	controllerLiveDoltPortResolve = fn
	t.Cleanup(func() { controllerLiveDoltPortResolve = orig })
}

// TestControllerDropManagedDoltDatabase_RefusesOnLegacyFallback pins the
// blast-radius guard on the chain's most destructive consumer. With the port
// file out of the resolution chain, a stopped managed dolt is a clean miss
// that lands on LegacyDefaultDoltPort — so the DROP must refuse rather than
// issue itself against whatever happens to be listening on 3307.
//
// The refusal is proven structurally: it returns before
// newSQLCleanupDoltClient, so no connection is opened and no DROP is sent.
// The city path is a t.TempDir() with no dolt server, and the error names the
// legacy port so a future change that silently re-enables the fallback fails
// here rather than in production.
func TestControllerDropManagedDoltDatabase_RefusesOnLegacyFallback(t *testing.T) {
	setControllerLiveDoltPortResolve(t, func(string) (liveDoltPortResolution, error) {
		// A clean miss: no live endpoint, no "error" attempt, so the chain
		// reaches the legacy default rather than hard-stopping at Port 0.
		return liveDoltPortResolution{
			Attempts: []PortResolutionAttempt{
				{Source: liveDoltHandleSource, Status: "not-found"},
				{Source: liveDoltProcessSource, Status: "not-found"},
			},
		}, errNoLiveDoltEndpoint
	})

	cs := &controllerState{cityPath: t.TempDir(), cfg: &config.City{}}
	err := controllerDropManagedDoltDatabase(cs, context.Background(), "gc_stale_db")
	if err == nil {
		t.Fatal("drop succeeded on a legacy-default fallback; it must refuse rather than DROP against port 3307")
	}
	for _, want := range []string{"refusing to drop", "gc_stale_db", "no live managed dolt endpoint"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to contain %q", err, want)
		}
	}
	if !strings.Contains(err.Error(), "3307") {
		t.Fatalf("error = %q, want it to name the legacy port %d it refused to use", err, LegacyDefaultDoltPort)
	}
	// Guard against a regression to the old "opening dolt connection" path:
	// that message would mean the refusal moved AFTER the client open.
	if strings.Contains(err.Error(), "opening dolt connection") {
		t.Fatalf("error = %q: the refusal must precede newSQLCleanupDoltClient", err)
	}
}

// TestControllerDropManagedDoltDatabase_PoisonedEnvDoesNotRedirectToForeignCity
// is the cleanup-safety regression carried onto the controller's DROP path:
// with GC_DOLT_* pointing at a foreign city's live dolt sql-server, a DROP
// issued for city A must not resolve city B's port (and so cannot drop a
// database out of the wrong server). Same shape as
// TestLiveDoltPortResolverForExplicitCity_PoisonedEnvDoesNotMatchForeignCity,
// asserted through the production call site rather than the resolver alone.
func TestControllerDropManagedDoltDatabase_PoisonedEnvDoesNotRedirectToForeignCity(t *testing.T) {
	const foreignConfig = "/cityB/.gc/runtime/packs/dolt/dolt-config.yaml"
	const foreignPort = 29999
	t.Setenv("GC_DOLT_CONFIG_FILE", foreignConfig)
	t.Setenv("GC_DOLT_DATA_DIR", "/cityB/.beads/dolt")

	foreignProcs := func() ([]DoltProcInfo, error) {
		return []DoltProcInfo{
			{PID: 99, Ports: []int{foreignPort}, Argv: []string{"dolt", "sql-server", "--config", foreignConfig}},
		}, nil
	}

	// The seam wires the SAME strict resolver production uses; only the
	// process table is faked, so the env-poison vector is exercised end to end.
	strict := newLiveDoltPortResolverForExplicitCity()
	strict.managedHandlePort = func(string) string { return "" }
	strict.discoverProcesses = foreignProcs
	setControllerLiveDoltPortResolve(t, strict.resolve)

	cityA := t.TempDir()
	cs := &controllerState{cityPath: cityA, cfg: &config.City{}}
	err := controllerDropManagedDoltDatabase(cs, context.Background(), "gc_victim_db")
	if err == nil {
		t.Fatal("drop succeeded; the poisoned env must not resolve a droppable endpoint for city A")
	}
	if strings.Contains(err.Error(), "opening dolt connection") {
		t.Fatalf("error = %q: the DROP reached the connection stage, meaning it resolved a port for a city with no live server", err)
	}
	if !strings.Contains(err.Error(), "refusing to drop") {
		t.Fatalf("error = %q, want the fallback refusal", err)
	}

	// Sanity: the env-honoring resolver WOULD have matched the foreign
	// process, confirming this test exercises a real poison vector and not a
	// vacuously empty process table.
	env := newLiveDoltPortResolver()
	env.managedHandlePort = func(string) string { return "" }
	env.discoverProcesses = foreignProcs
	got, resErr := env.resolve(cityA)
	if resErr != nil || got.Port != foreignPort {
		t.Fatalf("env-honoring resolve(%s) = (port %d, err %v), want a match on the foreign process via the GC_DOLT_CONFIG_FILE poison", cityA, got.Port, resErr)
	}
}

// TestControllerDropManagedDoltDatabase_UsesStrictResolverInProduction pins
// that the production call site leaves the live seam nil, so ResolveDoltPort
// wires newLiveDoltPortResolverForExplicitCity rather than the env-honoring
// resolver. Without this, the two tests above could pass while production
// silently used the redirectable chain.
func TestControllerDropManagedDoltDatabase_UsesStrictResolverInProduction(t *testing.T) {
	if controllerLiveDoltPortResolve != nil {
		t.Fatal("controllerLiveDoltPortResolve must be nil in production so ResolveDoltPort wires the strict explicit-city resolver")
	}
}
