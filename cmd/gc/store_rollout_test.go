package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

func TestResolvedConditionalWritesMode(t *testing.T) {
	t.Run("nil config is unset", func(t *testing.T) {
		if got := resolvedConditionalWritesMode(nil); got != gate.ModeUnset {
			t.Fatalf("mode = %q, want unset", got)
		}
	})
	t.Run("resolved config value threads through", func(t *testing.T) {
		cfg, err := config.Parse([]byte("[workspace]\nname = \"t\"\n\n[beads]\nconditional_writes = \"require\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if got := resolvedConditionalWritesMode(cfg); got != gate.Require {
			t.Fatalf("mode = %q, want require", got)
		}
	})
	t.Run("resolve error degrades to unset, never raises", func(t *testing.T) {
		// config.Parse rejects the typo at load now; this defensive cell
		// covers an invalid value arriving through a non-Parse construction.
		cfg := &config.City{Beads: config.BeadsConfig{ConditionalWrites: "requre"}}
		if got := resolvedConditionalWritesMode(cfg); got != gate.ModeUnset {
			t.Fatalf("mode = %q, want unset (best-effort open paths cannot honor an invalid value)", got)
		}
	})
	t.Run("out-of-enum config fails to load at all", func(t *testing.T) {
		if _, err := config.Parse([]byte("[beads]\nconditional_writes = \"requre\"\n")); err == nil {
			t.Fatal("config.Parse accepted an out-of-enum conditional_writes — a typo must never silently mean off")
		}
	})
}

// TestOpenStoreResultAtForCityThreadsConditionalWrites is the entry-point
// test for the shared CLI/city open helper: a real temp city.toml declaring
// require must be observable on the store every command path receives —
// through the policy wrapper — without any per-command threading.
func TestOpenStoreResultAtForCityThreadsConditionalWrites(t *testing.T) {
	cityDir := t.TempDir()
	toml := "[workspace]\nname = \"t\"\nprefix = \"ga\"\n\n[beads]\nprovider = \"file\"\nconditional_writes = \"require\"\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := openStoreResultAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreResultAtForCity: %v", err)
	}
	writer, diag, resolveErr := beads.ResolveConditionalWriter(result.Store)
	if resolveErr != nil || diag != nil {
		t.Fatalf("resolve = diag %v err %v, want the file store's writer under require", diag, resolveErr)
	}
	if writer == nil {
		t.Fatal("require in city.toml was not observed on the opened store: mode threading is broken")
	}
}

// TestOpenStoreResultWithConfigSkipsLoad pins the ga-237xpr fix at its most
// direct layer: openStoreResultAtForCityWithConfig must reuse an
// already-resolved *config.City instead of reloading city.toml + all pack
// includes, but must still fall back to a load when the caller has no config
// in hand (the nil branch every other pre-existing call site relies on).
func TestOpenStoreResultWithConfigSkipsLoad(t *testing.T) {
	cityDir := t.TempDir()
	toml := "[workspace]\nname = \"t\"\n\n[beads]\nprovider = \"file\"\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCityConfig(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	before := loadCityConfigCalls.Load()
	if _, err := openStoreResultAtForCityWithConfig(cityDir, cityDir, cfg, gate.ModeUnset, false, false, false, nil); err != nil {
		t.Fatalf("openStoreResultAtForCityWithConfig(cfg): %v", err)
	}
	if grew := loadCityConfigCalls.Load() - before; grew != 0 {
		t.Fatalf("openStoreResultAtForCityWithConfig re-parsed city config %d times despite a non-nil cfg", grew)
	}

	before = loadCityConfigCalls.Load()
	if _, err := openStoreResultAtForCityWithConfig(cityDir, cityDir, nil, gate.ModeUnset, false, false, false, nil); err != nil {
		t.Fatalf("openStoreResultAtForCityWithConfig(nil): %v", err)
	}
	if grew := loadCityConfigCalls.Load() - before; grew != 1 {
		t.Fatalf("openStoreResultAtForCityWithConfig(nil cfg) parsed city config %d times, want exactly 1 (fallback load)", grew)
	}
}

// TestOpenStoreResultWithConfigSkipsLoad_ExecProvider is the exec-provider
// analog of TestOpenStoreResultWithConfigSkipsLoad, covering the gap left
// open by the original ga-237xpr fix (PR #4682 review round 1, BLOCKER):
// openStoreResultAtForCityWithConfig already threads its cfg into
// OpenExecStore (main.go), but openExecStoreAtForCity's own call to
// resolveConfiguredExecStoreTarget dropped it and re-parsed city.toml
// unconditionally on every call regardless of whether the caller had a
// resolved config in hand.
func TestOpenStoreResultWithConfigSkipsLoad_ExecProvider(t *testing.T) {
	cityDir := t.TempDir()
	toml := "[workspace]\nname = \"t\"\n\n[beads]\nprovider = \"exec:noop.sh\"\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCityConfig(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	before := loadCityConfigCalls.Load()
	if _, err := openStoreResultAtForCityWithConfig(cityDir, cityDir, cfg, gate.ModeUnset, false, false, false, nil); err != nil {
		t.Fatalf("openStoreResultAtForCityWithConfig(cfg): %v", err)
	}
	if grew := loadCityConfigCalls.Load() - before; grew != 0 {
		t.Fatalf("openStoreResultAtForCityWithConfig re-parsed city config %d times despite a non-nil cfg (exec provider)", grew)
	}

	before = loadCityConfigCalls.Load()
	if _, err := openStoreResultAtForCityWithConfig(cityDir, cityDir, nil, gate.ModeUnset, false, false, false, nil); err != nil {
		t.Fatalf("openStoreResultAtForCityWithConfig(nil): %v", err)
	}
	if grew := loadCityConfigCalls.Load() - before; grew != 1 {
		t.Fatalf("openStoreResultAtForCityWithConfig(nil cfg) parsed city config %d times, want exactly 1 (fallback load, exec provider)", grew)
	}
}

// TestOpenStoreResultNilConfigMatchesLegacy is a regression guard for the
// ga-237xpr refactor: openStoreResultAtForCityWithAuthority (the pre-existing
// entry point every non-dispatcher caller still uses) now delegates to
// openStoreResultAtForCityWithConfig with a nil config, and must keep both of
// its legacy characteristics — conditional-writes threading still works, and
// every call still reloads city.toml from disk (correct for callers like CLI
// commands, where the config may have changed since the last invocation).
func TestOpenStoreResultNilConfigMatchesLegacy(t *testing.T) {
	cityDir := t.TempDir()
	toml := "[workspace]\nname = \"t\"\n\n[beads]\nprovider = \"file\"\nconditional_writes = \"require\"\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := openStoreResultAtForCityWithAuthority(cityDir, cityDir, gate.ModeUnset, false, false, false, nil)
	if err != nil {
		t.Fatalf("openStoreResultAtForCityWithAuthority: %v", err)
	}
	writer, diag, resolveErr := beads.ResolveConditionalWriter(result.Store)
	if resolveErr != nil || diag != nil {
		t.Fatalf("resolve = diag %v err %v, want the file store's writer under require", diag, resolveErr)
	}
	if writer == nil {
		t.Fatal("require in city.toml was not observed via the WithAuthority entry point after the WithConfig refactor")
	}

	before := loadCityConfigCalls.Load()
	if _, err := openStoreResultAtForCityWithAuthority(cityDir, cityDir, gate.ModeUnset, false, false, false, nil); err != nil {
		t.Fatalf("openStoreResultAtForCityWithAuthority (second call): %v", err)
	}
	if grew := loadCityConfigCalls.Load() - before; grew != 1 {
		t.Fatalf("openStoreResultAtForCityWithAuthority parsed city config %d times, want exactly 1 (legacy per-call reload preserved for non-dispatcher callers)", grew)
	}
}

// TestOpenRigStoreThreadsConditionalWrites drives the controller's rig-store
// open end-to-end with a file provider: the boot-latched rollout flags must
// reach the factory stamp, including on the file path (which previously
// bypassed the factory entirely via an early return).
func TestOpenRigStoreThreadsConditionalWrites(t *testing.T) {
	stubManagedDoltStoreOpeners(t)
	cityDir := t.TempDir()
	toml := "[workspace]\nname = \"t\"\n\n[beads]\nconditional_writes = \"require\"\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(toml))
	if err != nil {
		t.Fatal(err)
	}
	cs := newControllerState(context.Background(), cfg, nil, nil, "t", cityDir)

	rigPath := filepath.Join(cityDir, "rigs", "r1")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	store := cs.openRigStore("file", "r1", rigPath, "ga", cfg)
	writer, diag, resolveErr := beads.ResolveConditionalWriter(store)
	if resolveErr != nil || diag != nil {
		t.Fatalf("resolve = diag %v err %v, want the rig store's writer under require", diag, resolveErr)
	}
	if writer == nil {
		t.Fatal("boot-latched require was not observed on the rig store")
	}
}

// TestResolvedNativeTransportMode mirrors TestResolvedConditionalWritesMode
// for beads.native_transport.
func TestResolvedNativeTransportMode(t *testing.T) {
	t.Run("nil config is unset", func(t *testing.T) {
		if got := resolvedNativeTransportMode(nil); got != beads.NativeTransportUnset {
			t.Fatalf("mode = %q, want unset", got)
		}
	})
	t.Run("resolved config value threads through, normalized", func(t *testing.T) {
		cfg, err := config.Parse([]byte("[workspace]\nname = \"t\"\n\n[beads]\nnative_transport = \"off\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if got := resolvedNativeTransportMode(cfg); got != beads.NativeTransportOff {
			t.Fatalf("mode = %q, want off", got)
		}
	})
	t.Run("mixed case and whitespace normalize too", func(t *testing.T) {
		cfg := &config.City{Beads: config.BeadsConfig{NativeTransport: " OFF "}}
		if got := resolvedNativeTransportMode(cfg); got != beads.NativeTransportOff {
			t.Fatalf("mode = %q, want off: NormalizedNativeTransport did not fold case/whitespace before the cast to beads.NativeTransportMode", got)
		}
	})
}

// TestOpenStoreAtForCityThreadsNativeTransportFromLoadedConfig drives the
// shared, nil-cfg open path every hook-claim / convoy / order-dispatch / sweep
// call site uses (openStoreAtForCity -> ... -> openStoreResultAtForCityScoped,
// which loads cfg from disk itself because none of those callers has one in
// hand). A mutation that made openStoreResultAtForCityScoped ignore the cfg it
// just loaded — e.g. resolving native transport from a zero-value config
// instead — would leave beads.native_transport="off" unenforced on this exact
// path, silently, with no caller able to tell.
func TestOpenStoreAtForCityThreadsNativeTransportFromLoadedConfig(t *testing.T) {
	cityDir := t.TempDir()
	toml := "[workspace]\nname = \"t\"\n\n[beads]\nnative_transport = \"off\"\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	var captured beads.StoreOpenOptions
	restore := openStoreFactoryForCity
	openStoreFactoryForCity = func(_ context.Context, opts beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
		captured = opts
		return beads.StoreOpenResult{Store: beads.NewMemStore()}, nil
	}
	t.Cleanup(func() { openStoreFactoryForCity = restore })

	if _, err := openStoreAtForCity(cityDir, cityDir); err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	if captured.NativeTransport != beads.NativeTransportOff {
		t.Fatalf("StoreOpenOptions.NativeTransport = %q, want %q: beads.native_transport=\"off\" on disk did not reach the factory via the nil-cfg call path",
			captured.NativeTransport, beads.NativeTransportOff)
	}
}

// TestOpenStoreAtForCityFailsLoudlyOnConfigLoadError proves Finding 7: a
// city.toml that exists but fails to load (here, an out-of-enum
// native_transport value, which config.Parse rejects at load time) must not
// be silently swallowed into a nil cfg and an "auto" default — this is a
// native-transport kill switch, and defaulting to native on a config this
// process could not actually read is exactly the failure mode the switch
// cannot survive. The open must fail loudly instead, naming the underlying
// load error.
func TestOpenStoreAtForCityFailsLoudlyOnConfigLoadError(t *testing.T) {
	cityDir := t.TempDir()
	toml := "[workspace]\nname = \"t\"\n\n[beads]\nnative_transport = \"bogus\"\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	restore := openStoreFactoryForCity
	openStoreFactoryForCity = func(_ context.Context, _ beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
		t.Fatal("the store factory must not be reached when the city config failed to load")
		return beads.StoreOpenResult{}, nil
	}
	t.Cleanup(func() { openStoreFactoryForCity = restore })

	if _, err := openStoreAtForCity(cityDir, cityDir); err == nil {
		t.Fatal("openStoreAtForCity: want an error for an unloadable city.toml, got nil")
	}
}

// TestOpenStoreAtForCityToleratesNoCityTOMLAtAll proves the Finding 7 fix is
// scoped to real load errors, not to "there is no city.toml here at all" —
// the vast majority of this shared open body's callers (ad hoc store paths,
// rig/scope stores outside any city) pass a path with no city.toml, and that
// must keep resolving to the nil-cfg best-effort default it always has,
// matching missingRootCityTOML's existing use elsewhere in this file.
func TestOpenStoreAtForCityToleratesNoCityTOMLAtAll(t *testing.T) {
	storeDir := t.TempDir() // deliberately no city.toml anywhere above this.

	var captured beads.StoreOpenOptions
	restore := openStoreFactoryForCity
	openStoreFactoryForCity = func(_ context.Context, opts beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
		captured = opts
		return beads.StoreOpenResult{Store: beads.NewMemStore()}, nil
	}
	t.Cleanup(func() { openStoreFactoryForCity = restore })

	if _, err := openStoreAtForCity(storeDir, storeDir); err != nil {
		t.Fatalf("openStoreAtForCity: want no-city.toml to stay non-fatal, got: %v", err)
	}
	if captured.NativeTransport != beads.NativeTransportUnset {
		t.Fatalf("NativeTransport = %q, want unset (no config to resolve from)", captured.NativeTransport)
	}
}

// TestOpenRigStoreThreadsNativeTransport proves the controller's per-rig open
// (api_state.go's openRigStore) threads the boot-latched native_transport
// value into StoreOpenOptions, the way TestOpenRigStoreThreadsConditionalWrites
// proves it for conditional_writes. A mutation that dropped the
// NativeTransport field from that call would leave every rig in an "off" city
// still eligible to open natively.
func TestOpenRigStoreThreadsNativeTransport(t *testing.T) {
	prevOpen := controllerStateOpenRigStoreAtForCity
	t.Cleanup(func() { controllerStateOpenRigStoreAtForCity = prevOpen })

	cityDir := t.TempDir()
	cfg := &config.City{Workspace: config.Workspace{Name: "t"}}
	// A directly-built controllerState, not newControllerState: the
	// constructor's own best-effort city-store open spawns a real managed
	// dolt process when unstubbed (~10s), which this test has no need to pay
	// — it exercises openRigStore in isolation, exactly like
	// TestControllerStateBuildStoresRoutesBdRigThroughStoreFactory does.
	cs := &controllerState{cityPath: cityDir, cfg: cfg, nativeTransport: beads.NativeTransportOff}

	rigPath := filepath.Join(cityDir, "rigs", "r1")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	var captured beads.StoreOpenOptions
	controllerStateOpenRigStoreAtForCity = func(_ context.Context, opts beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
		captured = opts
		return beads.StoreOpenResult{Store: beads.NewMemStore()}, nil
	}

	cs.openRigStore("bd", "r1", rigPath, "ga", cfg)
	if captured.NativeTransport != beads.NativeTransportOff {
		t.Fatalf("openRigStore StoreOpenOptions.NativeTransport = %q, want %q", captured.NativeTransport, beads.NativeTransportOff)
	}
}

// TestNewControllerStateOpenCityStoreThreadsTheLatchedNativeTransport proves
// the DEFAULT newControllerStateOpenCityStore closure — not a test stub of
// it — passes its nativeTransport parameter (the boot latch) through to the
// store-open options, by exercising the real var directly and capturing what
// reaches the factory. api_state_rollout_test.go's
// TestControllerStateNativeTransportDoesNotFlipOnReload stubs this var
// entirely, which proves newControllerState calls it with the right argument
// but can never catch a bug inside the default implementation itself — such
// as passing nil instead of &nativeTransport.
func TestNewControllerStateOpenCityStoreThreadsTheLatchedNativeTransport(t *testing.T) {
	cityDir := t.TempDir()
	writeMinimalCityToml(t, cityDir)

	var captured beads.StoreOpenOptions
	restore := openStoreFactoryForCity
	openStoreFactoryForCity = func(_ context.Context, opts beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
		captured = opts
		return beads.StoreOpenResult{Store: beads.NewMemStore()}, nil
	}
	t.Cleanup(func() { openStoreFactoryForCity = restore })

	if _, err := newControllerStateOpenCityStore(cityDir, gate.ModeUnset, beads.NativeTransportOff); err != nil {
		t.Fatalf("newControllerStateOpenCityStore: %v", err)
	}
	if captured.NativeTransport != beads.NativeTransportOff {
		t.Fatalf("NativeTransport = %q, want %q: the default newControllerStateOpenCityStore did not thread its parameter through",
			captured.NativeTransport, beads.NativeTransportOff)
	}
}

// TestOpenControlBdStoreThroughFactoryStamps pins the control-dispatcher
// routing: the raw control-plane bd store must come back factory-stamped
// (and raw — control paths are deliberately unwrapped), with native
// selection impossible (no preflight checker is supplied).
func TestOpenControlBdStoreThroughFactoryStamps(t *testing.T) {
	cfg, err := config.Parse([]byte("[workspace]\nname = \"t\"\n\n[beads]\nconditional_writes = \"require\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	capableHelp := []byte("Usage:\n  bd update [flags]\n\nFlags:\n  --if-revision int\n")
	raw := beads.NewBdStore("/city", func(_, _ string, _ ...string) ([]byte, error) {
		return capableHelp, nil
	})
	store, err := openControlBdStoreThroughFactory("/city", "/city", "bd", cfg,
		func() (beads.Store, error) { return raw, nil })
	if err != nil {
		t.Fatalf("openControlBdStoreThroughFactory: %v", err)
	}
	if store != beads.Store(raw) {
		t.Fatalf("store = %T, want the raw control bd store back (no policy wrap on control paths)", store)
	}
	writer, diag, resolveErr := beads.ResolveConditionalWriter(store)
	if resolveErr != nil || diag != nil {
		t.Fatalf("resolve = diag %v err %v, want the control store's writer under require", diag, resolveErr)
	}
	if writer == nil {
		t.Fatal("require was not stamped onto the control-plane bd store")
	}
}

func TestConditionalWritesDegradedRecorder(t *testing.T) {
	t.Run("nil recorder yields nil callback", func(t *testing.T) {
		if cb := conditionalWritesDegradedRecorder(nil, rollout.Flags{}, "rig/r1"); cb != nil {
			t.Fatal("want nil callback for busless paths")
		}
	})
	t.Run("records the typed event with wire vocabulary", func(t *testing.T) {
		fake := events.NewFake()
		cfg, err := config.Parse([]byte("[workspace]\nname = \"t\"\n\n[beads]\nconditional_writes = \"auto\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		flags, err := rollout.Resolve(cfg, rollout.ResolveOptions{})
		if err != nil {
			t.Fatal(err)
		}
		cb := conditionalWritesDegradedRecorder(fake, flags, "rig/r1")
		cb(beads.ConditionalWritesDegrade{StoreKind: "BdStore", Mode: "auto", Reason: "bd lacks --if-revision"})

		recorded, err := fake.List(events.Filter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(recorded) != 1 || recorded[0].Type != events.BeadsConditionalWritesDegraded {
			t.Fatalf("recorded = %+v, want one beads.conditional_writes.degraded event", recorded)
		}
		var payload events.ConditionalWritesDegradedPayload
		if err := json.Unmarshal(recorded[0].Payload, &payload); err != nil {
			t.Fatalf("payload: %v", err)
		}
		if payload.StoreID != "rig/r1" || payload.StoreKind != "bd" || payload.Mode != "auto" || payload.Origin != "config" {
			t.Fatalf("payload = %+v, want wire vocabulary (bd) + origin config", payload)
		}
	})
}

// TestConditionalWritesEventStoreKind pins the internal→wire vocabulary map,
// including the build-tagged DoltliteReadStore, which beads cannot name and
// therefore reaches this layer as its %T spelling.
func TestConditionalWritesEventStoreKind(t *testing.T) {
	for in, want := range map[string]string{
		beads.BeadsStoreNameBdStore:         "bd",
		beads.BeadsStoreNameNativeDoltStore: "native",
		beads.BeadsStoreNameFileStore:       "file",
		"MemStore":                          "mem",
		"CachingStore":                      "caching",
		"SQLiteStore":                       "sqlite-graph",
		"*beads.DoltliteReadStore":          "bd",
		"someFutureStore":                   "someFutureStore",
	} {
		if got := conditionalWritesEventStoreKind(in); got != want {
			t.Errorf("conditionalWritesEventStoreKind(%q) = %q, want %q", in, got, want)
		}
	}
}
