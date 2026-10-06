package qualification_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// q1StorageClasses is the closed set of semantic classes Q1 must account for.
// The count is cross-checked against config.StorageClasses by reflection in
// TestQ1DefaultCityMapsEveryClassToTheReservedWorkBinding, so adding a seventh
// class to the config type without a Q1 row fails this suite instead of
// silently escaping it.
var q1StorageClasses = []config.StorageClass{
	config.StorageClassWork,
	config.StorageClassGraph,
	config.StorageClassSessions,
	config.StorageClassMessaging,
	config.StorageClassOrders,
	config.StorageClassNudges,
}

// TestQ1DefaultCityMapsEveryClassToTheReservedWorkBinding is the compatibility
// contract read off
// the deployed artifacts: every example city this repo ships that authors no
// [storage] section resolves all six semantic classes to the reserved `work`
// binding and defines no binding at all.
//
// The corpus is the shipped examples rather than a literal written here, so the
// assertion is about cities that exist independently of the storage work. The
// examples/storage city is the control: it DOES author [storage], and it must
// resolve differently — without that leg a resolver that returned the default
// map unconditionally would pass.
func TestQ1DefaultCityMapsEveryClassToTheReservedWorkBinding(t *testing.T) {
	fields := reflect.TypeOf(config.StorageClasses{}).NumField()
	if fields != len(q1StorageClasses) {
		t.Fatalf("config.StorageClasses has %d classes, Q1 accounts for %d; every semantic class needs a Q1 row", fields, len(q1StorageClasses))
	}

	defaults, explicit := 0, 0
	for _, shipped := range q1ShippedCities(t) {
		t.Run(shipped.Name, func(t *testing.T) {
			storage := shipped.City.EffectiveStorage()
			if shipped.City.Storage != nil {
				explicit++
				assigned := map[string]bool{}
				for _, class := range q1StorageClasses {
					assigned[storage.Classes.BindingFor(class)] = true
				}
				if len(assigned) == 1 && assigned[config.StorageWorkBinding] {
					t.Fatalf("%s authors [storage] but resolves every class to %q; it cannot serve as the control for the default map",
						shipped.Path, config.StorageWorkBinding)
				}
				return
			}
			defaults++
			for _, class := range q1StorageClasses {
				if got := storage.Classes.BindingFor(class); got != config.StorageWorkBinding {
					t.Errorf("%s authors no [storage] but class %s resolved to binding %q, want the reserved %q",
						shipped.Path, class, got, config.StorageWorkBinding)
				}
			}
			if len(storage.Bindings) != 0 {
				t.Errorf("%s authors no [storage] but resolved %d binding definitions, want none", shipped.Path, len(storage.Bindings))
			}
		})
	}
	if defaults == 0 {
		t.Fatal("no shipped example city authors a default (no [storage]) configuration; the default-city corpus is empty")
	}
	if explicit == 0 {
		t.Fatal("no shipped example city authors an explicit [storage] configuration; the default map has no control")
	}
}

// TestQ1MinimalDefaultCityNeedsNoStorageAuthoring pins the same claim for the
// smallest city that loads at all, and for a city whose include fragments carry
// the rest of its configuration. Composition is where a default could be lost:
// a fragment that materialized an empty [storage] table would turn "omitted"
// into "authored but incomplete", which validation rejects.
func TestQ1MinimalDefaultCityNeedsNoStorageAuthoring(t *testing.T) {
	cases := map[string]map[string]string{
		"single file": {"city.toml": q1MinimalNoStorageCity},
		"composed from an include fragment": {
			"city.toml": "include = [\"agents.toml\"]\n" + q1MinimalNoStorageCity,
			"agents.toml": `
[[agent]]
name = "worker"
scope = "city"
`,
		},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			city := q1LoadCityFrom(t, files)
			if city.Storage != nil {
				t.Fatalf("loading materialized a [storage] section for a city that authors none: %+v", city.Storage)
			}
			if err := config.ValidateStorageConfig(city); err != nil {
				t.Fatalf("validating a city with no [storage]: %v", err)
			}
			storage := city.EffectiveStorage()
			for _, class := range q1StorageClasses {
				if got := storage.Classes.BindingFor(class); got != config.StorageWorkBinding {
					t.Errorf("class %s resolved to %q, want the reserved %q", class, got, config.StorageWorkBinding)
				}
			}
		})
	}
}

// TestQ1DefaultCityConsultsNoStorageProvider is the "no provider registration,
// no provider configuration" half of Q1, at the configuration front door.
//
// config.ValidateStorageBindings is the pre-open seam a provider bundle uses to
// reject unknown providers and provider-owned configuration. A default city
// must never reach it: the validator is not called, and passing no validator at
// all still succeeds. The explicit-[storage] control proves the seam is live —
// it calls the validator exactly once, for the one binding its classes select.
func TestQ1DefaultCityConsultsNoStorageProvider(t *testing.T) {
	t.Run("default city never reaches the provider seam", func(t *testing.T) {
		city := q1LoadCityFrom(t, map[string]string{"city.toml": q1MinimalNoStorageCity})
		calls := 0
		if err := config.ValidateStorageBindings(city, func(name string, binding config.StorageBindingConfig, assigned []config.StorageClass) error {
			calls++
			t.Errorf("the provider validator was invoked for binding %q (provider %q, classes %v); a default city has no provider-backed binding",
				name, binding.Provider, assigned)
			return nil
		}); err != nil {
			t.Fatalf("validating storage bindings for a default city: %v", err)
		}
		if calls != 0 {
			t.Errorf("provider validator invoked %d times, want 0", calls)
		}
		if err := config.ValidateStorageBindings(city, nil); err != nil {
			t.Fatalf("a default city required a provider validator: %v", err)
		}
	})

	t.Run("explicit storage still reaches the provider seam", func(t *testing.T) {
		city := q1LoadCityFrom(t, map[string]string{"city.toml": q1MinimalNoStorageCity + `
[storage.classes]
work = "work"
graph = "infra"
sessions = "infra"
messaging = "infra"
orders = "infra"
nudges = "infra"

[storage.bindings.infra]
provider = "sqlite-beads"
`})
		var seen []string
		if err := config.ValidateStorageBindings(city, func(name string, binding config.StorageBindingConfig, assigned []config.StorageClass) error {
			names := make([]string, len(assigned))
			for index, class := range assigned {
				names[index] = class.String()
			}
			seen = append(seen, name+"/"+binding.Provider+"["+strings.Join(names, " ")+"]")
			return nil
		}); err != nil {
			t.Fatalf("validating storage bindings for an explicit city: %v", err)
		}
		want := []string{"infra/sqlite-beads[graph sessions messaging orders nudges]"}
		if !reflect.DeepEqual(seen, want) {
			t.Fatalf("provider seam saw %v, want %v; the default-city assertion above is only evidence if this seam is live", seen, want)
		}
	})
}

// TestQ1DefaultCityReloadNeverRequiresRestart pins the operational consequence
// of that contract for a running default city: because storage handles are immutable,
// gc reloads config in place only while the effective storage resolution is
// unchanged. Two default cities always agree, however much else differs — so no
// pre-existing city acquires a storage-driven restart it did not have before.
//
// The control is the pair that must disagree: introducing [storage] is exactly
// the edit that forces a restart, and without that leg an Equal that returned
// true unconditionally would pass.
func TestQ1DefaultCityReloadNeverRequiresRestart(t *testing.T) {
	current := q1LoadCityFrom(t, map[string]string{"city.toml": q1MinimalNoStorageCity})
	next := q1LoadCityFrom(t, map[string]string{"city.toml": q1MinimalNoStorageCity + `
[[agent]]
name = "added-after-boot"
scope = "city"
`})
	if config.StorageReloadRequiresRestart(current, next) {
		t.Error("an unrelated edit to a default city demanded a storage restart")
	}
	if config.StorageReloadRequiresRestart(next, current) {
		t.Error("reverting an unrelated edit to a default city demanded a storage restart")
	}

	relocated := q1LoadCityFrom(t, map[string]string{"city.toml": q1MinimalNoStorageCity + `
[storage.classes]
work = "work"
graph = "infra"
sessions = "infra"
messaging = "infra"
orders = "infra"
nudges = "infra"

[storage.bindings.infra]
provider = "sqlite-beads"
`})
	if !config.StorageReloadRequiresRestart(current, relocated) {
		t.Fatal("adopting [storage] did not require a restart; the default-city assertions above are vacuous if every reload compares equal")
	}
}
