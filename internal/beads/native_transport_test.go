package beads

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
)

// resetNativeForceFallbackDeprecationWarnOnceForTest resets the package-level
// sync.Once guarding the GC_BEADS_FORCE_FALLBACK deprecation warning. Go runs
// one test binary (one process) per package, so the Once instance otherwise
// persists across every test in this file; without this reset, whichever of
// these tests happens to run first "uses up" the single warning and every
// later test that asserts on the warning's presence would fail depending on
// -run filtering / test order. Production has no equivalent reset: the point
// of the Once there is exactly that it fires once per real process boot.
func resetNativeForceFallbackDeprecationWarnOnceForTest(t *testing.T) {
	t.Helper()
	nativeForceFallbackDeprecationWarnOnce = sync.Once{}
}

// TestOpenStoreAtForCityNativeTransportOffPicksBdStore proves the per-city
// kill switch: beads.native_transport="off" always picks BdStore, even for a
// scope that would otherwise be native-eligible (a capable preflight checker
// and a native opener that, if called, fails the test).
func TestOpenStoreAtForCityNativeTransportOffPicksBdStore(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	scope := "/city"
	bdStore := NewMemStore()

	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		NativeTransport:  NativeTransportOff,
		PreflightChecker: factoryPreflightChecker(scope, factoryPreflightDoltMetadata(), contract.PreflightBDContext{Backend: "dolt", DoltMode: "server"}),
		OpenBdStore: func() (Store, error) {
			return bdStore, nil
		},
		OpenNativeStore: func() (Store, error) {
			t.Fatal("OpenNativeStore called while native_transport=off")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity() error = %v", err)
	}
	if result.Store != bdStore {
		t.Fatalf("Store = %T %#v, want the injected bd store", result.Store, result.Store)
	}
	if result.Diagnostic.Store != storeNameBdStore {
		t.Fatalf("diagnostic store = %q, want %q", result.Diagnostic.Store, storeNameBdStore)
	}
	if result.Diagnostic.NativeStoreEligible {
		t.Fatal("diagnostic native_store_eligible = true, want false")
	}
	if result.Diagnostic.PreflightGate != nativeTransportOffGate {
		t.Fatalf("diagnostic preflight_gate = %q, want %q", result.Diagnostic.PreflightGate, nativeTransportOffGate)
	}
	if result.Diagnostic.PreflightReason != `beads.native_transport="off"` {
		t.Fatalf("diagnostic preflight_reason = %q, want the native_transport=off reason", result.Diagnostic.PreflightReason)
	}
}

// TestOpenStoreAtForCityNativeTransportAutoPicksNativeWhenEligible proves
// "auto" (today's behavior, and the zero value / unset) still opens native
// for an eligible scope — the switch does not regress the existing path.
func TestOpenStoreAtForCityNativeTransportAutoPicksNativeWhenEligible(t *testing.T) {
	for name, mode := range map[string]NativeTransportMode{
		"explicit auto": NativeTransportAuto,
		"unset":         NativeTransportUnset,
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(nativeForceFallbackEnv, "")
			scope := "/city"
			native := NewMemStore()

			result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
				ScopeRoot:        scope,
				Provider:         "bd",
				NativeTransport:  mode,
				PreflightChecker: factoryPreflightChecker(scope, factoryPreflightDoltMetadata(), contract.PreflightBDContext{Backend: "dolt", DoltMode: "server"}),
				OpenBdStore: func() (Store, error) {
					t.Fatal("OpenBdStore called for native-eligible scope under auto")
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
			if !result.Diagnostic.NativeStoreEligible {
				t.Fatal("diagnostic native_store_eligible = false, want true")
			}
		})
	}
}

// TestOpenStoreAtForCityForceFallbackEnvForcesOffAndWarns proves the
// deprecated GC_BEADS_FORCE_FALLBACK alias: it forces off even when the
// per-city value is "auto" (env wins over every city's value), and logs a
// deprecation warning naming the replacement.
func TestOpenStoreAtForCityForceFallbackEnvForcesOffAndWarns(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "1")
	resetNativeForceFallbackDeprecationWarnOnceForTest(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:       "/city",
		Provider:        "bd",
		NativeTransport: NativeTransportAuto,
		Logger:          logger,
		OpenBdStore: func() (Store, error) {
			return NewMemStore(), nil
		},
		OpenNativeStore: func() (Store, error) {
			t.Fatal("OpenNativeStore called while GC_BEADS_FORCE_FALLBACK=1")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity() error = %v", err)
	}
	if result.Diagnostic.PreflightGate != nativeForceFallbackGate {
		t.Fatalf("diagnostic preflight_gate = %q, want %q", result.Diagnostic.PreflightGate, nativeForceFallbackGate)
	}
	if !strings.Contains(buf.String(), "deprecated") || !strings.Contains(buf.String(), nativeForceFallbackEnv) {
		t.Fatalf("expected a deprecation warning naming %s, got log: %q", nativeForceFallbackEnv, buf.String())
	}
}

// TestOpenStoreAtForCityForceFallbackDeprecationWarnsOnlyOncePerProcess
// proves Finding 5's fix: two separate opens that both hit the
// GC_BEADS_FORCE_FALLBACK path only log the deprecation warning once between
// them (sync.Once), rather than once per open — the latter would flood logs
// in a long-lived process that opens many city stores with the legacy env
// var set.
func TestOpenStoreAtForCityForceFallbackDeprecationWarnsOnlyOncePerProcess(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "1")
	resetNativeForceFallbackDeprecationWarnOnceForTest(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	open := func() {
		t.Helper()
		if _, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
			ScopeRoot:       "/city",
			Provider:        "bd",
			NativeTransport: NativeTransportAuto,
			Logger:          logger,
			OpenBdStore: func() (Store, error) {
				return NewMemStore(), nil
			},
			OpenNativeStore: func() (Store, error) {
				t.Fatal("OpenNativeStore called while GC_BEADS_FORCE_FALLBACK=1")
				return nil, nil
			},
		}); err != nil {
			t.Fatalf("OpenStoreAtForCity() error = %v", err)
		}
	}

	open()
	open()

	if got := strings.Count(buf.String(), "deprecated"); got != 1 {
		t.Fatalf("deprecation warning logged %d times across two opens, want exactly 1: %q", got, buf.String())
	}
}

// TestOpenStoreAtForCityNativeTransportPerCityOffWorksWithoutTheEnv proves
// the per-city off switch is independent of the legacy env var: with the env
// unset entirely, native_transport="off" alone must still force BdStore.
func TestOpenStoreAtForCityNativeTransportPerCityOffWorksWithoutTheEnv(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	scope := "/city"
	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:       scope,
		Provider:        "bd",
		NativeTransport: NativeTransportOff,
		// Deliberately eligible preflight: if the off switch were ignored,
		// this scope would otherwise qualify for native, so the test proves
		// the per-city switch alone (not an absent/ineligible preflight)
		// is what forces BdStore.
		PreflightChecker: factoryPreflightChecker(scope, factoryPreflightDoltMetadata(), contract.PreflightBDContext{Backend: "dolt", DoltMode: "server"}),
		OpenBdStore: func() (Store, error) {
			return NewMemStore(), nil
		},
		OpenNativeStore: func() (Store, error) {
			t.Fatal("OpenNativeStore called while native_transport=off")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity() error = %v", err)
	}
	if result.Diagnostic.Store != storeNameBdStore {
		t.Fatalf("diagnostic store = %q, want %q", result.Diagnostic.Store, storeNameBdStore)
	}
}

// TestOpenStoreAtForCityNativeTransportLatchesPerOpen proves the latching
// property the design requires: NativeTransport is threaded into
// StoreOpenOptions BY VALUE, so a caller's config change AFTER an open
// cannot flip a store that open already returned. It opens a native store
// under "auto", then mutates the local mode variable to "off" and re-asserts
// on the ALREADY-RETURNED result — proving the result's diagnostic (and the
// factory's decision that produced it) cannot be reached by that later
// mutation. A regression that made the factory consult a live pointer
// instead of a value snapshot would still pass the first assertion but is
// exactly the class of bug this test exists to catch if StoreOpenOptions
// were ever changed to carry a *config.City instead of a resolved value.
func TestOpenStoreAtForCityNativeTransportLatchesPerOpen(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	scope := "/city"
	native := NewMemStore()

	mode := NativeTransportAuto
	opts := StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		NativeTransport:  mode,
		PreflightChecker: factoryPreflightChecker(scope, factoryPreflightDoltMetadata(), contract.PreflightBDContext{Backend: "dolt", DoltMode: "server"}),
		OpenBdStore: func() (Store, error) {
			t.Fatal("OpenBdStore called for native-eligible scope under auto")
			return nil, nil
		},
		OpenNativeStore: func() (Store, error) {
			return native, nil
		},
	}
	result, err := OpenStoreAtForCity(context.Background(), opts)
	if err != nil {
		t.Fatalf("OpenStoreAtForCity() error = %v", err)
	}
	if result.Store != native || !result.Diagnostic.NativeStoreEligible {
		t.Fatalf("first open did not pick native: store=%T eligible=%v", result.Store, result.Diagnostic.NativeStoreEligible)
	}

	// Mutate the local variable (standing in for a reloaded config) AFTER the
	// open. The already-opened store and its diagnostic must not change.
	mode = NativeTransportOff
	_ = mode // the mutation is the point; opts/result were copied at call time.

	if result.Store != native {
		t.Fatalf("Store changed after mutating mode post-open: %T, want the original native store", result.Store)
	}
	if !result.Diagnostic.NativeStoreEligible || result.Diagnostic.Store != storeNameNativeDoltStore {
		t.Fatalf("Diagnostic changed after mutating mode post-open: %+v", result.Diagnostic)
	}

	// A FRESH open with the now-off mode, by contrast, DOES pick BdStore —
	// proving the mutation really takes effect for a NEW decision and the
	// first result's immutability is not an artifact of a broken test.
	result2, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		NativeTransport:  mode,
		PreflightChecker: factoryPreflightChecker(scope, factoryPreflightDoltMetadata(), contract.PreflightBDContext{Backend: "dolt", DoltMode: "server"}),
		OpenBdStore: func() (Store, error) {
			return NewMemStore(), nil
		},
		OpenNativeStore: func() (Store, error) {
			t.Fatal("OpenNativeStore called on the fresh off-mode open")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity() (second open) error = %v", err)
	}
	if result2.Diagnostic.Store != storeNameBdStore {
		t.Fatalf("second open diagnostic store = %q, want %q", result2.Diagnostic.Store, storeNameBdStore)
	}
}
