package config

import (
	"strings"
	"testing"
)

// R10 (split-default read audit): an ordinary city or rig name derives a
// reserved class prefix — gas-city-gateway -> gcg, gas-city-sandbox -> gcs,
// gas-city-main -> gcm, gas-city-ops -> gco, gas-city-nightly -> gcn. On a split
// city the binding mints gcg-N, gcs-N, ... in the same namespace as that work
// store, so ids collide and the binding wins every by-id read. Boot only warned.

func splitCityNamed(name string, rigs ...Rig) *City {
	return &City{
		Workspace: Workspace{Name: name},
		Rigs:      rigs,
		Storage: &StorageConfig{
			Classes: StorageClasses{
				Work: StorageWorkBinding, Graph: "infra", Sessions: "infra",
				Messaging: "infra", Orders: "infra", Nudges: "infra",
			},
			Bindings: map[string]StorageBindingConfig{"infra": {Provider: StorageProviderSQLiteBeads}},
		},
	}
}

func TestReservedPrefixSplitRefusalForEveryDerivedCollision(t *testing.T) {
	for name, prefix := range map[string]string{
		"gas-city-gateway": "gcg", "gas-city-sandbox": "gcs", "gas-city-main": "gcm",
		"gas-city-ops": "gco", "gas-city-nightly": "gcn",
	} {
		t.Run(name, func(t *testing.T) {
			err := ReservedPrefixSplitRefusal(splitCityNamed(name))
			if err == nil {
				t.Fatalf("a split city named %q (HQ prefix %q) was not refused", name, prefix)
			}
			for _, want := range []string{prefix, "workspace_prefix", "reserved"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestReservedPrefixSplitRefusalNamesTheRigAndItsFix(t *testing.T) {
	err := ReservedPrefixSplitRefusal(splitCityNamed("my-city", Rig{Name: "gas-city-gateway"}))
	if err == nil {
		t.Fatal("a split city with a rig deriving gcg was not refused")
	}
	for _, want := range []string{`"gas-city-gateway"`, "gcg", "prefix ="} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}
}

func TestReservedPrefixSplitRefusalAcceptsAnExplicitPrefix(t *testing.T) {
	cfg := splitCityNamed("gas-city-gateway", Rig{Name: "gas-city-ops", Prefix: "ops"})
	cfg.Workspace.Prefix = "gw"
	if err := ReservedPrefixSplitRefusal(cfg); err != nil {
		t.Errorf("explicit non-reserved prefixes were refused: %v", err)
	}
	cfg.ResolvedWorkspacePrefix = "gcg"
	if err := ReservedPrefixSplitRefusal(cfg); err == nil {
		t.Error("a site-bound reserved workspace prefix was not refused")
	}
}

// A city that relocates nothing keeps today's advisory: no refusal.
func TestReservedPrefixSplitRefusalLeavesSingleStoreCitiesToTheWarning(t *testing.T) {
	single := &City{Workspace: Workspace{Name: "gas-city-gateway"}, Rigs: []Rig{{Name: "gas-city-ops"}}}
	if err := ReservedPrefixSplitRefusal(single); err != nil {
		t.Errorf("a single-store city was refused: %v", err)
	}
	allWork := splitCityNamed("gas-city-gateway")
	allWork.Storage = &StorageConfig{Classes: defaultStorageConfig().Classes}
	if err := ReservedPrefixSplitRefusal(allWork); err != nil {
		t.Errorf("a city whose [storage] keeps every class on work was refused: %v", err)
	}
	if len(ReservedPrefixWarnings(single.Rigs, EffectiveHQPrefix(single))) != 2 {
		t.Error("the single-store advisory no longer warns about both collisions")
	}
}
