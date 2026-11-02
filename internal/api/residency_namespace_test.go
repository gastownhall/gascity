package api

// The namespaces a binding declares, on the API plane.
//
// A binding's Prefixes list is what the resolver reads to decide it has
// AUTHORITY over an id rather than merely a guess worth probing. Building that
// list from the class's MINT prefix alone understates it: the nudges store also
// holds the nudge queue's own "gcnq-…" records, which a subsystem mints from a
// content hash rather than from the store's sequence. An unlisted namespace is
// not a weaker claim, it is no claim — the id falls through to the work ledger,
// which answers it emptily and confidently, and on a city whose probe has been
// retired there is not even a binding leg left to catch it.

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// The two halves of the residence-probe retirement condition, as this plane
// reports them.
//
// The mint bit is observed rather than assumed: a store that names the
// namespace it mints into, and names one the binding claims, has verified the
// only thing the retirement premise needs about future beads. A store that
// names nothing has verified nothing.
//
// The relic bit is the other half, and this row supplies no census — so it
// pins the default. "Not asked" is not "not known to hold relics", and neither
// is the claim that retires a probe. An observed mint bit over an optimistic
// relic bit would retire the probe on every converged city at boot, over
// exactly the ids `gc storage migrate` preserved.
func TestAPIBindingReportsBothHalvesOfTheRetirementCondition(t *testing.T) {
	graphPrefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok {
		t.Fatal("graph has no reserved mint prefix")
	}
	tests := []struct {
		name  string
		store beads.Store
		mints bool
	}{
		{"store declaring the binding's namespace", newPrefixDeclaringStore(graphPrefix), true},
		{"store minting work ids", newPrefixDeclaringStore("ga"), false},
		{"store declaring nothing", beads.NewMemStore(), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bindings, _ := apiResidencyBindings(
				[]beads.Store{tt.store},
				map[beads.Store][]coordclass.Class{tt.store: {coordclass.ClassGraph}},
				nil,
			)
			if len(bindings) != 1 {
				t.Fatalf("got %d bindings, want 1", len(bindings))
			}
			if bindings[0].MintsReserved != tt.mints {
				t.Errorf("MintsReserved = %v, want %v", bindings[0].MintsReserved, tt.mints)
			}
			if !bindings[0].HasLegacyResidents {
				t.Error("HasLegacyResidents = false for a caller that supplied no census; an unasked question is not a clean answer, and the probe would retire over every id the migration preserved")
			}
		})
	}
}

// prefixDeclaringStore names the namespace it mints into, which beads.MemStore
// does not.
type prefixDeclaringStore struct {
	beads.Store
	prefix string
}

func newPrefixDeclaringStore(prefix string) beads.Store {
	return prefixDeclaringStore{Store: beads.NewMemStore(), prefix: prefix}
}

func (s prefixDeclaringStore) IDPrefix() string { return s.prefix }

func TestAPIBindingDeclaresEveryHeldNamespace(t *testing.T) {
	tests := []struct {
		name     string
		observed []coordclass.Class
		want     []string
	}{
		{
			// A binding serving only the nudges class still holds the queue's
			// namespace, because the queue lives in the nudges store.
			name:     "nudges alone",
			observed: []coordclass.Class{coordclass.ClassNudges},
			want:     config.ReservedClassPrefixesFor(config.BeadClassNudges),
		},
		{
			// Observing every observable class is the whole-split shape:
			// completeObservedClasses rounds it up to all five infrastructure
			// classes, so the binding must declare the full reserved union.
			name:     "whole split",
			observed: observableInfraClasses(),
			want:     config.AllReservedClassPrefixes(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := beads.NewMemStore()
			bindings, refused := apiResidencyBindings(
				[]beads.Store{store},
				map[beads.Store][]coordclass.Class{store: tt.observed},
				nil,
			)
			if refused != nil {
				t.Fatalf("unexpected refusal: %v", refused)
			}
			if len(bindings) != 1 {
				t.Fatalf("got %d bindings, want 1", len(bindings))
			}
			got := map[string]bool{}
			for _, p := range bindings[0].Prefixes {
				got[p] = true
			}
			for _, want := range tt.want {
				if !got[want] {
					t.Errorf("binding declares %v, missing %q: an id in that namespace resolves past the binding to the work ledger", bindings[0].Prefixes, want)
				}
			}
		})
	}
}
