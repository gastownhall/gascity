package contract

import "strings"

// exactIDBackends are the metadata backends whose bd resolves a prefixed id
// argument only exactly. bd's HTTP store refuses partial-id search outright
// (internal/httpclient ErrPartialIDSearch, gastownhall/beads#7247; and
// Store.ExactIDLookupOnly, gastownhall/beads#7471, first in beads
// v1.3.1-0.20261010181156-c4e667cb14c3), so a prefixed id sent to it either
// names its issue exactly or the command fails. Every SQL backend instead
// falls back to prefix matching when no issue carries the id exactly (beads
// internal/utils.ResolvePartialID), which is the collision gc's exact-ID write
// guard reads for (gcy-g4o).
//
// gc pins beads v1.3.2, which predates both changes, so no bd-contract cell
// pins this resolution yet.
var exactIDBackends = map[BackendName]struct{}{
	"http": {},
}

// BackendResolvesIDsExactly reports whether bd resolves every prefixed id
// argument (LooksLikePrefixedID) exactly — never by prefix or substring
// match — against a workspace whose .beads/metadata.json names backend.
// Unknown and unset backends report false, so a caller that guards against
// bd's fuzzy resolution keeps guarding.
func BackendResolvesIDsExactly(backend string) bool {
	_, ok := exactIDBackends[BackendName(strings.TrimSpace(backend))]
	return ok
}

// LooksLikePrefixedID reports whether bd takes id as already carrying an issue
// prefix: one to eight lowercase letters or digits, a hyphen, then a suffix
// that starts with a lowercase letter or digit. It mirrors beads
// internal/utils.looksLikePrefixedID. bd's exact-only resolver retries an id
// that misses and fails this test (a bare hash such as "abc") as
// <issue_prefix>-<id>, so on an exact-ID backend only a prefixed id is sure to
// name its issue exactly or fail.
func LooksLikePrefixedID(id string) bool {
	head, suffix, found := strings.Cut(id, "-")
	if !found || head == "" || len(head) > 8 || suffix == "" {
		return false
	}
	for i := 0; i < len(head); i++ {
		if !isLowerAlnum(head[i]) {
			return false
		}
	}
	return isLowerAlnum(suffix[0])
}

func isLowerAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}
