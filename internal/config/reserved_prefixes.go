package config

import (
	"sort"
	"strings"
)

// reservedClassPrefixes maps each SQLite-relocated coordination class to the
// non-configurable bead-ID prefix its embedded store mints. This is the single
// source of truth, consolidating cmd/gc's classSQLitePrefix map and the
// graphStoreIDPrefix constant. Distinct prefixes keep cross-store ids
// unambiguous so a stranded bd-era id never resolves into the wrong store.
//
// BeadClassWork is intentionally absent: work beads stay on bd/Dolt under the
// rig/HQ EffectivePrefix, not a reserved class prefix.
var reservedClassPrefixes = map[string]string{
	BeadClassGraph:     "gcg",
	BeadClassMessaging: "gcm",
	BeadClassSessions:  "gcs",
	BeadClassOrders:    "gco",
	BeadClassNudges:    "gcn",
}

// ReservedClassPrefix returns the reserved id-prefix for a SQLite-relocated
// coordination class (e.g. BeadClassOrders -> "gco"), and whether the class has
// one. Classes without a reserved prefix (e.g. BeadClassWork) return ("", false).
func ReservedClassPrefix(class string) (string, bool) {
	p, ok := reservedClassPrefixes[class]
	return p, ok
}

// ReservedClassPrefixes returns a copy of the class -> reserved-prefix map.
func ReservedClassPrefixes() map[string]string {
	out := make(map[string]string, len(reservedClassPrefixes))
	for class, prefix := range reservedClassPrefixes {
		out[class] = prefix
	}
	return out
}

// IsReservedClassPrefix reports whether p (without a trailing "-") is a reserved
// class id-prefix. Case-insensitive, matching ValidateRigs' prefix handling.
func IsReservedClassPrefix(p string) bool {
	p = strings.ToLower(strings.TrimSpace(p))
	if p == "" {
		return false
	}
	for _, reserved := range reservedClassPrefixes {
		if strings.ToLower(reserved) == p {
			return true
		}
	}
	return false
}

// reservedClassPrefixListText returns the reserved class id-prefixes as a
// sorted, comma-separated string for use in validation error messages.
func reservedClassPrefixListText() string {
	prefixes := make([]string, 0, len(reservedClassPrefixes))
	for _, p := range reservedClassPrefixes {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)
	return strings.Join(prefixes, ", ")
}
