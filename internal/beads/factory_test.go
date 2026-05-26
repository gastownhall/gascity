package beads

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestOpenStoreAtForCityEligibleNativeWrapsInjectedNativeStoreInCache(t *testing.T) {
	scope := "/city"
	native := NewMemStore()

	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		PreflightChecker: factoryPreflightChecker(scope, factoryPreflightDoltMetadata("gc-local"), contract.PreflightBDContext{Backend: "dolt", DoltMode: "server"}, "gc-local"),
		OpenBdStore: func() (Store, error) {
			t.Fatal("OpenBdStore called for native-eligible scope")
			return nil, nil
		},
		OpenNativeStore: func() (Store, error) {
			return native, nil
		},
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity() error = %v", err)
	}

	cache, ok := result.Store.(*CachingStore)
	if !ok {
		t.Fatalf("Store = %T, want *CachingStore", result.Store)
	}
	if cache.Backing() != native {
		t.Fatalf("CachingStore backing = %T %#v, want injected native store", cache.Backing(), cache.Backing())
	}
	if result.Diagnostic.Store != storeNameCachingStore {
		t.Fatalf("diagnostic store = %q, want %q", result.Diagnostic.Store, storeNameCachingStore)
	}
	if !result.Diagnostic.NativeStoreEligible {
		t.Fatal("diagnostic native_store_eligible = false, want true")
	}
	if result.Diagnostic.PreflightGate != "" || result.Diagnostic.PreflightReason != "" {
		t.Fatalf("diagnostic preflight failure = (%q, %q), want empty", result.Diagnostic.PreflightGate, result.Diagnostic.PreflightReason)
	}
}

func TestOpenStoreAtForCityContextDriftFallsBackWithPreflightDiagnostic(t *testing.T) {
	scope := "/city"
	var bdOpened bool

	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		PreflightChecker: factoryPreflightChecker(scope, factoryPreflightDoltMetadata("gc-local"), contract.PreflightBDContext{Backend: "postgres"}, "gc-local"),
		OpenBdStore: func() (Store, error) {
			bdOpened = true
			return NewMemStore(), nil
		},
		OpenNativeStore: func() (Store, error) {
			t.Fatal("OpenNativeStore called for context-drifted scope")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity() error = %v", err)
	}
	if !bdOpened {
		t.Fatal("OpenBdStore was not called for context-drifted scope")
	}
	if _, ok := result.Store.(*CachingStore); ok {
		t.Fatalf("Store = %T, want fallback store without native cache", result.Store)
	}
	if result.Diagnostic.Store != storeNameBdStore {
		t.Fatalf("diagnostic store = %q, want %q", result.Diagnostic.Store, storeNameBdStore)
	}
	if result.Diagnostic.NativeStoreEligible {
		t.Fatal("diagnostic native_store_eligible = true, want false")
	}
	if result.Diagnostic.PreflightGate != string(contract.PreflightCheckBDContextAgreement) {
		t.Fatalf("diagnostic preflight_gate = %q, want %q", result.Diagnostic.PreflightGate, contract.PreflightCheckBDContextAgreement)
	}
	if !strings.Contains(result.Diagnostic.PreflightReason, "bd context reports backend=postgres") {
		t.Fatalf("diagnostic preflight_reason = %q, want bd context drift reason", result.Diagnostic.PreflightReason)
	}
}

func TestOpenStoreAtForCityForceFallbackSkipsPreflightAndNativeOpen(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "1")

	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        "/missing-scope",
		Provider:         "bd",
		PreflightChecker: contract.PreflightChecker{},
		OpenBdStore: func() (Store, error) {
			return NewMemStore(), nil
		},
		OpenNativeStore: func() (Store, error) {
			t.Fatal("OpenNativeStore called while force fallback is enabled")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity() error = %v", err)
	}
	if result.Diagnostic.Store != storeNameBdStore {
		t.Fatalf("diagnostic store = %q, want %q", result.Diagnostic.Store, storeNameBdStore)
	}
	if result.Diagnostic.NativeStoreEligible {
		t.Fatal("diagnostic native_store_eligible = true, want false")
	}
	if result.Diagnostic.PreflightGate != string(contract.PreflightCheckProviderContract) {
		t.Fatalf("diagnostic preflight_gate = %q, want provider_contract", result.Diagnostic.PreflightGate)
	}
	if result.Diagnostic.PreflightReason != nativeForceFallbackEnv+"=1" {
		t.Fatalf("diagnostic preflight_reason = %q, want force fallback reason", result.Diagnostic.PreflightReason)
	}
}

func factoryPreflightChecker(scope, metadata string, ctx contract.PreflightBDContext, dbProjectID string) contract.PreflightChecker {
	files := fsys.NewFake()
	files.Dirs[filepath.Join(scope, ".beads")] = true
	files.Files[filepath.Join(scope, ".beads", "metadata.json")] = []byte(metadata)
	if ctx.BDVersion == "" {
		ctx.BDVersion = "1.0.4"
	}
	if ctx.SchemaVersion == 0 {
		ctx.SchemaVersion = 1
	}
	return contract.PreflightChecker{
		FS:                  files,
		Provider:            "bd",
		BeadsLibraryVersion: "1.0.4",
		BDContext: func(string) (contract.PreflightBDContext, error) {
			return ctx, nil
		},
		DatabaseProjectID: func(string) (string, bool, error) {
			return dbProjectID, dbProjectID != "", nil
		},
	}
}

func factoryPreflightDoltMetadata(projectID string) string {
	return `{
		"backend": "dolt",
		"dolt_mode": "server",
		"dolt_database": "gascity",
		"project_id": "` + projectID + `"
	}`
}
