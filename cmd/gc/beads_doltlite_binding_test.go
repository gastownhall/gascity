package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// A doltlite city runs no Dolt server and no proxy: its store is the embedded
// engine under .beads/embeddeddolt. The proxied-local default must not reach it.
//
// The failure this pins is quiet. Since the default flipped, a scope with no
// persisted dolt_mode canonicalizes to "proxied-server", and that rule did not
// look at the backend — so a doltlite city came out of `gc init` carrying
// dolt_mode "proxied-server", which is exactly the shape the R1 classifier
// reads as a bd-owned proxied scope. gc then treated it as provider-owned and
// bd raised a proxy plus a Dolt child (at its own 30s idle timeout, since gc
// never asked for a resident one) over a workspace that is supposed to have
// neither.

func writeDoltliteCityScaffold(t *testing.T, cityPath string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	cityConfig := "[workspace]\nname = \"doltlite-city\"\nprefix = \"dl\"\n\n[beads]\nprovider = \"bd\"\nbackend = \"doltlite\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityConfig), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readScopeMetadata(t *testing.T, scopeRoot string) contract.MetadataState {
	t.Helper()
	state, ok, err := contract.LoadMetadataState(fsys.OSFS{}, filepath.Join(scopeRoot, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("load metadata for %s: %v", scopeRoot, err)
	}
	if !ok {
		t.Fatalf("no canonical metadata under %s", scopeRoot)
	}
	return state
}

func TestFreshScopeCanonicalDoltModeKeepsDoltliteOffTheProxiedDefault(t *testing.T) {
	cityPath := t.TempDir()
	writeDoltliteCityScaffold(t, cityPath)

	if got := freshScopeCanonicalDoltMode(cityPath); got != "server" {
		t.Fatalf("freshScopeCanonicalDoltMode(doltlite city) = %q, want server", got)
	}
}

func TestFreshScopeCanonicalDoltModeStillDefaultsDoltCitiesToProxied(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"dolt-city\"\nprefix = \"gc\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := freshScopeCanonicalDoltMode(cityPath); got != "proxied-server" {
		t.Fatalf("freshScopeCanonicalDoltMode(fresh dolt city) = %q, want proxied-server", got)
	}
}

func TestNormalizeCanonicalBdScopeFilesForInitKeepsDoltliteOffTheProxiedDefault(t *testing.T) {
	cityPath := t.TempDir()
	writeDoltliteCityScaffold(t, cityPath)

	if err := normalizeCanonicalBdScopeFilesForInit(cityPath, cityPath, "dl", "dl"); err != nil {
		t.Fatalf("normalizeCanonicalBdScopeFilesForInit: %v", err)
	}

	if got := readScopeMetadata(t, cityPath).DoltMode; !strings.EqualFold(got, "server") {
		t.Fatalf("doltlite city dolt_mode = %q, want server (the mode it had before the proxied default)", got)
	}
}

// The rig path is the same decision, and a rig is where the damage compounds:
// a proxied binding on a rig gets its own proxy root and its own Dolt child.
func TestNormalizeCanonicalBdScopeFilesForInitKeepsDoltliteRigsOffTheProxiedDefault(t *testing.T) {
	cityPath := t.TempDir()
	writeDoltliteCityScaffold(t, cityPath)
	rigPath := filepath.Join(cityPath, "rigs", "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := normalizeCanonicalBdScopeFilesForInit(cityPath, rigPath, "fe", "fe"); err != nil {
		t.Fatalf("normalizeCanonicalBdScopeFilesForInit: %v", err)
	}

	if got := readScopeMetadata(t, rigPath).DoltMode; strings.EqualFold(got, "proxied-server") {
		t.Fatalf("doltlite rig dolt_mode = %q, which gives it a proxy root of its own", got)
	}
}

// The classifier is the thing that actually spawns a proxy, so assert on it
// rather than only on the string that feeds it.
func TestDoltliteScopeIsNotProviderOwnedProxied(t *testing.T) {
	cityPath := t.TempDir()
	writeDoltliteCityScaffold(t, cityPath)

	if err := normalizeCanonicalBdScopeFilesForInit(cityPath, cityPath, "dl", "dl"); err != nil {
		t.Fatalf("normalizeCanonicalBdScopeFilesForInit: %v", err)
	}
	owned, err := scopeBindingIsProviderOwnedProxied(cityPath)
	if err != nil {
		t.Fatalf("scopeBindingIsProviderOwnedProxied: %v", err)
	}
	if owned {
		t.Fatal("a doltlite city classified as a bd-owned proxied scope")
	}
}
