package beads

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// TestDirectOpenWithholdsBDAllowRemoteMigrateByDefault and
// TestDirectOpenWithoutAmbientEnvWithholdsBDAllowRemoteMigrateByDefault are the
// G4 re-review's M7 regression tests: the direct native-open path must
// withhold BD_ALLOW_REMOTE_MIGRATE from the linked library unless the city has
// opted in (AllowSchemaBehindMigrateForScope), at BOTH of the two call
// sites that reach withWithheldBDRemoteMigrateLocked --
// openNativeStorageWithCredentialCommand (via OpenNativeDoltStoreAt /
// OpenNativeStorage) and openNativeStorageWithoutAmbientEnvWithCredentialCommand
// (via OpenNativeDoltStoreAtWithoutAmbientEnv), independently. A mutation that
// deletes the withhold's call at only one of the two sites, or deletes it
// inside withWithheldBDRemoteMigrateLocked itself, must fail at least one of
// these two tests.
func TestDirectOpenWithholdsBDAllowRemoteMigrateByDefault(t *testing.T) {
	t.Setenv("BD_ALLOW_REMOTE_MIGRATE", "ambient-poison")
	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })

	var seen string
	seenSet := false
	nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) {
		seen = os.Getenv("BD_ALLOW_REMOTE_MIGRATE")
		seenSet = true
		return &nativeDoltStorageSpy{
			getConfig: func(context.Context, string) (string, error) { return "gc", nil },
		}, nil
	}

	store, err := OpenNativeDoltStoreAt(context.Background(), filepath.Join(t.TempDir(), "scope"), nil)
	if err != nil {
		t.Fatalf("OpenNativeDoltStoreAt: %v", err)
	}
	t.Cleanup(func() { _ = store.CloseStore() })

	if !seenSet {
		t.Fatal("the open seam never ran")
	}
	if seen != "" {
		t.Errorf("BD_ALLOW_REMOTE_MIGRATE during the open = %q, want withheld (unset): an ambient unlock must not reach a database gc opens directly without the city's own opt-in", seen)
	}
	if got := os.Getenv("BD_ALLOW_REMOTE_MIGRATE"); got != "ambient-poison" {
		t.Errorf("BD_ALLOW_REMOTE_MIGRATE after the open = %q, want the ambient value restored", got)
	}
}

func TestDirectOpenWithoutAmbientEnvWithholdsBDAllowRemoteMigrateByDefault(t *testing.T) {
	t.Setenv("BD_ALLOW_REMOTE_MIGRATE", "ambient-poison")
	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })

	var seen string
	seenSet := false
	nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) {
		seen = os.Getenv("BD_ALLOW_REMOTE_MIGRATE")
		seenSet = true
		return &nativeDoltStorageSpy{
			getConfig: func(context.Context, string) (string, error) { return "gc", nil },
		}, nil
	}

	store, err := OpenNativeDoltStoreAtWithoutAmbientEnv(context.Background(), filepath.Join(t.TempDir(), "scope"))
	if err != nil {
		t.Fatalf("OpenNativeDoltStoreAtWithoutAmbientEnv: %v", err)
	}
	t.Cleanup(func() { _ = store.CloseStore() })

	if !seenSet {
		t.Fatal("the open seam never ran")
	}
	if seen != "" {
		t.Errorf("BD_ALLOW_REMOTE_MIGRATE during the hosted open = %q, want withheld (unset): an ambient unlock must not reach a database gc opens for a hosted workspace without the city's own opt-in", seen)
	}
	if got := os.Getenv("BD_ALLOW_REMOTE_MIGRATE"); got != "ambient-poison" {
		t.Errorf("BD_ALLOW_REMOTE_MIGRATE after the open = %q, want the ambient value restored", got)
	}
}

// TestDirectOpenProjectsBDAllowRemoteMigrateWhenScopeOptsIn and
// TestDirectOpenWithoutAmbientEnvProjectsBDAllowRemoteMigrateWhenScopeOptsIn
// are the LOW fix's regression tests: the open side must honor the
// beads.allow_schema_behind_migrate opt-in from a source beyond the ambient
// process env too (a city's workspace.env), through the exact same resolver
// function the native-store preflight schema gate uses
// (beads.AllowSchemaBehindMigrateForScope, which cmd/gc wires to its
// cityAllowSchemaBehindMigrate in an init()). These tests swap that package
// var directly, the same seam cmd/gc's init() uses in production, to prove
// the open path actually calls it per scopeRoot rather than only ever
// consulting the ambient process env.
func TestDirectOpenProjectsBDAllowRemoteMigrateWhenScopeOptsIn(t *testing.T) {
	oldResolver := AllowSchemaBehindMigrateForScope
	t.Cleanup(func() { AllowSchemaBehindMigrateForScope = oldResolver })
	var resolvedFor string
	AllowSchemaBehindMigrateForScope = func(scopeRoot string) bool {
		resolvedFor = scopeRoot
		return true
	}

	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })
	var seen string
	nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) {
		seen = os.Getenv("BD_ALLOW_REMOTE_MIGRATE")
		return &nativeDoltStorageSpy{
			getConfig: func(context.Context, string) (string, error) { return "gc", nil },
		}, nil
	}

	scopeRoot := filepath.Join(t.TempDir(), "scope")
	store, err := OpenNativeDoltStoreAt(context.Background(), scopeRoot, nil)
	if err != nil {
		t.Fatalf("OpenNativeDoltStoreAt: %v", err)
	}
	t.Cleanup(func() { _ = store.CloseStore() })

	if seen != "1" {
		t.Errorf("BD_ALLOW_REMOTE_MIGRATE during the open = %q, want %q: the scope's opt-in must reach the library", seen, "1")
	}
	if resolvedFor != scopeRoot {
		t.Errorf("AllowSchemaBehindMigrateForScope was asked about %q, want the open's own scopeRoot %q", resolvedFor, scopeRoot)
	}
}

func TestDirectOpenWithoutAmbientEnvProjectsBDAllowRemoteMigrateWhenScopeOptsIn(t *testing.T) {
	oldResolver := AllowSchemaBehindMigrateForScope
	t.Cleanup(func() { AllowSchemaBehindMigrateForScope = oldResolver })
	var resolvedFor string
	AllowSchemaBehindMigrateForScope = func(scopeRoot string) bool {
		resolvedFor = scopeRoot
		return true
	}

	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })
	var seen string
	nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) {
		seen = os.Getenv("BD_ALLOW_REMOTE_MIGRATE")
		return &nativeDoltStorageSpy{
			getConfig: func(context.Context, string) (string, error) { return "gc", nil },
		}, nil
	}

	scopeRoot := filepath.Join(t.TempDir(), "scope")
	store, err := OpenNativeDoltStoreAtWithoutAmbientEnv(context.Background(), scopeRoot)
	if err != nil {
		t.Fatalf("OpenNativeDoltStoreAtWithoutAmbientEnv: %v", err)
	}
	t.Cleanup(func() { _ = store.CloseStore() })

	if seen != "1" {
		t.Errorf("BD_ALLOW_REMOTE_MIGRATE during the hosted open = %q, want %q: the scope's opt-in must reach the library", seen, "1")
	}
	if resolvedFor != scopeRoot {
		t.Errorf("AllowSchemaBehindMigrateForScope was asked about %q, want the open's own scopeRoot %q", resolvedFor, scopeRoot)
	}
}

// TestAllowSchemaBehindMigrateForScopeDefaultsToFalse pins the package
// default (before any composition root overrides it) to the floor
// withWithheldBDRemoteMigrateLocked has always enforced, so a library import
// of this package with nobody wiring the override still fails closed. The
// real resolver (gc's cityAllowSchemaBehindMigrate, wired in cmd/gc's init())
// is backed by the internal/rollout-resolved beads.allow_schema_behind_migrate
// gate; this package itself stays rollout-agnostic and simply defaults to
// false (never consulting any env var directly).
func TestAllowSchemaBehindMigrateForScopeDefaultsToFalse(t *testing.T) {
	if AllowSchemaBehindMigrateForScope("/city") {
		t.Fatal("unwired package default reported true, want false (fail-closed floor)")
	}
}
