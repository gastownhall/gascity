package rollout

// This file is the canonical rollout-gate registry. It is CODEOWNERS-gated: a
// human owner reviews every Spec addition, Expires extension, and Category
// classification.
//
// The litmus for adding a gate here (all must hold, else it does not belong):
//  1. It selects between two MECHANICAL code paths (SelectsBetween), not agent
//     behavior — nothing a prompt could express, nothing a smarter model obviates.
//  2. It is terminal: a rollout/migration gate names when it dies (Expires +
//     VersionAnchor). Only a killswitch is long-lived.
//  3. Its value lives in its owning config section, read through internal/config;
//     this package never imports the consumer.

// ptr returns a pointer to v — the local literal helper for Default arms.
func ptr[T any](v T) *T { return &v }

// specs is the canonical registry. It is unexported so no test can append a
// phantom Spec that leaks into a sibling's ForTest defaults.
var specs = []Spec{
	{
		Key:            keyBeadsConditionalWrites,
		Category:       InfraRollout,
		ConfigPath:     "beads.conditional_writes",
		EnvOverride:    envBeadsConditionalWrites,
		EnvSemantics:   EnvOverrides,
		Default:        Default{Mode: ptr(Off)},
		Owner:          Owner{Bead: "ga-1ypn4t", GitHub: "@gastownhall/gascity-admin"},
		Expires:        "2027-01-15",
		VersionAnchor:  "BD_CONDITIONAL_WRITES_MIN_VERSION",
		SelectsBetween: [2]string{"unconditional bd write", "revision-guarded CAS write (bd --if-revision / UpdateIssueIfMatch)"},
		Justification: "Adopt beads whole-row compare-and-swap so gc.control_epoch and " +
			"gc.drain.reserved_by writes fail a lost race instead of silently clobbering a " +
			"concurrent peer; gated for mixed-fleet rollout while beads#4682 is untagged.",
	},
	{
		Key:            keyDaemonFormulaV2,
		Category:       InfraMigration,
		ConfigPath:     "daemon.formula_v2",
		EnvOverride:    "",
		Default:        Default{Bool: ptr(true)},
		Owner:          Owner{Bead: "ga-rdva30", GitHub: "@gastownhall/gascity-admin"},
		Expires:        "2026-12-31",
		VersionAnchor:  "gcFormulaV2RemovalFloor",
		SelectsBetween: [2]string{"formula v1 (legacy global-setter path)", "formula v2 (graph workflow path)"},
		Justification: "Retire the v1 formula path and its process-global atomic.Bool setter " +
			"anti-pattern; the migration whose completion deletes cmd/gc/feature_flags.go.",
	},
}

// Specs returns a defensive copy of the canonical registry. The Default pointers
// are deep-copied too, so a caller mutating a returned Spec's Default cannot
// reach through into the canonical registry.
func Specs() []Spec {
	out := make([]Spec, len(specs))
	copy(out, specs)
	for i := range out {
		if m := out[i].Default.Mode; m != nil {
			out[i].Default.Mode = ptr(*m)
		}
		if b := out[i].Default.Bool; b != nil {
			out[i].Default.Bool = ptr(*b)
		}
	}
	return out
}

// beadsConditionalWritesSpec returns the canonical Spec for the beads CAS gate
// (zero Spec if unregistered). It reads the package-private slice directly (no
// defensive copy needed for an internal, read-only lookup) so the resolver can
// source names/semantics from the registry. When a second gate needs a lookup,
// generalize this back to a by-key form — with one gate, a key parameter is a
// constant in disguise.
func beadsConditionalWritesSpec() Spec {
	for _, s := range specs {
		if s.Key == keyBeadsConditionalWrites {
			return s
		}
	}
	return Spec{}
}
