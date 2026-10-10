package beads

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
	beadslib "github.com/steveyegge/beads"
	beadsbackend "github.com/steveyegge/beads/backend"
)

// registerLibraryBackendForTest registers name with the linked beads library
// for the duration of the test, standing in for a build's distribution wiring
// (the enterprise build's bdhttp.Register).
func registerLibraryBackendForTest(t *testing.T, name string) {
	t.Helper()
	refuse := func(context.Context, string) (beadsbackend.DoltStorage, error) {
		return nil, errors.New("test backend does not open")
	}
	beadsbackend.Register(name, beadsbackend.Backend{Open: refuse, OpenReadOnly: refuse})
	t.Cleanup(func() { beadsbackend.Deregister(name) })
}

// libraryBackendScopeFixture registers name with the linked beads library and
// writes a scope whose metadata selects it, the shape `bd connect <url>
// --convert-workspace` leaves behind.
func libraryBackendScopeFixture(t *testing.T, name string) string {
	t.Helper()
	registerLibraryBackendForTest(t, name)

	scope := t.TempDir()
	beadsDir := filepath.Join(scope, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"),
		[]byte(`{"backend":"`+name+`","dolt_mode":"server","dolt_database":"gascity"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return scope
}

// preflightMustNotRun fails the test if the Dolt preflight is consulted: for a
// library extension backend its bd-context fork and SQL ping are meaningless.
func preflightMustNotRun(t *testing.T) contract.PreflightChecker {
	t.Helper()
	return contract.PreflightChecker{
		FS:       fsys.NewFake(),
		Provider: "bd",
		BDContext: func(string) (contract.PreflightBDContext, error) {
			t.Error("Dolt preflight ran bd context for a library extension backend")
			return contract.PreflightBDContext{}, nil
		},
		DatabaseProjectID: func(string) (string, bool, error) {
			t.Error("Dolt preflight pinged the database for a library extension backend")
			return "", false, nil
		},
	}
}

func TestOpenStoreAtForCityLibraryExtensionBackendSkipsPreflightAndOpensNative(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	scope := libraryBackendScopeFixture(t, "gctest-ext")
	native := NewMemStore()

	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		PreflightChecker: preflightMustNotRun(t),
		OpenBdStore: func() (Store, error) {
			t.Fatal("OpenBdStore called for a library extension backend that opened natively")
			return nil, nil
		},
		OpenNativeStore: func() (Store, error) { return native, nil },
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity() error = %v", err)
	}
	if result.Store != native {
		t.Fatalf("Store = %T, want the native store", result.Store)
	}
	if result.Diagnostic.Store != storeNameNativeDoltStore || !result.Diagnostic.NativeStoreEligible {
		t.Fatalf("diagnostic = %+v, want an eligible native store", result.Diagnostic)
	}
}

// With no injected opener the factory's default reaches the library's own
// open, which dispatches to the registered backend; a failure there is the
// answer, not a cue to fall back to a front door preflight never vetted.
func TestOpenStoreAtForCityLibraryExtensionBackendSurfacesLibraryOpenFailure(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	scope := libraryBackendScopeFixture(t, "gctest-ext")

	var openedDir string
	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })
	nativeDoltOpenBestAvailable = func(_ context.Context, beadsDir string) (beadslib.Storage, error) {
		openedDir = beadsDir
		return nil, errors.New("remote refused the bearer")
	}

	_, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		PreflightChecker: preflightMustNotRun(t),
		OpenBdStore: func() (Store, error) {
			t.Fatal("OpenBdStore called after a library extension backend failed to open")
			return nil, nil
		},
	})
	if err == nil {
		t.Fatal("OpenStoreAtForCity() error = nil, want the library open failure")
	}
	if want := filepath.Join(scope, ".beads"); openedDir != want {
		t.Fatalf("library open beadsDir = %q, want %q", openedDir, want)
	}
	for _, part := range []string{"gctest-ext", scope, "remote refused the bearer"} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("error %q does not name %q", err, part)
		}
	}
}

// The per-city kill switch still wins: native_transport=off means the bd front
// door, whatever the backend.
func TestOpenStoreAtForCityLibraryExtensionBackendHonorsNativeTransportOff(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	scope := libraryBackendScopeFixture(t, "gctest-ext")
	bd := NewMemStore()

	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		NativeTransport:  NativeTransportOff,
		PreflightChecker: preflightMustNotRun(t),
		OpenBdStore:      func() (Store, error) { return bd, nil },
		OpenNativeStore: func() (Store, error) {
			t.Fatal("OpenNativeStore called with native_transport=off")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity() error = %v", err)
	}
	if result.Store != Store(bd) || result.Diagnostic.PreflightGate != nativeTransportOffGate {
		t.Fatalf("result = (%T, %+v), want the bd store under %s", result.Store, result.Diagnostic, nativeTransportOffGate)
	}
}

// A backend name nothing registered is not an extension: the scope takes the
// path it took before, through preflight.
func TestOpenStoreAtForCityUnregisteredBackendStillRunsPreflight(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	scope := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scope, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, ".beads", "metadata.json"), []byte(`{"backend":"gctest-unregistered"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	bd := NewMemStore()
	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		PreflightChecker: factoryPreflightChecker(scope, `{"backend":"gctest-unregistered"}`, contract.PreflightBDContext{Backend: "gctest-unregistered"}),
		OpenBdStore:      func() (Store, error) { return bd, nil },
		OpenNativeStore: func() (Store, error) {
			t.Fatal("OpenNativeStore called for an unregistered backend")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity() error = %v", err)
	}
	if result.Store != Store(bd) || result.Diagnostic.PreflightGate != string(contract.PreflightCheckMetadataBackend) {
		t.Fatalf("result = (%T, %+v), want the bd store under the metadata_backend gate", result.Store, result.Diagnostic)
	}
}
