package orders

import "strings"

// CanonicalFlatOrderSuffix is the canonical extension for flat order TOML
// files under an orders/ directory (orders/<name>.toml).
const CanonicalFlatOrderSuffix = ".toml"

// LegacyFlatOrderSuffix is the pre-canonicalization extension for flat order
// TOML files (orders/<name>.order.toml). Still recognized by discovery so
// existing cities continue to load.
//
// PACKV2-CUTOVER: remove legacy flat order filename support after the infix
// migration window closes.
const LegacyFlatOrderSuffix = ".order.toml"

// IsFlatOrderFilename reports whether a basename uses the canonical or legacy
// flat order filename form.
func IsFlatOrderFilename(name string) bool {
	// Check legacy suffix first to stay symmetric with TrimFlatOrderFilename.
	return strings.HasSuffix(name, LegacyFlatOrderSuffix) || strings.HasSuffix(name, CanonicalFlatOrderSuffix)
}

// TrimFlatOrderFilename returns the order name encoded in a flat filename.
func TrimFlatOrderFilename(name string) (string, bool) {
	switch {
	case strings.HasSuffix(name, LegacyFlatOrderSuffix):
		return strings.TrimSuffix(name, LegacyFlatOrderSuffix), true
	case strings.HasSuffix(name, CanonicalFlatOrderSuffix):
		return strings.TrimSuffix(name, CanonicalFlatOrderSuffix), true
	default:
		return "", false
	}
}
