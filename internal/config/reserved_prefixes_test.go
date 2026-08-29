package config

// The reserved id namespaces, and the auxiliary one the registry did not know
// about.
//
// A class mints under one prefix, but a class STORE can hold beads under more
// than one: the nudge queue mints its own records as "gcnq-…" inside the nudges
// binding (internal/storebinding/beads_nudge_queue.go's nudgeQueueIDPrefix).
// That prefix was never registered, so every consumer that asks "is this id
// class-reserved" — the rig/HQ prefix validator, the `gc bd` by-id front door,
// the residency topology's binding namespaces — answered no for a queue id.
//
// The consequences are asymmetric and both bad. A rig could be configured with
// prefix "gcnq" and collide with the queue's own ids, and a by-id read of a
// queue bead falls through to the work ledger, which answers it emptily and
// confidently. Registration is what makes the union match the reality.

import (
	"testing"
)

func TestNudgeQueuePrefixIsReserved(t *testing.T) {
	if !IsReservedClassPrefix("gcnq") {
		t.Error("gcnq is not reserved; a rig could be configured with that prefix and collide with the nudge queue's own bead ids")
	}
	// Case-insensitive, matching ValidateRigs' prefix handling.
	if !IsReservedClassPrefix("  GCNQ ") {
		t.Error("gcnq is not reserved case-insensitively; the rig validator lowercases before comparing")
	}
}

// The auxiliary prefix must not displace the one the class MINTS under. A
// binding's store is opened with ReservedClassPrefix as its IDPrefix, so a
// change there is a change to every id the class creates from then on.
func TestReservedClassPrefixStillNamesTheMintPrefix(t *testing.T) {
	got, ok := ReservedClassPrefix(BeadClassNudges)
	if !ok {
		t.Fatal("nudges has no reserved mint prefix")
	}
	if got != "gcn" {
		t.Fatalf("nudges mints under %q, want gcn — this is the store's IDPrefix, not the namespace union", got)
	}
}

func TestReservedClassPrefixesForCoversAuxiliaryNamespaces(t *testing.T) {
	tests := []struct {
		class string
		want  []string
	}{
		{BeadClassNudges, []string{"gcn", "gcnq"}},
		{BeadClassGraph, []string{"gcg"}},
		{BeadClassWork, nil},
	}
	for _, tt := range tests {
		t.Run(tt.class, func(t *testing.T) {
			got := ReservedClassPrefixesFor(tt.class)
			if len(got) != len(tt.want) {
				t.Fatalf("ReservedClassPrefixesFor(%q) = %v, want %v", tt.class, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("ReservedClassPrefixesFor(%q) = %v, want %v (mint prefix first)", tt.class, got, tt.want)
				}
			}
		})
	}
}

// Every prefix the registry knows must be reserved, and the union must be the
// concatenation of the per-class unions. Stated as a property rather than a
// literal list so registering a sixth class cannot leave one accessor behind.
func TestReservedPrefixUnionAgreesWithPerClassUnions(t *testing.T) {
	union := map[string]bool{}
	for _, p := range AllReservedClassPrefixes() {
		if !IsReservedClassPrefix(p) {
			t.Errorf("AllReservedClassPrefixes returned %q, which IsReservedClassPrefix denies", p)
		}
		union[p] = true
	}
	for class := range reservedClassPrefixes {
		for _, p := range ReservedClassPrefixesFor(class) {
			if !union[p] {
				t.Errorf("class %q reserves %q, which is missing from AllReservedClassPrefixes", class, p)
			}
		}
	}
}

// The validator's message names what it refuses. An operator told "gcnq is
// reserved" and then shown a list without gcnq in it has been told the rule and
// shown a contradiction.
func TestReservedPrefixListTextNamesEveryReservedPrefix(t *testing.T) {
	text := reservedClassPrefixListText()
	for _, p := range AllReservedClassPrefixes() {
		if !containsWord(text, p) {
			t.Errorf("the validator's reserved-prefix list %q omits %q, which it refuses", text, p)
		}
	}
}

func containsWord(text, word string) bool {
	for _, field := range splitCommaSpace(text) {
		if field == word {
			return true
		}
	}
	return false
}

func splitCommaSpace(text string) []string {
	var out []string
	cur := ""
	for _, r := range text {
		if r == ',' || r == ' ' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
