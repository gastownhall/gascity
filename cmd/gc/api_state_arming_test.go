package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/rollout"
)

const armingTestDir = "/arming-test-city"

func armingFlags(t *testing.T, cfg config.BeadsConfig, env map[string]string) rollout.Flags {
	t.Helper()
	city := &config.City{Workspace: config.Workspace{Name: "arming-test"}, Beads: cfg}
	flags, err := rollout.Resolve(city, rollout.ResolveOptions{LookupEnv: func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	}})
	if err != nil {
		t.Fatal(err)
	}
	return flags
}

func memStoreWithoutFencing() *beads.MemStore {
	store := beads.NewMemStore()
	store.DisableConditionalWrites = true
	return store
}

func bdStoreAdvertising(flag string) *beads.BdStore {
	return beads.NewBdStore(armingTestDir, func(_, _ string, args ...string) ([]byte, error) {
		if len(args) > 1 && args[1] == "--help" {
			return []byte("Usage: bd " + args[0] + "\nFlags: --json " + flag + "\n"), nil
		}
		return []byte("[]"), nil
	})
}

func requireGates() config.BeadsConfig {
	return config.BeadsConfig{ConditionalWrites: "require", GuardedRelease: "require"}
}

func assertArmingRefusal(t *testing.T, err error, scope, reason string) {
	t.Helper()
	var refusal *ArmingConfigurationError
	if !errors.As(err, &refusal) {
		t.Fatalf("scope %s armed or returned an untyped error (%v); want an ArmingConfigurationError containing %q", scope, err, reason)
	}
	if refusal.Scope != scope || !strings.Contains(refusal.Reason, reason) {
		t.Fatalf("refusal = %#v; want scope %q and a reason containing %q", refusal, scope, reason)
	}
}

func TestScopeArmingPrecondition(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    config.BeadsConfig
		env    map[string]string
		store  beads.Store
		reason string
	}{
		{"unset conditional_writes", config.BeadsConfig{GuardedRelease: "require"}, nil, beads.NewMemStore(), "beads.conditional_writes is unset"},
		{"unset guarded_release", config.BeadsConfig{ConditionalWrites: "require"}, nil, beads.NewMemStore(), "beads.guarded_release is unset"},
		{"both unset", config.BeadsConfig{}, nil, beads.NewMemStore(), "is unset"},
		{"auto conditional_writes", config.BeadsConfig{ConditionalWrites: "auto", GuardedRelease: "require"}, nil, beads.NewMemStore(), `beads.conditional_writes is "auto"`},
		{"auto guarded_release", config.BeadsConfig{ConditionalWrites: "require", GuardedRelease: "auto"}, nil, beads.NewMemStore(), `beads.guarded_release is "auto"`},
		{"off guarded_release", config.BeadsConfig{ConditionalWrites: "require", GuardedRelease: "off"}, nil, beads.NewMemStore(), `beads.guarded_release is "off"`},
		{"env off over config require", requireGates(), map[string]string{"GC_BEADS_CONDITIONAL_WRITES": "off"}, beads.NewMemStore(), `beads.conditional_writes is "off"`},
		{"auto on a store that cannot fence", config.BeadsConfig{ConditionalWrites: "auto", GuardedRelease: "require"}, nil, memStoreWithoutFencing(), `beads.conditional_writes is "auto"`},
		{"require on a store that cannot fence", requireGates(), nil, memStoreWithoutFencing(), "cannot fence"},
		{"require on a bd without --if-revision", requireGates(), nil, bdStoreAdvertising(""), "cannot fence"},
		{"require with no store", requireGates(), nil, nil, "cannot fence"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := scopeArmingPrecondition("city", tc.store, armingFlags(t, tc.cfg, tc.env))
			assertArmingRefusal(t, err, "city", tc.reason)
		})
	}
}

func TestScopeArmingPreconditionArmsExplicitRequireOnCapableStore(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cfg   config.BeadsConfig
		env   map[string]string
		store beads.Store
	}{
		{"config require on MemStore", requireGates(), nil, beads.NewMemStore()},
		{"env require on MemStore", config.BeadsConfig{}, map[string]string{"GC_BEADS_CONDITIONAL_WRITES": "require", "GC_BEADS_GUARDED_RELEASE": "require"}, beads.NewMemStore()},
		{"config require on a bd advertising --if-revision", requireGates(), nil, bdStoreAdvertising("--if-revision int")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := scopeArmingPrecondition("city", tc.store, armingFlags(t, tc.cfg, tc.env)); err != nil {
				t.Fatalf("explicit require on a capable store refused: %v", err)
			}
		})
	}
}

func bootControllerForArming(t *testing.T, cfg config.BeadsConfig, store beads.Store) *controllerState {
	t.Helper()
	for _, key := range []string{"GC_BEADS_CONDITIONAL_WRITES", "GC_BEADS_GUARDED_RELEASE"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	stubControllerCityStore(t, store)
	city := &config.City{Workspace: config.Workspace{Name: "arming-test"}, Beads: cfg}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cs := newControllerState(ctx, city, nil, nil, city.Workspace.Name, t.TempDir())
	if cs == nil || cs.CityName() != city.Workspace.Name {
		t.Fatal("controller did not boot")
	}
	return cs
}

func TestControllerBootsWhenScopeRefusesToArm(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    config.BeadsConfig
		store  beads.Store
		reason string
	}{
		{"unset gates", config.BeadsConfig{}, beads.NewMemStore(), "is unset"},
		{"auto on a store that cannot fence", config.BeadsConfig{ConditionalWrites: "auto", GuardedRelease: "auto"}, memStoreWithoutFencing(), `is "auto"`},
		{"require on a BdStore without --if-revision", requireGates(), bdStoreAdvertising(""), "cannot fence"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := bootControllerForArming(t, tc.cfg, tc.store)
			assertArmingRefusal(t, cs.ArmingReadiness("city"), "city", tc.reason)
		})
	}
}

func TestControllerArmingReadinessArmsExplicitRequireOnCapableStore(t *testing.T) {
	cs := bootControllerForArming(t, requireGates(), beads.NewMemStore())
	if err := cs.ArmingReadiness("city"); err != nil {
		t.Fatalf("explicit require on a capable city store refused: %v", err)
	}
}

func TestRecordArmingReadinessCoversEveryScope(t *testing.T) {
	cs := &controllerState{rolloutFlags: armingFlags(t, requireGates(), nil)}
	cs.cityBeadStore = beads.NewMemStore()
	cs.beadStores = map[string]beads.Store{"capable": beads.NewMemStore(), "missing": nil}
	cs.storageRoutes = messagingSplitRoutes(memStoreWithoutFencing())
	var warnings []string
	cs.rolloutLogf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }

	cs.recordArmingReadiness()

	for scope, reason := range map[string]string{
		"rig/missing":   "cannot fence",
		"binding/infra": "cannot fence",
		"unchecked":     "not checked at startup",
	} {
		assertArmingRefusal(t, cs.ArmingReadiness(scope), scope, reason)
	}
	for _, scope := range []string{"city", "rig/capable"} {
		if err := cs.ArmingReadiness(scope); err != nil {
			t.Fatalf("scope %s refused: %v", scope, err)
		}
	}
	if len(warnings) != 2 || !strings.Contains(warnings[0], "rig/missing") || !strings.Contains(warnings[1], "binding/infra") {
		t.Fatalf("warnings = %q; want one warning each for rig/missing and binding/infra", warnings)
	}
}

func twoEngineRoutes(first, rest beads.Store) *storageRoutes {
	routes := &storageRoutes{stores: make(map[coordclass.Class]beads.Store), binding: "infra"}
	assigned := false
	for _, class := range coordclass.Classes() {
		if !class.IsInfrastructure() {
			continue
		}
		if !assigned {
			routes.stores[class] = first
			assigned = true
			continue
		}
		routes.stores[class] = rest
	}
	return routes
}

func TestRecordArmingReadinessBindingRefusalIsStickyAcrossEngines(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first beads.Store
		rest  beads.Store
	}{
		{"capable then incapable", beads.NewMemStore(), memStoreWithoutFencing()},
		{"incapable then capable", memStoreWithoutFencing(), beads.NewMemStore()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := &controllerState{rolloutFlags: armingFlags(t, requireGates(), nil)}
			cs.cityBeadStore = beads.NewMemStore()
			cs.storageRoutes = twoEngineRoutes(tc.first, tc.rest)
			if got := len(cs.storageRoutes.distinctEngines()); got != 2 {
				t.Fatalf("distinct engines = %d; want 2", got)
			}
			cs.rolloutLogf = func(string, ...any) {}

			cs.recordArmingReadiness()

			assertArmingRefusal(t, cs.ArmingReadiness("binding/infra"), "binding/infra", "cannot fence")
		})
	}
}

func TestRecordArmingReadinessScopeNamespacesDoNotCollide(t *testing.T) {
	cs := &controllerState{rolloutFlags: armingFlags(t, requireGates(), nil)}
	cs.cityBeadStore = beads.NewMemStore()
	cs.beadStores = map[string]beads.Store{"infra": beads.NewMemStore()}
	cs.storageRoutes = messagingSplitRoutes(memStoreWithoutFencing())
	cs.rolloutLogf = func(string, ...any) {}

	cs.recordArmingReadiness()

	if err := cs.ArmingReadiness("rig/infra"); err != nil {
		t.Fatalf("rig/infra refused: %v", err)
	}
	assertArmingRefusal(t, cs.ArmingReadiness("binding/infra"), "binding/infra", "cannot fence")
}
