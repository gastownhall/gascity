package orders

import "strings"

// CanonicalFlatOrderSuffix is the canonical extension for flat order TOML
// files under an orders/ directory (orders/<name>.toml).
const CanonicalFlatOrderSuffix = ".toml"

// LegacyFlatOrderSuffix is the supported infixed extension for flat order TOML
// files (orders/<name>.order.toml). The exported name is retained for existing
// internal callers, but this filename spelling is intentionally accepted.
const LegacyFlatOrderSuffix = ".order.toml"

// IsFlatOrderFilename reports whether a basename uses either supported flat
// order filename form.
func IsFlatOrderFilename(name string) bool {
	// Check legacy suffix first to stay symmetric with TrimFlatOrderFilename.
	return strings.HasSuffix(name, LegacyFlatOrderSuffix) || strings.HasSuffix(name, CanonicalFlatOrderSuffix)
}

// TrimFlatOrderFilename returns the order name encoded in a flat filename.
// The optional ".order" infix is not part of the symbolic order name.
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
