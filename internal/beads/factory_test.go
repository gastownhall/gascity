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
)

func TestOpenStoreAtForCityEligibleNativeReturnsInjectedNativeStore(t *testing.T) {
	scope := "/city"
	native := NewMemStore()

	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		PreflightChecker: factoryPreflightChecker(scope, factoryPreflightDoltMetadata(), contract.PreflightBDContext{Backend: "dolt", DoltMode: "server"}),
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

	if result.Store != native {
		t.Fatalf("Store = %T %#v, want injected native store", result.Store, result.Store)
	}
	if result.Diagnostic.Store != storeNameNativeDoltStore {
		t.Fatalf("diagnostic store = %q, want %q", result.Diagnostic.Store, storeNameNativeDoltStore)
	}
	if !result.Diagnostic.NativeStoreEligible {
		t.Fatal("diagnostic native_store_eligible = false, want true")
	}
	if result.Diagnostic.PreflightGate != "" || result.Diagnostic.PreflightReason != "" {
		t.Fatalf("diagnostic preflight failure = (%q, %q), want empty", result.Diagnostic.PreflightGate, result.Diagnostic.PreflightReason)
	}
}

func TestOpenStoreAtForCityIneligibleProviderSkipsPreflight(t *testing.T) {
	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot: "/city",
		Provider:  "unknown",
		// Missing metadata would fail if the preflight checker ran.
		PreflightChecker: contract.PreflightChecker{FS: fsys.NewFake()},
		OpenBdStore: func() (Store, error) {
			return NewMemStore(), nil
		},
		OpenNativeStore: func() (Store, error) {
			t.Fatal("OpenNativeStore called for ineligible provider")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity() error = %v", err)
	}
	if result.Diagnostic.Store != storeNameBdStore {
		t.Fatalf("diagnostic store = %q, want %q", result.Diagnostic.Store, storeNameBdStore)
	}
	if result.Diagnostic.PreflightGate != string(contract.PreflightCheckProviderContract) {
		t.Fatalf("preflight gate = %q, want provider contract", result.Diagnostic.PreflightGate)
	}
}

func TestOpenStoreAtForCityContextDriftFallsBackWithPreflightDiagnostic(t *testing.T) {
	scope := "/city"
	var bdOpened bool

	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		PreflightChecker: factoryPreflightChecker(scope, factoryPreflightDoltMetadata(), contract.PreflightBDContext{Backend: "postgres"}),
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
	if result.Diagnostic.PreflightGate != nativeForceFallbackGate {
		t.Fatalf("diagnostic preflight_gate = %q, want %q", result.Diagnostic.PreflightGate, nativeForceFallbackGate)
	}
	if result.Diagnostic.PreflightReason != nativeForceFallbackEnv+"=1" {
		t.Fatalf("diagnostic preflight_reason = %q, want force fallback reason", result.Diagnostic.PreflightReason)
	}
}

func TestOpenStoreAtForCityNativeOpenFailureFallsBackWithDiagnostic(t *testing.T) {
	scope := "/city"
	fallback := NewMemStore()
	var bdOpened bool

	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		PreflightChecker: factoryPreflightChecker(scope, factoryPreflightDoltMetadata(), contract.PreflightBDContext{Backend: "dolt", DoltMode: "server"}),
		OpenBdStore: func() (Store, error) {
			bdOpened = true
			return fallback, nil
		},
		OpenNativeStore: func() (Store, error) {
			return nil, errors.New("dial native: failed")
		},
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity() error = %v", err)
	}
	if !bdOpened {
		t.Fatal("OpenBdStore was not called after native open failure")
	}
	if result.Store != fallback {
		t.Fatalf("Store = %T, want fallback store", result.Store)
	}
	if result.Diagnostic.Store != storeNameBdStore {
		t.Fatalf("diagnostic store = %q, want %q", result.Diagnostic.Store, storeNameBdStore)
	}
	if result.Diagnostic.NativeStoreEligible {
		t.Fatal("diagnostic native_store_eligible = true, want false")
	}
	if result.Diagnostic.PreflightGate != "native_open" {
		t.Fatalf("diagnostic preflight_gate = %q, want native_open", result.Diagnostic.PreflightGate)
	}
	if !strings.Contains(result.Diagnostic.PreflightReason, "dial native") {
		t.Fatalf("diagnostic preflight_reason = %q, want native open error", result.Diagnostic.PreflightReason)
	}
}

func TestOpenStoreAtForCityExecBdContractFallbackUsesExecStore(t *testing.T) {
	scope := "/city"
	provider := "exec:/tmp/gc-beads-bd.sh"
	execStore := NewMemStore()
	checker := factoryPreflightChecker(scope, factoryPreflightDoltMetadata(), contract.PreflightBDContext{Backend: "dolt", DoltMode: "server"})
	checker.Provider = provider

	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         provider,
		PreflightChecker: checker,
		OpenBdStore: func() (Store, error) {
			t.Fatal("OpenBdStore called for exec bd-contract fallback")
			return nil, nil
		},
		OpenExecStore: func() (Store, error) {
			return execStore, nil
		},
		OpenNativeStore: func() (Store, error) {
			return nil, errors.New("native unavailable")
		},
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity() error = %v", err)
	}
	if result.Store != execStore {
		t.Fatalf("Store = %T, want exec fallback store", result.Store)
	}
	if result.Diagnostic.Store != storeNameExecStore {
		t.Fatalf("diagnostic store = %q, want %q", result.Diagnostic.Store, storeNameExecStore)
	}
	if result.Diagnostic.PreflightGate != "native_open" {
		t.Fatalf("diagnostic preflight_gate = %q, want native_open", result.Diagnostic.PreflightGate)
	}
}

func TestOpenStoreAtForCityExecutableHooksBlockNativeStore(t *testing.T) {
	scope := t.TempDir()
	hooksDir := filepath.Join(scope, ".beads", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "on_create"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fallback := NewMemStore()

	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		PreflightChecker: factoryPreflightChecker(scope, factoryPreflightDoltMetadata(), contract.PreflightBDContext{Backend: "dolt", DoltMode: "server"}),
		OpenBdStore: func() (Store, error) {
			return fallback, nil
		},
		OpenNativeStore: func() (Store, error) {
			t.Fatal("OpenNativeStore called while bd hooks are installed")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity() error = %v", err)
	}
	if result.Store != fallback {
		t.Fatalf("Store = %T, want fallback store", result.Store)
	}
	if result.Diagnostic.Store != storeNameBdStore {
		t.Fatalf("diagnostic store = %q, want %q", result.Diagnostic.Store, storeNameBdStore)
	}
	if result.Diagnostic.PreflightGate != nativeHooksGate {
		t.Fatalf("diagnostic preflight_gate = %q, want %q", result.Diagnostic.PreflightGate, nativeHooksGate)
	}
	if !strings.Contains(result.Diagnostic.PreflightReason, "remove .beads/hooks") {
		t.Fatalf("diagnostic preflight_reason = %q, want operator migration hint", result.Diagnostic.PreflightReason)
	}
}

func TestOpenStoreAtForCityGCStampedHooksDoNotBlockNativeStore(t *testing.T) {
	scope := t.TempDir()
	hooksDir := filepath.Join(scope, ".beads", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gcHookContent := "#!/bin/sh\n# gc-hook-stamp: 2026-01-01 abc123\nbd event emit bead.created\n"
	for _, name := range []string{"on_create", "on_update", "on_close"} {
		if err := os.WriteFile(filepath.Join(hooksDir, name), []byte(gcHookContent), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	native := NewMemStore()

	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		PreflightChecker: factoryPreflightChecker(scope, factoryPreflightDoltMetadata(), contract.PreflightBDContext{Backend: "dolt", DoltMode: "server"}),
		OpenBdStore: func() (Store, error) {
			t.Fatal("OpenBdStore called: gc-stamped hooks should not block native store")
			return nil, nil
		},
		OpenNativeStore: func() (Store, error) {
			return native, nil
		},
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity() error = %v", err)
	}
	if result.Store != native {
		t.Fatalf("Store = %T, want native store; gc-stamped hooks must not block it", result.Store)
	}
	if result.Diagnostic.Store != storeNameNativeDoltStore {
		t.Fatalf("diagnostic store = %q, want %q", result.Diagnostic.Store, storeNameNativeDoltStore)
	}
	if !result.Diagnostic.NativeStoreEligible {
		t.Fatalf("native_store_eligible = false, want true; preflight_gate=%q reason=%q",
			result.Diagnostic.PreflightGate, result.Diagnostic.PreflightReason)
	}
}

func TestOpenStoreAtForCityEmbeddedStampMarkerStillBlocksNativeStore(t *testing.T) {
	scope := t.TempDir()
	hooksDir := filepath.Join(scope, ".beads", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Marker appears only inside an echo body, not as a stamp line.
	hook := "#!/bin/sh\necho \"not really # gc-hook-stamp: spoofed\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(hooksDir, "on_create"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	fallback := NewMemStore()

	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		PreflightChecker: factoryPreflightChecker(scope, factoryPreflightDoltMetadata(), contract.PreflightBDContext{Backend: "dolt", DoltMode: "server"}),
		OpenBdStore:      func() (Store, error) { return fallback, nil },
		OpenNativeStore: func() (Store, error) {
			t.Fatal("OpenNativeStore called for embedded-marker non-gc hook")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity() error = %v", err)
	}
	if result.Store != fallback {
		t.Fatalf("Store = %T, want fallback store", result.Store)
	}
	if result.Diagnostic.PreflightGate != nativeHooksGate {
		t.Fatalf("preflight_gate = %q, want %q", result.Diagnostic.PreflightGate, nativeHooksGate)
	}
}

func factoryPreflightChecker(scope, metadata string, ctx contract.PreflightBDContext) contract.PreflightChecker {
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
			return "gc-local", true, nil
		},
	}
}

func factoryPreflightDoltMetadata() string {
	return `{
		"backend": "dolt",
		"dolt_mode": "server",
		"dolt_database": "gascity",
		"project_id": "gc-local"
	}`
}
