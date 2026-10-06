package integration

import (
	"regexp"
	"strings"
)

// beadIDERE is the Go twin of BEAD_ID_ERE in test/agents/lib/bead-id.sh: a
// whole-token bead ID matched by shape, never by a hardcoded prefix. Since
// #5416 a bead store's minted prefix is store-configured or derived from the
// city name, and the integration bd shim can mint gc-N into the same store
// where gc mints <prefix>-N, so neither side may assume bd/gc/mc.
//
// These helpers live in their own untagged file, split out of
// bead_id_test.go, because helpers_test.go (integration-tagged) calls
// parseBeadID: an untagged file whose only consumer was also untagged could
// move wholesale into the small unsharded go_test next to it, but this one
// is a dependency of integration-tagged code too, so it stays reachable from
// both go_test targets in this package.
const beadIDERE = `[A-Za-z][A-Za-z0-9]*(-[A-Za-z0-9]+)+([.][A-Za-z0-9]+)*`

var beadIDTokenRE = regexp.MustCompile("^" + beadIDERE + "$")

// isBeadIDToken reports whether s is, in its entirety, shaped like a bead ID.
func isBeadIDToken(s string) bool {
	return beadIDTokenRE.MatchString(s)
}

// parseBeadID extracts a bead ID from bd/gc output by shape, never by
// scanning free text for the shape regex as a substring — preambles contain
// shape-identical words such as "re-initializing".
func parseBeadID(output string) (string, bool) {
	for _, anchor := range []string{"Created bead:", "Created issue:", "Created convoy"} {
		idx := strings.Index(output, anchor)
		if idx < 0 {
			continue
		}
		fields := strings.Fields(output[idx+len(anchor):])
		if len(fields) == 0 {
			continue
		}
		if candidate := strings.TrimRight(fields[0], ":,"); isBeadIDToken(candidate) {
			return candidate, true
		}
	}

	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if isBeadIDToken(fields[0]) {
			return fields[0], true
		}
	}

	return "", false
}
