package orders

import "strings"

const (
	CanonicalFlatOrderSuffix = ".toml"
	// PACKV2-CUTOVER: remove legacy flat order filename support after the infix migration window closes.
	LegacyFlatOrderSuffix = ".order.toml"
)

// IsFlatOrderFilename reports whether a basename uses the canonical or legacy
// flat order filename form.
func IsFlatOrderFilename(name string) bool {
	return strings.HasSuffix(name, CanonicalFlatOrderSuffix) || strings.HasSuffix(name, LegacyFlatOrderSuffix)
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
