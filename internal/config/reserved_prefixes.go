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

// reservedAuxiliaryPrefixes maps a class to the prefixes its store holds beads
// under but does NOT mint from — a subsystem inside the class binding that
// mints its own ids.
//
// The nudge queue is the one such subsystem: it derives a queue record's id
// from the nudge it carries (storebinding's nudgeQueueIDPrefix, a content hash
// rather than a sequence) so that redelivering the same nudge is idempotent
// without a lookup. Those records live in the nudges binding and are nudges-class
// beads in every sense that matters to routing; they are simply not minted by
// the store's own sequence, which is the only thing ReservedClassPrefix
// describes.
//
// A prefix here is reserved exactly as strongly as a mint prefix: a rig
// configured with it collides with real ids, and an id carrying it belongs to
// the class binding and nowhere else.
var reservedAuxiliaryPrefixes = map[string][]string{
	BeadClassNudges: {NudgeQueueIDPrefix},
}

// NudgeQueueIDPrefix is the prefix the nudge queue mints its records under.
//
// It is exported and lives HERE, in the package that owns what "reserved" means,
// because two packages have to agree on it and only one of them can own it. The
// queue in internal/storebinding mints ids with it; the reserved-prefix table
// above routes ids carrying it to the nudges binding. A copy in each package
// would let one change alone, and that split is silent in the worst way: the
// minter writes ids under a prefix the router no longer reserves, so queue
// records land in the work ledger, the queue reads its own binding, finds
// nothing, and stops draining without an error anywhere.
//
// The trailing separator is NOT part of it. A reserved prefix is matched against
// the segment before the first "-" (see bdIDIsClassReserved), so carrying the
// dash here would make the table's entry match nothing at all.
const NudgeQueueIDPrefix = "gcnq"

// ReservedClassPrefix returns the id-prefix a SQLite-relocated coordination
// class MINTS under (e.g. BeadClassOrders -> "gco"), and whether the class has
// one. Classes without a reserved prefix (e.g. BeadClassWork) return ("", false).
//
// This is the store's IDPrefix, not the class's namespace union: a caller
// deciding whether an id belongs to the class wants ReservedClassPrefixesFor,
// which also covers the prefixes the class holds without minting.
func ReservedClassPrefix(class string) (string, bool) {
	p, ok := reservedClassPrefixes[class]
	return p, ok
}

// ReservedClassPrefixesFor returns every reserved id-prefix belonging to a
// class — the one it mints under first, then any auxiliary namespaces its
// store holds. Classes with no reserved prefix return nil.
//
// Mint-first ordering is load-bearing for callers that take the head as "the"
// prefix, and it is what lets a binding's namespace list be built by
// concatenation without a second lookup.
func ReservedClassPrefixesFor(class string) []string {
	mint, ok := reservedClassPrefixes[class]
	if !ok {
		return nil
	}
	out := make([]string, 0, 1+len(reservedAuxiliaryPrefixes[class]))
	out = append(out, mint)
	out = append(out, reservedAuxiliaryPrefixes[class]...)
	return out
}

// AllReservedClassPrefixes returns every reserved id-prefix across all classes,
// sorted. It is the namespace union an id is tested against when the class it
// might belong to is not known in advance.
func AllReservedClassPrefixes() []string {
	out := make([]string, 0, len(reservedClassPrefixes)+len(reservedAuxiliaryPrefixes))
	for class := range reservedClassPrefixes {
		out = append(out, ReservedClassPrefixesFor(class)...)
	}
	sort.Strings(out)
	return out
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
	for _, reserved := range AllReservedClassPrefixes() {
		if strings.ToLower(reserved) == p {
			return true
		}
	}
	return false
}

// reservedClassPrefixListText returns the reserved class id-prefixes as a
// sorted, comma-separated string for use in validation error messages.
func reservedClassPrefixListText() string {
	return strings.Join(AllReservedClassPrefixes(), ", ")
}
