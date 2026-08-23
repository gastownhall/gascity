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
