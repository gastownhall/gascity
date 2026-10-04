package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/storebinding"
	"github.com/gastownhall/gascity/internal/storebinding/beadsworkspace"
	sqlitebinding "github.com/gastownhall/gascity/internal/storebinding/sqlite"
)

// TestOpenStorageRoutesStampsBindingWithResolvedMode proves storage boot
// stamps the binding engine it opens with the city's conditional_writes mode.
// No store factory opens that engine, so before the stamp a split city's
// session rows resolved as legacy under every mode and their fenced writes
// landed unconditionally.
func TestOpenStorageRoutesStampsBindingWithResolvedMode(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		stamped gate.Mode
		fenced  bool
	}{
		{mode: "", stamped: gate.Off, fenced: false},
		{mode: "off", stamped: gate.Off, fenced: false},
		{mode: "auto", stamped: gate.Auto, fenced: true},
		{mode: "require", stamped: gate.Require, fenced: true},
	} {
		t.Run("mode="+tc.mode, func(t *testing.T) {
			root := t.TempDir()
			cfg := infraSplitConfig(filepath.Join(root, "store"))
			cfg.Beads.ConditionalWrites = tc.mode
			plan, err := resolveCityStoragePlan(root, cfg)
			if err != nil {
				t.Fatalf("resolving the storage plan: %v", err)
			}
			routes, err := openStorageRoutes(plan, mustResolveInfraTarget(t, root, cfg), cfg, root, nil)
			if err != nil {
				t.Fatalf("openStorageRoutes: %v", err)
			}
			t.Cleanup(func() { _ = routes.close() })

			store, relocated := routes.storeFor(coordclass.ClassSessions)
			if !relocated {
				t.Fatal("the split routes do not relocate the sessions class")
			}
			if got := beads.InspectConditionalWrites(store).Mode; got != tc.stamped {
				t.Fatalf("binding engine stamped %q, want %q", got, tc.stamped)
			}
			writer, _, err := beads.ResolveConditionalWriter(beads.SessionStore{Store: store})
			if err != nil || (writer != nil) != tc.fenced {
				t.Fatalf("ResolveConditionalWriter(session binding) = (%T, %v), want fenced=%v", writer, err, tc.fenced)
			}

			// The controller serves the binding through its CachingStore,
			// which must resolve to itself so the cache keeps its eviction
			// rules on every fenced write.
			cached, _ := routes.withControllerCache(context.Background(), nil).storeFor(coordclass.ClassSessions)
			writer, _, err = beads.ResolveConditionalWriter(beads.SessionStore{Store: cached})
			if err != nil || (writer != nil) != tc.fenced {
				t.Fatalf("ResolveConditionalWriter(cached session binding) = (%T, %v), want fenced=%v", writer, err, tc.fenced)
			}
			if _, isCache := writer.(*beads.CachingStore); tc.fenced && !isCache {
				t.Fatalf("cached session binding resolved to %T, want the controller's *beads.CachingStore", writer)
			}
		})
	}
}

// TestPreflightProbesRelocatedClassStores proves the require-mode boot probe
// covers a split city's binding engine as well as the work stores, once per
// engine rather than once per class it serves. Otherwise an engine that cannot
// fence boots quietly and refuses on its first session write.
func TestPreflightProbesRelocatedClassStores(t *testing.T) {
	incapable := beads.NewMemStore()
	incapable.DisableConditionalWrites = true
	if err := beads.StampOpenedStore(incapable, "MemStore", gate.Require, nil, nil); err != nil {
		t.Fatalf("StampOpenedStore: %v", err)
	}
	routes := &storageRoutes{stores: map[coordclass.Class]beads.Store{}, binding: "infra"}
	for _, class := range coordclass.Classes() {
		if class != coordclass.ClassWork {
			routes.stores[class] = incapable
		}
	}

	var logs []string
	cs := &controllerState{
		rolloutFlags:  rollout.ForTest(rollout.WithBeadsConditionalWrites(rollout.Require)),
		rolloutLogf:   func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
		storageRoutes: routes,
	}
	cs.preflightConditionalWrites()
	if len(logs) != 1 || !strings.Contains(logs[0], "ERROR") || !strings.Contains(logs[0], "binding/infra") {
		t.Fatalf("require preflight lines = %v, want exactly one ERROR naming binding/infra", logs)
	}
}

// carrierlessEngineFactory serves the SQLite engine behind a wrapper that hides
// its conditional-writes carrier, the shape of a provider whose engine cannot
// hold the stamp, and counts every close of the handle it hands out.
type carrierlessEngineFactory struct{ closeCountingProviderFactory }

func (f carrierlessEngineFactory) New(spec storebinding.BindingSpec) (storebinding.Provider, error) {
	provider, err := f.closeCountingProviderFactory.New(spec)
	if err != nil {
		return nil, err
	}
	return carrierlessEngineProvider{provider.(closeCountingProvider)}, nil
}

type carrierlessEngineProvider struct{ closeCountingProvider }

func (p carrierlessEngineProvider) OpenEngine(spec storebinding.BindingSpec, classes storebinding.ClassSet) (beads.Store, io.Closer, error) {
	store, closer, err := p.closeCountingProvider.OpenEngine(spec, classes)
	if err != nil {
		return nil, nil, err
	}
	return &struct{ beads.Store }{Store: store}, closer, nil
}

// TestOpenStorageRoutesRefusesACarrierlessEngineUnderRequire pins the
// factory's cell contract on the binding: an engine that cannot carry the
// stamp is refused under require, with the engine it opened released exactly
// once, and served under auto with the degrade reported to the controller's
// recorder as binding/<name>. Believing such an engine stamped is the silent
// fallback require exists to rule out.
func TestOpenStorageRoutesRefusesACarrierlessEngineUnderRequire(t *testing.T) {
	for _, mode := range []string{"require", "auto"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			cfg := infraSplitConfig(filepath.Join(root, "store"))
			cfg.Beads.ConditionalWrites = mode

			closes := 0
			prevRegistry := newStorageRegistryForPlan
			newStorageRegistryForPlan = func() (*storebinding.ProviderRegistry, error) {
				registry := storebinding.NewProviderRegistry()
				if err := registry.Register(carrierlessEngineFactory{closeCountingProviderFactory{
					inner: sqlitebinding.BeadsProviderFactory{}, closes: &closes,
				}}); err != nil {
					return nil, err
				}
				if err := registry.Freeze(); err != nil {
					return nil, err
				}
				return registry, nil
			}
			t.Cleanup(func() { newStorageRegistryForPlan = prevRegistry })
			plan, err := resolveCityStoragePlan(root, cfg)
			if err != nil {
				t.Fatalf("resolving the storage plan: %v", err)
			}

			rec := events.NewFake()
			routes, err := openStorageRoutes(plan, mustResolveInfraTarget(t, root, cfg), cfg, root, rec)
			if mode == "require" {
				if err == nil {
					_ = routes.close()
					t.Fatal("require served a binding engine that cannot carry the stamp")
				}
				if !beads.IsConditionalWritesRequired(err) {
					t.Fatalf("err = %v, want the typed require refusal", err)
				}
				if closes != 1 {
					t.Fatalf("the refused engine was closed %d time(s), want exactly 1", closes)
				}
				return
			}
			if err != nil {
				t.Fatalf("auto refused a carrier-less binding engine: %v", err)
			}
			t.Cleanup(func() { _ = routes.close() })
			if closes != 0 {
				t.Fatalf("auto closed the engine it serves %d time(s)", closes)
			}
			if len(rec.Events) != 1 || rec.Events[0].Type != events.BeadsConditionalWritesDegraded {
				t.Fatalf("recorded = %+v, want one beads.conditional_writes.degraded event on the controller's recorder", rec.Events)
			}
			var payload events.ConditionalWritesDegradedPayload
			if err := json.Unmarshal(rec.Events[0].Payload, &payload); err != nil {
				t.Fatalf("payload: %v", err)
			}
			if payload.StoreID != "binding/infra" || payload.Mode != "auto" {
				t.Fatalf("payload = %+v, want store_id binding/infra mode auto", payload)
			}
		})
	}
}

// TestOpenStorageRoutesRefusesEngineOpenUnderNativeTransportOff proves the
// per-city beads.native_transport="off" kill switch reaches the storebinding
// open path, not only the beads factory, for a NATIVE-TRANSPORT provider
// (beads-workspace / native Dolt): such a binding has no BdStore fallback of
// its own (see EngineOpener's doc), so letting it open while "off" would
// silently break the switch's promise that this city's stores never open
// natively.
//
// It deliberately uses workspaceSplitConfig with NO real workspace directory
// on disk: the refusal this test proves fires in openStorageRoutes BEFORE
// opener.OpenEngine is ever called, so it must not depend on — or touch — a
// workspace that does not exist. "auto" (today's behavior) still reaches
// OpenEngine and fails for the workspace-not-there reason instead
// (beadsworkspace.ErrWorkspaceUnavailable), proving the native_transport gate
// let it through rather than refusing it too.
func TestOpenStorageRoutesRefusesEngineOpenUnderNativeTransportOff(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		refused bool
	}{
		{mode: "", refused: false},
		{mode: "auto", refused: false},
		{mode: "off", refused: true},
	} {
		t.Run("native_transport="+tc.mode, func(t *testing.T) {
			root := t.TempDir()
			cfg := workspaceSplitConfig("infra")
			cfg.Beads.NativeTransport = tc.mode
			plan, err := resolveCityStoragePlan(root, cfg)
			if err != nil {
				t.Fatalf("resolving the storage plan: %v", err)
			}
			// infraBindingTarget{Binding: "infra"} directly, not
			// mustResolveInfraTarget: that helper resolves the sqlite-beads
			// migration destination specifically and does not recognize a
			// beads-workspace binding at all.
			routes, err := openStorageRoutes(plan, infraBindingTarget{Binding: "infra"}, cfg, root, nil)
			if tc.refused {
				if err == nil {
					_ = routes.close()
					t.Fatal("native_transport=\"off\" served a binding engine that only opens natively")
				}
				if !strings.Contains(err.Error(), "native_transport") || !strings.Contains(err.Error(), `"off"`) {
					t.Fatalf("err = %v, want it to name native_transport and off", err)
				}
				if errors.Is(err, beadsworkspace.ErrWorkspaceUnavailable) {
					t.Fatalf("err = %v, refused for the wrong reason (workspace-not-there instead of native_transport)", err)
				}
				return
			}
			// Not refused by native_transport: the provider's own open still
			// runs and fails because no real workspace directory exists here.
			// That is the proof the native_transport gate let it through — a
			// workspace that refused for ANY other reason, or that opened the
			// routes it fails to close, would both hide a native_transport
			// refusal that silently stopped firing.
			if err == nil {
				_ = routes.close()
				t.Fatalf("openStorageRoutes (native_transport=%q) served a workspace that was never created", tc.mode)
			}
			if !errors.Is(err, beadsworkspace.ErrWorkspaceUnavailable) {
				t.Fatalf("openStorageRoutes (native_transport=%q) error = %v, want %v (proves native_transport did not refuse it)",
					tc.mode, err, beadsworkspace.ErrWorkspaceUnavailable)
			}
			if strings.Contains(err.Error(), "native_transport") {
				t.Fatalf("openStorageRoutes (native_transport=%q) error names native_transport: %v", tc.mode, err)
			}
		})
	}
}

// TestOpenStorageRoutesRefusesEngineOpenUnderForceFallbackEnv proves Finding
// 4's fix: the deprecated process-wide GC_BEADS_FORCE_FALLBACK alias also
// refuses a native-transport (beads-workspace) EngineOpener bind, even though
// the per-city beads.native_transport value is "auto". Before this fix,
// "process-wide off" was only true for the OpenStoreAtForCity family — a
// beads-workspace binding opened natively regardless of the env var, which is
// not what an operator reaching for the legacy kill switch expects.
func TestOpenStorageRoutesRefusesEngineOpenUnderForceFallbackEnv(t *testing.T) {
	t.Setenv("GC_BEADS_FORCE_FALLBACK", "1")
	root := t.TempDir()
	cfg := workspaceSplitConfig("infra")
	cfg.Beads.NativeTransport = "auto"
	plan, err := resolveCityStoragePlan(root, cfg)
	if err != nil {
		t.Fatalf("resolving the storage plan: %v", err)
	}
	routes, err := openStorageRoutes(plan, infraBindingTarget{Binding: "infra"}, cfg, root, nil)
	if err == nil {
		_ = routes.close()
		t.Fatal("GC_BEADS_FORCE_FALLBACK=1 served a binding engine that only opens natively")
	}
	if !strings.Contains(err.Error(), "native_transport") || !strings.Contains(err.Error(), `"off"`) {
		t.Fatalf("err = %v, want it to name native_transport and off", err)
	}
	if errors.Is(err, beadsworkspace.ErrWorkspaceUnavailable) {
		t.Fatalf("err = %v, refused for the wrong reason (workspace-not-there instead of the force-fallback refusal)", err)
	}
}

// TestOpenStorageRoutesDoesNotRefuseSQLiteBeadsUnderNativeTransportOff proves
// the EngineOpener refusal is scoped to native-transport providers, not to
// "implements EngineOpener" in general: sqlite-beads also implements
// EngineOpener (how else would it bind classes?) but it is not native
// transport, so beads.native_transport="off" must never stop a city with a
// sqlite infra binding from booting.
func TestOpenStorageRoutesDoesNotRefuseSQLiteBeadsUnderNativeTransportOff(t *testing.T) {
	for _, mode := range []string{"", "auto", "off"} {
		t.Run("native_transport="+mode, func(t *testing.T) {
			root := t.TempDir()
			cfg := infraSplitConfig(filepath.Join(root, "store"))
			cfg.Beads.NativeTransport = mode
			plan, err := resolveCityStoragePlan(root, cfg)
			if err != nil {
				t.Fatalf("resolving the storage plan: %v", err)
			}
			routes, err := openStorageRoutes(plan, mustResolveInfraTarget(t, root, cfg), cfg, root, nil)
			if err != nil {
				t.Fatalf("openStorageRoutes (native_transport=%q): sqlite-beads binding refused to open: %v", mode, err)
			}
			t.Cleanup(func() { _ = routes.close() })
		})
	}
}

// TestConditionalWritesStatusReportsTheBindingEngine proves gc status sees a
// split city's binding engine. Without its row, require over a legacy binding
// (no revision column, so every fenced session write refuses) reported active.
func TestConditionalWritesStatusReportsTheBindingEngine(t *testing.T) {
	dir := t.TempDir()
	opened, err := beads.OpenSQLiteStore(dir)
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	if err := opened.(interface{ CloseStore() error }).CloseStore(); err != nil {
		t.Fatalf("CloseStore: %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "beads.sqlite"))
	if err != nil {
		t.Fatalf("opening the database: %v", err)
	}
	db.SetMaxOpenConns(1) // writable_schema is per connection
	// Dropping the column leaves the deployed pre-revision layout except for
	// the closing parenthesis, which SQLite pulls up onto the last column;
	// the layout check compares the schema text, so it is put back.
	for _, statement := range []string{
		"ALTER TABLE beads DROP COLUMN revision",
		"PRAGMA writable_schema=ON",
		"UPDATE sqlite_schema SET sql = replace(sql, 'bead_json TEXT NOT NULL)', 'bead_json TEXT NOT NULL )') WHERE name = 'beads'",
		"PRAGMA writable_schema=OFF",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("rewriting to the pre-revision layout (%s): %v", statement, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing the database: %v", err)
	}
	legacy, err := beads.OpenSQLiteStore(dir)
	if err != nil {
		t.Fatalf("reopening the legacy layout: %v", err)
	}
	t.Cleanup(func() { _ = legacy.(interface{ CloseStore() error }).CloseStore() })
	if err := beads.StampOpenedStore(legacy, "sqlite-graph", gate.Require, nil, nil); err != nil {
		t.Fatalf("StampOpenedStore: %v", err)
	}

	routes := &storageRoutes{stores: map[coordclass.Class]beads.Store{}, binding: "infra"}
	for _, class := range coordclass.Classes() {
		if class != coordclass.ClassWork {
			routes.stores[class] = legacy
		}
	}
	cs := &controllerState{
		rolloutFlags:  rollout.ForTest(rollout.WithBeadsConditionalWrites(rollout.Require)),
		cityBeadStore: beads.NewMemStore(),
		storageRoutes: routes,
	}
	got := cs.ConditionalWritesStatus()
	if got.Effective != "fail_closed" {
		t.Fatalf("effective = %q, want fail_closed for require over a legacy binding", got.Effective)
	}
	var rows []string
	for _, v := range got.Stores {
		if strings.HasPrefix(v.StoreID, "binding/") {
			rows = append(rows, v.StoreID)
			if v.StoreID != "binding/infra" || v.Kind != "sqlite-graph" || v.Capable || v.Probe != beads.ConditionalWriteProbeIncapable {
				t.Errorf("binding verdict = %+v, want binding/infra kind=sqlite-graph probe=incapable", v)
			}
		}
	}
	if len(rows) != 1 {
		t.Fatalf("binding rows = %v, want exactly one for the engine every relocated class shares", rows)
	}
}
