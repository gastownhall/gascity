package beads

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/rollout/gate"

	"github.com/gastownhall/gascity/internal/beads/contract"
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
