package contract

import "strings"

// exactIDBackends are the metadata backends whose bd resolves an id argument
// only exactly. bd's HTTP store refuses partial-id search outright (beads
// internal/httpclient: ErrPartialIDSearch, and Store.ExactIDLookupOnly), so an
// id sent to it either names its issue exactly or the command fails. Every SQL
// backend instead falls back to prefix matching when no issue carries the id
// exactly (beads internal/utils.ResolvePartialID), which is the collision gc's
// exact-ID write guard reads for (gcy-g4o).
var exactIDBackends = map[BackendName]struct{}{
	"http": {},
}

// BackendResolvesIDsExactly reports whether bd resolves every id argument
// exactly — never by prefix or substring match — against a workspace whose
// .beads/metadata.json names backend. Unknown and unset backends report false,
// so a caller that guards against bd's fuzzy resolution keeps guarding.
func BackendResolvesIDsExactly(backend string) bool {
	_, ok := exactIDBackends[BackendName(strings.TrimSpace(backend))]
	return ok
}
