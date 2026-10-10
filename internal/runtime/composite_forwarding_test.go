package runtime_test

import (
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/runtime/hybrid"
)

// unforwarded is each composite's reviewed list of optional interfaces a
// leaf implements and it does not, by composite then interface. Each entry
// is deliberate, its reason saying what reads the leaf instead, or a gap,
// with its bead. The list only shrinks: a composite that gains one fails until
// its entry is dropped.
var unforwarded = map[string]map[string]struct{ Class, Reason, Bead string }{
	"auto": {
		"FreshByConstruction":     {"deliberate", "a leaf's own read; readRuntime judges it on the routed leaf (cmd/gc/reconcile_effect_runtime.go freshReadable)", ""},
		"IdentitySidecarProvider": {"deliberate", "identity is read on the routed leaf (cmd/gc/runtime_inventory_lane.go identityReadable)", ""},
		"InventoryProvider":       {"deliberate", "the inventory lane reads each backend's leaf through Backends (cmd/gc/runtime_inventory_lane.go)", ""},
	},
	"hybrid": {
		"FreshByConstruction":         {"deliberate", "as auto's", ""},
		"IdentitySidecarProvider":     {"deliberate", "as auto's", ""},
		"InventoryProvider":           {"deliberate", "as auto's", ""},
		"FreshLivenessObserver":       {"deliberate", "hybrid's read is its leaves' cached one, so a hop through it proves absence only by its attested listing (cmd/gc/reconcile_effect_runtime.go unlistedElsewhere)", ""},
		"TransportCapabilityProvider": {"deliberate", "production's hybrid is tmux over k8s (cmd/gc/providers.go newHybridProvider), and neither has it", ""},
	},
}

// Kills a composite that drops a capability a leaf has, unreviewed: auto and
// hybrid implement every optional interface any production backend does,
// but for their reviewed entries, and no entry names one they implement.
func TestCompositesForwardEveryLeafCapability(t *testing.T) {
	var leaves []string
	for _, p := range productionBackends(t) {
		for _, name := range implemented(p) {
			if !slices.Contains(leaves, name) {
				leaves = append(leaves, name)
			}
		}
	}
	slices.Sort(leaves)
	f := func() runtime.Provider { return runtime.NewFake() }
	composites := map[string]runtime.Provider{
		"auto":   auto.New(f(), f()),
		"hybrid": hybrid.New(f(), f(), func(string) bool { return false }),
	}
	for name, c := range composites {
		has := implemented(c)
		for _, iface := range leaves {
			e, listed := unforwarded[name][iface]
			switch forwards := slices.Contains(has, iface); {
			case !forwards && !listed:
				t.Errorf("%s does not implement %s, which a leaf does: forward it, or list it with a reason", name, iface)
			case forwards && listed:
				t.Errorf("%s implements %s now: drop its unforwarded entry", name, iface)
			case listed && (e.Reason == "" || e.Class != "deliberate" && e.Class != "gap" || (e.Bead == "") == (e.Class == "gap")):
				t.Errorf("%s's %s entry: a class, a reason, and a bead exactly when it is a gap", name, iface)
			}
		}
		for iface := range unforwarded[name] {
			if !slices.Contains(leaves, iface) {
				t.Errorf("%s's entry %s: no leaf implements it", name, iface)
			}
		}
	}
}
