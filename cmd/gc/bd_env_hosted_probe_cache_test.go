package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeHostedProbeCity writes a composed city (city.toml including
// storage.toml) whose storage binding selects the hosted credential provider
// when hosted is true. Files get a fixed mtime so a rewrite is always
// observable through the stat-based cache validation, independent of the
// filesystem's timestamp resolution.
func writeHostedProbeCity(t *testing.T, cityPath string, hosted bool, stamp time.Time) {
	t.Helper()
	cityTOML := "include = [\"storage.toml\"]\n[workspace]\nname = \"probe-cache\"\n"
	storageTOML := strings.TrimPrefix(hostedBeadsCityTOML("https://beads.example", "gasworks", !hosted), "[workspace]\nname = \"hosted-provider-test\"\n\n")
	for name, body := range map[string]string{"city.toml": cityTOML, "storage.toml": storageTOML} {
		path := filepath.Join(cityPath, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHostedCredentialProbeReusesTheComposedLoad(t *testing.T) {
	resetHostedCredentialProbeCache()
	t.Cleanup(resetHostedCredentialProbeCache)
	cityPath := t.TempDir()
	stamp := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	writeHostedProbeCity(t, cityPath, true, stamp)

	loadsBefore := hostedCredentialProbeLoads.Load()
	for i := 0; i < 3; i++ {
		selected, err := citySelectsHostedBeadsCredentialProvider(cityPath)
		if err != nil {
			t.Fatalf("probe %d: %v", i, err)
		}
		if !selected {
			t.Fatalf("probe %d: hosted binding not recognized", i)
		}
	}
	if got := hostedCredentialProbeLoads.Load() - loadsBefore; got != 1 {
		t.Fatalf("config loads for three probes = %d, want 1 (the answer must be memoized)", got)
	}

	// The selector lives in the INCLUDED file: rewriting it must invalidate
	// the entry even though city.toml itself is untouched.
	writeHostedProbeCity(t, cityPath, false, stamp.Add(time.Second))
	selected, err := citySelectsHostedBeadsCredentialProvider(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if selected {
		t.Fatal("probe still reports the hosted binding after storage.toml dropped it")
	}
	if got := hostedCredentialProbeLoads.Load() - loadsBefore; got != 2 {
		t.Fatalf("config loads after the include changed = %d, want 2", got)
	}
}

func TestHostedCredentialProbeDoesNotCacheErrorsOrMissingCities(t *testing.T) {
	resetHostedCredentialProbeCache()
	t.Cleanup(resetHostedCredentialProbeCache)
	cityPath := t.TempDir()

	if selected, err := citySelectsHostedBeadsCredentialProvider(cityPath); err != nil || selected {
		t.Fatalf("missing city.toml: selected=%v err=%v, want false,nil", selected, err)
	}

	cityConfig := filepath.Join(cityPath, "city.toml")
	if err := os.WriteFile(cityConfig, []byte("[storage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := citySelectsHostedBeadsCredentialProvider(cityPath); err == nil {
		t.Fatal("invalid city.toml did not fail closed")
	}
	if _, cached := hostedCredentialProbeCache.Load(normalizePathForCompare(cityConfig)); cached {
		t.Fatal("a failed load was memoized")
	}

	stamp := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	writeHostedProbeCity(t, cityPath, true, stamp)
	loadsBefore := hostedCredentialProbeLoads.Load()
	if selected, err := citySelectsHostedBeadsCredentialProvider(cityPath); err != nil || !selected {
		t.Fatalf("repaired city: selected=%v err=%v, want true,nil", selected, err)
	}
	if selected, err := citySelectsHostedBeadsCredentialProvider(cityPath); err != nil || !selected {
		t.Fatalf("repaired city (second probe): selected=%v err=%v, want true,nil", selected, err)
	}
	if got := hostedCredentialProbeLoads.Load() - loadsBefore; got != 1 {
		t.Fatalf("config loads after repair = %d, want 1", got)
	}
}
