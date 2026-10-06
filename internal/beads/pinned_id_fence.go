package beads

import (
	"fmt"
	"strings"
)

// The pinned-id fence, shared by every store that serves a class binding.
//
// Two providers ship today — SQLite and beads-workspace — and the rule they
// apply is the invariant itself (engdocs/architecture/beads.md, invariant 16),
// not an implementation detail either one owns. Spelling it twice would let the
// two drift, and a binding whose fence disagrees with its sibling's is exactly
// the divergence a namespace claim exists to rule out.

// checkPinnedIDNamespace refuses a pinned id outside the given namespaces.
//
// An empty namespace set leaves the store unfenced, which is the shipped
// default everywhere the store is not a class binding — and how a binding
// serving the work class is opened, since work beads carry whatever prefix an
// operator configured.
//
// An empty id is a MINT request, not a pin, so it is exempt: minting settles
// the ids a store generates and those are the store's own by construction.
//
// The id is tested EXACTLY as it will be stored. Trimming it first would let a
// caller pin "  gcn-1" — or an id that is nothing but spaces — past a check the
// stored row then fails, which is the one outcome the fence exists to prevent:
// a resident bead that no id-shaped lookup of this namespace can reach.
//
// Membership is tested on the SEPARATOR. Plain string containment admits
// "gcnx-1", which belongs to no binding at all, and admits every "gcnq-" id
// through a fence declaring only "gcn", collapsing two namespaces into one.
//
// Case is folded without rewriting: the comparison lowercases a copy, and the
// caller stores the id it was given.
//
// The refusal wraps ErrPinnedIDOutsideNamespace so a caller can route the id to
// a sibling binding instead of parsing the message. op names the store in the
// prose, because the operator reading this in a log needs both halves — which
// id was refused, and which namespaces this binding would have accepted.
func checkPinnedIDNamespace(op, id string, namespaces []string) error {
	if len(namespaces) == 0 || id == "" {
		return nil
	}
	lowered := strings.ToLower(id)
	for _, prefix := range namespaces {
		if strings.HasPrefix(lowered, prefix+"-") {
			return nil
		}
	}
	return fmt.Errorf("%s: id %q is outside this store's namespaces (%s): a pinned id must carry one of them, or use the foreign-id create the store migration uses: %w", op, id, strings.Join(namespaces, ", "), ErrPinnedIDOutsideNamespace)
}

// collectReservedIDPrefixes normalizes a configured namespace set for the
// fence, dropping the empties normalization produces.
//
// It does NOT branch on the result being empty. An option that special-cased
// "no namespaces given" would diverge from production's own unfenced case,
// which is the control the conformance suite needs to tell a fence from a
// blanket refusal.
func collectReservedIDPrefixes(into []string, prefixes []string) []string {
	for _, p := range prefixes {
		if p = normalizeIDPrefix(p); p != "" {
			into = append(into, p)
		}
	}
	return into
}
