// Package deps provides semver comparison utilities for version checking.
package deps

import (
	"strconv"
	"strings"
)

// CompareVersions compares two semver strings.
// Returns -1 if a < b, 0 if a == b, 1 if a > b.
//
// Input is normalized via [ParseVersion], so leading "v" prefixes and
// pre-release / build metadata suffixes (e.g. "1.2.3-rc.1", "1.2.3+abc")
// are tolerated but ignored. Pre-release ordering per semver spec is NOT
// implemented — "1.2.3" and "1.2.3-rc.1" compare as equal.
func CompareVersions(a, b string) int {
	aParts := ParseVersion(a)
	bParts := ParseVersion(b)

	for i := 0; i < 3; i++ {
		if aParts[i] < bParts[i] {
			return -1
		}
		if aParts[i] > bParts[i] {
			return 1
		}
	}
	return 0
}

// ParseVersion parses a "X.Y.Z" numeric string into [3]int.
//
// The input is normalized before parsing so that common real-world formats
// returned by "<tool> --version" commands are accepted:
//
//   - surrounding whitespace is trimmed
//   - a single leading "v" or "V" prefix is stripped ("v1.2.3" -> "1.2.3")
//   - pre-release and build metadata suffixes are stripped
//     ("1.2.3-rc.1" -> "1.2.3", "1.2.3+build.5" -> "1.2.3")
//
// Missing components default to 0 ("1.2" -> [1, 2, 0]). Non-numeric
// components that survive normalization also default to 0; callers that
// need strict validation should check their input against a regex before
// calling.
func ParseVersion(v string) [3]int {
	v = normalizeVersion(v)
	var parts [3]int
	split := strings.Split(v, ".")
	for i := 0; i < 3 && i < len(split); i++ {
		parts[i], _ = strconv.Atoi(split[i])
	}
	return parts
}

// normalizeVersion trims whitespace, strips a leading "v"/"V", and drops
// any pre-release ("-") or build metadata ("+") suffix per semver 2.0.0.
func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 0 && (v[0] == 'v' || v[0] == 'V') {
		v = v[1:]
	}
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	return v
}
