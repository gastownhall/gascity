package main

// The namespaces a binding declares, on the CLI plane, and the front door that
// decides an id is not bd's to answer for.
//
// Both read the reserved set, and both were reading only the prefixes each
// class MINTS under. The nudges store additionally holds the nudge queue's
// "gcnq-…" records, whose ids come from a content hash of the nudge they carry
// rather than from the store's sequence. Unlisted, such an id is planned as an
// ordinary work id: `gc bd show gcnq-…` goes to bd, which does not have it.

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
)

// The retirement condition's two halves, as the CLI plane reports them.
//
// The rule itself is now one function (storeref.BuildBindings), so this is no
// longer a second derivation of it — it is a pin on what this plane ASKS FOR.
// The options are the drift surface that survives sharing the body: a plane
// that started supplying a census where it holds none, or stopped, would report
// a different retirement verdict for the same store without either package
// changing the rule.
func TestCLIBindingReportsBothHalvesOfTheRetirementCondition(t *testing.T) {
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
			bindings, _ := residencyBindingsFor(
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

func TestReservedPrefixesForDeclaresHeldNamespaces(t *testing.T) {
	got := map[string]bool{}
	for _, p := range storeref.ReservedPrefixesFor([]coordclass.Class{coordclass.ClassNudges}) {
		got[p] = true
	}
	for _, want := range config.ReservedClassPrefixesFor(config.BeadClassNudges) {
		if !got[want] {
			t.Errorf("the nudges binding does not declare %q; an id in that namespace resolves past the binding", want)
		}
	}
}

func TestBdByIDFrontDoorClaimsHeldNamespaces(t *testing.T) {
	for _, prefix := range config.AllReservedClassPrefixes() {
		id := prefix + "-abc"
		if !bdIDIsClassReserved(id) {
			t.Errorf("bdIDIsClassReserved(%q) = false; the by-id front door hands a class id to bd, which cannot answer for it", id)
		}
	}
	// The control: a work id must stay bd's to answer.
	if bdIDIsClassReserved("ga-abc") {
		t.Error("bdIDIsClassReserved claimed a work id; the front door would divert every ordinary read")
	}
}
