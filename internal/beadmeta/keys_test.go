package beadmeta

import (
	"strings"
	"testing"
)

// TestKnownMetadataKeysWellFormed asserts the declared vocabulary is internally
// consistent: every key is namespaced, non-empty, and unique. Identifier
// uniqueness is already guaranteed by the compiler (duplicate const names do not
// build); this covers the value side the compiler cannot see.
func TestKnownMetadataKeysWellFormed(t *testing.T) {
	if len(KnownMetadataKeys) == 0 {
		t.Fatal("KnownMetadataKeys is empty")
	}
	seen := make(map[string]struct{}, len(KnownMetadataKeys))
	for _, k := range KnownMetadataKeys {
		if k == "" {
			t.Error("KnownMetadataKeys contains an empty string")
			continue
		}
		if !strings.HasPrefix(k, Namespace) {
			t.Errorf("key %q does not start with namespace %q", k, Namespace)
		}
		if _, dup := seen[k]; dup {
			t.Errorf("key %q is declared more than once", k)
		}
		seen[k] = struct{}{}
	}
}

// TestKnownMetadataPrefixesWellFormed asserts every declared open-world prefix is
// namespaced and is a strict prefix (not a whole declared key).
func TestKnownMetadataPrefixesWellFormed(t *testing.T) {
	keys := make(map[string]struct{}, len(KnownMetadataKeys))
	for _, k := range KnownMetadataKeys {
		keys[k] = struct{}{}
	}
	for _, p := range KnownMetadataPrefixes {
		if !strings.HasPrefix(p, Namespace) {
			t.Errorf("prefix %q does not start with namespace %q", p, Namespace)
		}
		if _, isKey := keys[p]; isKey {
			t.Errorf("prefix %q is also declared as a whole key", p)
		}
	}
}

// TestPinnedValues guards the highest-churn keys against accidental value edits.
// These exact strings are part of the cross-module contract; renaming a value
// here without migrating call sites would silently break reads/writes, so they
// are pinned independently of the generator.
func TestPinnedValues(t *testing.T) {
	pinned := map[string]string{
		KindMetadataKey:              "gc.kind",
		RootBeadIDMetadataKey:        "gc.root_bead_id",
		StepRefMetadataKey:           "gc.step_ref",
		OutcomeMetadataKey:           "gc.outcome",
		RoutedToMetadataKey:          "gc.routed_to",
		ScopeRefMetadataKey:          "gc.scope_ref",
		AttemptMetadataKey:           "gc.attempt",
		ExecutionRoutedToMetadataKey: "gc.execution_routed_to",
		InstantiatingMetadataKey:     "gc.instantiating",
		FormulaVarPrefix:             "gc.var.",
		Namespace:                    "gc.",
		OptionMetadataPrefix:         "opt_",
		MoleculeFailedMetadataKey:    "molecule_failed",
		// The session-bead claim back-channel: `gc hook --claim` writes it and
		// `gc hook current` reads it back, so a value drift here would silently
		// break the only route a pool step has to its own bead id.
		CurrentClaimBeadIDMetadataKey: "current_claim_bead_id",
	}
	for got, want := range pinned {
		if got != want {
			t.Errorf("pinned value drift: got %q, want %q", got, want)
		}
	}
}

// TestMoleculeFailedIsExactEquality pins the one rule behind every dispatch
// reader of MoleculeFailedMetadataKey: exact equality with the value
// markFailed writes. Trimming or case-folding here would let the Go surfaces
// hide a row the jq twins (the pool-demand count-form, the control-ready scan)
// still count or serve — the drift the shared helper exists to close.
func TestMoleculeFailedIsExactEquality(t *testing.T) {
	cases := map[string]bool{
		"true":  true,
		"":      false,
		"false": false,
		"TRUE":  false,
		" true": false,
		"true ": false,
	}
	for value, want := range cases {
		if got := MoleculeFailed(value); got != want {
			t.Errorf("MoleculeFailed(%q) = %v, want %v", value, got, want)
		}
	}
}
