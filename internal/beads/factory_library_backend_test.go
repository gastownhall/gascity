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
// (the enterprise build's bdhttp.Register). A name the build's own wiring
// already registered is left alone: the library panics on a duplicate.
func registerLibraryBackendForTest(t *testing.T, name string) {
	t.Helper()
	if beadsbackend.Registered(name) {
		return
	}
	refuse := func(context.Context, string) (beadsbackend.DoltStorage, error) {
		return nil, errors.New("test backend does not open")
	}
	beadsbackend.Register(name, beadsbackend.Backend{Open: refuse, OpenReadOnly: refuse})
	t.Cleanup(func() { beadsbackend.Deregister(name) })
}

// libraryBackendScopeFixture registers the http backend with the linked beads
// library and writes the scope `bd connect <url> --convert-workspace` leaves
// behind: metadata names the backend with no dolt_mode and no storage fields,
// and the server target lives beside it in http_target.json.
func libraryBackendScopeFixture(t *testing.T) string {
	t.Helper()
	registerLibraryBackendForTest(t, "http")
	scope := t.TempDir()
	writeLibraryBackendScopeFiles(t, scope, `{"database":"dolt","backend":"http","dolt_database":"gascity","project_id":"proj-1"}`)
	return scope
}

// writeLibraryBackendScopeFiles writes metadata plus the http_target.json
// sidecar a converted workspace carries.
func writeLibraryBackendScopeFiles(t *testing.T, scope, metadata string) {
	t.Helper()
	beadsDir := filepath.Join(scope, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0o644); err != nil {
		t.Fatal(err)
	}
	target := `{"url":"https://beads.example.test","expect_project_id":"proj-1","api":"v0"}`
	if err := os.WriteFile(filepath.Join(beadsDir, "http_target.json"), []byte(target), 0o644); err != nil {
		t.Fatal(err)
	}
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
	scope := libraryBackendScopeFixture(t)
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
	if result.Diagnostic.Store != BeadsStoreNameLibraryBackendStore || !result.Diagnostic.NativeStoreEligible {
		t.Fatalf("diagnostic = %+v, want an eligible %s", result.Diagnostic, BeadsStoreNameLibraryBackendStore)
	}
}

// With no injected opener the factory's default reaches the library's own
// open, which dispatches to the registered backend; a failure there is the
// answer, not a cue to fall back to a front door preflight never vetted.
func TestOpenStoreAtForCityLibraryExtensionBackendSurfacesLibraryOpenFailure(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	scope := libraryBackendScopeFixture(t)

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
	for _, part := range []string{`"http"`, scope, "remote refused the bearer"} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("error %q does not name %q", err, part)
		}
	}
}

// The per-city kill switch still wins: native_transport=off means the bd front
// door, whatever the backend.
func TestOpenStoreAtForCityLibraryExtensionBackendHonorsNativeTransportOff(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	scope := libraryBackendScopeFixture(t)
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

// A scope that carries storage fields is a complete storage binding, whatever
// backend it names: it keeps the preflight verdict and the bd front-door
// fallback. Only a binding-free scope takes the extension arm.
func TestOpenStoreAtForCityLibraryExtensionBackendWithStorageFieldsKeepsPreflight(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	registerLibraryBackendForTest(t, "http")
	scope := t.TempDir()
	const metadata = `{"backend":"http","storage_endpoint":"https://beads.example.test","storage_database":"gascity"}`
	writeLibraryBackendScopeFiles(t, scope, metadata)
	assertPreflightServesWithBdFallback(t, scope, metadata, "http")
}

// Registration with the library does not make a backend an extension: one the
// library registers whose open still depends on gc's preflight verdict and
// fallback — postgres — keeps both, so an existing binding opens through the
// bd front door exactly as it did before the library learned the name.
func TestOpenStoreAtForCityLibraryRegisteredBackendOffTheAllowlistKeepsPreflight(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	for _, tc := range []struct{ backend, metadata string }{
		{backend: "postgres", metadata: `{"database":"beads","backend":"postgres","storage_endpoint":"postgres://bd@db.example.test:5432","storage_database":"beads_pg"}`},
		{backend: "gctest-ext", metadata: `{"database":"dolt","backend":"gctest-ext","dolt_database":"gascity"}`},
	} {
		t.Run(tc.backend, func(t *testing.T) {
			registerLibraryBackendForTest(t, tc.backend)
			scope := t.TempDir()
			writeLibraryBackendScopeFiles(t, scope, tc.metadata)
			assertPreflightServesWithBdFallback(t, scope, tc.metadata, tc.backend)
		})
	}
}

// assertPreflightServesWithBdFallback opens scope and requires the preflight
// to refuse the backend by name (its metadata_backend gate) and the bd front
// door to answer.
func assertPreflightServesWithBdFallback(t *testing.T, scope, metadata, backend string) {
	t.Helper()
	bd := NewMemStore()
	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		PreflightChecker: factoryPreflightChecker(scope, metadata, contract.PreflightBDContext{Backend: backend}),
		OpenBdStore:      func() (Store, error) { return bd, nil },
		OpenNativeStore: func() (Store, error) {
			t.Fatalf("OpenNativeStore called for backend %q; it must keep the preflight verdict", backend)
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity() error = %v", err)
	}
	if result.Store != Store(bd) || result.Diagnostic.Store != storeNameBdStore {
		t.Fatalf("result = (%T, %+v), want the bd front door", result.Store, result.Diagnostic)
	}
	if result.Diagnostic.PreflightGate != string(contract.PreflightCheckMetadataBackend) {
		t.Fatalf("PreflightGate = %q, want %q", result.Diagnostic.PreflightGate, contract.PreflightCheckMetadataBackend)
	}
}
