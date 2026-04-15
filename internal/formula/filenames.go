package formula

import "strings"

const (
	CanonicalTOMLExt = ".toml"
	// PACKV2-CUTOVER: remove legacy formula filename support after the infix migration window closes.
	LegacyTOMLExt = ".formula.toml"
)

// IsTOMLFilename reports whether path names a TOML formula file in either the
// canonical or legacy infixed form.
func IsTOMLFilename(path string) bool {
	return strings.HasSuffix(path, CanonicalTOMLExt) || strings.HasSuffix(path, LegacyTOMLExt)
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
