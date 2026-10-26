package config

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/fsys"
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

// ReservedPrefixCollision reports the reserved class id-prefix a work-store
// prefix collides with, if any: the prefix itself, or its first hyphen
// segment. A rig prefixed "gcg-alpha" mints "gcg-alpha-1", which is inside the
// binding's "gcg" namespace (storeref.IDInNamespace), so it collides exactly
// as "gcg" does.
func ReservedPrefixCollision(prefix string) (string, bool) {
	p := strings.ToLower(strings.TrimSpace(prefix))
	if p == "" {
		return "", false
	}
	if IsReservedClassPrefix(p) {
		return p, true
	}
	if first, _, cut := strings.Cut(p, "-"); cut && IsReservedClassPrefix(first) {
		return first, true
	}
	return "", false
}

// ReservedPrefixSplitRefusal is the refusal for a SPLIT city whose HQ or rig
// prefix collides with a reserved class id-prefix (split-default read audit
// R10), or nil.
//
// On a split city the binding mints gcg-N, gcs-N, ... and a work store under
// the same namespace collides with it: storeref makes the binding the
// authority for a reserved-namespace id, and the event scan ties, so ids
// collide and work beads are answered from the wrong store. Ordinary names
// derive these prefixes (gas-city-gateway -> gcg), so the refusal names the
// fix: an explicit prefix. A city that relocates nothing keeps
// ReservedPrefixWarnings' advisory.
func ReservedPrefixSplitRefusal(cfg *City) error {
	if cfg == nil || !RelocatesAnyClass(cfg) {
		return nil
	}
	var problems []string
	if hq := strings.ToLower(strings.TrimSpace(EffectiveHQPrefix(cfg))); hq != "" {
		if reserved, ok := ReservedPrefixCollision(hq); ok {
			problems = append(problems, fmt.Sprintf("the city's bead prefix %q (derived from %q unless set) is in the reserved %q namespace; set an explicit prefix: workspace_prefix = \"<prefix>\" in .gc/site.toml", hq, cfg.EffectiveCityName(), reserved))
		}
	}
	for _, r := range cfg.Rigs {
		prefix := strings.ToLower(strings.TrimSpace(r.EffectivePrefix()))
		if reserved, ok := ReservedPrefixCollision(prefix); ok {
			problems = append(problems, fmt.Sprintf("rig %q bead prefix %q is in the reserved %q namespace; set an explicit prefix: prefix = \"<prefix>\" on that [[rigs]] entry", r.Name, prefix, reserved))
		}
	}
	// A raw (unpatched) config being edited still carries its [[patches.rigs]]
	// entries; a prefix one sets is the prefix the rig will mint under.
	for _, p := range cfg.Patches.Rigs {
		if p.Prefix == nil {
			continue
		}
		prefix := strings.ToLower(strings.TrimSpace(*p.Prefix))
		if reserved, ok := ReservedPrefixCollision(prefix); ok {
			problems = append(problems, fmt.Sprintf("the rig patch for %q sets bead prefix %q, in the reserved %q namespace; choose another prefix", p.Name, prefix, reserved))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("this city splits its coordination classes into a binding that mints ids under the reserved prefixes (%s), and %s. A work store whose prefix is, or starts with, a reserved prefix shares the binding's id namespace, so their ids collide. Choose a prefix whose first segment is outside the reserved set",
		reservedClassPrefixListText(), strings.Join(problems, "; and "))
}

// ReservedPrefixSplitRefusalForEdit is ReservedPrefixSplitRefusal for a
// config about to be WRITTEN: next is the raw (pre-include) config an edit
// produced for the city at tomlPath. A raw config does not carry a [storage]
// declared in an include fragment, so when next names none and has fragments
// the composed config on disk decides whether the city is split. A composed
// load that fails decides nothing here; the boot gate still refuses.
//
// It is the check where a reserved prefix is INTRODUCED (gc rig add, the API
// rig create and rig/config edits, gc init --from): refusing at boot alone
// lets one command write a config every later CLI process refuses.
func ReservedPrefixSplitRefusalForEdit(fs fsys.FS, tomlPath string, next *City) error {
	if next == nil {
		return nil
	}
	if RelocatesAnyClass(next) {
		return ReservedPrefixSplitRefusal(next)
	}
	if len(next.Include) == 0 || fs == nil || tomlPath == "" {
		return nil
	}
	composed, _, err := LoadWithIncludes(fs, tomlPath)
	if err != nil || !RelocatesAnyClass(composed) {
		return nil
	}
	check := *next
	check.Storage = composed.Storage
	return ReservedPrefixSplitRefusal(&check)
}
