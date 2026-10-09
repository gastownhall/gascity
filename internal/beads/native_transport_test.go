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
// proves two separate opens that both hit the GC_BEADS_FORCE_FALLBACK path log
// the deprecation warning once between them (sync.Once), rather than once per
// open — the latter would flood logs in a long-lived process that opens many
// city stores with the legacy env var set.
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

// TestOpenStoreAtForCityNativeTransportDecidesPerOpen proves each open decides
// from its own NativeTransport value: opens of one eligible scope under "auto",
// then "off", then "auto" again pick native, BdStore, and native. An "off" open
// leaves nothing behind that a later "auto" open inherits.
func TestOpenStoreAtForCityNativeTransportDecidesPerOpen(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	scope := "/city"
	native := NewMemStore()
	bd := NewMemStore()
	open := func(mode NativeTransportMode) StoreOpenResult {
		t.Helper()
		result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
			ScopeRoot:        scope,
			Provider:         "bd",
			NativeTransport:  mode,
			PreflightChecker: factoryPreflightChecker(scope, factoryPreflightDoltMetadata(), contract.PreflightBDContext{Backend: "dolt", DoltMode: "server"}),
			OpenBdStore: func() (Store, error) {
				return bd, nil
			},
			OpenNativeStore: func() (Store, error) {
				return native, nil
			},
		})
		if err != nil {
			t.Fatalf("OpenStoreAtForCity(native_transport=%q) error = %v", mode, err)
		}
		return result
	}

	for i, step := range []struct {
		mode      NativeTransportMode
		wantStore Store
		wantName  string
	}{
		{mode: NativeTransportAuto, wantStore: native, wantName: storeNameNativeDoltStore},
		{mode: NativeTransportOff, wantStore: bd, wantName: storeNameBdStore},
		{mode: NativeTransportAuto, wantStore: native, wantName: storeNameNativeDoltStore},
	} {
		result := open(step.mode)
		if result.Store != step.wantStore || result.Diagnostic.Store != step.wantName {
			t.Fatalf("open %d (native_transport=%q): diagnostic store = %q, want %q", i+1, step.mode, result.Diagnostic.Store, step.wantName)
		}
	}
}

// TestProviderConsultsNativeTransport pins which providers reach the
// native_transport decision, and checks each answer against the factory: an
// open under native_transport="off" records the off gate exactly when the
// helper says the provider consults it. Callers use the helper to decide what
// a city.toml load error costs, so it must not drift from the factory's routing.
func TestProviderConsultsNativeTransport(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	cases := []struct {
		name     string
		provider string
		want     bool
	}{
		{name: "empty defaults to bd", provider: "", want: true},
		{name: "bd", provider: "bd", want: true},
		{name: "doltlite", provider: "doltlite", want: true},
		{name: "bd-contract exec by name", provider: "exec:gc-beads-bd", want: true},
		{name: "bd-contract exec by path", provider: "exec:/city/.gc/scripts/gc-beads-bd.sh", want: true},
		{name: "file", provider: "file", want: false},
		{name: "file with surrounding space", provider: " file ", want: false},
		{name: "exec outside the bd contract", provider: "exec:noop.sh", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProviderConsultsNativeTransport(tc.provider); got != tc.want {
				t.Fatalf("ProviderConsultsNativeTransport(%q) = %v, want %v", tc.provider, got, tc.want)
			}
			stub := func() (Store, error) { return NewMemStore(), nil }
			result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
				ScopeRoot:       "/city",
				Provider:        tc.provider,
				NativeTransport: NativeTransportOff,
				OpenBdStore:     stub,
				OpenFileStore:   stub,
				OpenExecStore:   stub,
				OpenNativeStore: func() (Store, error) {
					t.Fatal("OpenNativeStore called while native_transport=off")
					return nil, nil
				},
			})
			if err != nil {
				t.Fatalf("OpenStoreAtForCity(provider %q) error = %v", tc.provider, err)
			}
			if consulted := result.Diagnostic.PreflightGate == nativeTransportOffGate; consulted != tc.want {
				t.Fatalf("factory open for provider %q recorded preflight_gate %q (consulted native_transport = %v), but the helper says %v",
					tc.provider, result.Diagnostic.PreflightGate, consulted, tc.want)
			}
		})
	}
}
