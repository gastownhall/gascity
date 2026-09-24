package beads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/rollout/gate"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/fsys"
)

// TestLogNativeUnavailableDowngradesBDContextAgreementToDebug pins the noise
// fix: the bd_context_agreement gate (a benign degrade-not-block check that
// fires on every non-git city root) logs at Debug so it is silent at the
// default Info threshold, while identity_match keeps Error and every other
// gate keeps Warn. The structured BeadsDiagnostic still carries the signal.
func TestLogNativeUnavailableDowngradesBDContextAgreementToDebug(t *testing.T) {
	newLogger := func(buf *bytes.Buffer, level slog.Level) *slog.Logger {
		return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: level}))
	}

	// bd_context_agreement is suppressed at the default Info threshold.
	var buf bytes.Buffer
	logNativeUnavailable(newLogger(&buf, slog.LevelInfo), "scope", string(contract.PreflightCheckBDContextAgreement), "reason")
	if buf.Len() != 0 {
		t.Fatalf("bd_context_agreement at Info threshold should be silent (Debug), got: %q", buf.String())
	}

	// ...but it still emits at Debug threshold, so it is downgraded, not deleted.
	buf.Reset()
	logNativeUnavailable(newLogger(&buf, slog.LevelDebug), "scope", string(contract.PreflightCheckBDContextAgreement), "reason")
	if !strings.Contains(buf.String(), "level=DEBUG") || !strings.Contains(buf.String(), nativeUnavailableMessage) {
		t.Fatalf("bd_context_agreement should log at DEBUG, got: %q", buf.String())
	}

	// identity_match still logs at Error.
	buf.Reset()
	logNativeUnavailable(newLogger(&buf, slog.LevelInfo), "scope", string(contract.PreflightCheckIdentityMatch), "reason")
	if !strings.Contains(buf.String(), "level=ERROR") {
		t.Fatalf("identity_match should log at ERROR, got: %q", buf.String())
	}

	// Any other gate keeps Warn (blast radius is contained to the one gate).
	buf.Reset()
	logNativeUnavailable(newLogger(&buf, slog.LevelInfo), "scope", "force_fallback", "reason")
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("a non-special gate should log at WARN, got: %q", buf.String())
	}
}

func TestOpenStoreAtForCityEligibleNativeReturnsInjectedNativeStore(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
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
	t.Setenv(nativeForceFallbackEnv, "")
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
	t.Setenv(nativeForceFallbackEnv, "")
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
	t.Setenv(nativeForceFallbackEnv, "")
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
	t.Setenv(nativeForceFallbackEnv, "")
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
	t.Setenv(nativeForceFallbackEnv, "")
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

// The proxied binding lives in .beads/metadata.json. bd commits it there and
// writes no dolt.mode into config.yaml at all — its own validator accepts only
// "server"|"embedded" for that key — so a config.yaml carrying
// "proxied-server" is drift, not a topology decision, and must not decide the
// store. Preflight answers instead.
func TestOpenStoreAtForCityConfigMarkerIsNotProxiedAuthority(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	scope := t.TempDir()
	beadsDir := filepath.Join(scope, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("dolt.mode: proxied-server\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	native := NewMemStore()

	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		PreflightChecker: factoryPreflightChecker(scope, factoryPreflightDoltMetadata(), contract.PreflightBDContext{Backend: "dolt", DoltMode: "server"}),
		OpenBdStore: func() (Store, error) {
			t.Fatal("a config.yaml marker alone routed the scope to the bd provider")
			return nil, nil
		},
		OpenNativeStore: func() (Store, error) { return native, nil },
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity() error = %v", err)
	}
	if result.Store != native {
		t.Fatalf("Store = %T, want the preflight-selected native store", result.Store)
	}
	if result.Diagnostic.PreflightGate == BeadsGateProxiedProvider {
		t.Fatal("config.yaml decided the proxied gate; metadata.json is the authority")
	}
}

// Metadata is that authority: the same marker there does route to the bd
// provider under the proxied gate, without ever opening a native store.
func TestOpenStoreAtForCityMetadataProxiedModeBlocksNativeStore(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	scope := t.TempDir()
	beadsDir := filepath.Join(scope, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"),
		[]byte(`{"backend":"dolt","dolt_mode":"proxied-server"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	fallback := NewMemStore()

	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		PreflightChecker: factoryPreflightChecker(scope, factoryPreflightDoltMetadata(), contract.PreflightBDContext{Backend: "dolt", DoltMode: "server"}),
		OpenBdStore:      func() (Store, error) { return fallback, nil },
		OpenNativeStore: func() (Store, error) {
			t.Fatal("OpenNativeStore called for a proxied metadata binding")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity() error = %v", err)
	}
	if result.Store != fallback {
		t.Fatalf("Store = %T, want fallback store", result.Store)
	}
	if result.Diagnostic.PreflightGate != BeadsGateProxiedProvider {
		t.Fatalf("preflight_gate = %q, want proxied_provider", result.Diagnostic.PreflightGate)
	}
}

func TestOpenStoreAtForCityUnknownPersistedDoltModeFallsBack(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	scope := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scope, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, ".beads", "metadata.json"), []byte(`{"backend":"dolt","dolt_mode":"mystery"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	fallback := NewMemStore()
	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		PreflightChecker: factoryPreflightChecker(scope, `{"backend":"dolt","dolt_mode":"mystery","project_id":"gc-local"}`, contract.PreflightBDContext{Backend: "dolt", DoltMode: "server"}),
		OpenBdStore:      func() (Store, error) { return fallback, nil },
		OpenNativeStore: func() (Store, error) {
			t.Fatal("OpenNativeStore called for unknown persisted dolt mode")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity() error = %v", err)
	}
	if result.Store != fallback || result.Diagnostic.PreflightGate != "unsupported_dolt_mode" {
		t.Fatalf("result = (%T, %+v), want fallback with unsupported_dolt_mode", result.Store, result.Diagnostic)
	}
}

func TestOpenStoreAtForCityUnreadableConfigFailsClosed(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	scope := t.TempDir()
	beadsDir := filepath.Join(scope, ".beads")
	if err := os.MkdirAll(filepath.Join(beadsDir, "config.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}
	fallback := NewMemStore()
	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		PreflightChecker: factoryPreflightChecker(scope, factoryPreflightDoltMetadata(), contract.PreflightBDContext{Backend: "dolt", DoltMode: "server"}),
		OpenBdStore:      func() (Store, error) { return fallback, nil },
		OpenNativeStore: func() (Store, error) {
			t.Fatal("OpenNativeStore called with unreadable config")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity() error = %v", err)
	}
	if result.Store != fallback || result.Diagnostic.PreflightGate != "config_unreadable" {
		t.Fatalf("result = (%T, %+v), want fallback with config_unreadable", result.Store, result.Diagnostic)
	}
}

func TestOpenStoreAtForCityUnknownConfigDoltModeFallsBack(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	scope := t.TempDir()
	beadsDir := filepath.Join(scope, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("dolt.mode: mystery\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fallback := NewMemStore()
	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		PreflightChecker: factoryPreflightChecker(scope, factoryPreflightDoltMetadata(), contract.PreflightBDContext{Backend: "dolt", DoltMode: "server"}),
		OpenBdStore:      func() (Store, error) { return fallback, nil },
		OpenNativeStore: func() (Store, error) {
			t.Fatal("OpenNativeStore called for unknown config dolt mode")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity() error = %v", err)
	}
	if result.Store != fallback || result.Diagnostic.PreflightGate != "unsupported_dolt_mode" {
		t.Fatalf("result = (%T, %+v), want fallback with unsupported_dolt_mode", result.Store, result.Diagnostic)
	}
}

func TestOpenStoreAtForCityGCStampedHooksDoNotBlockNativeStore(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
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
	t.Setenv(nativeForceFallbackEnv, "")
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

// TestOpenStoreAtForCityStampsConditionalWritesMode pins §6.3: the factory is
// the ONE home of the conditional-writes mode — every store it opens comes
// back stamped, on every selection path (file, bd fallback, injected native).
func TestOpenStoreAtForCityStampsConditionalWritesMode(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	scope := "/city"

	assertStamped := func(t *testing.T, store Store, wantMode gate.Mode, wantDefaulted bool) {
		t.Helper()
		carrier, ok := store.(conditionalWritesModeCarrier)
		if !ok {
			t.Fatalf("store %T carries no conditional-writes stamp", store)
		}
		mode, defaulted := carrier.conditionalWritesMode()
		if mode != wantMode || defaulted != wantDefaulted {
			t.Fatalf("stamp = (%q, %v), want (%q, %v)", mode, defaulted, wantMode, wantDefaulted)
		}
	}

	t.Run("file path stamps the resolved mode", func(t *testing.T) {
		result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
			ScopeRoot:         scope,
			Provider:          "file",
			ConditionalWrites: gate.Require,
			OpenFileStore:     func() (Store, error) { return NewMemStore(), nil },
		})
		if err != nil {
			t.Fatalf("OpenStoreAtForCity: %v", err)
		}
		assertStamped(t, result.Store, gate.Require, false)
	})

	t.Run("bd fallback path stamps the resolved mode", func(t *testing.T) {
		result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
			ScopeRoot:         scope,
			Provider:          "unknown-provider",
			ConditionalWrites: gate.Auto,
			OpenBdStore: func() (Store, error) {
				return NewBdStore(scope, func(string, string, ...string) ([]byte, error) {
					t.Fatal("no bd subprocess may run during open")
					return nil, nil
				}), nil
			},
		})
		if err != nil {
			t.Fatalf("OpenStoreAtForCity: %v", err)
		}
		assertStamped(t, result.Store, gate.Auto, false)
	})

	t.Run("native path stamps the resolved mode", func(t *testing.T) {
		result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
			ScopeRoot:         scope,
			Provider:          "bd",
			PreflightChecker:  factoryPreflightChecker(scope, factoryPreflightDoltMetadata(), contract.PreflightBDContext{Backend: "dolt", DoltMode: "server"}),
			ConditionalWrites: gate.Auto,
			OpenBdStore: func() (Store, error) {
				t.Fatal("OpenBdStore called for native-eligible scope")
				return nil, nil
			},
			OpenNativeStore: func() (Store, error) { return NewMemStore(), nil },
		})
		if err != nil {
			t.Fatalf("OpenStoreAtForCity: %v", err)
		}
		assertStamped(t, result.Store, gate.Auto, false)
	})

	t.Run("exec-direct path stamps a carrier store", func(t *testing.T) {
		// exec.Store itself is carrier-less, but the stamp wrap must still sit
		// on the exec-direct arm (red-team F7): inject a carrier double.
		result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
			ScopeRoot:         scope,
			Provider:          "exec:custom-tool",
			ConditionalWrites: gate.Auto,
			OpenExecStore:     func() (Store, error) { return NewMemStore(), nil },
		})
		if err != nil {
			t.Fatalf("OpenStoreAtForCity: %v", err)
		}
		assertStamped(t, result.Store, gate.Auto, false)
	})

	t.Run("exec-bd-contract fallback path stamps a carrier store", func(t *testing.T) {
		provider := "exec:/tmp/gc-beads-bd.sh"
		checker := factoryPreflightChecker(scope, factoryPreflightDoltMetadata(), contract.PreflightBDContext{Backend: "dolt", DoltMode: "server"})
		checker.Provider = provider
		result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
			ScopeRoot:         scope,
			Provider:          provider,
			ConditionalWrites: gate.Auto,
			PreflightChecker:  checker,
			OpenExecStore:     func() (Store, error) { return NewMemStore(), nil },
			OpenNativeStore:   func() (Store, error) { return nil, errors.New("native unavailable") },
		})
		if err != nil {
			t.Fatalf("OpenStoreAtForCity: %v", err)
		}
		if result.Diagnostic.Store != storeNameExecStore {
			t.Fatalf("diagnostic store = %q, want the exec fallback arm", result.Diagnostic.Store)
		}
		assertStamped(t, result.Store, gate.Auto, false)
	})

	t.Run("unset maps to off and marks the default", func(t *testing.T) {
		result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
			ScopeRoot:     scope,
			Provider:      "file",
			OpenFileStore: func() (Store, error) { return NewMemStore(), nil },
		})
		if err != nil {
			t.Fatalf("OpenStoreAtForCity: %v", err)
		}
		assertStamped(t, result.Store, gate.Off, true)
		w, diag, resolveErr := ResolveConditionalWriter(result.Store)
		if w != nil || diag != nil || resolveErr != nil {
			t.Fatal("defaulted-off store must resolve to the legacy path")
		}
	})

	t.Run("require refuses a carrier-less store at open", func(t *testing.T) {
		bare := &struct{ Store }{Store: NewMemStore()}
		_, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
			ScopeRoot:         scope,
			Provider:          "file",
			ConditionalWrites: gate.Require,
			OpenFileStore:     func() (Store, error) { return bare, nil },
		})
		if !IsConditionalWritesRequired(err) {
			t.Fatalf("err = %v, want the typed require refusal: a store that cannot carry the mode must never be handed to a caller whose config promises fencing", err)
		}
	})

	t.Run("auto degrades a carrier-less store loudly at open", func(t *testing.T) {
		bare := &struct{ Store }{Store: NewMemStore()}
		var degraded []ConditionalWritesDegrade
		result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
			ScopeRoot:                   scope,
			Provider:                    "file",
			ConditionalWrites:           gate.Auto,
			OpenFileStore:               func() (Store, error) { return bare, nil },
			OnConditionalWritesDegraded: func(d ConditionalWritesDegrade) { degraded = append(degraded, d) },
		})
		if err != nil {
			t.Fatalf("OpenStoreAtForCity: %v", err)
		}
		if result.Store != Store(bare) {
			t.Fatalf("store = %T, want the carrier-less store returned under auto", result.Store)
		}
		if len(degraded) != 1 || degraded[0].Mode != "auto" {
			t.Fatalf("degrade notifications = %+v, want exactly one auto degrade at open", degraded)
		}
		// The seam then takes the legacy path — degraded, never fenced.
		if w, diag, resolveErr := ResolveConditionalWriter(result.Store); w != nil || diag != nil || resolveErr != nil {
			t.Fatal("carrier-less store must resolve to the legacy path under auto")
		}
	})

	t.Run("off leaves a carrier-less store silent", func(t *testing.T) {
		bare := &struct{ Store }{Store: NewMemStore()}
		result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
			ScopeRoot:         scope,
			Provider:          "file",
			ConditionalWrites: gate.Off,
			OpenFileStore:     func() (Store, error) { return bare, nil },
		})
		if err != nil || result.Store != Store(bare) {
			t.Fatalf("off over carrier-less = (%T, %v), want the store as-is", result.Store, err)
		}
	})

	t.Run("open error does not stamp", func(t *testing.T) {
		_, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
			ScopeRoot:         scope,
			Provider:          "file",
			ConditionalWrites: gate.Require,
			OpenFileStore:     func() (Store, error) { return nil, errors.New("boom") },
		})
		if err == nil {
			t.Fatal("want open error to propagate")
		}
	})
}

// TestOpenStoreAtForCityNilPreflightCheckerFallsBackToBd pins the control-
// plane routing contract: a caller that supplies no PreflightChecker can
// never be given the native store — the factory treats the missing checker
// as preflight-unavailable and takes the bd fallback, stamping it like every
// other path. (The control dispatcher routes its raw bd store through the
// factory this way; native selection there would be a behavior change.)
func TestOpenStoreAtForCityNilPreflightCheckerFallsBackToBd(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	bd := NewMemStore()
	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:         "/city",
		Provider:          "bd",
		ConditionalWrites: gate.Require,
		OpenBdStore:       func() (Store, error) { return bd, nil },
		OpenNativeStore: func() (Store, error) {
			t.Fatal("native store must never open without a preflight checker")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity: %v", err)
	}
	if result.Store != Store(bd) {
		t.Fatalf("store = %T, want the bd fallback store", result.Store)
	}
	if result.Diagnostic.PreflightGate != "preflight_unavailable" {
		t.Fatalf("PreflightGate = %q, want preflight_unavailable", result.Diagnostic.PreflightGate)
	}
	carrier, ok := result.Store.(conditionalWritesModeCarrier)
	if !ok {
		t.Fatal("fallback store carries no stamp")
	}
	if mode, _ := carrier.conditionalWritesMode(); mode != gate.Require {
		t.Fatalf("stamped mode = %q, want require", mode)
	}
}

// proxiedScopeFixture writes the metadata.json that makes a scope
// proxied-server: the only authority persistedDoltModeRefusal accepts for that
// topology.
func proxiedScopeFixture(t *testing.T) string {
	t.Helper()
	scope := t.TempDir()
	beadsDir := filepath.Join(scope, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"),
		[]byte(`{"backend":"dolt","dolt_mode":"proxied-server"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return scope
}

// refusingPreflightChecker fails the test if preflight runs at all.
//
// Preflight must never be reached on a proxied scope, in EITHER flag lane. Its
// bd-context probe is a fork per open, and `bd context` starts a stopped proxy
// -- so a diagnostic path that ran here would both cost the fork the lane
// exists to remove and change the city it was asked to describe.
func refusingPreflightChecker(t *testing.T) contract.PreflightChecker {
	t.Helper()
	return contract.PreflightChecker{
		FS:                  fsys.NewFake(),
		Provider:            "bd",
		BeadsLibraryVersion: "1.0.4",
		BDContext: func(string) (contract.PreflightBDContext, error) {
			t.Error("preflight ran on a proxied scope: that is a bd fork per open, and bd context restarts a stopped proxy")
			return contract.PreflightBDContext{}, nil
		},
		DatabaseProjectID: func(string) (string, bool, error) {
			t.Error("preflight read the database project id on a proxied scope")
			return "", false, nil
		},
	}
}

func proxiedOpenReportFixture() ProxiedOpenReport {
	return ProxiedOpenReport{
		Endpoint:   ProxiedEndpointStamp{Port: 44561, PID: 6001, Generation: "6001:44556677"},
		Evidence:   "argv+birth",
		IdlePolicy: "never",
		Cursors:    proxyendpoint.Cursors{Main: SchemaCursorMain, Ignored: SchemaCursorIgnored},
	}
}

// TestOpenStoreAtForCityProxiedFlagOffKeepsProviderGate is the rollout fence.
//
// With GC_BEADS_PROXIED_NATIVE unset, a proxied scope must take the path it
// takes today, down to the serialized bytes: the same store, the same gate, the
// same reason, and NO new field. `gc doctor --json` and the topology matrix
// both assert on this struct, so "flag off is byte-identical" is checked as
// bytes rather than field by field -- a field-by-field check passes a struct
// that grew a `"proxied":null`.
func TestOpenStoreAtForCityProxiedFlagOffKeepsProviderGate(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	t.Setenv(proxiedNativeEnv, "")
	scope := proxiedScopeFixture(t)
	fallback := NewMemStore()

	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		PreflightChecker: refusingPreflightChecker(t),
		OpenBdStore:      func() (Store, error) { return fallback, nil },
		LongLived:        true,
		OpenProxiedStore: func(context.Context, bool) (Store, ProxiedOpenReport, error) {
			t.Fatal("the proxied opener ran with GC_BEADS_PROXIED_NATIVE unset")
			return nil, ProxiedOpenReport{}, nil
		},
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity: %v", err)
	}
	if result.Store != Store(fallback) {
		t.Fatalf("Store = %T, want the bd fallback", result.Store)
	}
	if result.Diagnostic.Proxied != nil {
		t.Errorf("flag-off diagnostic carries a proxied account: %+v", result.Diagnostic.Proxied)
	}

	encoded, err := json.Marshal(result.Diagnostic)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"beads_store":"BdStore","native_store_eligible":false,"preflight_gate":"proxied_provider","preflight_reason":"proxied-server mode is owned by the bd provider"}`
	if string(encoded) != want {
		t.Fatalf("flag-off diagnostic JSON =\n  %s\nwant\n  %s\nThe proxied lane may ADD an omitempty field; it may never change what a flag-off open serializes.",
			encoded, want)
	}
}

// TestOpenStoreAtForCityProxiedFlagOnUsesProxiedOpenerAndReportsNativeDoltStore
// is the arm itself.
//
// The store name is the part worth pinning: the flag-on lane reports
// NativeDoltStore, not a third name. What a caller gets back IS a native
// handle's reads, and inventing "ProxiedStore" on the wire would break every
// consumer that already knows exactly two store names.
func TestOpenStoreAtForCityProxiedFlagOnUsesProxiedOpenerAndReportsNativeDoltStore(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	t.Setenv(proxiedNativeEnv, "1")
	scope := proxiedScopeFixture(t)
	proxied := NewMemStore()

	calls, gotLongLived := 0, false
	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		PreflightChecker: refusingPreflightChecker(t),
		LongLived:        true,
		OpenBdStore: func() (Store, error) {
			t.Fatal("the bd fallback opened on a healthy proxied native open")
			return nil, nil
		},
		OpenNativeStore: func() (Store, error) {
			t.Fatal("the DIRECT native opener ran for a proxied scope")
			return nil, nil
		},
		OpenProxiedStore: func(_ context.Context, longLived bool) (Store, ProxiedOpenReport, error) {
			calls++
			gotLongLived = longLived
			return proxied, proxiedOpenReportFixture(), nil
		},
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity: %v", err)
	}
	if calls != 1 {
		t.Fatalf("the proxied opener ran %d times, want exactly 1", calls)
	}
	if !gotLongLived {
		t.Error("LongLived did not reach the opener; the idle-policy rule turns on it")
	}
	if result.Store != Store(proxied) {
		t.Fatalf("Store = %T, want the proxied store", result.Store)
	}
	if result.Diagnostic.Store != BeadsStoreNameNativeDoltStore {
		t.Errorf("beads_store = %q, want %q", result.Diagnostic.Store, BeadsStoreNameNativeDoltStore)
	}
	if !result.Diagnostic.NativeStoreEligible {
		t.Error("native_store_eligible = false on a served proxied native open")
	}
	if result.Diagnostic.PreflightGate != "" {
		t.Errorf("preflight_gate = %q, want empty: nothing refused this open", result.Diagnostic.PreflightGate)
	}
	proxiedDiag := result.Diagnostic.Proxied
	if proxiedDiag == nil {
		t.Fatal("a served proxied open reported no proxied account")
	}
	if proxiedDiag.Verdict != ProxiedVerdictNone {
		t.Errorf("verdict = %q on a served open, want empty", proxiedDiag.Verdict)
	}
	if proxiedDiag.Endpoint.Generation != "6001:44556677" || proxiedDiag.Endpoint.Port != 44561 {
		t.Errorf("endpoint = %+v, want the pinned generation", proxiedDiag.Endpoint)
	}
	if proxiedDiag.Cursors.Main != SchemaCursorMain || proxiedDiag.IdlePolicy != "never" {
		t.Errorf("proxied account = %+v, want the probed cursors and idle policy", proxiedDiag)
	}
	// The stamp still reaches the store: the proxied arm funnels through
	// stampedResult like every other selection path.
	carrier, ok := result.Store.(conditionalWritesModeCarrier)
	if !ok {
		t.Fatal("the proxied store carries no conditional-writes stamp")
	}
	if _, defaulted := carrier.conditionalWritesMode(); !defaulted {
		t.Error("an unthreaded conditional-writes mode did not default on the proxied arm")
	}
}

// TestOpenStoreAtForCityProxiedVerdictFallsBackKeepingTheGate pins the healthy
// fallback.
//
// A refusal is an expected outcome, not a degradation, so the scope takes the
// SAME bd front door under the SAME proxied_provider gate it takes today --
// internal/doctor keys its proxied branch on that exact value, and renaming it
// for a lane the operator may not even have enabled would break a matcher for
// everybody. The verdict rides in the additive field.
func TestOpenStoreAtForCityProxiedVerdictFallsBackKeepingTheGate(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	t.Setenv(proxiedNativeEnv, "true")
	scope := proxiedScopeFixture(t)
	fallback := NewMemStore()

	report := proxiedOpenReportFixture()
	report.Cursors = proxyendpoint.Cursors{Main: SchemaCursorMain + 1, Ignored: SchemaCursorIgnored}
	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		PreflightChecker: refusingPreflightChecker(t),
		OpenBdStore:      func() (Store, error) { return fallback, nil },
		OpenProxiedStore: func(context.Context, bool) (Store, ProxiedOpenReport, error) {
			return nil, report, NewSchemaSkewVerdictError(ProxiedSkewLaneMain, ProxiedSkewDirAhead, "main=67 pinned=66")
		},
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity: %v", err)
	}
	if result.Store != Store(fallback) {
		t.Fatalf("Store = %T, want the bd fallback", result.Store)
	}
	if result.Diagnostic.Store != BeadsStoreNameBdStore {
		t.Errorf("beads_store = %q, want BdStore", result.Diagnostic.Store)
	}
	if result.Diagnostic.PreflightGate != BeadsGateProxiedProvider {
		t.Fatalf("preflight_gate = %q, want proxied_provider unchanged: internal/doctor matches on it", result.Diagnostic.PreflightGate)
	}
	if result.Diagnostic.PreflightReason != "proxied-server mode is owned by the bd provider" {
		t.Errorf("preflight_reason = %q, want today's text unchanged", result.Diagnostic.PreflightReason)
	}
	proxiedDiag := result.Diagnostic.Proxied
	if proxiedDiag == nil {
		t.Fatal("a refused proxied open reported no verdict")
	}
	if proxiedDiag.Verdict != ProxiedVerdictSchemaSkew {
		t.Errorf("verdict = %q, want schema_skew", proxiedDiag.Verdict)
	}
	if proxiedDiag.Detail != "" {
		t.Errorf("detail = %q on a typed verdict; the verdict IS the explanation", proxiedDiag.Detail)
	}
	// The refusal still carries what it saw, which is the difference between a
	// diagnostic and a mystery.
	if proxiedDiag.Cursors.Main != SchemaCursorMain+1 || proxiedDiag.Endpoint.Port != 44561 {
		t.Errorf("proxied account = %+v, want the cursors and endpoint the refusal observed", proxiedDiag)
	}
}

// TestOpenStoreAtForCityProxiedUntypedErrorFallsBackLoudly separates the two
// failure shapes. Admission is supposed to name every outcome, so an untyped
// error is a bug or an unhandled case. The city still gets a store -- an
// operator who turned on a rollout flag must not lose a city to it -- but the
// text is preserved and the fallback is logged, where a typed verdict is
// deliberately silent.
func TestOpenStoreAtForCityProxiedUntypedErrorFallsBackLoudly(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	t.Setenv(proxiedNativeEnv, "1")
	scope := proxiedScopeFixture(t)
	fallback := NewMemStore()

	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		Logger:           logger,
		PreflightChecker: refusingPreflightChecker(t),
		OpenBdStore:      func() (Store, error) { return fallback, nil },
		OpenProxiedStore: func(context.Context, bool) (Store, ProxiedOpenReport, error) {
			return nil, ProxiedOpenReport{}, errors.New("nobody classified this")
		},
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity: %v", err)
	}
	if result.Store != Store(fallback) {
		t.Fatalf("Store = %T, want the bd fallback", result.Store)
	}
	if result.Diagnostic.Proxied == nil || result.Diagnostic.Proxied.Detail != "nobody classified this" {
		t.Fatalf("proxied account = %+v, want the untyped error text preserved", result.Diagnostic.Proxied)
	}
	if result.Diagnostic.Proxied.Verdict != ProxiedVerdictNone {
		t.Errorf("verdict = %q, want empty: nothing classified this failure", result.Diagnostic.Proxied.Verdict)
	}
	if !strings.Contains(logged.String(), "nobody classified this") {
		t.Errorf("an unclassified proxied failure was not logged:\n%s", logged.String())
	}
}

// TestOpenStoreAtForCityProxiedHeadMovedIsLoud is council pr2 D-F3's factory
// half. Every other verdict is an expected refusal and is deliberately quiet;
// head_moved is not a refusal but an incident — HEAD moved across gc's own
// library open, which may be gc having committed to bd's database — so it must
// reach the operator at WARN with the hashes, and the diagnostic must keep the
// detail the verdict name alone does not carry. The draining row beside it is
// the control: loudness is specific to the incident, not a new default.
func TestOpenStoreAtForCityProxiedHeadMovedIsLoud(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantLoud   bool
		wantDetail string
	}{
		{
			name:       "head_moved is logged with both hashes",
			err:        NewProxiedVerdictError(ProxiedVerdictHeadMoved, "moved HEAD from before0000 to after11111", nil),
			wantLoud:   true,
			wantDetail: "moved HEAD from before0000 to after11111",
		},
		{
			name: "an expected refusal stays quiet",
			err:  NewProxiedVerdictError(ProxiedVerdictDraining, "the proxy is still refusing", nil),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(nativeForceFallbackEnv, "")
			t.Setenv(proxiedNativeEnv, "1")
			scope := proxiedScopeFixture(t)
			fallback := NewMemStore()

			var logged bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
			result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
				ScopeRoot:        scope,
				Provider:         "bd",
				Logger:           logger,
				PreflightChecker: refusingPreflightChecker(t),
				OpenBdStore:      func() (Store, error) { return fallback, nil },
				OpenProxiedStore: func(context.Context, bool) (Store, ProxiedOpenReport, error) {
					return nil, proxiedOpenReportFixture(), tc.err
				},
			})
			if err != nil {
				t.Fatalf("OpenStoreAtForCity: %v", err)
			}
			if result.Store != Store(fallback) || result.Diagnostic.PreflightGate != BeadsGateProxiedProvider {
				t.Fatalf("result = (%T, %+v), want the bd fallback under proxied_provider", result.Store, result.Diagnostic)
			}
			if result.Diagnostic.Proxied == nil {
				t.Fatal("the refusal reported no proxied account")
			}
			if got := result.Diagnostic.Proxied.Detail; got != tc.wantDetail {
				t.Errorf("detail = %q, want %q", got, tc.wantDetail)
			}
			loud := strings.Contains(logged.String(), "level=WARN") && strings.Contains(logged.String(), "head_moved")
			if loud != tc.wantLoud {
				t.Fatalf("logged loudly = %v, want %v:\n%s", loud, tc.wantLoud, logged.String())
			}
			if tc.wantLoud && !strings.Contains(logged.String(), "after11111") {
				t.Errorf("the WARN line omits the hash an operator needs:\n%s", logged.String())
			}
		})
	}
}

// TestOpenStoreAtForCityProxiedForceFallbackWinsOverTheFlag pins the escape
// hatch's precedence. GC_BEADS_FORCE_FALLBACK is checked before the persisted
// topology is even read, so an operator turning it on gets BdStore on a box
// where the proxied lane is enabled -- which is the entire value of an escape
// hatch.
func TestOpenStoreAtForCityProxiedForceFallbackWinsOverTheFlag(t *testing.T) {
	t.Setenv(proxiedNativeEnv, "1")
	t.Setenv(nativeForceFallbackEnv, "1")
	scope := proxiedScopeFixture(t)
	fallback := NewMemStore()

	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		PreflightChecker: refusingPreflightChecker(t),
		OpenBdStore:      func() (Store, error) { return fallback, nil },
		OpenProxiedStore: func(context.Context, bool) (Store, ProxiedOpenReport, error) {
			t.Fatal("the proxied opener ran with GC_BEADS_FORCE_FALLBACK=1")
			return nil, ProxiedOpenReport{}, nil
		},
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity: %v", err)
	}
	if result.Store != Store(fallback) {
		t.Fatalf("Store = %T, want the bd fallback", result.Store)
	}
	if result.Diagnostic.PreflightGate != nativeForceFallbackGate {
		t.Fatalf("preflight_gate = %q, want force_fallback", result.Diagnostic.PreflightGate)
	}
	if result.Diagnostic.Proxied != nil {
		t.Errorf("the force-fallback arm reported a proxied account: %+v", result.Diagnostic.Proxied)
	}
}

// TestOpenStoreAtForCityProxiedFlagOnWithoutAnOpenerFallsBack covers the
// binary that has the flag but not the wiring: the lane needs BOTH the flag
// and a composition root that supplied an opener, so a partially-rolled-out
// build behaves exactly like flag-off.
func TestOpenStoreAtForCityProxiedFlagOnWithoutAnOpenerFallsBack(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	t.Setenv(proxiedNativeEnv, "1")
	scope := proxiedScopeFixture(t)
	fallback := NewMemStore()

	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		PreflightChecker: refusingPreflightChecker(t),
		OpenBdStore:      func() (Store, error) { return fallback, nil },
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity: %v", err)
	}
	if result.Store != Store(fallback) || result.Diagnostic.PreflightGate != BeadsGateProxiedProvider {
		t.Fatalf("result = (%T, %+v), want the bd fallback under proxied_provider", result.Store, result.Diagnostic)
	}
	if result.Diagnostic.Proxied != nil {
		t.Errorf("an unwired binary reported a proxied account: %+v", result.Diagnostic.Proxied)
	}
}

// TestOpenStoreAtForCityProxiedFlagOnLeavesNonProxiedScopesAlone is the unit
// fence for the matrix's "one regression shape" (round3 review, completeness).
//
// The rollout flag must change what a PROXIED scope opens and nothing else:
// with it on, a direct, embedded, legacy or unsupported scope must open exactly
// the store — and report exactly the diagnostic — it opens with the flag off,
// and the proxied opener must never be consulted for it. The acceptance matrix
// was meant to pin that (M2's native-lane and every non-proxied shape's
// payload), but CI runs only M1 of it, and every flag-on unit test used a
// proxied fixture, so a factory that consulted the proxied lane for every
// scope passed the whole package. Each row runs both lanes over the same
// inputs and compares them.
func TestOpenStoreAtForCityProxiedFlagOnLeavesNonProxiedScopesAlone(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	serverCtx := contract.PreflightBDContext{Backend: "dolt", DoltMode: "server"}

	for _, tc := range []struct {
		name     string
		metadata string // .beads/metadata.json on disk; "" writes none
		config   string // .beads/config.yaml on disk; "" writes none
		provider string
		// preflight builds the checker for the scope; nil means the row must
		// not reach preflight at all.
		preflight func(scope string) contract.PreflightChecker
		wantStore string
	}{
		{
			name:     "a direct server-mode scope, native eligible",
			metadata: `{"backend":"dolt","dolt_mode":"server"}`,
			provider: "bd",
			preflight: func(scope string) contract.PreflightChecker {
				return factoryPreflightChecker(scope, factoryPreflightDoltMetadata(), serverCtx)
			},
			wantStore: storeNameNativeDoltStore,
		},
		{
			name:     "an embedded scope",
			metadata: `{"backend":"dolt","dolt_mode":"embedded"}`,
			provider: "bd",
			preflight: func(scope string) contract.PreflightChecker {
				return factoryPreflightChecker(scope, factoryPreflightDoltMetadata(), serverCtx)
			},
			wantStore: storeNameNativeDoltStore,
		},
		{
			name:     "a scope with no persisted mode (preflight decides)",
			provider: "bd",
			preflight: func(scope string) contract.PreflightChecker {
				return factoryPreflightChecker(scope, factoryPreflightDoltMetadata(), serverCtx)
			},
			wantStore: storeNameNativeDoltStore,
		},
		{
			name:     "a legacy config.yaml proxied-server marker, which is drift and not authority",
			config:   "dolt.mode: proxied-server\n",
			provider: "bd",
			preflight: func(scope string) contract.PreflightChecker {
				return factoryPreflightChecker(scope, factoryPreflightDoltMetadata(), serverCtx)
			},
			wantStore: storeNameNativeDoltStore,
		},
		{
			name:     "a scope whose preflight refuses (bd context drift)",
			metadata: `{"backend":"dolt","dolt_mode":"server"}`,
			provider: "bd",
			preflight: func(scope string) contract.PreflightChecker {
				return factoryPreflightChecker(scope, factoryPreflightDoltMetadata(),
					contract.PreflightBDContext{Backend: "dolt", DoltMode: "embedded"})
			},
			wantStore: storeNameBdStore,
		},
		{
			name:      "an unsupported persisted dolt_mode",
			metadata:  `{"backend":"dolt","dolt_mode":"mystery"}`,
			provider:  "bd",
			wantStore: storeNameBdStore,
		},
		{
			name:      "a provider off the bd contract",
			metadata:  `{"backend":"dolt","dolt_mode":"server"}`,
			provider:  "unknown",
			wantStore: storeNameBdStore,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := t.TempDir()
			beadsDir := filepath.Join(scope, ".beads")
			if err := os.MkdirAll(beadsDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.metadata != "" {
				if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(tc.metadata), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.config != "" {
				if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(tc.config), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			native, fallback := NewMemStore(), NewMemStore()

			open := func(t *testing.T, flag string) StoreOpenResult {
				t.Helper()
				t.Setenv(proxiedNativeEnv, flag)
				checker := contract.PreflightChecker{
					FS: fsys.NewFake(),
					BDContext: func(string) (contract.PreflightBDContext, error) {
						t.Error("preflight ran for a row that decides before it")
						return contract.PreflightBDContext{}, nil
					},
				}
				if tc.preflight != nil {
					checker = tc.preflight(scope)
				}
				result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
					ScopeRoot:        scope,
					Provider:         tc.provider,
					PreflightChecker: checker,
					LongLived:        true,
					OpenBdStore:      func() (Store, error) { return fallback, nil },
					OpenNativeStore:  func() (Store, error) { return native, nil },
					OpenProxiedStore: func(context.Context, bool) (Store, ProxiedOpenReport, error) {
						t.Fatalf("GC_BEADS_PROXIED_NATIVE=%q consulted the proxied opener for a scope that is not proxied-server", flag)
						return nil, ProxiedOpenReport{}, nil
					},
				})
				if err != nil {
					t.Fatalf("OpenStoreAtForCity (flag %q): %v", flag, err)
				}
				return result
			}

			off := open(t, "")
			on := open(t, "1")
			if off.Diagnostic.Store != tc.wantStore {
				t.Fatalf("flag-off store = %q, want %q: the row does not exercise the shape it names (%+v)",
					off.Diagnostic.Store, tc.wantStore, off.Diagnostic)
			}
			if on.Store != off.Store {
				t.Fatalf("flag on opened %T, flag off %T: the flag changed a non-proxied scope's store", on.Store, off.Store)
			}
			offJSON, err := json.Marshal(off.Diagnostic)
			if err != nil {
				t.Fatal(err)
			}
			onJSON, err := json.Marshal(on.Diagnostic)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(onJSON, offJSON) {
				t.Fatalf("flag-on diagnostic =\n  %s\nflag-off =\n  %s\nthe flag changed what a non-proxied scope reports", onJSON, offJSON)
			}
		})
	}
}
