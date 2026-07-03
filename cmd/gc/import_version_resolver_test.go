package main

import (
	"bytes"
	"strings"
	"testing"
)

// seedTidesRegistry installs the unsorted-catalog fixture (pack "tides" with
// releases 1.0.0, 2.0.0, and a withdrawn 3.0.0) into a cached "main" registry
// under a fresh GC_HOME, and returns that home.
func seedTidesRegistry(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	writeEmptyRegistryConfig(t, home)
	catalogDir := writeRegistryCatalog(t, packRegistryUnsortedCatalog)
	var out, errb bytes.Buffer
	if code := doPackRegistryAdd("main", catalogDir, false, false, &out, &errb); code != 0 {
		t.Fatalf("doPackRegistryAdd: code=%d stderr=%q", code, errb.String())
	}
}

func TestRegistryVersionResolverPicksHighestNonWithdrawnReleaseCommit(t *testing.T) {
	seedTidesRegistry(t)
	resolve := newRegistryVersionResolver()

	got, ok, err := resolve("https://packages.example/tides.git", ">=1.0.0")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !ok {
		t.Fatal("ok=false; want registry resolution for a known pack")
	}
	// 3.0.0 is withdrawn, so the highest eligible release is 2.0.0.
	if got.Version != "2.0.0" || got.Commit != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("resolved = %#v, want {2.0.0 0123...}", got)
	}
}

func TestRegistryVersionResolverNormalizesSource(t *testing.T) {
	seedTidesRegistry(t)
	resolve := newRegistryVersionResolver()

	// Catalog source is "...tides.git"; a user-typed source without the .git
	// suffix and with a trailing slash must still match.
	got, ok, err := resolve("https://packages.example/tides/", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !ok || got.Version != "2.0.0" {
		t.Fatalf("resolve = (%#v, ok=%v); want matched 2.0.0", got, ok)
	}
}

func TestRegistryVersionResolverFailsClosedWhenNoReleaseMatches(t *testing.T) {
	seedTidesRegistry(t)
	resolve := newRegistryVersionResolver()

	_, ok, err := resolve("https://packages.example/tides.git", ">=9.0.0")
	if ok {
		t.Fatal("ok=true; want fail-closed for a known pack with no matching release")
	}
	if err == nil {
		t.Fatal("err=nil; want a fail-closed error, not silent fallback to git tags")
	}
	// The error must name the available (non-withdrawn) versions to guide the user.
	if !strings.Contains(err.Error(), "1.0.0") || !strings.Contains(err.Error(), "2.0.0") {
		t.Fatalf("error %q must list available versions 1.0.0 and 2.0.0", err.Error())
	}
	if strings.Contains(err.Error(), "3.0.0") {
		t.Fatalf("error %q must not list the withdrawn 3.0.0", err.Error())
	}
}

func TestRegistryVersionResolverUnknownSourceFallsBackToTags(t *testing.T) {
	seedTidesRegistry(t)
	resolve := newRegistryVersionResolver()

	got, ok, err := resolve("https://github.com/someone/not-in-registry.git", ">=1.0.0")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ok {
		t.Fatalf("ok=true for unknown source; want fallback (ok=false). got=%#v", got)
	}
}

func TestRegistryVersionResolverShaConstraintFallsThrough(t *testing.T) {
	seedTidesRegistry(t)
	resolve := newRegistryVersionResolver()

	_, ok, err := resolve("https://packages.example/tides.git", "sha:0123456789abcdef0123456789abcdef01234567")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ok {
		t.Fatal("ok=true for sha constraint; pinned commits must flow through the sha path")
	}
}

func TestRegistryVersionResolverNoCachedCatalogFallsBack(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	resolve := newRegistryVersionResolver()

	_, ok, err := resolve("https://packages.example/tides.git", ">=1.0.0")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ok {
		t.Fatal("ok=true with no cached catalog; want offline fallback (ok=false)")
	}
}
