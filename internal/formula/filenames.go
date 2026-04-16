package formula

import "strings"

// CanonicalTOMLExt is the canonical extension for formula TOML files under a
// formulas/ directory (formulas/<name>.toml).
const CanonicalTOMLExt = ".toml"

// LegacyTOMLExt is the pre-canonicalization extension for formula TOML files
// (formulas/<name>.formula.toml). Still recognized by discovery so existing
// cities continue to load.
//
// PACKV2-CUTOVER: remove legacy formula filename support after the infix
// migration window closes.
const LegacyTOMLExt = ".formula.toml"

// IsTOMLFilename reports whether path names a TOML formula file in either the
// canonical or legacy infixed form.
func IsTOMLFilename(path string) bool {
	// Check legacy suffix first to stay symmetric with TrimTOMLFilename; the
	// result is the same either way (both suffixes end in ".toml"), but the
	// symmetry avoids a future-reordering hazard.
	return strings.HasSuffix(path, LegacyTOMLExt) || strings.HasSuffix(path, CanonicalTOMLExt)
}

// TrimTOMLFilename returns the formula name encoded in a TOML filename.
func TrimTOMLFilename(path string) (string, bool) {
	switch {
	case strings.HasSuffix(path, LegacyTOMLExt):
		return strings.TrimSuffix(path, LegacyTOMLExt), true
	case strings.HasSuffix(path, CanonicalTOMLExt):
		return strings.TrimSuffix(path, CanonicalTOMLExt), true
	default:
		return "", false
	}
}
