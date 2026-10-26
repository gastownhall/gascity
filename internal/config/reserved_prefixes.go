package config

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// The reserved-prefix table moved to internal/beadmeta, a stdlib-only leaf.
// config's dependency cone reaches internal/git, internal/remotesource and
// internal/worker/builtin -> internal/runtime, so every package that needed to
// ask "which namespace does this class mint under" was dragging a
// process-spawning cone behind it — internal/storeref imported config for this
// table alone. These re-exports keep every existing call site spelling the
// question through config.

// NudgeQueueIDPrefix is the prefix the nudge queue mints its records under.
const NudgeQueueIDPrefix = beadmeta.NudgeQueueIDPrefix

// ReservedClassPrefix returns the id-prefix a SQLite-relocated coordination
// class MINTS under (e.g. BeadClassOrders -> "gco"), and whether the class has
// one. Classes without a reserved prefix (e.g. BeadClassWork) return ("", false).
func ReservedClassPrefix(class string) (string, bool) { return beadmeta.ReservedClassPrefix(class) }

// ReservedClassPrefixesFor returns every reserved id-prefix belonging to a
// class — the one it mints under first, then any auxiliary namespaces its
// store holds. Classes with no reserved prefix return nil.
func ReservedClassPrefixesFor(class string) []string {
	return beadmeta.ReservedClassPrefixesFor(class)
}

// AllReservedClassPrefixes returns every reserved id-prefix across all classes,
// sorted. It is the namespace union an id is tested against when the class it
// might belong to is not known in advance.
func AllReservedClassPrefixes() []string { return beadmeta.AllReservedClassPrefixes() }

// ReservedClassPrefixes returns a copy of the class -> reserved-prefix map.
func ReservedClassPrefixes() map[string]string { return beadmeta.ReservedClassPrefixes() }

// IsReservedClassPrefix reports whether p (without a trailing "-") is a reserved
// class id-prefix. Case-insensitive, matching ValidateRigs' prefix handling.
func IsReservedClassPrefix(p string) bool { return beadmeta.IsReservedClassPrefix(p) }

// reservedClassPrefixListText returns the reserved class id-prefixes as a
// sorted, comma-separated string for use in validation error messages.
func reservedClassPrefixListText() string { return beadmeta.ReservedClassPrefixListText() }

// RelocatesAnyClass reports whether cfg's [storage] places any coordination
// class outside the work binding: a split city.
func RelocatesAnyClass(cfg *City) bool {
	storage := cfg.EffectiveStorage()
	for _, class := range storageConfigClassOrder() {
		if class == StorageClassWork {
			continue
		}
		if binding := storage.Classes.BindingFor(class); binding != "" && binding != StorageWorkBinding {
			return true
		}
	}
	return false
}

// ReservedPrefixSplitRefusal is the refusal for a SPLIT city whose HQ or rig
// prefix is a reserved class id-prefix (split-default read audit R10), or nil.
//
// On a split city the binding mints gcg-N, gcs-N, ... and a work store under
// the same prefix shares that namespace: storeref makes the binding the
// authority for a reserved-prefix id, and the event scan ties, so ids collide
// and work beads are answered from the wrong store. Ordinary names derive these
// prefixes (gas-city-gateway -> gcg), so the refusal names the fix: an explicit
// prefix. A city that relocates nothing keeps ReservedPrefixWarnings' advisory.
func ReservedPrefixSplitRefusal(cfg *City) error {
	if cfg == nil || !RelocatesAnyClass(cfg) {
		return nil
	}
	var problems []string
	if hq := strings.ToLower(strings.TrimSpace(EffectiveHQPrefix(cfg))); IsReservedClassPrefix(hq) {
		problems = append(problems, fmt.Sprintf("the city's bead prefix %q (derived from %q unless set) is reserved; set an explicit prefix: workspace_prefix = \"<prefix>\" in .gc/site.toml", hq, cfg.EffectiveCityName()))
	}
	for _, r := range cfg.Rigs {
		if prefix := strings.ToLower(strings.TrimSpace(r.EffectivePrefix())); IsReservedClassPrefix(prefix) {
			problems = append(problems, fmt.Sprintf("rig %q bead prefix %q is reserved; set an explicit prefix: prefix = \"<prefix>\" on that [[rigs]] entry", r.Name, prefix))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("this city splits its coordination classes into a binding that mints ids under the reserved prefixes (%s), and %s. A work store under a reserved prefix shares the binding's id namespace, so their ids collide. Choose a prefix outside the reserved set and start again",
		reservedClassPrefixListText(), strings.Join(problems, "; and "))
}
