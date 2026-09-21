package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCitySelectsHostedBeadsCredentialProviderIsMemoisedButFollowsCityConfigRewrites
// pins the stat-signature-keyed memoization required by ga-v0agbz.1:
// citySelectsHostedBeadsCredentialProvider must load city.toml (and whatever
// it includes) at most once across repeated calls against an unchanged tree,
// and must reload — picking up the new answer — the moment city.toml's own
// stat signature changes.
//
// This mirrors TestProxiedCityRuntimeEnvIsMemoisedButFollowsCityConfigRewrites
// in bd_env_proxied_test.go, the sibling cache this one is modeled on.
func TestCitySelectsHostedBeadsCredentialProviderIsMemoisedButFollowsCityConfigRewrites(t *testing.T) {
	cityPath := writeHostedBeadsCity(t, "https://beads.example/workspaces/infra", "gasworks", false)
	t.Cleanup(func() { forgetCitySelectsHostedBeadsCredentialProvider(cityPath) })

	before := hostedCredentialProbeBuilds.Load()
	for range 5 {
		hosted, err := citySelectsHostedBeadsCredentialProvider(cityPath)
		if err != nil {
			t.Fatalf("citySelectsHostedBeadsCredentialProvider: %v", err)
		}
		if !hosted {
			t.Fatal("hosted = false, want true before any rewrite")
		}
	}
	if built := hostedCredentialProbeBuilds.Load() - before; built != 1 {
		t.Fatalf("probe loaded config %d times for 5 calls against an unchanged tree, want 1", built)
	}

	// Rewrite city.toml to a shape that no longer selects the hosted
	// provider (no storage config at all). A stale cache would keep
	// answering true forever.
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"),
		[]byte("[workspace]\nname = \"rewritten\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hosted, err := citySelectsHostedBeadsCredentialProvider(cityPath)
	if err != nil {
		t.Fatalf("citySelectsHostedBeadsCredentialProvider after rewrite: %v", err)
	}
	if hosted {
		t.Fatal("hosted = true after rewrite dropped the storage config entirely, want false")
	}
	if built := hostedCredentialProbeBuilds.Load() - before; built != 2 {
		t.Fatalf("a rewritten city.toml did not rebuild the probe (builds = %d), want 2", built)
	}

	// It settles: nothing changed on this call, so no third load.
	if _, err := citySelectsHostedBeadsCredentialProvider(cityPath); err != nil {
		t.Fatalf("citySelectsHostedBeadsCredentialProvider after settling: %v", err)
	}
	if built := hostedCredentialProbeBuilds.Load() - before; built != 2 {
		t.Fatalf("the settled probe was rebuilt again (builds = %d), want 2", built)
	}
}

// TestCitySelectsHostedBeadsCredentialProviderNeverCachesTheErrorPath pins
// that a load failure is never memoised: every call against a broken
// city.toml must re-attempt the load (and keep surfacing the error) rather
// than latching a cached answer from a transient failure.
func TestCitySelectsHostedBeadsCredentialProviderNeverCachesTheErrorPath(t *testing.T) {
	cityPath := t.TempDir()
	t.Cleanup(func() { forgetCitySelectsHostedBeadsCredentialProvider(cityPath) })
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[storage"), 0o600); err != nil {
		t.Fatal(err)
	}

	before := hostedCredentialProbeBuilds.Load()
	for i := range 3 {
		if _, err := citySelectsHostedBeadsCredentialProvider(cityPath); err == nil {
			t.Fatalf("call %d: want error for invalid city.toml, got nil", i)
		}
	}
	if built := hostedCredentialProbeBuilds.Load() - before; built != 0 {
		t.Fatalf("error-path calls incremented the memoised-build counter (built = %d), want 0 — errors must never be cached", built)
	}
}
