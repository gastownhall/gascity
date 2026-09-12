package config

import "github.com/gastownhall/gascity/internal/beadmeta"

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
