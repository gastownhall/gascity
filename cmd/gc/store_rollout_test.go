package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
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

// TestOpenStoreAtForCityConfigLoadError pins which opens fail when the
// city's city.toml exists but does not load. Only an open the city's
// native_transport value would decide fails: no boot-latched value is in hand
// and the provider reaches that decision, so an unreadable "off" must not
// open as "auto". Every other open proceeds on a nil config, best-effort,
// because the load error cannot change it.
func TestOpenStoreAtForCityConfigLoadError(t *testing.T) {
	const brokenInclude = "include = [\"broken.toml\"]\n\n[workspace]\nname = \"t\"\n"
	off := beads.NativeTransportOff
	cases := []struct {
		name     string
		cityTOML string
		override *beads.NativeTransportMode
		wantErr  bool
	}{
		{
			name:     "bd provider with an out-of-enum native_transport fails",
			cityTOML: "[workspace]\nname = \"t\"\n\n[beads]\nnative_transport = \"bogus\"\n",
			wantErr:  true,
		},
		{
			name:     "bd provider with a broken include fails",
			cityTOML: brokenInclude,
			wantErr:  true,
		},
		{
			name:     "bd-contract exec provider with a broken include fails",
			cityTOML: brokenInclude + "\n[beads]\nprovider = \"exec:gc-beads-bd\"\n",
			wantErr:  true,
		},
		{
			name:     "file provider with a broken include opens",
			cityTOML: brokenInclude + "\n[beads]\nprovider = \"file\"\n",
		},
		{
			name:     "file provider with an out-of-enum conditional_writes opens",
			cityTOML: "[workspace]\nname = \"t\"\n\n[beads]\nprovider = \"file\"\nconditional_writes = \"bogus\"\n",
		},
		{
			name:     "exec provider outside the bd contract with a broken include opens",
			cityTOML: brokenInclude + "\n[beads]\nprovider = \"exec:noop.sh\"\n",
		},
		{
			name:     "boot-latched native_transport with a broken include opens",
			cityTOML: brokenInclude,
			override: &off,
		},
		{
			name:     "boot-latched native_transport with an unparseable city.toml opens",
			cityTOML: "[workspace\n",
			override: &off,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(cityDir, "broken.toml"), []byte("["), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(tc.cityTOML), 0o644); err != nil {
				t.Fatal(err)
			}
			var captured *beads.StoreOpenOptions
			restore := openStoreFactoryForCity
			openStoreFactoryForCity = func(_ context.Context, opts beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
				captured = &opts
				return beads.StoreOpenResult{Store: beads.NewMemStore()}, nil
			}
			t.Cleanup(func() { openStoreFactoryForCity = restore })

			_, err := openStoreResultAtForCityWithMode(cityDir, cityDir, gate.ModeUnset, false, false, tc.override)
			if tc.wantErr {
				if err == nil {
					t.Fatal("open succeeded; want the city.toml load error, because this city's unread native_transport would decide the open")
				}
				if captured != nil {
					t.Fatal("the store factory was reached after the city config failed to load")
				}
				return
			}
			if err != nil {
				t.Fatalf("open failed on a load error that cannot change it: %v", err)
			}
			if captured == nil {
				t.Fatal("the store factory was not reached")
			}
			if tc.override != nil && captured.NativeTransport != *tc.override {
				t.Fatalf("NativeTransport = %q, want the boot-latched %q", captured.NativeTransport, *tc.override)
			}
		})
	}
}

// TestCmdHookClaimUnderABrokenPackIncludeStopsAtItsOwnConfigLoad pins what a
// claim costs when a pack include does not load: gc hook --claim exits on its
// own config-load error, naming the include, before it runs a bd command or
// opens a store. The fail-closed load inside the store open
// (loadCityConfigForStoreOpen) is therefore never what refuses a claim.
func TestCmdHookClaimUnderABrokenPackIncludeStopsAtItsOwnConfigLoad(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "broken.toml"), []byte("["), 0o644); err != nil {
		t.Fatal(err)
	}
	cityTOML := "include = [\"broken.toml\"]\n\n[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"worker\"\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	// Any bd subprocess, the work query and the claim alike, appends here.
	fakeBin := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd.log")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\nprintf '[]'\n", logPath)
	if err := os.WriteFile(filepath.Join(fakeBin, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_AGENT", "worker")
	opened := false
	restore := openStoreFactoryForCity
	openStoreFactoryForCity = func(context.Context, beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
		opened = true
		return beads.StoreOpenResult{Store: beads.NewMemStore()}, nil
	}
	t.Cleanup(func() { openStoreFactoryForCity = restore })

	var stdout, stderr bytes.Buffer
	if code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true}, &stdout, &stderr); code != 1 {
		t.Fatalf("gc hook --claim = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	_, loadErr := loadCityConfig(cityDir, io.Discard)
	if loadErr == nil || !strings.Contains(loadErr.Error(), "broken.toml") {
		t.Fatalf("loadCityConfig = %v, want an error naming broken.toml", loadErr)
	}
	if want := "gc hook: " + loadErr.Error() + "\n"; stderr.String() != want {
		t.Fatalf("stderr = %q, want the hook's own config-load error %q", stderr.String(), want)
	}
	if opened {
		t.Fatal("a store was opened under a city config that did not load")
	}
	if ran, err := os.ReadFile(logPath); err == nil {
		t.Fatalf("bd ran under a city config that did not load:\n%s", ran)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

// TestOpenStoreAtForCityToleratesNoCityTOMLAtAll pins that a path with no
// city.toml at all is not a load error: most callers of this shared open body
// (ad hoc store paths, rig/scope stores outside any city) pass one, and it
// keeps resolving to the nil-cfg best-effort default.
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

// TestOpenBdStoreAtScopedConsultsTheDoltliteReadOptimizationOnlyOutsideOff
// pins, in the default build, which bd store opens may consult the
// GC_NATIVE_DOLTLITE_BEADS read optimization. Under "off", per city or through
// the deprecated GC_BEADS_FORCE_FALLBACK alias, no scope consults it and each
// gets the plain BdStore; otherwise every scope consults it and takes what it
// returns. The city scope, its one-shot variant and a rig scope are covered.
// The tagged TestOpenBdStoreAtScopedSkipsDoltliteOptimizationUnderNativeTransportOff
// proves the city refusal against the real optimization, which only a
// gascity_native_beads build has.
func TestOpenBdStoreAtScopedConsultsTheDoltliteReadOptimizationOnlyOutsideOff(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	writeMinimalCityToml(t, cityDir)
	rigDir := filepath.Join(cityDir, "rigs", "app")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}

	optimized := beads.NewMemStore()
	available := true
	var consulted []string
	restore := openDoltliteReadOptimization
	openDoltliteReadOptimization = func(storePath string, _ *beads.BdStore) (beads.Store, bool) {
		consulted = append(consulted, storePath)
		if !available {
			return nil, false
		}
		return optimized, true
	}
	t.Cleanup(func() { openDoltliteReadOptimization = restore })

	scopes := []struct {
		name      string
		storePath string
		oneShot   bool
	}{
		{name: "city", storePath: cityDir},
		{name: "one-shot city", storePath: cityDir, oneShot: true},
		{name: "rig", storePath: rigDir},
	}
	for _, tc := range []struct {
		name          string
		mode          beads.NativeTransportMode
		forceFallback string
		available     bool
		wantConsulted bool
		wantOptimized bool
	}{
		{name: "unset", mode: beads.NativeTransportUnset, available: true, wantConsulted: true, wantOptimized: true},
		{name: "auto", mode: beads.NativeTransportAuto, available: true, wantConsulted: true, wantOptimized: true},
		{name: "auto without the optimization", mode: beads.NativeTransportAuto, wantConsulted: true},
		{name: "off", mode: beads.NativeTransportOff, available: true},
		{name: "auto under GC_BEADS_FORCE_FALLBACK", mode: beads.NativeTransportAuto, forceFallback: "1", available: true},
	} {
		for _, scope := range scopes {
			t.Run(tc.name+"/"+scope.name, func(t *testing.T) {
				t.Setenv("GC_BEADS_FORCE_FALLBACK", tc.forceFallback)
				available = tc.available
				consulted = nil

				store, err := openBdStoreAtScoped(scope.storePath, cityDir, &config.City{}, scope.oneShot, tc.mode)
				if err != nil {
					t.Fatalf("openBdStoreAtScoped: %v", err)
				}
				switch {
				case !tc.wantConsulted && len(consulted) != 0:
					t.Fatalf("the doltlite read optimization was consulted for %v; it must not be under this native_transport", consulted)
				case tc.wantConsulted && (len(consulted) != 1 || consulted[0] != scope.storePath):
					t.Fatalf("doltlite read optimization consulted for %v, want exactly [%s]", consulted, scope.storePath)
				}
				if tc.wantOptimized {
					if store != optimized {
						t.Fatalf("openBdStoreAtScoped returned %T, want the store the optimization returned", store)
					}
					return
				}
				if _, isPlainBd := store.(*beads.BdStore); !isPlainBd {
					t.Fatalf("openBdStoreAtScoped returned %T, want a plain *beads.BdStore", store)
				}
			})
		}
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
// (and raw — control paths are deliberately unwrapped) when the scope fails
// preflight (here: no .beads metadata under /city), i.e. the bd fallback is
// still the factory's own, stamped, fallback.
func TestOpenControlBdStoreThroughFactoryStamps(t *testing.T) {
	cfg, err := config.Parse([]byte("[workspace]\nname = \"t\"\n\n[beads]\nconditional_writes = \"require\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	capableHelp := []byte("Usage:\n  bd update [flags]\n\nFlags:\n  --if-revision int\n")
	raw := beads.NewBdStore("/city", func(_, _ string, _ ...string) ([]byte, error) {
		return capableHelp, nil
	})
	// A zero checker fails preflight on the metadata read without forking bd.
	prev := newControlPreflightChecker
	newControlPreflightChecker = func(_, _ string) contract.PreflightChecker { return contract.PreflightChecker{} }
	t.Cleanup(func() { newControlPreflightChecker = prev })
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

// TestControlStoreRegistryRetainsOnlyNativeStores pins that per-open
// fallbacks (bd, mem, file) are never memoized: only a native store, whose
// open is a preflight plus a connection pool, is kept for the process.
func TestControlStoreRegistryRetainsOnlyNativeStores(t *testing.T) {
	reg := &controlStoreRegistry{byRoot: map[string]beads.Store{}}
	mem := beads.NewMemStore()
	if got := reg.retain("/scope", mem); got != beads.Store(mem) {
		t.Fatalf("retain(mem) = %T, want the same store back", got)
	}
	if got := reg.lookup("/scope"); got != nil {
		t.Fatalf("lookup after retaining a MemStore = %T, want nil (non-native stores are per-open)", got)
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
