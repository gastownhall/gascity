package splittest

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/storeref"
)

// TestNewClassStoreCoversEveryReservedClass pins the generic keying: the kit
// reads config.ReservedClassPrefixes rather than hardcoding a prefix, so a
// class that gains, loses, or changes its reserved prefix moves the kit with
// it instead of leaving a stale literal behind.
func TestNewClassStoreCoversEveryReservedClass(t *testing.T) {
	t.Parallel()
	reserved := config.ReservedClassPrefixes()
	if len(reserved) == 0 {
		t.Fatal("config declares no reserved class prefixes")
	}
	for class, prefix := range reserved {
		store := NewClassStore(t, class)
		if got := store.(storeref.HasIDPrefix).IDPrefix(); got != prefix {
			t.Errorf("class %q: IDPrefix() = %q, want %q", class, got, prefix)
		}
		minted, err := store.Create(beads.Bead{Title: class})
		if err != nil {
			t.Errorf("class %q: create: %v", class, err)
			continue
		}
		if !strings.HasPrefix(minted.ID, prefix+"-") {
			t.Errorf("class %q: minted %q, want the %q namespace", class, minted.ID, prefix)
		}
		// Every class store is FENCED to the namespaces its binding claims, the
		// way OpenEngine fences the real one, so a work-prefixed pinned id is
		// refused rather than landed. The refusal carries the sentinel, which is
		// how a caller tells "route this elsewhere" from "this bead could not be
		// created"; what the namespace set admits is pinned in
		// TestNewClassStoreIsFencedToEveryNamespaceItsClassHolds.
		if _, err := store.Create(beads.Bead{ID: "gc-foreign", Title: "foreign"}); !errors.Is(err, beads.ErrPinnedIDOutsideNamespace) {
			t.Errorf("class %q: a work-prefixed pinned id got %v, want ErrPinnedIDOutsideNamespace", class, err)
		}
	}
}

// TestClassStoresAreMutuallyPrefixDisjoint pins the property the whole kit
// rests on: no two class stores claim the same id namespace, so by-id routing
// across a real city's store set is unambiguous.
func TestClassStoresAreMutuallyPrefixDisjoint(t *testing.T) {
	t.Parallel()
	seen := map[string]string{}
	for class, prefix := range config.ReservedClassPrefixes() {
		if other, dup := seen[prefix]; dup {
			t.Errorf("classes %q and %q both mint under %q", other, class, prefix)
		}
		seen[prefix] = class
		if prefix == defaultWorkPrefix {
			t.Errorf("class %q mints under the work prefix %q; work and class ids would be indistinguishable", class, defaultWorkPrefix)
		}
	}
}

func TestClassPrefixRejectsClassesWithoutOne(t *testing.T) {
	t.Parallel()
	if _, err := classPrefix(config.BeadClassWork); err == nil {
		t.Fatal("classPrefix accepted the work class; work beads mint under a rig/HQ EffectivePrefix, not a reserved prefix")
	}
	if _, err := classPrefix("no-such-class"); err == nil {
		t.Fatal("classPrefix accepted an unknown class")
	}
	prefix, err := classPrefix(config.BeadClassGraph)
	if err != nil {
		t.Fatalf("classPrefix(graph): %v", err)
	}
	if prefix != "gcg" {
		t.Errorf("classPrefix(graph) = %q, want %q", prefix, "gcg")
	}
}

func TestWorkPrefixRejectsNonWorkPrefixes(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"", "   ", "-", "gcg", "GCG-", "gco"} {
		if got, err := workPrefix(bad); err == nil {
			t.Errorf("workPrefix(%q) = %q, want an error", bad, got)
		}
	}
	got, err := workPrefix(" RA- ")
	if err != nil {
		t.Fatalf("workPrefix(rig prefix): %v", err)
	}
	if got != "ra" {
		t.Errorf("workPrefix(\" RA- \") = %q, want %q", got, "ra")
	}
}

// TestStoreTrioRoutesByPrefix pins the third-store case a real city has: an HQ
// work store, a coordination-class store, and a per-rig work store. All three
// must be routable by id alone.
func TestStoreTrioRoutesByPrefix(t *testing.T) {
	t.Parallel()
	hq, graph := NewSplitStores(t)
	rig := NewWorkStore(t, "ra")
	stores := []beads.Store{hq, graph, rig}

	rigBead, err := rig.Create(beads.Bead{ID: "ra-1", Title: "rig work"})
	if err != nil {
		t.Fatalf("create rig bead: %v", err)
	}
	for _, tc := range []struct {
		id   string
		want beads.Store
	}{
		{rigBead.ID, rig},
		{"gc-1", hq},
		{"gcg-wisp-abc", graph},
	} {
		if got := storeref.PrefixOwner(tc.id, stores); got != tc.want {
			t.Errorf("PrefixOwner(%q) routed to the wrong store", tc.id)
		}
	}
	// A rig work store is no more permissive than the HQ one.
	if _, err := rig.Create(beads.Bead{ID: "gcg-wisp-abc", Title: "graph wisp in a rig store"}); err == nil {
		t.Error("rig work store accepted a graph-prefixed id")
	}
	if err := rig.DepAdd(rigBead.ID, "gcg-wisp-abc", "blocks"); err == nil {
		t.Error("rig work store accepted a cross-store dep")
	}
}
